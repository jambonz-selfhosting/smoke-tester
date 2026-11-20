//go:build detail

package verbs

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

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
		// gather completes on the 4th tone and the scripted hangup sends BYE
		// while the WAV's trailing silence is still being written
		byeCtx, byeCancel := context.WithTimeout(ctx, 2*time.Second)
		_, byeErr := call.AwaitReceivedRequest(byeCtx, "BYE")
		byeCancel()
		if byeErr != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		s.Logf("tone burst cut short by jambonz's BYE (gather already completed): %v", err)
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
