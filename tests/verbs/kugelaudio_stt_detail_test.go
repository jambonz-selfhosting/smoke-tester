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

// TestVerb_Agent_Echo_Kugelaudio — two-turn agent echo on kugelaudio; guards its native end-of-turn.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Kugelaudio(t *testing.T) {
	if !kugelaudioSttReady(t) {
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

	s = Step(t, "script-agent-verb-kugelaudio")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT: map[string]any{
			"vendor":   "kugelaudio",
			"label":    kugelaudioSttLabel,
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

// TestVerb_Gather_Speech_Kugelaudio_Options — every kugelaudioOptions field
// reaches KugelAudio and still yields the phrase: model, languages,
// turnDetection + level, boostedPhrases, and the EU host via kugelaudioSttUri.
//
// Steps:
//   - script-gather-speech-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Kugelaudio_Options(t *testing.T) {
	if !kugelaudioSttReady(t) {
		return
	}
	wav := resolveFixture(t, speechWAV)
	transcript := kugelaudioGather(t, map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioSttLabel,
		"language": "en-US",
		"kugelaudioOptions": map[string]any{
			"model":              "kugel-ear-1",
			"languages":          []string{"en", "de"},
			"turnDetection":      "kugelturn",
			"turnDetectionLevel": "conservative",
			"boostedPhrases":     []string{"sun", "shining"},
			"kugelaudioSttUri":   "api.eu.kugelaudio.com",
		},
	}, wav)
	assertHits(t, transcript, 1, "sun", "shining")

	// vad with a level: the level must be dropped, or KugelAudio fails the session
	transcript = kugelaudioGather(t, map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioSttLabel,
		"language": "en-US",
		"kugelaudioOptions": map[string]any{
			"turnDetection":      "vad",
			"turnDetectionLevel": "conservative",
		},
	}, wav)
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Kugelaudio_Spanish — es-ES maps to KugelAudio's "es";
// gather returns the clip's first sentence, "una mesa para hoy, por favor".
//
// Steps:
//   - script-gather-speech-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Kugelaudio_Spanish(t *testing.T) {
	if !kugelaudioSttReady(t) {
		return
	}
	transcript := kugelaudioGather(t, map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioSttLabel,
		"language": "es-ES",
	}, resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
}

// TestVerb_Gather_Speech_Kugelaudio_RegionCredential — the credential's
// options.api_uri selects the STT host: the EU host transcribes, an
// unreachable one does not (so the field is really used, not ignored).
//
// Steps:
//   - provision-credentials
//   - script-gather-speech-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-no-transcript-unreachable
func TestVerb_Gather_Speech_Kugelaudio_RegionCredential(t *testing.T) {
	if !kugelaudioSttReady(t) {
		return
	}

	s := Step(t, "provision-credentials")
	labels := map[string]string{}
	for name, uri := range map[string]string{
		"eu":          "https://api.eu.kugelaudio.com",
		"unreachable": "https://unreachable.invalid",
	} {
		label := provision.Name("kugelaudio-stt-" + name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
			Vendor: "kugelaudio", Label: label, APIKey: cfg.KugelaudioAPIKey, UseForSTT: true,
			Options: `{"api_uri":"` + uri + `"}`,
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
	eu := kugelaudioGather(t, map[string]any{"vendor": "kugelaudio", "label": labels["eu"], "language": "en-US"}, wav)
	assertHits(t, eu, 1, "sun", "shining")

	bad := kugelaudioGather(t, map[string]any{
		"vendor": "kugelaudio", "label": labels["unreachable"], "language": "en-US",
	}, wav)
	s = Step(t, "assert-no-transcript-unreachable")
	if bad != "" {
		s.Errorf("unreachable api_uri still transcribed %q: api_uri is not reaching the STT connection", bad)
	}
	s.Done()
}
