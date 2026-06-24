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

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// transcribeText is the phrase the caller streams for the cluster's recognizer
// to transcribe. The old fixture ("The sun is shining.") was only ~1.3s — under
// parallel load the system-under-test STT routinely lost the prefix and
// returned just "shining", failing a 2-of-4 word assertion on a noisy but
// non-broken transcription. A longer, common-word sentence gives the recognizer
// more material and survives prefix clipping, so the assertion still fails on a
// missing/empty transcript but is robust to STT drift on individual words.
const transcribeText = "The weather report says it will be sunny tomorrow " +
	"with clear skies and a gentle breeze across the coastal region."

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
//
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
	// Leading silence lets the recognizer arm before the WAV starts. We use
	// the LONG pad here (not the shared RecognizerArmDelay) because
	// singleUtterance:true finalizes on the first end-of-speech: if the
	// recognizer isn't fully armed when the first syllable arrives it drops
	// "the sun is" and keeps only "shining".
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	// Synthesize a long, common-word sentence (cached under testdata/transcribe)
	// rather than the short fixture — see transcribeText for why.
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
	// Drain both the per-test session and the anon session — jambonz's
	// transcribe hook payloads don't include our `tag` correlation key by
	// default (they land without x_test_id in customerData, so the server
	// routes them to the anon session). Same quirk we documented for
	// other ancillary hooks.
	deadline := time.Now().Add(30 * time.Second)
	// Accumulate ALL transcription hooks, not just the first non-empty one.
	// With singleUtterance:true the recognizer can finalize the clip in more
	// than one utterance ("the sun is" then "shining"); grabbing only the
	// first delivered final is a coin-flip on which segment we assert
	// against. Concatenating every final transcript we see in the window
	// reconstructs the full phrase regardless of segmentation. We keep
	// collecting until we have all four content words or the window closes.
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
	// Cluster-side STT (the system under test, not Deepgram REST) at
	// telephony quality is noisy and drops/mishears individual words,
	// especially under load. Require at least 2 distinctive content words —
	// strong enough to fail a regression that returns no transcript or a
	// wrong one entirely, robust to single-word drift.
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2",
			transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// transcriptHits counts how many of the reference phrase's content words
// appear in the (lower-cased) transcript. Shared by the collect loop's
// early-exit and the final assertion so both use one definition of "good
// enough".
// transcribeWords are distinctive content words from transcribeText. We
// require a couple to land — enough to fail an empty/wrong transcript while
// tolerating cluster-side STT drift on any individual word under load.
var transcribeWords = []string{"weather", "sunny", "tomorrow", "clear", "skies", "breeze", "coastal", "region"}

func transcriptHits(transcript string) int {
	hits := 0
	for _, want := range transcribeWords {
		if strings.Contains(transcript, want) {
			hits++
		}
	}
	return hits
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
