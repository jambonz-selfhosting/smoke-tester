//go:build detail

package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_SpeechCredential_Assemblyai — the api-server's credential test
// passes for the provisioned key. Creating a credential never validates it,
// so this is the only check that the key itself is good.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Assemblyai(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, assemblyaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Stream_Assemblyai_TwoTurns — two streaming says on one call
// share the socket; the second must still speak after the first's FlushDone.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Assemblyai_TwoTurns(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	synth := assemblyaiSynth(assemblyaiVoice, "english")
	runAssemblyaiScript(t, "say-stream-assemblyai-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Assemblyai_German — the service's own language name
// ("german") reaches the socket; a code it does not know is refused at connect.
// Audio duration is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Assemblyai_German(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	runAssemblyaiScript(t, "say-stream-assemblyai-de", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", "Guten Tag, dieser Sprachtest funktioniert einwandfrei.", "stream", true,
			"synthesizer", assemblyaiSynth(assemblyaiGermanVoice, "german")))
}

// TestVerb_Say_Assemblyai_Options — one-shot say on the EU credential
// (api_uri) with every HTTP option: speed (only 1 is accepted) and
// extend_biometric_consent=false.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Assemblyai_Options(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	synth := assemblyaiSynth(assemblyaiVoice, "english")
	synth["label"] = assemblyaiEULabel
	synth["options"] = map[string]any{"speed": 1, "extend_biometric_consent": false}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-assemblyai-options",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb:       V("say", "text", "The European region is working.", "synthesizer", synth),
		wantWords:  []string{"region", "working"},
	})
}

// TestVerb_Say_Stream_Assemblyai_Options — streaming say on the EU
// credential (api_uri) with every websocket option: inactivity_timeout and
// extend_biometric_consent=false.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Assemblyai_Options(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	synth := assemblyaiSynth(assemblyaiVoice, "english")
	synth["label"] = assemblyaiEULabel
	synth["options"] = map[string]any{"inactivity_timeout": 30, "extend_biometric_consent": false}
	runAssemblyaiScript(t, "say-stream-assemblyai-options", 1*time.Second, 12*time.Second,
		[]string{"european", "working"},
		V("say", "text", "The European region is working.", "stream", true, "synthesizer", synth))
}

// TestVerb_Agent_Assemblyai_BargeIn — a live agent on assemblyai: the caller
// barges into the greeting (Cancel on the socket) and the reply to the
// barge-in must be heard, i.e. the next turn is not swallowed while the
// cancel is acknowledged.
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
func TestVerb_Agent_Assemblyai_BargeIn(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
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
		TTS:               assemblyaiSynth(assemblyaiVoice, "english"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
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
	rec := filepath.Join(t.TempDir(), "assemblyai-barge-reply.pcm")
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
			s.Logf("assemblyai turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Assemblyai_HistoryTrimmedToSpoken — assemblyai is an
// alignment vendor (word boundaries), so after a barge-in turn_end.response
// must be trimmed to what the caller heard. Same flow as the Defect1 test on
// deepgram.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. request-count
//  8. wait-into-count
//  9. send-interrupt-wav
//
// 10. collect-events
// 11. assert-interruption-confirmed
// 12. assert-response-matches-spoken
func TestVerb_Agent_Assemblyai_HistoryTrimmedToSpoken(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	runHistoryTrimAfterBargeIn(t, assemblyaiSynth(assemblyaiVoice, "english"))
}
