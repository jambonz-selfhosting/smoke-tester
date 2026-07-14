// Tests for the `gather` and `transcribe` verbs with STT vendor
// "speechmatics", exercising the speechmaticsOptions added for
// transcript filtering and model selection:
//
//   - transcription_config.model                      (replaces operating_point)
//   - transcription_config.transcript_filtering_config.replacements
//   - transcription_config.transcript_filtering_config.remove_disfluencies
//   - transcription_config.operating_point            (deprecated, back-compat)
//
// speechmatics is an OPTIONAL vendor (see config.HasSpeechmatics /
// provisionSpeechmaticsCredential in verbsmain_test.go): TestMain only
// provisions the speechmatics SpeechCredential when SPEECHMATICS_API_KEY is
// set. When it is unset, speechmaticsLabel stays "" and the tests below pass
// immediately without exercising speechmatics — a plain `return` after a
// log, never t.Skip, never a failure, so the suite stays green with or
// without the key.
//
// The gather test is the interesting one: the fixture WAV says "The sun is
// shining." and the recognizer is configured with a word-replacement rule
// /[Ss]hining/ -> glowing. Seeing "glowing" in the returned transcript
// proves the transcript_filtering_config traveled the whole path
// (webhook JSON -> feature-server channel vars -> media layer ->
// Speechmatics StartRecognition) and was applied by the vendor.
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

// TestVerb_Gather_Speech_Speechmatics — stream a WAV into `gather
// input=[speech]` using recognizer vendor "speechmatics" with
// model:"enhanced" and a transcript_filtering_config replacement rule,
// then assert the replacement shows up in the returned transcript. Clone
// of TestVerb_Gather_Speech_Xai with the recognizer swapped and the new
// speechmaticsOptions exercised.
func TestVerb_Gather_Speech_Speechmatics(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
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

	s = Step(t, "script-gather-speech-speechmatics")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "speechmatics",
				"label":    speechmaticsLabel,
				"language": "en-US",
				"speechmaticsOptions": map[string]any{
					"transcription_config": map[string]any{
						"model": "enhanced",
						"transcript_filtering_config": map[string]any{
							"replacements": []any{
								map[string]any{"from": "/[Ss]hining/", "to": "glowing"},
							},
						},
					},
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
	// speechmatics is streaming with silence-based endpointing plus its own
	// network round-trip — use the LONG pad (>= the deepgram variant's
	// RecognizerArmDelay) so the recognizer is fully armed before the WAV.
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

	s = Step(t, "assert-word-replacement-applied")
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	normalized := strings.ToLower(transcript)
	// "sun" proves the recognizer heard the fixture at all; "glowing"
	// proves the /[Ss]hining/ -> glowing replacement was applied by
	// Speechmatics (the word never occurs in the audio).
	if !strings.Contains(normalized, "sun") && !strings.Contains(normalized, "glowing") {
		s.Fatalf("transcript %q matched neither sun nor glowing (truth=%q)", transcript, truth)
	}
	if strings.Contains(normalized, "shining") {
		s.Errorf("transcript %q still contains %q — replacement rule not applied", transcript, "shining")
	}
	if !strings.Contains(normalized, "glowing") {
		s.Errorf("transcript %q missing replacement %q (truth=%q)", transcript, "glowing", truth)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_Speechmatics — `transcribe` runs continuous STT via
// recognizer vendor "speechmatics" using the DEPRECATED operating_point
// property (back-compat: it must keep working until Speechmatics sunsets
// it) plus remove_disfluencies. Clone of TestVerb_Transcribe_Xai with the
// recognizer swapped.
func TestVerb_Transcribe_Speechmatics(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-speechmatics")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":   "speechmatics",
				"label":    speechmaticsLabel,
				"language": "en-US",
				"speechmaticsOptions": map[string]any{
					"transcription_config": map[string]any{
						"operating_point": "enhanced",
						"transcript_filtering_config": map[string]any{
							"remove_disfluencies": true,
						},
					},
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
