// Tests for the `gather` and `transcribe` verbs with STT vendor "google"
// routed to the Gemini Live transcription models.
//
// Gemini is NOT a separate vendor: the credential is a normal google speech
// credential, and what selects the Gemini Live API is the model name — here
// stored on the credential as stt_model_id (provisionGeminiCredential), which
// is the "set a default and forget it" path, and passed per-verb as
// recognizer.model in the transcribe test, which is the override path. Both
// have to work, so each test exercises one of them.
//
// Optional vendor (see config.HasGeminiStt / provisionGeminiCredential in
// verbsmain_test.go): TestMain provisions the credential only when
// GEMINI_API_KEY is set alongside a service-account key. When unset,
// geminiLabel stays "" and both tests below pass immediately after a log —
// a plain `return`, never t.Skip, never a failure.
//
// Mirrors xai_stt_test.go. Gemini Live is a websocket with server-side
// endpointing plus a network round-trip, so timings match the xai variants.
package verbs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Gemini — the credential's stt_model_id alone routes
// the call to Gemini Live: the recognizer names no model at all, exactly as a
// user who set a default in the portal would write it.
func TestVerb_Gather_Speech_Gemini(t *testing.T) {
	if !cfg.HasGeminiStt() || geminiLabel == "" {
		t.Log("GEMINI_API_KEY not set — passing without exercising gemini STT")
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

	s = Step(t, "script-gather-speech-gemini")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "google",
				"label":    geminiLabel,
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
		// The likeliest cause is a cluster whose mediajam predates the gemini
		// dialect: it logs `stt vendor "google"` reaching the v1 path with a
		// gemini model, or the model being refused. Check
		// /var/log/mediajam/mediajam.log for this callSid before suspecting audio.
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

// TestVerb_Transcribe_Gemini — the override path: recognizer.model names the
// gemini model per-verb, and googleOptions carries the gemini-only knobs
// (SMART formatting, a custom vocabulary). Diarization and word timestamps
// are deliberately absent — Gemini Live does not support them.
func TestVerb_Transcribe_Gemini(t *testing.T) {
	if !cfg.HasGeminiStt() || geminiLabel == "" {
		t.Log("GEMINI_API_KEY not set — passing without exercising gemini STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-gemini")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":   "google",
				"label":    geminiLabel,
				"language": "en-US",
				"model":    geminiSttModel,
				"googleOptions": map[string]any{
					"mode":             "SMART",
					"customVocabulary": []any{"jambonz", "drachtio"},
				},
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

	s = Step(t, "assert-transcript")
	if transcript == "" {
		s.Fatalf("no transcript delivered to transcriptionHook")
	}
	s.Logf("recognized: %q", transcript)
	if transcriptHits(transcript) < 2 {
		s.Errorf("transcript %q matched fewer than 2 expected words", transcript)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}
