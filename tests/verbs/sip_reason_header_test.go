// Tests for `sip_reason_header` on call status events.
//
// Carriers fronting ISDN/E1 PRI trunks put the authoritative disconnect cause
// in an RFC 3326 Reason header rather than in the SIP status line:
//
//	SIP/2.0 408 Request Timeout
//	Reason: Q.850 ;cause=18
//
// Q.850 cause 18 is "no user responding" - nobody answered, not a platform
// fault. Two different causes also arrive under one SIP status (503 with
// cause=38 network-out-of-order, or cause=41 temporary-failure), so the status
// code alone cannot classify the outcome. jambonz therefore surfaces the header
// verbatim as `sip_reason_header` on the call status webhook.
//
// ---- why both outbound AND inbound origination -----------------------------
// The header is read from whichever SIP message caused the status change, and
// jambonz has a DIFFERENT call-session subclass per origination direction:
//
//   - OUTBOUND (POST /Calls, application_sid) -> RestCallSession, header read
//     off the final failure response to jambonz's INVITE
//   - INBOUND  (UAC dials sip:app-<sid>@realm) -> InboundCallSession, header
//     read off the caller's BYE
//
// Those are separate emit sites feeding a shared derivation, so a regression
// that wires up only one is invisible to a single-direction test. The outbound
// case is also the one that matters commercially - a call nobody answered
// currently reports call_status "failed", and the cause is what lets a
// consumer tell that apart from a real fault.
//
// Phase-2 tests; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Distinctive causes per direction so a cross-test leak cannot false-pass.
//
// We deliberately SEND the "Q.850 ;cause=N" spacing (the variant real carriers
// emit alongside the compact one) to pin what jambonz does with it. Verified on
// the wire: the SBC re-serializes the header when relaying to the feature
// server, so the compact "Q.850;cause=N" is what actually arrives:
//
//	14.226.234.142 -> 10.0.197.31:5060   Reason: Q.850 ;cause=31   (as sent)
//	10.0.197.31:5060 -> :5070            Reason: Q.850;cause=31    (to fs)
//
// RFC 3326 makes that whitespace optional, so this is a legal rewrite, not a
// bug - but it does mean sip_reason_header is the header as the feature server
// received it, NOT necessarily the carrier's exact bytes. Assertions therefore
// compare with normalizeReason rather than byte-for-byte.
const (
	outboundReasonHeader = "Q.850 ;cause=31" // normal, unspecified
	inboundReasonHeader  = "Q.850 ;cause=16" // normal call clearing
)

// normalizeReason collapses the optional whitespace RFC 3326 permits around the
// ';' parameter separator, so a comparison is insensitive to a relay rewriting
// it. Whitespace inside a quoted text="..." value is preserved unless that
// value itself contains a ';' - good enough here, as none of these do.
func normalizeReason(s string) string {
	parts := strings.Split(s, ";")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(p)
	}
	return strings.Join(parts, ";")
}

// TestSIPReasonHeader_OutboundFailure — jambonz places an outbound call
// (POST /Calls) that the far end rejects with 480 + a Q.850 Reason header. The
// "failed" status event must carry that header verbatim.
//
// Steps:
//  1. claim-session — webhook session for status-callback correlation
//  2. place-outbound-call — POST /Calls to our registered SIP client
//  3. reject-with-reason-header — 480 Temporarily Unavailable + Reason: Q.850 ;cause=31
//  4. drain-status-callbacks — wait for the terminal status event
//  5. assert-sip-reason-header — failed + sip_status 480 + sip_reason_header
//
// Test    --POST /Calls to=<uas>-->                  Jambonz
// Jambonz --INVITE-->                                 UAS (us)
// UAS     --480 + Reason: Q.850 ;cause=31-->          Jambonz
// Jambonz --call_status_hook {call_status: failed}--> Webhook server
func TestSIPReasonHeader_OutboundFailure(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "place-outbound-call")
	// placeWebhookCallToWithSID blocks until jambonz's INVITE reaches our UAS,
	// so `call` is the leg we are about to reject.
	callSid, call := placeWebhookCallToWithSID(ctx, t, uas, sess)
	s.Logf("jambonz call_sid=%s, INVITE received", callSid)
	s.Done()

	s = Step(t, "reject-with-reason-header")
	// 480 rather than 486/487: those two are mapped to busy/no-answer by
	// jambonz on their status code alone, so they would not demonstrate that
	// the Reason header is what carries the detail.
	if err := call.RejectWithHeaders(480, "Temporarily Unavailable",
		sip.NewHeader("Reason", outboundReasonHeader)); err != nil {
		s.Fatalf("RejectWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "drain-status-callbacks")
	cbs := DrainCallbacks(sess, 5*time.Second)
	s.Logf("drained %d callbacks", len(cbs))
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	assertReasonHeaderOnTerminalStatus(s, cbs, callSid, reasonExpectation{
		wantHeader:    outboundReasonHeader,
		wantStatuses:  []string{"failed"},
		wantSipStatus: 480,
		wantDirection: "outbound",
	})
	s.Done()
}

// TestSIPReasonHeader_InboundBye — a UAC calls into a jambonz Application and
// hangs up with a Q.850 Reason header on the BYE. The "completed" status event
// must carry that header verbatim.
//
// This is the answered-call case: the cause explains why a call that DID
// connect ended, which is the half a failure-path-only implementation misses.
//
// Steps:
//  1. script-answer-pause — call_hook returns [answer, pause] so jambonz holds the call
//  2. provision-application — Application bound to our webhook tunnel
//  3. invite-app-uri — UAC INVITE sip:app-<sid>@<realm>
//  4. bye-with-reason-header — BYE + Reason: Q.850 ;cause=16
//  5. drain-status-callbacks / assert-sip-reason-header
//
// Test    --INVITE sip:app-<sid>@realm-->               Jambonz
// Jambonz --200 OK-->                                    Test
// Test    --BYE + Reason: Q.850 ;cause=16-->             Jambonz
// Jambonz --call_status_hook {call_status: completed}--> Webhook server
func TestSIPReasonHeader_InboundBye(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause")
	// pause long enough that WE are the side that ends the call - if jambonz
	// hung up first the BYE would travel the other way and carry no header.
	sess.ScriptCallHook(webhook.Script{
		V("answer"),
		V("pause", "length", 20),
	})
	s.Done()

	s = Step(t, "provision-application")
	appSID := provisionWebhookApp(t, ctx, "sipreason-app")
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-app-uri")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers: jsip.H{
			webhook.CorrelationHeader: testID,
		},
	})
	if err != nil {
		s.Fatalf("Invite: %v", err)
	}
	// Registered before any assertion that can Fatalf. Hangup is idempotent
	// and HangupWithHeaders marks the call ended, so this is a no-op on the
	// happy path.
	t.Cleanup(func() { _ = call.Hangup() })
	if got := call.AnsweredStatus(); got != 200 {
		s.Fatalf("answered status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "bye-with-reason-header")
	if err := call.HangupWithHeaders(ctx,
		sip.NewHeader("Reason", inboundReasonHeader)); err != nil {
		s.Fatalf("HangupWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "drain-status-callbacks")
	cbs := DrainCallbacks(sess, 5*time.Second)
	s.Logf("drained %d callbacks", len(cbs))
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	// The inbound leg's call_sid is jambonz's own, which the UAC never learns,
	// so match on direction+status instead of call_sid.
	assertReasonHeaderOnTerminalStatus(s, cbs, "", reasonExpectation{
		wantHeader:    inboundReasonHeader,
		wantStatuses:  []string{"completed"},
		wantDirection: "inbound",
	})
	s.Done()
}

type reasonExpectation struct {
	wantHeader    string
	wantStatuses  []string
	wantSipStatus int    // 0 = don't check
	wantDirection string // "" = don't check
}

// assertReasonHeaderOnTerminalStatus finds the terminal call_status_hook
// callback and checks sip_reason_header on it. It also asserts sip_reason still
// holds the status-LINE phrase, since the whole point of adding a new field was
// to avoid repurposing that one.
//
// callSid may be "" to skip call_sid filtering (inbound, where the UAC never
// learns jambonz's call_sid).
func assertReasonHeaderOnTerminalStatus(s *StepCtx, cbs []webhook.Callback, callSid string, want reasonExpectation) {
	terminal := func(status string) bool {
		for _, w := range want.wantStatuses {
			if status == w {
				return true
			}
		}
		return false
	}

	var found bool
	var seen []string
	for _, cb := range cbs {
		if cb.Hook != "call_status_hook" {
			continue
		}
		if callSid != "" {
			if sid := cb.NestedString("call_sid"); sid != "" && sid != callSid {
				continue
			}
		}
		status := cb.NestedString("call_status")
		hdr := cb.NestedString("sip_reason_header")
		seen = append(seen, fmt.Sprintf("%s(sip_status=%d, sip_reason=%q, sip_reason_header=%q)",
			status, cb.Int("sip_status"), cb.NestedString("sip_reason"), hdr))

		if !terminal(status) {
			// Pre-terminal events (trying/ringing) legitimately carry no
			// Reason header - but if one shows up it must not be OUR value
			// leaking early from a stale CallInfo field.
			if normalizeReason(hdr) == normalizeReason(want.wantHeader) {
				s.Errorf("sip_reason_header %q appeared on non-terminal status %q", hdr, status)
			}
			continue
		}
		if want.wantDirection != "" {
			if got := cb.NestedString("direction"); got != want.wantDirection {
				continue
			}
		}
		found = true
		if normalizeReason(hdr) != normalizeReason(want.wantHeader) {
			s.Errorf("sip_reason_header on %q: got %q want %q (compared whitespace-normalized)",
				status, hdr, want.wantHeader)
		}
		if want.wantSipStatus != 0 {
			if got := cb.Int("sip_status"); got != want.wantSipStatus {
				s.Errorf("sip_status on %q: got %d want %d", status, got, want.wantSipStatus)
			}
		}
		// sip_reason must remain the status-line phrase, NOT the Reason
		// header. A regression that repurposed sip_reason instead of adding a
		// field would pass the check above and fail here.
		if got := cb.NestedString("sip_reason"); normalizeReason(got) == normalizeReason(want.wantHeader) {
			s.Errorf("sip_reason was overwritten with the Reason header (%q); it must stay the status-line phrase", got)
		}
	}

	if !found {
		s.Errorf("no terminal call_status_hook callback %v found; saw: %v", want.wantStatuses, seen)
		return
	}
	s.Logf("status events: %v", seen)
}
