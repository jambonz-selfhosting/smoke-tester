// Async (vendor asyncai) TTS: one-shot say over /text_to_speech/streaming
// (headerless PCM), streaming say and the agent over the websocket (one
// context per turn, closeContext on flush/clear). TTS only. Every test passes
// without exercising asyncai when ASYNCAI_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireAsyncai logs and reports false when the key is unset; callers return.
func requireAsyncai(t *testing.T) bool {
	t.Helper()
	if !cfg.HasAsyncai() || asyncaiLabel == "" {
		t.Log("ASYNCAI_API_KEY not set — passing without exercising asyncai")
		return false
	}
	return true
}

func asyncaiSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "asyncai",
		"label":    asyncaiLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Asyncai — one-shot say on the credential's async_flash_v1.5.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Asyncai(t *testing.T) {
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-asyncai",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Asyncai — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Asyncai(t *testing.T) {
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-asyncai", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil))
}

// runAsyncaiScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runAsyncaiScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
	t.Helper()
	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-say")
	script := webhook.Script{}
	for _, v := range says {
		script = append(script, v)
	}
	sess.ScriptCallHook(WithWarmupScript(append(script, V("hangup"))))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, minDur, maxDur, tag)
	s.Done()

	if wav != "" && len(wantOrdered) > 0 {
		s = Step(t, "assert-transcript-ordered")
		AssertTranscriptContainsInOrder(s, ctx, wav, wantOrdered...)
		s.Done()
	}
}
