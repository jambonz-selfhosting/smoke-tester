// Tests for `gather` and `transcribe` with STT vendor "openai", covering both
// model families and the openaiOptions gpt-live-transcribe introduced
// (delay, keywords, languages).
//
// Why gpt-live-transcribe needs more than a model-string swap: it rejects
// OpenAI's server-side turn detection and, left alone, streams deltas forever
// without finalizing an utterance — jambonz endpoints turns itself and commits
// the buffer. `gather` fires its actionHook only on a FINAL transcript, so a
// callback carrying the fixture's words IS the proof that ran mid-call; a
// missing commit shows up here as a gather timeout with no speech.
//
// Optional vendor: with OPENAI_API_KEY unset these pass after a log (see
// speechmatics_stt_test.go for the convention).
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

// liveTranscribeRecognizer is the block shared by both live-transcribe tests.
// "shining" is seeded as a keyword: it is the fixture's last word, the one a
// prematurely committed turn would clip.
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

// serverEndpointedRecognizer is the older family, where OpenAI's server VAD
// segments turns. One session-config path in the media layer builds both
// families, so this guards the half whose behavior did not change.
func serverEndpointedRecognizer() map[string]any {
	return map[string]any{
		"vendor": "openai",
		"label":  openaiLabel,
		"openaiOptions": map[string]any{
			"model":          "gpt-4o-transcribe",
			"turn_detection": map[string]any{"type": "server_vad", "silence_duration_ms": 500},
		},
	}
}

// Stream the fixture WAV ("The sun is shining.") into `gather input=[speech]`
// on gpt-live-transcribe. Receiving a final transcript at all is the assertion
// that matters — see the file header.
func TestVerb_Gather_Speech_OpenAI_LiveTranscribe(t *testing.T) {
	gatherWithOpenaiRecognizer(t, liveTranscribeRecognizer)
}

// Same flow on gpt-4o-transcribe, which endpoints server-side: pins that adding
// the client-endpointed family did not disturb it.
func TestVerb_Gather_Speech_OpenAI_ServerVad(t *testing.T) {
	gatherWithOpenaiRecognizer(t, serverEndpointedRecognizer)
}

func gatherWithOpenaiRecognizer(t *testing.T, buildRecognizer func() map[string]any) {
	t.Helper()
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

	s = Step(t, "script-gather-speech-openai")
	actionURL := SessionURL(sess, "gather")
	recognizer := buildRecognizer()
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
	// openai negotiates a transcription session over its websocket first — LONG pad.
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	// The local endpointer needs trailing silence to end the turn; without it the
	// final transcript arrives only at teardown, after gather gave up.
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
		s.Fatalf("no transcript in action/gather payload — on a client-endpointed "+
			"model this means the turn was never committed, so openai never "+
			"finalized it: %s", string(cb.Body))
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

// Continuous STT on gpt-live-transcribe, where a missing turn commit hurts most:
// every utterance must finalize on its own rather than batch up until hangup.
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
	// Bound the wait well inside the script's 15s pause: a transcript that only
	// arrives at session teardown would still land within a 30s window, which is
	// exactly the missing-commit failure this test exists to catch.
	sentAt := time.Now()
	deadline := sentAt.Add(12 * time.Second)
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
			s.Logf("first usable transcript after %s", time.Since(sentAt).Round(time.Millisecond))
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
