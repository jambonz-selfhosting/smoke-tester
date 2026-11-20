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

// TestVerb_Agent_Echo_Gradium — two-turn agent echo on gradium; guards gradium's semantic-VAD end-of-turn.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Gradium(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
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

	s = Step(t, "script-agent-verb-gradium")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT: map[string]any{
			"vendor":   "gradium",
			"label":    gradiumLabel,
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

// TestVerb_Gather_Speech_Gradium_Options — every gradiumOptions field reaches
// Gradium and still yields the phrase: keywords + boost, delayInFrames, temp,
// paddingBonus, turn_detection, and the EU host via gradiumSttUri.
//
// Steps:
//   - script-gather-speech-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gradium_Options(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
		return
	}
	transcript := gradiumGather(t, map[string]any{
		"vendor":   "gradium",
		"label":    gradiumLabel,
		"language": "en-US",
		"gradiumOptions": map[string]any{
			"keywords":       []string{"sun", "shining"},
			"keywordBoost":   3,
			"delayInFrames":  16,
			"temp":           0,
			"paddingBonus":   -1,
			"turn_detection": map[string]any{"threshold": 0.6, "horizon": 2},
			"gradiumSttUri":  "eu.api.gradium.ai",
		},
	}, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Gradium_Spanish — es-ES maps to Gradium's "es";
// gather returns the clip's first sentence, "una mesa para hoy, por favor".
//
// Steps:
//   - script-gather-speech-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gradium_Spanish(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
		return
	}
	transcript := gradiumGather(t, map[string]any{
		"vendor":   "gradium",
		"label":    gradiumLabel,
		"language": "es-ES",
	}, resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
}

// TestVerb_Gather_Speech_Gradium_RegionCredential — the credential's api_uri
// selects the STT host: the EU host transcribes, an unreachable one does not
// (so the field is really used, not ignored).
//
// Steps:
//   - provision-credentials
//   - script-gather-speech-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-no-transcript-unreachable
func TestVerb_Gather_Speech_Gradium_RegionCredential(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
		return
	}

	s := Step(t, "provision-credentials")
	labels := map[string]string{}
	for name, uri := range map[string]string{
		"eu":          "https://eu.api.gradium.ai",
		"unreachable": "https://unreachable.invalid",
	} {
		label := provision.Name("gradium-" + name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
			Vendor: "gradium", Label: label, APIKey: cfg.GradiumAPIKey, UseForSTT: true, APIURI: uri,
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
	eu := gradiumGather(t, map[string]any{"vendor": "gradium", "label": labels["eu"], "language": "en-US"}, wav)
	assertHits(t, eu, 1, "sun", "shining")

	bad := gradiumGather(t, map[string]any{
		"vendor": "gradium", "label": labels["unreachable"], "language": "en-US",
	}, wav)
	s = Step(t, "assert-no-transcript-unreachable")
	if bad != "" {
		s.Errorf("unreachable api_uri still transcribed %q: api_uri is not reaching the STT connection", bad)
	}
	s.Done()
}
