// Bland AI TTS: one-shot say over POST /v2/tts (raw pcm), streaming say and
// the agent over the /v2/tts/ws socket (one context per turn, cancel on clear).
// The voice UUID picks the model; there is no language parameter. TTS only.
// Never t.Parallel: the default bland org allows one concurrent request.
// Every test passes without exercising bland when BLAND_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Built-in Matthew voice (btts-3).
const blandMatthewVoice = "1d4dc605-071b-4eee-81b3-099b2894d0d1"

// requireBland logs and reports false when the key is unset; callers return.
func requireBland(t *testing.T) bool {
	t.Helper()
	if !cfg.HasBland() || blandLabel == "" {
		t.Log("BLAND_API_KEY not set — passing without exercising bland")
		return false
	}
	return true
}

func blandSynth(voice string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "bland",
		"label":    blandLabel,
		"voice":    voice,
		"language": "en-US",
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// blandControls sets every documented control, each inside 0.0–1.0.
func blandControls() map[string]any {
	return map[string]any{
		"expressiveness": 0.8,
		"stability":      0.4,
	}
}

// TestVerb_Say_Bland — one-shot say with the default voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Bland(t *testing.T) {
	if !requireBland(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-bland",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", blandSynth(cfg.BlandVoiceID, nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Bland — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Bland(t *testing.T) {
	if !requireBland(t) {
		return
	}
	runStreamingSay(t, "say-stream-bland", blandSynth(cfg.BlandVoiceID, nil))
}

// runBlandScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in order.
func runBlandScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
