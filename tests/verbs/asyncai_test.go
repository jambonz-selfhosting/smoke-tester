// Async (vendor asyncai) TTS: one-shot say over /text_to_speech/streaming
// (headerless PCM), streaming say and the agent over the websocket (one
// context per turn, closeContext on flush/clear). TTS only. Every test passes
// without exercising asyncai when ASYNCAI_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
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

// TestVerb_SpeechCredential_Asyncai — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Asyncai(t *testing.T) {
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, asyncaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
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

// TestVerb_Say_Asyncai_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Asyncai_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	synth := asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runAsyncaiScript(t, "say-asyncai-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Asyncai_FlashV1Options — async_flash_v1.0 (the only model that
// honors speed_control/stability) with both options set.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Asyncai_FlashV1Options(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-asyncai-flash-v1",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", map[string]any{
				"model_id":      "async_flash_v1.0",
				"speed_control": 1.2,
				"stability":     60,
			})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Asyncai_Pro — model_id overrides the credential's flash v1.5
// with the English-only async_pro_v1.0.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Asyncai_Pro(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-asyncai-pro",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Thank you for waiting, your agent will join shortly.",
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US",
				map[string]any{"model_id": "async_pro_v1.0"})),
		wantWords: []string{"agent", "shortly"},
	})
}

// TestVerb_Say_Asyncai_LanguageOption — the raw language option wins over
// the mapped jambonz language.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Asyncai_LanguageOption(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-asyncai-lang",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call Acme Corporation on Monday morning.",
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "en-GB",
				map[string]any{"language": "en"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Asyncai_Spanish — one-shot es-ES (mapped to es). Duration
// only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Asyncai_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-asyncai-es",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Hola, esta prueba de voz funciona correctamente.",
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "es-ES", nil)),
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

// TestVerb_Say_Stream_Asyncai_AllOptions — streaming say with every option
// in initializeConnection (flash v1.0 for speed_control/stability).
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Asyncai_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-asyncai-options", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", map[string]any{
		"model_id":      "async_flash_v1.0",
		"language":      "en",
		"speed_control": 1.1,
		"stability":     50,
	}))
}

// TestVerb_Say_Stream_Asyncai_TwoTurns — two streaming says on one socket;
// the second turn rotates to a fresh context since the first was closed.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Asyncai_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	synth := asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil)
	runAsyncaiScript(t, "say-stream-asyncai-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Asyncai_MultiSentence — one streaming say of several
// sentences (several transcript frames in one context).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Asyncai_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runAsyncaiScript(t, "say-stream-asyncai-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil)))
}

// TestVerb_Say_Stream_Asyncai_Spanish — es-ES over the socket. Duration
// only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Asyncai_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	runAsyncaiScript(t, "say-stream-asyncai-es", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Hola, esta prueba de voz funciona correctamente.", "stream", true,
			"synthesizer", asyncaiSynth(cfg.AsyncaiVoiceID, "es-ES", nil)))
}

// TestVerb_Agent_Asyncai_BargeIn — a live agent on asyncai: the caller barges
// into the greeting (closeContext + fresh context, stale audio dropped) and
// the reply must be heard.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. hangup-and-wait-ended
func TestVerb_Agent_Asyncai_BargeIn(t *testing.T) {
	if !requireAsyncai(t) {
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
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
		TTS:               asyncaiSynth(cfg.AsyncaiVoiceID, "en-US", nil),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
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
	rec := filepath.Join(t.TempDir(), "asyncai-barge-reply.pcm")
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
			s.Logf("asyncai turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
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
