// Camb.ai MARS TTS: one-shot say over /apis/tts-stream (headerless PCM),
// streaming say and the agent over the live-tts websocket (socket per turn,
// reconnect on clear). TTS only. Every test passes without exercising camb
// when CAMB_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireCamb logs and reports false when the key is unset; callers return.
func requireCamb(t *testing.T) bool {
	t.Helper()
	if !cfg.HasCamb() || cambLabel == "" {
		t.Log("CAMB_API_KEY not set — passing without exercising camb")
		return false
	}
	return true
}

func cambSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "camb",
		"label":    cambLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Camb — one-shot say on the credential's mars-flash model.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Camb(t *testing.T) {
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-camb",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", cambSynth(cfg.CambVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Camb — streaming say over the live-tts websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Camb(t *testing.T) {
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	// Camb normally takes ~3.7s to first audio on a session (measured live)
	runStreamingSayMax(t, "say-stream-camb", cambSynth(cfg.CambVoiceID, "en-US", nil), 12*time.Second)
}

// runCambScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runCambScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
