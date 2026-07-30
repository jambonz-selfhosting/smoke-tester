// Tests for the `gather` and `transcribe` verbs with STT vendor "openai"
// using the gpt-live-transcribe model, plus the openaiOptions it introduced:
//
//   - openaiOptions.model = "gpt-live-transcribe"
//   - openaiOptions.delay      (latency/accuracy trade-off)
//   - openaiOptions.keywords   (literal term hints)
//   - openaiOptions.languages  (language hints as a LIST, not a single code)
//
// Why this needs its own test rather than a model-string swap: unlike
// gpt-4o-transcribe, gpt-live-transcribe rejects OpenAI's server-side turn
// detection outright and — left alone — streams transcript deltas forever
// without ever finalizing an utterance. jambonz therefore endpoints turns
// itself (local VAD in the media layer, then an explicit buffer commit).
//
// The gather test is what pins that: `gather` only fires its actionHook once
// a FINAL transcript arrives, so a callback carrying the fixture's words is
// proof the local endpointing ran mid-call. Were the commit missing, the
// verb would time out with no speech instead.
//
// openai is an OPTIONAL vendor (see config.HasOpenAI /
// provisionOpenaiCredential in verbsmain_test.go): when OPENAI_API_KEY is
// unset, openaiLabel stays "" and these tests pass immediately after a log —
// a plain `return`, never t.Skip, never a failure.
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

// liveTranscribeRecognizer is the recognizer block shared by both tests: the
// model that needs client-side turn endpointing, plus the fields only it
// accepts. "shining" is seeded as a keyword because it is the fixture's last
// word and the one most likely to be clipped if the turn were committed early.
func liveTranscribeRecognizer() map[string]any {
	return map[string]any{
		"vendor": "openai",
		"label":  openaiLabel,
		"openaiOptions": map[string]any{
			"model":     "gpt-live-transcribe",
			"delay":     "low",
			"keywords":  []any{"jambonz", "shining"},
			"languages": []any{"en"},
		},
	}
}

// TestVerb_Gather_Speech_OpenAI_LiveTranscribe — stream the fixture WAV
// ("The sun is shining.") into `gather input=[speech]` with the
// gpt-live-transcribe recognizer and assert the action callback carries the
// spoken words. Receiving a final transcript at all is the assertion that
// matters: this model produces one only when jambonz commits the turn.
func TestVerb_Gather_Speech_OpenAI_LiveTranscribe(t *testing.T) {
	if !cfg.HasOpenAI() || openaiLabel == "" {
		t.Log("OPENAI_API_KEY not set — passing without exercising openai STT")
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

	s = Step(t, "script-gather-speech-openai-live-transcribe")
	actionURL := SessionURL(sess, "gather")
	recognizer := liveTranscribeRecognizer()
	recognizer["language"] = "en-US"
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
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
	// openai realtime dials a websocket and negotiates a transcription
	// session before audio counts — use the LONG pad.
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	// The local endpointer needs trailing silence to call the turn over and
	// commit it; without this the final transcript would only arrive at
	// session teardown, after gather had already given up.
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

	s = Step(t, "assert-transcript")
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload — the turn was never "+
			"committed, so gpt-live-transcribe never finalized: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	normalized := strings.ToLower(transcript)
	if !strings.Contains(normalized, "sun") && !strings.Contains(normalized, "shining") {
		s.Errorf("transcript %q matched neither sun nor shining (truth=%q)", transcript, truth)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_OpenAI_LiveTranscribe — `transcribe` runs continuous
// STT with gpt-live-transcribe. Continuous transcription is where a missing
// turn commit hurts most: every utterance in the call has to be finalized on
// its own, not batched into one transcript at hangup.
func TestVerb_Transcribe_OpenAI_LiveTranscribe(t *testing.T) {
	if !cfg.HasOpenAI() || openaiLabel == "" {
		t.Log("OPENAI_API_KEY not set — passing without exercising openai STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-openai")
	transcriptionURL := SessionURL(sess, "transcription")
	recognizer := liveTranscribeRecognizer()
	recognizer["language"] = "en-US"
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", recognizer),
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

	s = Step(t, "assert-transcript-weather-words")
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
