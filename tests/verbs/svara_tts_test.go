// Svara TTS: TestVerb_Say_Svara* cover the HTTP leg, TestVerb_Say_Stream_Svara*
// and the agent test the websocket leg. All skip without SVARA_API_KEY.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
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

// TestVerb_SpeechCredential_Svara — the api-server's credential test
// passes for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Svara(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, svaraSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
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

// TestVerb_Say_Svara_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Svara_CachedReplay(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	synth := svaraSynth(cfg.SvaraVoiceID, "en", nil)
	text := "Your table for four is confirmed for tonight."
	runSvaraScript(t, "say-svara-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Svara_Options — speed 1.5 (Svara's max) must be accepted: an
// out-of-range or rejected option fails the request and nothing is heard.
// Words only; the speed effect on duration is not measured yet.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Svara_Options(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-svara-options",
		minDur:     2 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse, "+
			"and the courier will text you a delivery window tomorrow morning.",
			"synthesizer", svaraSynth(cfg.SvaraVoiceID, "en", map[string]any{"speed": 1.5})),
		wantWords: []string{"warehouse", "delivery window"},
	})
}

// TestVerb_Say_Svara_Hindi — one-shot Hindi say with the Hindi voice and
// language "hi". Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Svara_Hindi(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-svara-hi",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", svaraHindiText,
			"synthesizer", svaraSynth(cfg.SvaraHindiVoiceID, "hi", nil)),
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

// TestVerb_Say_Stream_Svara_TwoTurns — two streaming says on one call. The
// docs describe one socket per utterance; the second turn must still speak
// after the first turn's flush completed on the same socket.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Svara_TwoTurns(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	synth := svaraSynth(cfg.SvaraVoiceID, "en", nil)
	runSvaraScript(t, "say-stream-svara-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Svara_MultiSentence — one streaming say of several
// sentences; the server chunks them and all must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Svara_MultiSentence(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runSvaraScript(t, "say-stream-svara-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", svaraSynth(cfg.SvaraVoiceID, "en", nil)))
}

// TestVerb_Say_Stream_Svara_Hindi — Hindi over the socket: "hi" must reach
// the service as lang. Duration only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Svara_Hindi(t *testing.T) {
	requireSvara(t)
	t.Parallel()
	runSvaraScript(t, "say-stream-svara-hi", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", svaraHindiText, "stream", true,
			"synthesizer", svaraSynth(cfg.SvaraHindiVoiceID, "hi", nil)))
}

// TestVerb_Agent_Svara_BargeIn — a live agent on svara: the caller barges
// into the greeting (the socket is dropped and redialed) and the reply to the
// barge-in must be heard on the new socket. A turn after 30s with no tokens
// proves the idle socket is still usable.
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
// 10. idle-no-tokens
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. assert-idle-turn-end
// 14. hangup-and-wait-ended
func TestVerb_Agent_Svara_BargeIn(t *testing.T) {
	requireSvara(t)
	t.Parallel()
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
		TTS:               svaraSynth(cfg.SvaraVoiceID, "en", nil),
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
	rec := filepath.Join(t.TempDir(), "svara-barge-reply.pcm")
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
			s.Logf("svara turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	WaitFor(t, "idle-no-tokens", 30*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "svara-idle-reply.pcm")
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
