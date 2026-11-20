//go:build detail

package verbs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Xai_PinnedModel — same flow as
// TestVerb_Gather_Speech_Xai, but pins xaiOptions.model to Grok Voice
// Transcribe 2.0 rather than riding whatever xAI currently defaults to.
// Guards the model -> XAI_SPEECH_MODEL -> `model=` query param path end to end.
func TestVerb_Gather_Speech_Xai_PinnedModel(t *testing.T) {
	if !cfg.HasXai() || xaiLabel == "" {
		t.Log("XAI_API_KEY not set — passing without exercising xai STT")
		return
	}

	t.Parallel()
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

	s = Step(t, "script-gather-speech-xai-model")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "xai",
				"label":    xaiLabel,
				"language": "en-US",
				"xaiOptions": map[string]any{
					"model": xaiSttModel,
				},
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
		// A rejected/unknown model would show up here as no transcript at all.
		s.Fatalf("WaitCallbackFor action/gather (model=%s): %v", xaiSttModel, err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transcript-sun-shining")
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload (model=%s): %s", xaiSttModel, string(cb.Body))
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
