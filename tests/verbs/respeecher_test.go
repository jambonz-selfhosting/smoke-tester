// Respeecher TTS: one-shot say over /tts/bytes (WAV), streaming say and the
// agent over the websocket (one context per turn, cancel on clear). The model
// is the URL path, picked from the language. TTS only. Every test passes
// without exercising respeecher when RESPEECHER_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Only bundled voice for the ua-rt model.
const respeecherUkVoice = "olesia-conversation"

// requireRespeecher logs and reports false when the key is unset; callers return.
func requireRespeecher(t *testing.T) bool {
	t.Helper()
	if !cfg.HasRespeecher() || respeecherLabel == "" {
		t.Log("RESPEECHER_API_KEY not set — passing without exercising respeecher")
		return false
	}
	return true
}

func respeecherSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "respeecher",
		"label":    respeecherLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// respeecherSampling sets every sampling param, each inside its documented range.
func respeecherSampling() map[string]any {
	return map[string]any{
		"seed":               42,
		"temperature":        0.7,
		"top_k":              50,
		"top_p":              0.9,
		"min_p":              0.05,
		"presence_penalty":   0.2,
		"repetition_penalty": 1.1,
		"frequency_penalty":  0.2,
	}
}

// TestVerb_Say_Respeecher — one-shot say; en-US resolves to the en-rt model.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Respeecher(t *testing.T) {
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-respeecher",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Respeecher — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Respeecher(t *testing.T) {
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-respeecher", respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil))
}

// runRespeecherScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in order.
func runRespeecherScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
