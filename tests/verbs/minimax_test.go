// MiniMax TTS: one-shot say over /v1/t2a_v2 (hex audio), streaming say and the
// agent over the /ws/v1/t2a_v2_bidi socket. TTS only. Every test passes
// without exercising minimax when MINIMAX_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	minimaxTTSModel   = "speech-2.8-turbo"
	minimaxTTSModelHD = "speech-2.8-hd"
	// first listed zh-CN system voice
	minimaxChineseVoice = "Chinese (Mandarin)_Reliable_Executive"
)

// minimaxChineseText is "Hello, this voice test is working correctly." in Mandarin.
const minimaxChineseText = "你好，这个语音测试运行正常。"

// requireMinimax logs and reports false when the key is unset; callers return.
func requireMinimax(t *testing.T) bool {
	t.Helper()
	if !cfg.HasMinimax() || minimaxLabel == "" {
		t.Log("MINIMAX_API_KEY not set — passing without exercising minimax")
		return false
	}
	return true
}

func minimaxSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "minimax",
		"label":    minimaxLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// minimaxAllOptions sets every option both legs send, at in-range values.
func minimaxAllOptions() map[string]any {
	return map[string]any{
		"speed": 1.2, "vol": 2, "pitch": 1, "emotion": "calm",
		"text_normalization": true, "latex_read": false,
		"language_boost": "English", "text_normalization_mode": "basic",
		"voice_modify": map[string]any{
			"pitch": 10, "intensity": -10, "timbre": 5, "sound_effects": "lofi_telephone",
		},
		"pronunciation_dict": map[string]any{"tone": []string{"jambonz/jam bones"}},
		"timbre_weights":     []map[string]any{{"voice_id": cfg.MinimaxVoiceID, "weight": 100}},
	}
}

// TestVerb_Say_Minimax — one-shot say on the default model and voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Minimax(t *testing.T) {
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-minimax",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Minimax — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Minimax(t *testing.T) {
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-minimax", minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil))
}

// runMinimaxScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runMinimaxScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
