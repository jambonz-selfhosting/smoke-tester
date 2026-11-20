// Soniox TTS: one-shot say over HTTP POST /tts and streaming say over the
// /tts-websocket socket (one stream per turn). The credential also serves the
// existing soniox STT. Every test passes without exercising soniox when
// SONIOX_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	sonioxTTSModel = "tts-rt-v2"
	sonioxVoice    = "Adrian"
	// every voice speaks every language; Lucia is a Spanish-accent voice
	sonioxSpanishVoice = "Lucia"
)

// sonioxSpanishText is "Hello, this voice test is working correctly." in Spanish.
const sonioxSpanishText = "Hola, esta prueba de voz funciona correctamente."

// requireSoniox logs and reports false when the key is unset; callers return.
func requireSoniox(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSoniox() || sonioxLabel == "" {
		t.Log("SONIOX_API_KEY not set — passing without exercising soniox")
		return false
	}
	return true
}

func sonioxSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "soniox",
		"label":    sonioxLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Soniox — one-shot say over the HTTP leg; en-US maps to "en".
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Soniox(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-soniox",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", sonioxSynth(sonioxVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Soniox — streaming say over the websocket leg.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Soniox(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-soniox", sonioxSynth(sonioxVoice, "en-US", nil))
}

// runSonioxScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runSonioxScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
