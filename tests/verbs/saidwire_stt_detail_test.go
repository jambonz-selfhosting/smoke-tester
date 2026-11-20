//go:build detail

package verbs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_SpeechCredential_Saidwire — the api-server's STT credential test
// passes for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential-stt
func TestVerb_SpeechCredential_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, saidwireSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Gather_Speech_Saidwire_Options — every saidwireOptions field
// reaches Saidwire and the phrase still comes back (a rejected query fails
// the gather): endpointing 500, utteranceEndMs, vadEvents, punctuate,
// smartFormatting, plus interim results.
//
// Steps:
//   - script-gather-speech-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Saidwire_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSaidwire(t) {
		return
	}
	r := saidwireRecognizer(saidwireLabel, map[string]any{
		"endpointing":     500,
		"utteranceEndMs":  1000,
		"vadEvents":       true,
		"punctuate":       true,
		"smartFormatting": true,
	})
	r["interim"] = true
	transcript := saidwireGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Saidwire_ApiUriCredential — the credential's api_uri
// selects the STT host: the explicit default host transcribes, an unreachable
// one does not (so the field is really used, not ignored).
//
// Steps:
//   - provision-credentials
//   - script-gather-speech-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-no-transcript-unreachable
func TestVerb_Gather_Speech_Saidwire_ApiUriCredential(t *testing.T) {
	vendorExtra(t)
	if !requireSaidwire(t) {
		return
	}

	s := Step(t, "provision-credentials")
	labels := map[string]string{}
	for name, uri := range map[string]string{
		"explicit":    "https://api.saidwire.com",
		"unreachable": "https://unreachable.invalid",
	} {
		label := provision.Name("saidwire-" + name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
			Vendor: "saidwire", Label: label, APIKey: cfg.SaidwireAPIKey, UseForSTT: true, APIURI: uri,
		})
		cancel()
		if err != nil {
			s.Fatalf("create %s credential: %v", name, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, sid)
		})
		labels[name] = label
	}
	s.Done()

	wav := resolveFixture(t, speechWAV)
	opts := map[string]any{"endpointing": 500}
	good := saidwireGather(t, saidwireRecognizer(labels["explicit"], opts), wav)
	assertHits(t, good, 2, "sun", "shining")

	bad := saidwireGather(t, saidwireRecognizer(labels["unreachable"], opts), wav)
	s = Step(t, "assert-no-transcript-unreachable")
	if bad != "" {
		s.Errorf("unreachable api_uri still transcribed %q: api_uri is not reaching the STT connection", bad)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Saidwire — two-turn agent echo with saidwire STT;
// turns end on Saidwire's speech_final (endpointing 500), as with deepgram.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
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

	s = Step(t, "script-agent-verb-saidwire")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          saidwireRecognizer(saidwireLabel, map[string]any{"endpointing": 500}),
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
