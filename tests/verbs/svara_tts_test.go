// Svara TTS: TestVerb_Say_Svara* cover the HTTP leg, TestVerb_Say_Stream_Svara*
// and the agent test the websocket leg. All skip without SVARA_API_KEY.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// svaraHindiText is "Hello, this voice test is working correctly." in Hindi.
const svaraHindiText = "नमस्ते, यह आवाज़ परीक्षण ठीक से काम कर रहा है।"

func requireSvara(t *testing.T) {
	t.Helper()
	if !cfg.HasSvara() || svaraLabel == "" {
		t.Skip("Svara tests need SVARA_API_KEY (credential missing — skipping, not a failure)")
	}
}

func svaraSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "svara",
		"label":    svaraLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Svara — one-shot say over the HTTP leg.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Svara(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-svara",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", svaraSynth(cfg.SvaraVoiceID, "en", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Svara — streaming say over the WebSocket leg.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Svara(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runStreamingSay(t, "say-stream-svara", svaraSynth(cfg.SvaraVoiceID, "en", nil))
}

// runSvaraScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runSvaraScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
