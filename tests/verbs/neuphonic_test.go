// Neuphonic TTS: one-shot say over /sse/speak (base64 SSE), streaming say and
// the agent over the /speak websocket (socket per turn). TTS only. Every test
// passes without exercising neuphonic when NEUPHONIC_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	neuphonicSpanishVoice = "3c8d7261-b917-4085-90ad-e7de015b3030" // Alejandra
	neuphonicGermanVoice  = "e46dd6b6-6eee-4133-a717-1b551f3dc430" // Emilia
)

// requireNeuphonic logs and reports false when the key is unset; callers return.
func requireNeuphonic(t *testing.T) bool {
	t.Helper()
	if !cfg.HasNeuphonic() || neuphonicLabel == "" {
		t.Log("NEUPHONIC_API_KEY not set — passing without exercising neuphonic")
		return false
	}
	return true
}

func neuphonicSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "neuphonic",
		"label":    neuphonicLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Neuphonic — one-shot say on the default voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Neuphonic(t *testing.T) {
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-neuphonic",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Neuphonic — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Neuphonic(t *testing.T) {
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-neuphonic", neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil))
}

// runNeuphonicScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runNeuphonicScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
