// Tests for the `hangup` verb.
//
// Schema: schemas/verbs/hangup — has no required fields. Supports optional
// `headers` (custom SIP headers to include in the BYE jambonz sends us).
//
// ---- template for verb tests ---------------------------------------------
// Each test is preceded by a minimal ASCII sequence diagram. Conventions:
//
//   Test     = the test goroutine (what Go code does)
//   Jambonz  = the jambonz cluster under test
//   UAS      = our SIP user agent answering an inbound INVITE
//   UAC      = our SIP user agent placing an outbound INVITE
//   Webhook  = our local webhook server (Phase-2 tests only)
//   -->      SIP or HTTP message
//   ==>      media / RTP
//   //       assertion or note
//
// Keep it terse. Diagram should show only messages this test actually cares
// about; boilerplate (Trying, ACK) is elided unless asserted on.
//
// Shared helpers (V, AnswerRecordAndWaitEnded, AssertAudioDuration,
// RequireRecvMethods, RequireSentStatus, …) live in helpers_test.go.
// --------------------------------------------------------------------------
package verbs

import (
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// TestVerb_Hangup_Basic — jambonz hangs up immediately; assert the BYE
// arrives and the call ends as "remote-bye" under 3s.
//
// Steps:
//  1. place-call — POST /Calls with [hangup]; wait for inbound INVITE
//  2. answer-and-wait-end — 200 OK, block on StateEnded (expect BYE from jambonz)
//  3. assert-duration-and-end-reason — duration < 3s, end reason == remote-bye
//  4. assert-sip-methods — received INVITE+BYE, sent 200
//
// Test    --POST /Calls [hangup]--> Jambonz
// Jambonz --INVITE-->                UAS
// UAS     --200 OK-->                Jambonz   (Answer)
// Jambonz --BYE-->                   UAS       // end=remote-bye, duration<3s
// UAS     --200 OK-->                Jambonz   // diago auto-responds to BYE
func TestVerb_Hangup_Basic(t *testing.T) {
	t.Parallel()
	ctx := WithTimeout(t, 15*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, []map[string]any{V("hangup")})
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call)
	s.Done()

	s = Step(t, "assert-duration-and-end-reason")
	// hangup should be near-instant. Warmup pause (1s) + a small RTT
	// puts a real upper bound around 1.5s; 2s catches the regression
	// where jambonz delays BYE noticeably without flaking on cold-start.
	if d := call.Duration(); d > 2*time.Second {
		s.Errorf("duration too long: got %s want <2s", d)
	}
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("expected end reason 'remote-bye', got %q", reason)
	}
	s.Done()

	s = Step(t, "assert-sip-methods")
	RequireRecvMethods(s, call, "INVITE", "BYE")
	RequireSentStatus(s, call, 200)
	s.Done()
}

// TestVerb_Hangup_WithHeaders — hangup with custom headers; assert the BYE
// carries those headers on the wire.
//
// Steps:
//  1. place-call — POST /Calls with [hangup + X-Custom-A/B headers]
//  2. answer-and-wait-end — 200 OK, block on StateEnded
//  3. assert-end-reason — end reason == remote-bye
//  4. assert-bye-headers — captured BYE has X-Custom-A=foo, X-Custom-B=bar
//
// Test    --POST /Calls [hangup + headers]--> Jambonz
// Jambonz --INVITE-->                          UAS
// UAS     --200 OK-->                          Jambonz   (Answer)
// Jambonz --BYE (X-Custom-A/B)-->              UAS       // end=remote-bye
//                                                        // BYE headers include
//                                                        // X-Custom-A/B
func TestVerb_Hangup_WithHeaders(t *testing.T) {
	t.Parallel()
	ctx := WithTimeout(t, 15*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, []map[string]any{
		V("hangup", "headers", jsip.H{"X-Custom-A": "foo", "X-Custom-B": "bar"}),
	})
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call)
	s.Done()

	s = Step(t, "assert-end-reason")
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("expected end reason 'remote-bye', got %q", reason)
	}
	s.Done()

	s = Step(t, "assert-bye-headers")
	byes := call.ReceivedByMethod("BYE")
	if len(byes) == 0 {
		s.Fatalf("no BYE captured; got methods=%v", MethodsOf(call.Received()))
	}
	bye := byes[0]
	if got := bye.Headers["X-Custom-A"]; got != "foo" {
		s.Errorf("BYE X-Custom-A: got %q want %q", got, "foo")
	}
	if got := bye.Headers["X-Custom-B"]; got != "bar" {
		s.Errorf("BYE X-Custom-B: got %q want %q", got, "bar")
	}
	s.Done()
}
