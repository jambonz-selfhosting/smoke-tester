// Tests for the `gather` verb — needs the webhook path (actionHook).
//
// Flow:
//   1. Test creates a webhook session + registers a script: gather + hangup.
//   2. Test registers an action-hook handler that returns an empty verb list
//      (acknowledgement) when jambonz posts the collected digits.
//   3. Test POSTs /Calls with application_sid=webhookApp + tag for correlation.
//   4. jambonz fetches the call_hook, runs gather, dials us via the `to`
//      user target, we answer, send DTMF, jambonz posts actionHook.
//   5. Test reads the captured actionHook body and asserts digits.
//
// NOTE: Phase 2 tests only. Skipped if NGROK_AUTHTOKEN is unset.
package verbs

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Digits — send DTMF "1234" and verify jambonz reports it
// via the action-hook callback.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup on release
//  2. script-gather-and-action-ack — register [gather, hangup] call_hook +
//     empty action-hook ack
//  3. place-call — POST /Calls with application_sid=webhookApp, tag=x_test_id
//  4. answer-and-silence — 200 OK + SendSilence to open the RTP pinhole
//  5. wait-for-gather-detector — 1500ms so jambonz's DTMF detector arms
//     after the server-side warmup pause
//  6. send-dtmf-1234 — outbound RFC 2833 burst
//  7. wait-action-gather-callback — block on /action/gather HTTP callback
//  8. assert-digits-1234 — digits field in callback body == "1234"
//  9. hangup — best-effort tear-down
//
// Test     --POST /Calls [app=webhookApp, tag.x_test_id]--> Jambonz
// Jambonz  --GET /hook-->                                   Webhook  // call_hook
// Webhook  --[gather, hangup]-->                            Jambonz
// Jambonz  --INVITE-->                                      UAS
// UAS      --200 OK-->                                      Jambonz   (Answer)
// UAS      ==RTP 2833 "1234"==>                             Jambonz
// Jambonz  --POST /action/gather {digits:"1234"}-->         Webhook
// Jambonz  --BYE-->                                         UAS
func TestVerb_Gather_Digits(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-gather-and-action-ack")
	actionURL := webhookSrv.PublicURL() + "/action/gather"
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"digits"},
			"numDigits", 4,
			"timeout", 10,
			"actionHook", actionURL),
		V("hangup"),
	}))
	// Empty action-hook response = "ack, don't chain more verbs".
	sess.ScriptActionHook("gather", webhook.Script{})
	s.Done()

	// Gather's flow diverges from other verb tests: we must send DTMF
	// *before* waiting for the call to end, and we tear the call down
	// ourselves after the action-hook fires. The AnswerRecordAndWait helper
	// doesn't fit — do the lifecycle inline.
	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// The server-side warmup (answer + pause) fires *before* gather, so
	// gather's DTMF detector only arms after the pause ends. Without this
	// client-side delay, our 2833 packets occasionally land before gather's
	// detector is listening and every digit is missed. 1.5s is empirically
	// the smallest value that's been reliable across cold + warm tunnel runs.
	s = Step(t, "wait-for-gather-detector")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-dtmf-1234")
	if err := call.SendDTMF("1234"); err != nil {
		s.Fatalf("SendDTMF: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-digits-1234")
	digits, _ := cb.JSON["digits"].(string)
	if digits != "1234" {
		s.Errorf("digits mismatch: got %q want %q (body: %s)", digits, "1234", string(cb.Body))
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	// Sanity: verify the call_hook was ALSO captured (callback queue is
	// FIFO; we only drained action/gather, so call_hook should be there).
	if cbs := DrainCallbacks(sess, 100*time.Millisecond); !ContainsHook(cbs, "call_hook") {
		s.Logf("note: did not observe call_hook in queue (may have been consumed already)")
	}
	s.Done()
}
