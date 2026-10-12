// gather / transcribe / agent with STT vendor "gradium"; pass without exercising gradium when GRADIUM_API_KEY is unset.
//
// Not parallel: a Gradium account caps concurrent sessions (3 on the test key),
// and a refused session fails the verb with "Concurrency limit exceeded".
package verbs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Gradium — gather input=[speech] on gradium returns the spoken phrase.
//
// Steps:
//   - load-ground-truth
//   - script-gather-speech-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - wait-action-gather-callback
//   - assert-transcript-sun-shining
//   - hangup
func TestVerb_Gather_Speech_Gradium(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
		return
	}

	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "load-ground-truth")
	wavPath, truthPath := resolveFixture(t, speechWAV), resolveFixture(t, speechTranscriptTxt)
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		s.Fatalf("read truth transcript: %v", err)
	}
	truth := strings.ToLower(strings.TrimSpace(string(truthBytes)))
	s.Logf("ground truth: %q", truth)
	s.Done()

	s = Step(t, "script-gather-speech-gradium")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "gradium",
				"label":    gradiumLabel,
				"language": "en-US",
			}),
		V("hangup"),
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

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
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

	s = Step(t, "assert-transcript-sun-shining")
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	normalized := strings.ToLower(transcript)
	hits := 0
	for _, want := range []string{"sun", "shining"} {
		if strings.Contains(normalized, want) {
			hits++
		}
	}
	if hits == 0 {
		s.Errorf("transcript %q matched neither sun nor shining (truth=%q)", transcript, truth)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_Gradium — transcribe on gradium posts the utterance to transcriptionHook.
//
// Steps:
//   - script-transcribe-pause-hangup-gradium
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - collect-transcription-hook
//   - assert-transcript-sun-shining
//   - hangup
func TestVerb_Transcribe_Gradium(t *testing.T) {
	if !cfg.HasGradium() || gradiumLabel == "" {
		t.Log("GRADIUM_API_KEY not set — passing without exercising gradium STT")
		return
	}

	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-gradium")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":          "gradium",
				"label":           gradiumLabel,
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

	s = Step(t, "wait-for-recognizer")
	// Gradium finalizes on its own semantic end-of-turn; LONG pad so it is armed first.
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

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

	s = Step(t, "assert-transcript-sun-shining")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2",
			transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

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

// gradiumGather runs one gather on gradium with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func gradiumGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-gradium")
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

// assertHits fails unless transcript holds at least min of words (case-insensitive).
func assertHits(t *testing.T, transcript string, min int, words ...string) {
	t.Helper()
	s := Step(t, "assert-transcript")
	normalized := strings.ToLower(transcript)
	hits := 0
	for _, w := range words {
		if strings.Contains(normalized, w) {
			hits++
		}
	}
	s.Logf("recognized: %q (%d/%d of %v)", transcript, hits, len(words), words)
	if hits < min {
		s.Errorf("transcript %q matched %d of %v; want >= %d", transcript, hits, words, min)
	}
	s.Done()
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
