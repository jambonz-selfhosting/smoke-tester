// gather / transcribe / agent with STT vendor "kugelaudio" (kugel-ear-1); pass without exercising it when KUGELAUDIO_API_KEY is unset.
//
// Not parallel, so these tests don't share the account with the KugelAudio TTS
// and noise isolation tests running at the same time.
package verbs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func kugelaudioSttReady(t *testing.T) bool {
	t.Helper()
	if !cfg.HasKugelaudio() || kugelaudioSttLabel == "" {
		t.Log("KUGELAUDIO_API_KEY not set — passing without exercising kugelaudio STT")
		return false
	}
	return true
}

// kugelaudioGather runs one gather with recognizer, plays wavPath, and returns
// the recognized transcript ("" when the action has none).
func kugelaudioGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-kugelaudio")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		// keep the call up while a long clip is still streaming; the test hangs up
		V("pause", "length", 20),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	_ = call.Hangup()
	return extractTranscript(cb)
}

// TestVerb_Gather_Speech_Kugelaudio — gather input=[speech] on kugelaudio returns the spoken phrase.
//
// Steps:
//   - script-gather-speech-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Kugelaudio(t *testing.T) {
	if !kugelaudioSttReady(t) {
		return
	}
	transcript := kugelaudioGather(t, map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioSttLabel,
		"language": "en-US",
	}, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Transcribe_Kugelaudio — transcribe on kugelaudio posts the utterance to transcriptionHook.
//
// Steps:
//   - script-transcribe-pause-hangup-kugelaudio
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - collect-transcription-hook
//   - assert-transcript
//   - hangup
func TestVerb_Transcribe_Kugelaudio(t *testing.T) {
	if !kugelaudioSttReady(t) {
		return
	}

	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-kugelaudio")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":          "kugelaudio",
				"label":           kugelaudioSttLabel,
				"language":        "en-US",
				"singleUtterance": true,
			}),
		V("pause", "length", 15),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "send-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/transcribe", transcribeText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hook")
	deadline := time.Now().Add(30 * time.Second)
	var parts []string
	transcript := ""
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			if cb, err := tryPop(sess); err == nil {
				if cb.Hook != "action/transcription" {
					continue
				}
				s.Logf("action/transcription body: %s", string(cb.Body))
				if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
					parts = append(parts, seg)
					transcript = strings.Join(parts, " ")
				}
			}
		}
		if transcript != "" && transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	s = Step(t, "assert-transcript")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

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
