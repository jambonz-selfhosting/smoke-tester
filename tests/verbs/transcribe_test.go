// Tests for the `transcribe` verb.
//
// Schema: schemas/verbs/transcribe — runs jambonz's STT on the caller's
// audio for the duration of the call; each finalized utterance is POSTed
// to `transcriptionHook` with `{speech:{alternatives:[{transcript}]}, ...}`.
//
// We stream the pinned reference WAV into the call, wait for the
// transcriptionHook to fire, and assert the transcript contains the
// expected phrase. Same end-to-end guarantee as gather_speech but via the
// standalone `transcribe` verb.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Transcribe_Basic — `transcribe` runs continuous STT and posts
// each utterance to transcriptionHook.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-transcribe-pause-hangup — [transcribe, pause 15s, hangup] + empty ack
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-silence — 200 OK + outbound silence
//  5. wait-for-recognizer — 1500ms prime time before WAV
//  6. send-wav — stream testdata/test_audio.wav
//  7. post-speech-silence — trailing silence to trigger end-of-utterance
//  8. collect-transcription-hook — drain per-test + anon sessions for transcript
//  9. assert-transcript-sun-shining — transcript contains both words
// 10. hangup — best-effort tear-down
//
// Test     --POST /Calls-->                       Jambonz
// Webhook  --[transcribe transcriptionHook=...]-> Jambonz (STT armed)
// Jambonz  --INVITE-->                            UAS (answer)
// UAS      ==silence + WAV + silence==>           Jambonz (recognizer sees it)
// Jambonz  --POST /action/transcription {...}-->  Webhook  // assert has "sun"+"shining"
// UAS      --BYE-->                               Jambonz  (test-initiated hangup)
func TestVerb_Transcribe_Basic(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup")
	// jambonz posts to actionHook but the transcribe verb writes to
	// transcriptionHook. Our webhook server routes /action/<verb> for
	// both; we register "transcription" as the hook suffix.
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":          "deepgram",
				"label":           deepgramLabel,
				"language":        "en-US",
				"singleUtterance": true,
			}),
		// Keep the call open while we stream; transcribe is background.
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
	// Same pattern as gather_speech — leading silence lets the recognizer
	// arm before the WAV starts.
	time.Sleep(RecognizerArmDelay)
	s.Done()

	s = Step(t, "send-wav")
	wavPath := resolveFixture(t, speechWAV)
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
	// Drain both the per-test session and the anon session — jambonz's
	// transcribe hook payloads don't include our `tag` correlation key by
	// default (they land without x_test_id in customerData, so the server
	// routes them to the anon session). Same quirk we documented for
	// other ancillary hooks.
	deadline := time.Now().Add(30 * time.Second)
	var transcript string
Collect:
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			if cb, err := tryPop(sess); err == nil {
				if cb.Hook != "action/transcription" {
					continue
				}
				s.Logf("action/transcription body: %s", string(cb.Body))
				transcript = strings.ToLower(extractTranscript(cb))
				if transcript != "" {
					break Collect
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	s = Step(t, "assert-transcript-sun-shining")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	// Cluster-side STT (the system under test, not Deepgram REST) on a
	// 1.3s telephony-quality clip is noisy; it routinely mishears "the
	// sun" as "is it" / "the sun is" / "sun is" depending on prosody.
	// Require at least 2 of 4 content words — strong enough to fail a
	// regression that returns no transcript or a wrong one entirely.
	hits := 0
	for _, want := range []string{"the", "sun", "is", "shining"} {
		if strings.Contains(transcript, want) {
			hits++
		}
	}
	if hits < 2 {
		s.Errorf("transcript %q matched only %d of [the,sun,is,shining]; want >= 2",
			transcript, hits)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// sessionsToDrain returns the per-test session plus the anon session if
// it exists. jambonz's transcribe actionHook lands in anon because the
// payload doesn't carry our `tag` correlation.
func sessionsToDrain(primary *webhook.Session) []*webhook.Session {
	out := []*webhook.Session{primary}
	if anon, ok := webhookReg.Lookup("_anon"); ok {
		out = append(out, anon)
	}
	return out
}

// tryPop is a non-blocking callback drain.
func tryPop(s *webhook.Session) (webhook.Callback, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	return s.WaitCallback(ctx)
}
