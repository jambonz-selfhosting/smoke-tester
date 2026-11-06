// Hume TTS (Octave): streaming say and the agent use the /v0/tts/stream/input
// socket, one-shot say POSTs /v0/tts/stream/file. TTS only; every test passes
// without exercising hume when HUME_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireHume logs and reports false when the key is unset; callers return.
func requireHume(t *testing.T) bool {
	t.Helper()
	if !cfg.HasHume() || humeLabel == "" {
		t.Log("HUME_API_KEY not set — passing without exercising hume")
		return false
	}
	return true
}

func humeSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "hume",
		"label":    humeLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Hume — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Hume(t *testing.T) {
	if !requireHume(t) {
		return
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, humeSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Hume — one-shot say over the HTTP leg.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Hume(t *testing.T) {
	if !requireHume(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-hume",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Hume_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Hume_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	synth := humeSynth(cfg.HumeVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runHumeScript(t, "say-hume-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Hume_Options — every say-leg option at an in-range value; a
// rejected field fails the request and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Hume_Options(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-hume-options",
		minDur:     1 * time.Second,
		maxDur:     14 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "en-US", map[string]any{
				"voice_provider":   "HUME_AI",
				"description":      "calm, friendly and unhurried",
				"speed":            0.9,
				"trailing_silence": 0.5,
				"temperature":      0.7,
				"split_utterances": true,
			})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Hume_Octave2 — model_id "2" reaches the body as version.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Hume_Octave2(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-hume-octave2",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "en-US", map[string]any{"model_id": "2"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Hume_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Hume_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-hume-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "The library opens at nine tomorrow.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.hume.ai/"})),
		wantWords: []string{"library", "tomorrow"},
	})
}

// TestVerb_Say_Hume_LanguageIgnored — Hume has no language field, so a
// non-English jambonz language must not break the call.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Hume_LanguageIgnored(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runHumeScript(t, "say-hume-lang", 1*time.Second, 12*time.Second,
		[]string{"garden", "weather"},
		V("say", "text", "The garden looks lovely in this weather.",
			"synthesizer", humeSynth(cfg.HumeVoiceID, "de-DE", nil)))
}

// TestVerb_Say_Stream_Hume — streaming say over the socket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Hume(t *testing.T) {
	if !requireHume(t) {
		return
	}
	runStreamingSay(t, "say-stream-hume", humeSynth(cfg.HumeVoiceID, "en-US", nil))
}

// TestVerb_Say_Stream_Hume_Options — streaming say with the socket options,
// carried over the HUME_TTS_STREAMING_* channel vars.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Hume_Options(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runStreamingSay(t, "say-stream-hume-options",
		humeSynth(cfg.HumeVoiceID, "en-US", map[string]any{
			"voice_provider":   "HUME_AI",
			"description":      "warm and upbeat",
			"speed":            1.1,
			"trailing_silence": 0.3,
			"instant_mode":     true,
			"temperature":      0.7,
		}))
}

// TestVerb_Say_Stream_Hume_TwoTurns — two streaming says on one call; the
// voice is resent after the first turn's flush, so the second must speak.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Hume_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	synth := humeSynth(cfg.HumeVoiceID, "en-US", nil)
	runHumeScript(t, "say-stream-hume-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Hume_MultiSentence — one streaming say of several
// sentences; chunks are joined with a space and all must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Hume_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireHume(t) {
		return
	}
	runHumeScript(t, "say-stream-hume-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", humeSynth(cfg.HumeVoiceID, "en-US", nil)))
}

// runHumeScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runHumeScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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

// TestVerb_Agent_Hume_BargeIn — a live agent on hume: the caller barges into
// the greeting (no cancel message, so the socket is redialed) and the reply
// must be heard. A turn after 30s with no tokens covers an idle-closed socket.
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
func TestVerb_Agent_Hume_BargeIn(t *testing.T) {
	if !requireHume(t) {
		return
	}
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
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
		TTS:               humeSynth(cfg.HumeVoiceID, "en-US", nil),
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
	rec := filepath.Join(t.TempDir(), "hume-barge-reply.pcm")
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
			s.Logf("hume turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	WaitFor(t, "idle-no-tokens", 30*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "hume-idle-reply.pcm")
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
