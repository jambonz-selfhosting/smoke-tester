// Tests for the `gather` verb — needs the webhook path (actionHook).
//
// Flow:
//  1. Test creates a webhook session + registers a script: gather + hangup.
//  2. Test registers an action-hook handler that returns an empty verb list
//     (acknowledgement) when jambonz posts the collected digits.
//  3. Test POSTs /Calls with application_sid=webhookApp + tag for correlation.
//  4. jambonz fetches the call_hook, runs gather, dials us via the `to`
//     user target, we answer, send DTMF, jambonz posts actionHook.
//  5. Test reads the captured actionHook body and asserts digits.
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

	_, sess := claimSession(t)

	s := Step(t, "script-gather-and-action-ack")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"digits"},
			"numDigits", 4,
			"timeout", 10,
			"actionHook", actionURL),
		V("hangup"),
	}))
	// Empty action-hook response = "ack, don't chain more verbs".
	SessionAckEmpty(sess, "gather")
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
	if got := cb.String("digits"); got != "1234" {
		s.Errorf("digits mismatch: got %q want %q (body: %s)", got, "1234", string(cb.Body))
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

// TestVerb_Gather_InbandDigits — the same verb driven by a caller that never
// negotiated telephone-event and sends DTMF as audio tones.
//
// This is the other half of DTMF, and the more consequential one: relay only
// affects bridged calls, but detection affects every IVR. Neither jambonz nor
// the media server decodes tones — rtpengine does, converting them to RFC 2833
// before they reach the feature server, which is why the verb sees digits at
// all. Nothing in jambonz's own code makes this work, so nothing in jambonz
// would report it breaking either.
//
// The caller must answer PCMU-only. Answering with telephone-event and then
// sending tones anyway is not a shape real gear produces, and it tells the SBC
// the leg speaks RFC 2833, which suppresses the inband handling under test — an
// earlier version of this suite made that mistake and the resulting empty
// digits were read as a platform outage.
//
// Steps mirror TestVerb_Gather_Digits, differing at 4 (PCMU-only answer) and
// 6 (a synthesized tone burst instead of an RFC 2833 burst).
func TestVerb_Gather_InbandDigits(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	const digits = "1234"
	s := Step(t, "synthesize-tones")
	wavPath := SynthesizeDTMFWAV(t, digits, 100, 100)
	s.Done()

	s = Step(t, "script-gather-and-action-ack")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"digits"},
			"numDigits", len(digits),
			"timeout", 10,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-pcmu-only-and-silence")
	if err := call.AnswerWithoutTelephoneEvent(); err != nil {
		s.Fatalf("AnswerWithoutTelephoneEvent: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-gather-detector")
	// Same 1500ms as the RFC 2833 test: the server-side warmup pause runs
	// before gather, so tones sent earlier land before anything is listening.
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-inband-tones")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
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

	s = Step(t, "assert-digits")
	if got := cb.String("digits"); got != digits {
		s.Errorf("digits = %q, want %q (reason %q) — an inband caller's keypresses "+
			"never reached the verb", got, digits, cb.String("reason"))
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}
