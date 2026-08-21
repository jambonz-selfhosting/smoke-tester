// Tests for `sip_reason_header` on call status events.
//
// Carriers fronting ISDN/E1 PRI trunks put the authoritative disconnect cause in an
// RFC 3326 Reason header rather than in the SIP status line - "408 Request Timeout" +
// "Reason: Q.850 ;cause=18" is a call nobody answered, not a fault - and the same SIP
// status can carry different causes, so the status code alone cannot classify a call.
// jambonz surfaces that header as `sip_reason_header`.
//
// There is one test per RELAY path, because the header reaches the feature server three
// structurally different ways and a regression in any one is invisible to the others:
//
//	failure response  sbc-outbound proxies all response headers
//	BYE               sbc-inbound copies every header except a small immutable set
//	CANCEL            drachtio builds a FRESH request and copies a hardcoded allowlist
//
// The CANCEL path is the most fragile of the three - an srf upgrade that drops Reason
// from that allowlist would break it silently - and the one where the header is worth
// most, since jambonz generates the 487 and its reason phrase itself, leaving every
// abandoned call otherwise identical.
//
// Phase-2 tests; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Distinct cause per test so a cross-test leak cannot false-pass. Sent with the
// "Q.850 ;cause=N" spacing real carriers use, to pin what jambonz does with it.
const (
	outboundReasonHeader = "Q.850 ;cause=31" // normal, unspecified
	inboundReasonHeader  = "Q.850 ;cause=16" // normal call clearing
	// a CANCEL carries a SIP cause, not Q.850: the "another branch answered" case
	cancelReasonHeader = `SIP ;cause=200 ;text="Call completed elsewhere"`
)

// TestSIPReasonHeader_OutboundFailure — POST /Calls, far end rejects with 480 + a Q.850
// cause. This is the REST outdial path (RestCallSession).
func TestSIPReasonHeader_OutboundFailure(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "place-outbound-call")
	// blocks until jambonz's INVITE reaches our UAS, so `call` is the leg we reject
	callSid, call := placeWebhookCallToWithSID(ctx, t, uas, sess)
	s.Logf("jambonz call_sid=%s", callSid)
	s.Done()

	s = Step(t, "reject-with-reason-header")
	// 480 rather than 486/487: jambonz maps those two from the status code alone, so
	// they would not show that the header is what carries the detail.
	if err := call.RejectWithHeaders(480, "Temporarily Unavailable",
		sip.NewHeader("Reason", outboundReasonHeader)); err != nil {
		s.Fatalf("RejectWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	assertReasonHeader(s, DrainCallbacks(sess, 5*time.Second), "failed", 480, outboundReasonHeader)
	s.Done()
}

// TestSIPReasonHeader_InboundBye — a UAC calls in, jambonz answers, the UAC hangs up
// with a Q.850 cause on the BYE. The answered-call path (InboundCallSession): the cause
// explains why a call that DID connect ended.
func TestSIPReasonHeader_InboundBye(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause")
	// long pause so WE end the call: if jambonz hung up first the BYE would travel the
	// other way and carry no header
	sess.ScriptCallHook(webhook.Script{V("answer"), V("pause", "length", 20)})
	appSID := provisionWebhookApp(t, ctx, "sipreason-bye-app")
	s.Done()

	s = Step(t, "invite-app-uri")
	call, err := inviteApp(ctx, uas, appSID, testID)
	if err != nil {
		s.Fatalf("Invite: %v", err)
	}
	t.Cleanup(func() { _ = call.Hangup() })
	if got := call.AnsweredStatus(); got != 200 {
		s.Fatalf("answered status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "bye-with-reason-header")
	if err := call.HangupWithHeaders(ctx, sip.NewHeader("Reason", inboundReasonHeader)); err != nil {
		s.Fatalf("HangupWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	assertReasonHeader(s, DrainCallbacks(sess, 5*time.Second), "completed", 200, inboundReasonHeader)
	s.Done()
}

// TestSIPReasonHeader_InboundCancel — a UAC calls in and CANCELs before the call is
// answered. Needs early media (say + earlyMedia): once a call is answered there is no
// INVITE transaction left to CANCEL, so 183 + SDP is what keeps it cancelable while a
// verb runs.
func TestSIPReasonHeader_InboundCancel(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-say-early-media")
	// earlyMedia keeps the call unanswered: 183 + SDP and TTS instead of a 200 OK
	sess.ScriptCallHook(webhook.Script{
		V("say", "text", "This call is deliberately left unanswered while the smoke test "+
			"cancels it, so please keep talking for a good few seconds without stopping.",
			"earlyMedia", true),
	})
	appSID := provisionWebhookApp(t, ctx, "sipreason-cancel-app")
	s.Done()

	s = Step(t, "invite-and-await-early-media")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	pending, err := uas.Stack.InviteEarlyMedia(ctx, dest, inviteOpts(uas, testID))
	if err != nil {
		s.Fatalf("InviteEarlyMedia: %v", err)
	}
	t.Cleanup(func() { _ = pending.Close() })
	s.Done()

	s = Step(t, "cancel-with-reason-header")
	if err := pending.CancelWithHeaders(ctx, sip.NewHeader("Reason", cancelReasonHeader)); err != nil {
		s.Fatalf("CancelWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	// 487 and "Request Terminated" are jambonz's own on every abandoned call, which is
	// exactly why the header is the only thing that distinguishes them
	assertReasonHeader(s, DrainCallbacks(sess, 5*time.Second), "no-answer", 487, cancelReasonHeader)
	s.Done()
}

// --- helpers ---------------------------------------------------------------------

func inviteOpts(uas *UAS, testID string) jsip.InviteOptions {
	return jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers:   jsip.H{webhook.CorrelationHeader: testID},
	}
}

func inviteApp(ctx context.Context, uas *UAS, appSID, testID string) (*jsip.Call, error) {
	return uas.Stack.Invite(ctx, fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm), inviteOpts(uas, testID))
}

// normalizeReason collapses the optional whitespace RFC 3326 permits around ';'.
// Relaying re-serializes the header, so a carrier's "Q.850 ;cause=18" arrives as
// "Q.850;cause=18" (confirmed on the wire) - a legal rewrite, so comparisons must not be
// byte-exact. Spaces inside text="..." survive unless that value contains a ';'.
func normalizeReason(s string) string {
	parts := strings.Split(s, ";")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ";")
}

// assertReasonHeader checks that the wantStatus event carries wantHeader, and that NO
// other status event does - the header describes the message that caused one particular
// change, so it must not linger onto the next.
//
// Matching on call_status alone is unambiguous: each test owns its webhook session and
// drives a single call whose terminal status is distinct.
func assertReasonHeader(s *StepCtx, cbs []webhook.Callback, wantStatus string, wantSipStatus int, wantHeader string) {
	var seen, carriers []string
	found := false

	for _, cb := range cbs {
		if cb.Hook != "call_status_hook" {
			continue
		}
		status := cb.NestedString("call_status")
		hdr := cb.NestedString("sip_reason_header")
		seen = append(seen, fmt.Sprintf("%s(sip_status=%d, sip_reason=%q, sip_reason_header=%q)",
			status, cb.Int("sip_status"), cb.NestedString("sip_reason"), hdr))
		if hdr != "" {
			carriers = append(carriers, status)
		}
		if status != wantStatus {
			continue
		}
		found = true
		if normalizeReason(hdr) != normalizeReason(wantHeader) {
			s.Errorf("sip_reason_header on %q: got %q want %q (whitespace-normalized)", status, hdr, wantHeader)
		}
		if got := cb.Int("sip_status"); got != wantSipStatus {
			s.Errorf("sip_status on %q: got %d want %d", status, got, wantSipStatus)
		}
		// a change that repurposed sip_reason instead of adding a field passes the
		// check above and fails here
		if got := cb.NestedString("sip_reason"); normalizeReason(got) == normalizeReason(wantHeader) {
			s.Errorf("sip_reason was overwritten with the Reason header (%q)", got)
		}
	}

	if !found {
		s.Errorf("no %q status event; saw %v", wantStatus, seen)
		return
	}
	if len(carriers) != 1 || carriers[0] != wantStatus {
		s.Errorf("sip_reason_header must appear on %q only, saw it on %v", wantStatus, carriers)
	}
	s.Logf("status events: %v", seen)
}
