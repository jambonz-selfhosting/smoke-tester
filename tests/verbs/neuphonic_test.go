// Neuphonic TTS: one-shot say over /sse/speak (base64 SSE), streaming say and
// the agent over the /speak websocket (socket per turn). TTS only. Every test
// passes without exercising neuphonic when NEUPHONIC_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
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

// TestVerb_SpeechCredential_Neuphonic — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Neuphonic(t *testing.T) {
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, neuphonicSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
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

// TestVerb_Say_Neuphonic_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Neuphonic_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	synth := neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runNeuphonicScript(t, "say-neuphonic-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Neuphonic_Options — one-shot say with speed, temperature and
// the raw language option overriding the mapped en-US.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Neuphonic_Options(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-neuphonic-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", neuphonicSynth(cfg.NeuphonicVoiceID, "en-US",
				map[string]any{"speed": 1.0, "temperature": 0.5, "language": "en"})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Neuphonic_Spanish — one-shot es-ES say with ipa_key
// es-castilian. Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Neuphonic_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-neuphonic-es",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Hola, esta prueba de voz funciona correctamente.",
			"synthesizer", neuphonicSynth(neuphonicSpanishVoice, "es-ES",
				map[string]any{"ipa_key": "es-castilian"})),
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

// TestVerb_Say_Stream_Neuphonic_AllOptions — streaming say with every socket
// option, including the WS-only timestamps.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Neuphonic_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-neuphonic-options", neuphonicSynth(cfg.NeuphonicVoiceID, "en-US",
		map[string]any{"speed": 1.0, "temperature": 0.5, "language": "en", "timestamps": true}))
}

// TestVerb_Say_Stream_Neuphonic_TwoTurns — two streaming says; the second
// turn gets a fresh socket after the first turn's stop.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Neuphonic_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	synth := neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil)
	runNeuphonicScript(t, "say-stream-neuphonic-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Neuphonic_MultiSentence — one streaming say of several
// sentences; all must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Neuphonic_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runNeuphonicScript(t, "say-stream-neuphonic-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil)))
}

// TestVerb_Say_Stream_Neuphonic_German — de-DE over the socket. Duration
// only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Neuphonic_German(t *testing.T) {
	vendorExtra(t)
	if !requireNeuphonic(t) {
		return
	}
	t.Parallel()
	runNeuphonicScript(t, "say-stream-neuphonic-de", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Hallo, dieser Sprachtest funktioniert einwandfrei.", "stream", true,
			"synthesizer", neuphonicSynth(neuphonicGermanVoice, "de-DE", nil)))
}

// TestVerb_Agent_Neuphonic_BargeIn — a live agent on neuphonic: the caller barges
// into the greeting (socket redial on clear) and the reply must be
// heard. A turn after 65s with no tokens must redial past Neuphonic's 60s
// idle close.
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
func TestVerb_Agent_Neuphonic_BargeIn(t *testing.T) {
	if !requireNeuphonic(t) {
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
		TTS:               neuphonicSynth(cfg.NeuphonicVoiceID, "en-US", nil),
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
	rec := filepath.Join(t.TempDir(), "neuphonic-barge-reply.pcm")
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
			s.Logf("neuphonic turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	WaitFor(t, "idle-no-tokens", 65*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "neuphonic-idle-reply.pcm")
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
