//go:build detail

package verbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// googleStreamLimit is google v1's hard cap on one streaming recognize call.
const googleStreamLimit = 305 * time.Second

// TestVerb_Transcribe_Google_Rollover — a background transcribe on google v1
// keeps transcribing after google ends the stream at 305s: jambonz must open a
// fresh stream (max_duration_exceeded → restart) on the same bugname.
//
// Steps:
//  1. provision-google-v1-credential — service key, no gemini model, tested
//  2. script-transcribe-pause-hangup — [transcribe google, pause 400s, hangup]
//  3. place-call / answer-and-silence
//  4. speak-before-limit + collect — baseline transcript on the first stream
//  5. wait-past-stream-limit — silence until 315s after answer
//  6. speak-after-limit + collect — transcript must arrive on the new stream
//
// Runs ~6 minutes, so detail-only: -tags detail -timeout 15m.
func TestVerb_Transcribe_Google_Rollover(t *testing.T) {
	if !cfg.HasGeminiStt() {
		t.Log("GEMINI_KEYFILE not set — passing without exercising google v1 rollover")
		return
	}
	requireWebhook(t)
	ctx := WithTimeout(t, 9*time.Minute)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "provision-google-v1-credential")
	label := "it-google-v1-" + provision.RunID()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "google",
		Label:      label,
		ServiceKey: cfg.GeminiServiceKey,
		UseForSTT:  true,
	})
	if err != nil {
		s.Fatalf("create google credential: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = client.DeleteAccountSpeechCredential(cctx, suite.AccountSID, sid)
	})
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, sid); err != nil {
		s.Fatalf("test google credential: %v", err)
	}
	s.Done()

	s = Step(t, "script-transcribe-pause-hangup")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", SessionURL(sess, "transcription"),
			"recognizer", map[string]any{
				"vendor":   "google",
				"label":    label,
				"language": "en-US",
			}),
		V("pause", "length", 400),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(450))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	answered := time.Now()
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	wavPath, err := tts.EnsureWAV(ctx, "testdata/transcribe", transcribeText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		t.Fatalf("EnsureWAV: %v", err)
	}
	speakAndCollect := func(name string) {
		s := Step(t, name)
		if err := call.SendWAV(wavPath); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("post-SendSilence: %v", err)
		}
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			for _, ds := range sessionsToDrain(sess) {
				cb, err := tryPop(ds)
				if err != nil || cb.Hook != "action/transcription" {
					continue
				}
				seg := strings.ToLower(extractTranscript(cb))
				if strings.Contains(seg, "weather") || strings.Contains(seg, "sunny") {
					s.Logf("transcript %.0fs after answer: %q", time.Since(answered).Seconds(), seg)
					s.Done()
					return
				}
			}
			time.Sleep(200 * time.Millisecond)
		}
		s.Fatalf("no transcript containing weather/sunny within 30s (%.0fs after answer)",
			time.Since(answered).Seconds())
	}

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)
	speakAndCollect("speak-before-limit")

	WaitFor(t, "wait-past-stream-limit", time.Until(answered.Add(googleStreamLimit+10*time.Second)))
	speakAndCollect("speak-after-limit")

	_ = call.Hangup()
}
