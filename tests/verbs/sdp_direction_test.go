package verbs

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// SDP media-direction negotiation (RFC 3264 §6.1).
//
// Five9 offers a=sendonly on its initial INVITE. jambonz answered a=sendrecv
// instead of the required complement (recvonly), so Five9 immediately
// re-INVITEd with a=sendonly to force the direction it asked for — and that
// surprise renegotiation broke the media-endpoint update. Fixed in mediajam
// (internal/endpoint/sdp.go + Endpoint.Modify).
//
// These live in tests/verbs, NOT tests/drachtio, on purpose: the drachtio
// package is behind a build tag (its session-timer tests wait 90-150s each),
// so a guard parked there would never run in `make test` / `make test-report`
// / the release gate. These are fast (~3s each) and guard a customer-facing
// carrier interop regression, so they belong in the default suite — and in
// RELEASE_GATE_VERBS.

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

// sdpDirectionApp provisions an Application whose script answers, holds the
// call open for the given seconds, then hangs up — long enough for a test to
// drive re-INVITEs on the answered dialog. Returns the app SID.
//
// The Application name is derived from the test name: Application names are
// unique per account, so a shared literal would 422 with a duplicate-key
// error as soon as two of these tests run in parallel.
func sdpDirectionApp(t *testing.T, ctx context.Context, sess *webhook.Session, holdSecs int) string {
	t.Helper()
	sess.ScriptCallHook(webhook.Script{
		V("answer"),
		V("pause", "length", holdSecs),
		V("hangup"),
	})
	suffix := strings.ToLower(strings.ReplaceAll(strings.TrimPrefix(t.Name(), "Test"), "_", "-"))
	return provisionWebhookApp(t, ctx, suffix)
}

// inviteAppMode INVITEs the given Application with control over the initial
// offer's direction attribute (jsip.InviteOptions.SDPMode).
func inviteAppMode(s *StepCtx, ctx context.Context, uas *UAS, appSID, testID, sdpMode string) *jsip.Call {
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		SDPMode:   sdpMode,
		Headers:   jsip.H{webhook.CorrelationHeader: testID},
	})
	if err != nil {
		s.Fatalf("Invite(SDPMode=%s): %v", sdpMode, err)
	}
	if got := call.AnsweredStatus(); got != 200 {
		s.Fatalf("answered status: got %d want 200", got)
	}
	return call
}

// reinviteDirection sends an in-dialog re-INVITE whose offer is the call's
// current local SDP rewritten to carry the given direction (diago regenerates
// the body with a fresh o= session-version on every LocalSDP call, so it is a
// well-formed new offer), asserts the final response is 200 — framing a 488
// as the reproduced media-endpoint renegotiation failure — and returns it.
func reinviteDirection(s *StepCtx, ctx context.Context, call *jsip.Call, mode string) *sip.Response {
	offer := setSDPDirection(call.LocalSDP(), mode)
	s.Logf("sending a=%s re-INVITE offer:\n%s", mode, offer)

	reCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	res, err := call.SendReinviteWithSDP(reCtx, offer, nil)
	if err != nil {
		s.Fatalf("SendReinviteWithSDP(a=%s): %v", mode, err)
	}
	s.Logf("re-INVITE final response: %d %s; answer SDP:\n%s", res.StatusCode, res.Reason, res.Body())
	if res.StatusCode == 488 {
		s.Fatalf("REPRODUCED: a=%s re-INVITE rejected with 488 Not Acceptable Here "+
			"(media endpoint renegotiation failed). body:\n%s", mode, res.Body())
	}
	if res.StatusCode != 200 {
		s.Fatalf("a=%s re-INVITE: got %d %s, want 200", mode, res.StatusCode, res.Reason)
	}
	return res
}

// requireComplementOfSendonly asserts that an answer SDP to an a=sendonly
// offer carries the RFC 3264 §6.1 complement: recvonly (or inactive). An
// explicit sendrecv — or no direction attribute at all, which RFC 4566
// defines as implied sendrecv — is the Five9 interop bug.
func requireComplementOfSendonly(s *StepCtx, label string, sdp []byte) {
	switch dir := sdpDirection(sdp); dir {
	case "recvonly":
		// The expected complement.
	case "inactive":
		// RFC-valid answer to sendonly, but odd for a media server that
		// intends to receive the caller's audio — worth an eyeball.
		s.Logf("%s: answered a=inactive to our a=sendonly offer — RFC-valid but unusual", label)
	case "":
		s.Fatalf("REPRODUCED %s: answer SDP has no direction attribute (implied sendrecv) "+
			"for our a=sendonly offer; RFC 3264 requires recvonly/inactive. "+
			"Five9 reacts to this with a corrective a=sendonly re-INVITE. answer SDP:\n%s",
			label, sdp)
	default:
		s.Fatalf("REPRODUCED %s: answer SDP direction is a=%s for our a=sendonly offer; "+
			"RFC 3264 requires recvonly/inactive. "+
			"Five9 reacts to this with a corrective a=sendonly re-INVITE. answer SDP:\n%s",
			label, dir, sdp)
	}
}

// TestSDP_Sendonly_InitialOffer — the trigger of the Five9 interop bug: an
// initial INVITE offering one-way media (a=sendonly) must be answered with
// the RFC 3264 §6.1 complement, not a=sendrecv.
//
// Steps:
//   - script-answer-pause-hangup
//   - provision-application
//   - invite-sendonly-expect-200
//   - assert-offer-carried-sendonly
//   - assert-answer-complement
func TestSDP_Sendonly_InitialOffer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause-hangup")
	appSID := sdpDirectionApp(t, ctx, sess, 2)
	s.Done()

	s = Step(t, "provision-application")
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-sendonly-expect-200")
	call := inviteAppMode(s, ctx, uas, appSID, testID, "sendonly")
	t.Cleanup(func() { _ = call.Hangup() })
	s.Done()

	s = Step(t, "assert-offer-carried-sendonly")
	// Asserted on the INVITE body as transmitted, NOT on LocalSDP(): diago
	// folds the far end's answer into the media session's negotiated mode, so
	// an RFC-valid a=inactive answer would flip LocalSDP to inactive and
	// falsely implicate the harness plumbing.
	if dir := sdpDirection(call.InviteOfferSDP()); dir != "sendonly" {
		s.Fatalf("transmitted INVITE offer carries a=%q, want sendonly", dir)
	}
	s.Done()

	s = Step(t, "assert-answer-complement")
	answer, ok := call.AnsweredResponse()
	if !ok || answer.RawResponse == nil {
		s.Fatal("no recorded 200 OK response on the call")
	}
	s.Logf("200 OK answer SDP:\n%s", answer.RawResponse.Body())
	requireComplementOfSendonly(s, "initial INVITE", answer.RawResponse.Body())
	s.Done()
}

// TestSDP_Sendonly_ReinviteLoop — the second half of the Five9 sequence: when
// the answer is not the complement, Five9 re-INVITEs with a=sendonly to force
// it. That renegotiation must be answered 200 with the complement and must
// not tear the dialog down. Performed regardless of what the initial answer
// said, so the test stays meaningful on both fixed and buggy builds.
//
// Steps:
//   - script-answer-pause-hangup
//   - invite-sendonly-expect-200
//   - corrective-reinvite-sendonly
//   - assert-dialog-survived
func TestSDP_Sendonly_ReinviteLoop(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause-hangup")
	appSID := sdpDirectionApp(t, ctx, sess, 20)
	s.Done()

	s = Step(t, "invite-sendonly-expect-200")
	call := inviteAppMode(s, ctx, uas, appSID, testID, "sendonly")
	t.Cleanup(func() { _ = call.Hangup() })
	if answer, ok := call.AnsweredResponse(); ok && answer.RawResponse != nil {
		s.Logf("initial answer direction: %q (\"\" = implied sendrecv)",
			sdpDirection(answer.RawResponse.Body()))
	}
	s.Done()

	s = Step(t, "corrective-reinvite-sendonly")
	res := reinviteDirection(s, ctx, call, "sendonly")
	requireComplementOfSendonly(s, "corrective re-INVITE", res.Body())
	s.Done()

	s = Step(t, "assert-dialog-survived")
	if state := call.State(); state != jsip.StateAnswered {
		s.Fatalf("dialog did not survive the a=sendonly re-INVITE: state=%v", state)
	}
	s.Done()
}

// TestSDP_Sendonly_HoldResume — mid-call direction flips, the general shape
// behind the Five9 case (and behind carrier hold/music-on-hold): an
// established sendrecv call is re-INVITEd to a=sendonly, then back. The
// resume step is the one that matters most — it proves media-endpoint updates
// still work AFTER a one-way renegotiation, the surface reported broken.
//
// Steps:
//   - script-answer-pause-hangup
//   - invite-sendrecv-expect-200
//   - hold-reinvite-sendonly
//   - resume-reinvite-sendrecv
//   - assert-dialog-survived
func TestSDP_Sendonly_HoldResume(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause-hangup")
	appSID := sdpDirectionApp(t, ctx, sess, 20)
	s.Done()

	s = Step(t, "invite-sendrecv-expect-200")
	call := inviteAppMode(s, ctx, uas, appSID, testID, "")
	t.Cleanup(func() { _ = call.Hangup() })
	s.Done()

	s = Step(t, "hold-reinvite-sendonly")
	hold := reinviteDirection(s, ctx, call, "sendonly")
	requireComplementOfSendonly(s, "hold re-INVITE", hold.Body())
	s.Done()

	s = Step(t, "resume-reinvite-sendrecv")
	resume := reinviteDirection(s, ctx, call, "sendrecv")
	// "" (no attribute) is implied sendrecv per RFC 4566 — accept both.
	if dir := sdpDirection(resume.Body()); dir != "sendrecv" && dir != "" {
		s.Fatalf("resume answer direction: got a=%s, want sendrecv — media endpoint "+
			"stuck in the hold direction. answer SDP:\n%s", dir, resume.Body())
	}
	s.Done()

	s = Step(t, "assert-dialog-survived")
	if state := call.State(); state != jsip.StateAnswered {
		s.Fatalf("dialog did not survive hold/resume renegotiation: state=%v", state)
	}
	s.Done()
}

// TestSDP_OfferMode_EarlyAnswerDirectionChange — the OTHER side of the
// direction fix: the leg where jambonz is the OFFERER.
//
// Everything above drives the leg where jambonz answers our offer. When
// jambonz dials out (the `dial` verb's B leg) it offers, and the far end's
// SDP arrives as an ANSWER — possibly more than once, since a carrier may
// answer first in a 183 and again in the 200. A media server must never
// re-render its own offer as an answer on that path; doing so replaces the
// multi-codec offer with a single-codec body (and, once the direction
// differs between the two answers, flips its direction too), which corrupts
// the leg. mediajam's first cut of the direction fix had exactly that bug —
// it guarded only the FIRST answer.
//
// This drives the sequence over real SIP: our callee answers the dial leg
// twice with DIFFERENT directions — 183 carrying a=recvonly, then 200
// carrying sendrecv (diago's negotiated answer). The observable is
// end-to-end: the dial must complete and real audio must bridge caller <->
// callee afterwards, which it cannot do if the B-leg SDP was rewritten
// mid-sequence.
//
// Steps:
//   - resolve-fixture
//   - script-dial-to-callee
//   - spawn-callee-goroutine
//   - place-caller-and-record
//   - wait-callee-done
//   - wait-action-dial-callback
//   - assert-dial-status
//   - assert-bridge-audio
func TestSDP_OfferMode_EarlyAnswerDirectionChange(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-dial-to-callee")
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{"type": "user", "name": target}},
			"timeout", 30,
			"actionHook", SessionURL(sess, "dial"),
			// anchorMedia keeps both legs' RTP inside the cluster; without it
			// the two legs can negotiate a peer-to-peer SDP using each side's
			// private NAT address and no audio crosses (see dial_test.go).
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			// Answer #1: a 183 with SDP, deliberately a=recvonly — a
			// direction the eventual 200 will NOT carry.
			sdp, err := jsip.EarlyMediaSDPWithDirection("PCMU",
				earlyMediaSDPHost, earlyMediaSDPPort, 2000, "recvonly")
			if err != nil {
				GoroutineFailf(t, "callee:build-sdp", "EarlyMediaSDPWithDirection: %v", err)
				return
			}
			t.Logf("[callee:183] sending Session Progress with a=recvonly")
			if err := c.SendEarlyMedia183(sdp); err != nil {
				GoroutineFailf(t, "callee:183", "SendEarlyMedia183: %v", err)
				return
			}
			time.Sleep(2 * time.Second)
			// Answer #2: the real 200 OK, which diago answers sendrecv.
			t.Logf("[callee:answer] start (200 OK, sendrecv)")
			if err := c.Answer(); err != nil {
				t.Logf("[callee:answer] FAILED (leg may have been torn down): %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-prime", "SendSilence: %v", err)
				return
			}
			time.Sleep(RecognizerArmDelay)
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "callee:send-wav", "SendWAV: %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-trail", "SendSilence: %v", err)
				return
			}
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
			}
			<-c.Done()
		case <-calleeCtx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
		}
	}()
	t.Cleanup(func() {
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("sdp-offermode-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-callee-done")
	<-calleeDone
	s.Done()

	s = Step(t, "wait-action-dial-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/dial")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/dial: %v", err)
	}
	s.Logf("action/dial body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-dial-status")
	dcs := cb.String("dial_call_status")
	if dcs != "completed" {
		s.Errorf("dial_call_status: got %q want \"completed\" (dial_sip_status=%d) — "+
			"the two-answers-different-direction sequence broke the outbound leg",
			dcs, cb.Int("dial_sip_status"))
	}
	s.Done()

	s = Step(t, "assert-bridge-audio")
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	// If the offering side re-rendered its own offer as an answer partway
	// through, the B leg's media is wrong and the bridge carries silence.
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}
