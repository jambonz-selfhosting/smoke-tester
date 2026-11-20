// gather / transcribe with STT vendor "assemblyai" across the streaming speech
// models; pass without exercising it when ASSEMBLYAI_API_KEY is unset.
//
// Not parallel: the AssemblyAI account caps concurrent streaming sessions and
// keeps a closed session counted for a while, refusing new ones with
// "Too many concurrent sessions".
package verbs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	assemblyaiRealtimeAgent = "assemblyai-stt-3-7-realtime-agent"
	assemblyaiRealtime      = "assemblyai-stt-3-7-realtime"
	// AssemblyAI rejects an unknown speech_model with error 3006, so a
	// transcript proves the model that reached it was not this one
	assemblyaiInvalidModel = "jambonz-invalid-model"
	assemblyaiSessionCap   = "Too many concurrent sessions"
)

func assemblyaiSttReady(t *testing.T) bool {
	t.Helper()
	if !cfg.HasAssemblyai() {
		t.Log("ASSEMBLYAI_API_KEY not set — passing without exercising assemblyai STT")
		return false
	}
	return true
}

// assemblyaiSttCredential creates an STT-only AssemblyAI credential with the
// given speech_model and returns its label.
func assemblyaiSttCredential(t *testing.T, ctx context.Context, name, speechModel string) string {
	t.Helper()
	s := Step(t, "provision-stt-credential")
	label := provision.Name("assemblyai-stt-" + name)
	client.ManagedAccountSpeechCredential(t, ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor: "assemblyai", Label: label, APIKey: cfg.AssemblyaiAPIKey,
		UseForSTT: true, SpeechModel: speechModel,
	})
	s.Done()
	return label
}

// assemblyaiResult is the speech part of a gather action.
type assemblyaiResult struct {
	Reason     string
	Details    string
	Transcript string
	Languages  []string
}

// assemblyaiGather runs one gather with recognizer, plays wavPath, and returns
// the action's outcome. A refusal for the account's session cap is retried
// after the vendor releases a slot; any other error is returned as is.
func assemblyaiGather(t *testing.T, ctx context.Context, recognizer map[string]any, wavPath string) assemblyaiResult {
	t.Helper()
	var r assemblyaiResult
	for attempt := 1; attempt <= 3; attempt++ {
		r = assemblyaiGatherOnce(t, ctx, recognizer, wavPath)
		if !strings.Contains(r.Details, assemblyaiSessionCap) {
			return r
		}
		t.Logf("attempt %d refused by the AssemblyAI account session cap; retrying", attempt)
		WaitFor(t, "wait-for-assemblyai-session-release", 30*time.Second)
	}
	return r
}

func assemblyaiGatherOnce(t *testing.T, ctx context.Context, recognizer map[string]any, wavPath string) assemblyaiResult {
	t.Helper()
	requireWebhook(t)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-assemblyai")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
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
	var body struct {
		Reason  string `json:"reason"`
		Details string `json:"details"`
		Speech  struct {
			Languages []string `json:"languages"`
		} `json:"speech"`
	}
	if err := json.Unmarshal(cb.Body, &body); err != nil {
		s.Fatalf("decode action/gather body: %v", err)
	}
	s.Done()

	_ = call.Hangup()
	return assemblyaiResult{
		Reason: body.Reason, Details: body.Details,
		Transcript: extractTranscript(cb), Languages: body.Speech.Languages,
	}
}

// assertAssemblyaiRejectedModel asserts the gather failed on AssemblyAI's
// speech_model validation, i.e. the invalid model reached the vendor.
func assertAssemblyaiRejectedModel(t *testing.T, r assemblyaiResult) {
	t.Helper()
	s := Step(t, "assert-invalid-model-rejected")
	if r.Reason != "error" || !strings.Contains(r.Details, "speech_model") {
		s.Errorf("want reason=error naming speech_model, got reason=%q details=%q transcript=%q",
			r.Reason, r.Details, r.Transcript)
	}
	s.Done()
}

// TestVerb_Gather_Speech_Assemblyai_Models — assemblyAiOptions.speechModel
// overrides the credential's model: the credential carries a model AssemblyAI
// rejects, and each streaming model set on the verb transcribes anyway.
//
// Steps:
//   - provision-stt-credential
//   - script-gather-speech-assemblyai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Assemblyai_Models(t *testing.T) {
	if !assemblyaiSttReady(t) {
		return
	}
	// covers all subtests, which run inside this test
	ctx := WithTimeout(t, 12*time.Minute)
	label := assemblyaiSttCredential(t, ctx, "models", assemblyaiInvalidModel)
	wav := resolveFixture(t, speechWAV)
	for _, model := range []string{assemblyaiRealtimeAgent, assemblyaiRealtime, "universal-3-6-pro", "u3-rt-pro"} {
		t.Run(model, func(t *testing.T) {
			ctx := WithTimeout(t, 3*time.Minute)
			r := assemblyaiGather(t, ctx, map[string]any{
				"vendor": "assemblyai", "label": label, "language": "en-US",
				"assemblyAiOptions": map[string]any{"speechModel": model},
			}, wav)
			assertHits(t, r.Transcript, 1, "sun", "shining")
		})
	}
}

// TestVerb_Transcribe_Assemblyai — transcribe on 3.7 Realtime posts the
// utterance to transcriptionHook.
//
// Steps:
//   - provision-stt-credential
//   - script-transcribe-pause-hangup-assemblyai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - post-speech-silence
//   - collect-transcription-hook
//   - assert-transcript
//   - hangup
func TestVerb_Transcribe_Assemblyai(t *testing.T) {
	if !assemblyaiSttReady(t) {
		return
	}
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	label := assemblyaiSttCredential(t, ctx, "transcribe", assemblyaiRealtime)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-assemblyai")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor": "assemblyai", "label": label, "language": "en-US",
				"singleUtterance":   true,
				"assemblyAiOptions": map[string]any{"speechModel": assemblyaiRealtime},
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
