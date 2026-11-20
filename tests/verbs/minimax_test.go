// MiniMax TTS: one-shot say over /v1/t2a_v2 (hex audio), streaming say and the
// agent over the /ws/v1/t2a_v2_bidi socket. TTS only. Every test passes
// without exercising minimax when MINIMAX_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
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

// TestVerb_SpeechCredential_Minimax — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Minimax(t *testing.T) {
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, minimaxSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
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

// TestVerb_Say_Minimax_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Minimax_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	synth := minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runMinimaxScript(t, "say-minimax-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Minimax_ModelOverride — options.model_id switches the one-shot
// say to speech-2.8-hd over the credential's turbo default.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Minimax_ModelOverride(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-minimax-hd",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "Your package arrives on Thursday afternoon.",
			"synthesizer", minimaxSynth(cfg.MinimaxVoiceID, "en-US",
				map[string]any{"model_id": minimaxTTSModelHD})),
		wantWords: []string{"package", "thursday"},
	})
}

// TestVerb_Say_Minimax_AllOptions — every one-shot option at in-range values.
// MiniMax rejects a bad value in base_resp, so a rejected option means silence.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Minimax_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-minimax-all-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", minimaxSynth(cfg.MinimaxVoiceID, "en-US", minimaxAllOptions())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Minimax_Chinese — one-shot Mandarin say with a zh-CN system
// voice. Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Minimax_Chinese(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-minimax-zh",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", minimaxChineseText,
			"synthesizer", minimaxSynth(minimaxChineseVoice, "zh-CN", nil)),
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

// TestVerb_Say_Stream_Minimax_AllOptions — streaming say with every option,
// including the socket-only english_normalization and continuous_sound.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Minimax_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	opts := minimaxAllOptions()
	opts["english_normalization"] = true
	opts["continuous_sound"] = true
	runStreamingSay(t, "say-stream-minimax-options", minimaxSynth(cfg.MinimaxVoiceID, "en-US", opts))
}

// TestVerb_Say_Stream_Minimax_TwoTurns — two streaming says on one socket;
// the second turn must speak after the first turn's task_flushed.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Minimax_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	synth := minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil)
	runMinimaxScript(t, "say-stream-minimax-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Minimax_MultiSentence — one streaming say of several
// sentences; all must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Minimax_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runMinimaxScript(t, "say-stream-minimax-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil)))
}

// TestVerb_Say_Stream_Minimax_Chinese — Mandarin over the socket. Duration
// only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Minimax_Chinese(t *testing.T) {
	vendorExtra(t)
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	runMinimaxScript(t, "say-stream-minimax-zh", 1*time.Second, 12*time.Second, nil,
		V("say", "text", minimaxChineseText, "stream", true,
			"synthesizer", minimaxSynth(minimaxChineseVoice, "zh-CN", nil)))
}

// TestVerb_Agent_Minimax_BargeIn — a live agent on minimax: the caller barges
// into the greeting (task_cancel on the open socket) and the reply must be
// heard. A turn after 130s with no tokens proves the 90s keepalive held the
// socket past MiniMax's 120s idle close.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. idle-no-tokens
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. assert-idle-turn-end
// 14. hangup-and-wait-ended
func TestVerb_Agent_Minimax_BargeIn(t *testing.T) {
	if !requireMinimax(t) {
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 300*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wavs")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	idlePrompt := agentEchoTurns[1].prompt
	idleWAV, err := tts.EnsureWAV(ctx, "testdata/agent", idlePrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting:          true,
		BargeIn:           true,
		NoResponseTimeout: &noReprompt,
		TTS:               minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(270))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-into-greeting", 4*time.Second)

	s = Step(t, "barge-in-and-record-reply")
	rec := filepath.Join(t.TempDir(), "minimax-barge-reply.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(bargeWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-barge-in-reply")
	AssertTranscriptHasMost(s, ctx, rec, 2, "alpha", "bravo", "charlie", "delta")
	s.Done()

	s = Step(t, "assert-user-interruption")
	cbs := DrainCallbacks(sess, time.Second)
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		s.Errorf("no user_interruption event: %s", summarizeEventTypes(cbs))
	}
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if lat, ok := te.JSON["latency"].(map[string]any); ok {
			s.Logf("minimax turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	WaitFor(t, "idle-no-tokens", 130*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "minimax-idle-reply.pcm")
	if err := call.StartRecording(idleRec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	if err := call.SendWAV(idleWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-idle-turn-reply")
	AssertTranscriptHasMost(s, ctx, idleRec, 2, contentWords(idlePrompt)...)
	s.Done()

	s = Step(t, "assert-idle-turn-end")
	idleCbs := DrainCallbacks(sess, 2*time.Second)
	if n := len(findAgentEvents(idleCbs, "turn_end")); n != 1 {
		s.Errorf("idle turn produced %d turn_end events, want 1: %s", n, summarizeEventTypes(idleCbs))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
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
