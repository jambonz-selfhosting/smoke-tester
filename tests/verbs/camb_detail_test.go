//go:build detail

package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_SpeechCredential_Camb — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Camb(t *testing.T) {
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, cambSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Camb_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Camb_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	synth := cambSynth(cfg.CambVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runCambScript(t, "say-camb-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Camb_ModelPro — model_id option overrides the credential's
// mars-flash with mars-pro (48 kHz native rate).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Camb_ModelPro(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-camb-pro",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", cambSynth(cfg.CambVoiceID, "en-US",
				map[string]any{"model_id": "mars-pro"})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Camb_AllOptions — one-shot say with every HTTP option
// (voice_settings, inference_options, output enhancement, raw language).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Camb_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-camb-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call Acme Corporation on Monday morning.",
			"synthesizer", cambSynth(cfg.CambVoiceID, "en-US", map[string]any{
				"model_id":                             "mars-flash",
				"language":                             "en-us",
				"speaking_rate":                        1.0,
				"enhance_named_entities_pronunciation": true,
				"apply_enhancement":                    true,
				"enhance_reference_audio_quality":      true,
				"maintain_source_accent":               true,
				"inference_steps":                      20,
				"stability":                            0.5,
				"temperature":                          0.7,
				"speaker_similarity":                   0.5,
				"localize_speaker_weight":              0.5,
				"acoustic_quality_boost":               true,
			})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Camb_Instruct — mars-instruct with user_instructions.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Camb_Instruct(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-camb-instruct",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Thank you for waiting, your agent will join shortly.",
			"synthesizer", cambSynth(cfg.CambVoiceID, "en-US", map[string]any{
				"model_id":          "mars-instruct",
				"user_instructions": "Speak in a calm, friendly tone.",
			})),
		wantWords: []string{"agent", "shortly"},
	})
}

// TestVerb_Say_Camb_German — one-shot de-DE (lowercased to de-de). Duration
// only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Camb_German(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-camb-de",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Hallo, dieser Sprachtest funktioniert einwandfrei.",
			"synthesizer", cambSynth(cfg.CambVoiceID, "de-DE", nil)),
	})
}

// TestVerb_Say_Stream_Camb_AllOptions — streaming say with every socket
// option, including the WS-only idle_timeout and word_timestamps.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Camb_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runStreamingSayMax(t, "say-stream-camb-options", cambSynth(cfg.CambVoiceID, "en-US", map[string]any{
		"language":                             "en-us",
		"speaking_rate":                        1.0,
		"idle_timeout":                         0.5,
		"word_timestamps":                      true,
		"inference_steps":                      20,
		"enhance_named_entities_pronunciation": true,
		"apply_enhancement":                    true,
		"enhance_reference_audio_quality":      true,
		"maintain_source_accent":               true,
	}), 12*time.Second)
}

// TestVerb_Say_Stream_Camb_TwoTurns — two streaming says; the second turn
// gets a fresh socket since the session closes after text.done.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Camb_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	synth := cambSynth(cfg.CambVoiceID, "en-US", nil)
	runCambScript(t, "say-stream-camb-2turns", 1*time.Second, 20*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Camb_MultiSentence — one streaming say of several
// sentences (several segments, each may carry its own WAV header).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Camb_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runCambScript(t, "say-stream-camb-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", cambSynth(cfg.CambVoiceID, "en-US", nil)))
}

// TestVerb_Say_Stream_Camb_German — de-DE over the socket. Duration only:
// the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Camb_German(t *testing.T) {
	vendorExtra(t)
	if !requireCamb(t) {
		return
	}
	t.Parallel()
	runCambScript(t, "say-stream-camb-de", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Hallo, dieser Sprachtest funktioniert einwandfrei.", "stream", true,
			"synthesizer", cambSynth(cfg.CambVoiceID, "de-DE", nil)))
}

// TestVerb_Agent_Camb_BargeIn — a live agent on camb: the caller barges into
// the greeting (no cancel message, so the socket is redialed on clear) and
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
func TestVerb_Agent_Camb_BargeIn(t *testing.T) {
	if !requireCamb(t) {
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
		TTS:               cambSynth(cfg.CambVoiceID, "en-US", nil),
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
	rec := filepath.Join(t.TempDir(), "camb-barge-reply.pcm")
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
			s.Logf("camb turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
