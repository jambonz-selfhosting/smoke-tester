//go:build detail

package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_Gather_Speech_Giggy_Hints — Giggy rejects hints, so jambonz must
// drop them; a gather with hints still transcribes.
//
// Steps:
//   - load-ground-truth
//   - script-gather-speech-giggy
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - wait-action-gather-callback
//   - assert-transcript-sun-shining
//   - hangup
func TestVerb_Gather_Speech_Giggy_Hints(t *testing.T) {
	runGiggyGather(t, map[string]any{"hints": []any{"sunshine", "weather"}, "hintsBoost": 10})
}

// TestVerb_Gather_Speech_Giggy_AutoLanguage — language "any" is sent as no
// language, so Giggy auto-detects (it rejects "any" itself).
//
// Steps:
//   - load-ground-truth
//   - script-gather-speech-giggy
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - wait-action-gather-callback
//   - assert-transcript-sun-shining
//   - hangup
func TestVerb_Gather_Speech_Giggy_AutoLanguage(t *testing.T) {
	runGiggyGather(t, map[string]any{"language": "any"})
}

// TestVerb_SpeechCredential_Giggy_STT — the api-server's STT credential test
// opens a live Giggy session for the provisioned key.
//
// Steps:
//  1. test-credential-stt
func TestVerb_SpeechCredential_Giggy_STT(t *testing.T) {
	requireGiggy(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, giggySID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Giggy — two-turn agent echo on giggy STT (finals end the turn, Silero gives speech start).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-giggy
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Giggy(t *testing.T) {
	requireGiggy(t)
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	wavs := make([]string, len(agentEchoTurns))
	for i, turn := range agentEchoTurns {
		s = Step(t, "ensure-prompt-wav")
		path, err := tts.EnsureWAV(ctx, "testdata/agent", turn.prompt, tts.PromptOptions{
			Model: "aura-asteria-en",
		})
		if err != nil {
			s.Fatalf("EnsureWAV turn %d: %v", i+1, err)
		}
		wavs[i] = path
		s.Done()
	}

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-giggy")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT: map[string]any{
			"vendor":   "giggy",
			"label":    giggyLabel,
			"language": "en-US",
		},
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	for i, turn := range agentEchoTurns {
		recPath := filepath.Join(t.TempDir(), formatAgentTurnRecPath(i+1))

		s = Step(t, formatAgentTurnStep(i+1, "record-and-speak"))
		if err := call.StartRecording(recPath); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (pre): %v", err)
		}
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, formatAgentTurnStep(i+1, "assert-echo"))
		AssertTranscriptHasMost(s, ctx, recPath, 2, contentWords(turn.prompt)...)
		s.Done()
	}

	HangupAndWaitEnded(t, ctx, call)
}
