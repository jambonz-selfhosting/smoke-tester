//go:build drachtio

package drachtio

import (
	"context"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// sdpDirRe matches an SDP direction attribute line. The trailing \r is
// captured so replacements preserve CRLF line endings.
var sdpDirRe = regexp.MustCompile(`(?m)^a=(sendrecv|sendonly|recvonly|inactive)(\r?)$`)

// sdpDirection returns the effective direction attribute of sdp. When both a
// session-level and a media-level attribute are present the media-level one
// (last in a single-m-line body) wins; when none is present RFC 4566 defines
// the session as implicitly sendrecv, reported here as "" so callers can
// distinguish explicit from implied.
func sdpDirection(sdp []byte) string {
	ms := sdpDirRe.FindAllSubmatch(sdp, -1)
	if len(ms) == 0 {
		return ""
	}
	return string(ms[len(ms)-1][1])
}

// setSDPDirection returns a copy of sdp with every direction attribute
// replaced by mode (or with "a=<mode>" appended when none is present).
func setSDPDirection(sdp []byte, mode string) []byte {
	if sdpDirRe.Match(sdp) {
		return sdpDirRe.ReplaceAll(sdp, []byte("a="+mode+"${2}"))
	}
	out := append([]byte(nil), sdp...)
	return append(out, []byte("a="+mode+"\r\n")...)
}

// inviteAppMode is inviteApp with control over the initial offer's SDP
// direction attribute (jsip.InviteOptions.SDPMode).
func inviteAppMode(t *testing.T, ctx context.Context, uas *jsip.Stack, sdpMode string) (*jsip.Call, error) {
	t.Helper()
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	return uas.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		SDPMode:   sdpMode,
	})
}

// reinviteDirection sends an in-dialog re-INVITE whose offer is the call's
// current local SDP rewritten to carry the given direction (diago regenerates
// the body with a fresh o= session-version on every LocalSDP call, so it is a
// well-formed new offer), asserts the final response is 200 — framing a 488
// as the reproduced media-endpoint renegotiation failure — and returns it.
func reinviteDirection(t *testing.T, ctx context.Context, call *jsip.Call, mode, label string) *sip.Response {
	t.Helper()
	offer := setSDPDirection(call.LocalSDP(), mode)
	t.Logf("%s: sending a=%s re-INVITE offer:\n%s", label, mode, offer)

	reCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := call.SendReinviteWithSDP(reCtx, offer, nil)
	if err != nil {
		t.Fatalf("%s: SendReinviteWithSDP: %v", label, err)
	}
	t.Logf("%s: re-INVITE final response: %d %s; answer SDP:\n%s",
		label, res.StatusCode, res.Reason, res.Body())
	if res.StatusCode == 488 {
		t.Fatalf("REPRODUCED %s: a=%s re-INVITE rejected with 488 Not Acceptable Here "+
			"(media endpoint renegotiation failed). body:\n%s", label, mode, res.Body())
	}
	if res.StatusCode != 200 {
		t.Fatalf("%s: a=%s re-INVITE: got %d %s, want 200", label, mode, res.StatusCode, res.Reason)
	}
	return res
}

// requireComplementOfSendonly asserts that an answer SDP to an a=sendonly
// offer carries the RFC 3264 §6.1 complement: recvonly (or inactive). An
// explicit sendrecv — or no direction attribute at all, which RFC 4566
// defines as implied sendrecv — reproduces the Five9 interop bug: Five9
// sees the non-complementary answer and immediately re-INVITEs with
// a=sendonly to correct it, and that surprise media renegotiation is what
// broke the freeswitch endpoint update.
func requireComplementOfSendonly(t *testing.T, label string, sdp []byte) {
	t.Helper()
	switch dir := sdpDirection(sdp); dir {
	case "recvonly":
		// The expected complement.
	case "inactive":
		// RFC-valid answer to sendonly, but odd for a media server that
		// intends to receive the caller's audio — worth an eyeball.
		t.Logf("%s: answered a=inactive to our a=sendonly offer — RFC-valid but unusual", label)
	case "":
		t.Fatalf("REPRODUCED %s: answer SDP has no direction attribute (implied sendrecv) "+
			"for our a=sendonly offer; RFC 3264 requires recvonly/inactive. "+
			"Five9 reacts to this with a corrective a=sendonly re-INVITE. answer SDP:\n%s",
			label, sdp)
	default:
		t.Fatalf("REPRODUCED %s: answer SDP direction is a=%s for our a=sendonly offer; "+
			"RFC 3264 requires recvonly/inactive. "+
			"Five9 reacts to this with a corrective a=sendonly re-INVITE. answer SDP:\n%s",
			label, dir, sdp)
	}
}

// TestDrachtio_Sendonly_InitialOffer — reproduces the trigger of the Five9
// interop bug. Five9 sends the initial INVITE with a one-way-media offer
// (a=sendonly); jambonz answered 200 OK with a=sendrecv instead of the
// RFC 3264 §6.1 complement (recvonly/inactive). Five9 then re-INVITEs with
// a=sendonly to force the direction it asked for, and that renegotiation
// broke the freeswitch endpoint update (fixed in mediajam).
//
// Steps:
//  1. INVITE the inline answer+pause Application with an a=sendonly offer;
//     assert 200.
//  2. Sanity-check the harness actually offered sendonly, asserted on the
//     transmitted INVITE body.
//  3. Assert the 200 OK SDP direction is recvonly (or inactive), not
//     sendrecv — explicit or implied.
func TestDrachtio_Sendonly_InitialOffer(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteAppMode(t, ctx, uas, "sendonly")
	if err != nil {
		t.Fatalf("inviteAppMode(sendonly): %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	// Plumbing guard: the INVITE body as transmitted must carry a=sendonly.
	// Asserted on the wire bytes (InviteOfferSDP), NOT the regenerated local
	// SDP — diago folds the far end's answer into the local mode, so an
	// RFC-valid a=inactive answer would flip LocalSDP to inactive and
	// falsely implicate the harness plumbing.
	if dir := sdpDirection(call.InviteOfferSDP()); dir != "sendonly" {
		t.Fatalf("harness offer direction: transmitted INVITE offer carries %q, want sendonly", dir)
	}

	answer, ok := call.AnsweredResponse()
	if !ok || answer.RawResponse == nil {
		t.Fatalf("no recorded 200 OK response on the call")
	}
	answerSDP := answer.RawResponse.Body()
	t.Logf("200 OK answer SDP to a=sendonly offer:\n%s", answerSDP)

	requireComplementOfSendonly(t, "initial INVITE", answerSDP)
}

// TestDrachtio_Sendonly_ReinviteLoop — the second half of the Five9 sequence.
// When jambonz answers a sendonly offer with sendrecv, Five9 immediately
// sends an in-dialog re-INVITE re-asserting a=sendonly. On the freeswitch
// media path that surprise renegotiation broke the endpoint update; a
// healthy cluster answers 200 with the recvonly complement and keeps the
// dialog up. This test performs Five9's corrective re-INVITE regardless of
// what the initial answer said, so it stays meaningful on both fixed and
// buggy builds.
//
// Steps:
//  1. INVITE the inline answer+pause Application with an a=sendonly offer;
//     assert 200 (initial answer direction is logged, not asserted — the
//     initial-offer test owns that assertion).
//  2. Send an in-dialog re-INVITE re-asserting a=sendonly; assert the final
//     response is 200 — 488/5xx means the media endpoint renegotiation broke.
//  3. Assert the re-INVITE 200 OK SDP direction is recvonly (or inactive).
//  4. Assert the dialog survived the renegotiation.
func TestDrachtio_Sendonly_ReinviteLoop(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteAppMode(t, ctx, uas, "sendonly")
	if err != nil {
		t.Fatalf("inviteAppMode(sendonly): %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}
	if answer, ok := call.AnsweredResponse(); ok && answer.RawResponse != nil {
		t.Logf("initial answer direction: %q (\"\" = implied sendrecv)",
			sdpDirection(answer.RawResponse.Body()))
	}

	res := reinviteDirection(t, ctx, call, "sendonly", "corrective re-INVITE")
	requireComplementOfSendonly(t, "re-INVITE", res.Body())

	if state := call.State(); state != jsip.StateAnswered {
		t.Fatalf("dialog did not survive the a=sendonly re-INVITE: state=%v", state)
	}
}

// TestDrachtio_Sendonly_HoldResume — mid-call direction flips, the general
// shape behind the Five9 case (and behind carrier hold/music-on-hold): an
// established sendrecv call is re-INVITEd to a=sendonly, then back to
// a=sendrecv. Each flip forces a media-endpoint update on the jambonz side;
// the resume step proves endpoint updates still work AFTER a sendonly
// renegotiation — the exact surface reported broken on freeswitch.
//
// Steps:
//  1. INVITE the inline answer+pause Application with a default (sendrecv)
//     offer; assert 200.
//  2. Re-INVITE flipping the direction to a=sendonly (hold); assert 200 and
//     a recvonly/inactive answer.
//  3. Re-INVITE back to a=sendrecv (resume); assert 200 and a sendrecv
//     answer (explicit or implied) — a stuck recvonly here means the
//     endpoint never recovered from the hold.
//  4. Assert the dialog survived both renegotiations.
func TestDrachtio_Sendonly_HoldResume(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteApp(t, ctx, uas, nil)
	if err != nil {
		t.Fatalf("inviteApp: %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	hold := reinviteDirection(t, ctx, call, "sendonly", "hold re-INVITE")
	requireComplementOfSendonly(t, "hold re-INVITE", hold.Body())

	resume := reinviteDirection(t, ctx, call, "sendrecv", "resume re-INVITE")
	// "" (no attribute) is implied sendrecv per RFC 4566 — accept both.
	if dir := sdpDirection(resume.Body()); dir != "sendrecv" && dir != "" {
		t.Fatalf("resume answer direction: got a=%s, want sendrecv — "+
			"media endpoint stuck in the hold direction. answer SDP:\n%s", dir, resume.Body())
	}

	if state := call.State(); state != jsip.StateAnswered {
		t.Fatalf("dialog did not survive hold/resume renegotiation: state=%v", state)
	}
}
