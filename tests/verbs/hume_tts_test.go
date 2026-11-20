// Hume TTS (Octave): streaming say and the agent use the /v0/tts/stream/input
// socket, one-shot say POSTs /v0/tts/stream/file. TTS only; every test passes
// without exercising hume when HUME_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireHume logs and reports false when the key is unset; callers return.
func requireHume(t *testing.T) bool {
	t.Helper()
	if !cfg.HasHume() || humeLabel == "" {
		t.Log("HUME_API_KEY not set — passing without exercising hume")
		return false
	}
	return true
}

func humeSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "hume",
		"label":    humeLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Hume — one-shot say over the HTTP leg.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Hume(t *testing.T) {
	if !requireHume(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-hume",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Hume — streaming say over the socket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Hume(t *testing.T) {
	if !requireHume(t) {
		return
	}
	runStreamingSay(t, "say-stream-hume", humeSynth(cfg.HumeVoiceID, "en-US", nil))
}

// runHumeScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runHumeScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
