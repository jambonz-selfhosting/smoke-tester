// Tests for the `gather` verb with `input=[speech]`.
//
// Companion to gather_test.go (which covers input=[digits] via DTMF). This
// test streams a known WAV file as the caller's outbound audio and asserts
// that jambonz's STT recognizer — running under the `gather` verb — posts
// a transcript back to us that contains the expected phrase.
//
// The WAV is `tests/testdata/test_audio.wav` (copied from
// api-server/data/test_audio.wav, which jambonz itself uses to smoke-test
// Deepgram credentials). The expected transcript is pinned in
// `tests/testdata/test_audio.transcript` — one-shot Deepgram-generated
// source of truth. Do not regenerate casually.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	speechWAV           = "testdata/test_audio.wav"
	speechTranscriptTxt = "testdata/test_audio.transcript"
)

// TestVerb_Gather_Speech — stream a WAV into `gather input=[speech]`,
// assert jambonz's STT returns the expected phrase via action/gather.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. load-ground-truth — read pinned transcript from testdata/
//  3. script-gather-speech — call_hook=[gather speech], action/gather=empty ack
//  4. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  5. answer-and-silence — 200 OK + outbound silence to prime recognizer
//  6. wait-for-recognizer — 1500ms so STT arms before speech starts
//  7. send-wav — stream testdata/test_audio.wav into the call
//  8. post-speech-silence — trailing silence to trigger end-of-utterance
//  9. wait-action-gather-callback — block on /action/gather HTTP callback
// 10. assert-transcript-sun-shining — returned transcript contains "sun" + "shining"
// 11. hangup — best-effort tear-down
//
// Test     --POST /Calls-->                              Jambonz
// Jambonz  --GET /hook-->                                Webhook
// Webhook  --[answer, pause, gather input=[speech], hangup]--> Jambonz
// Jambonz  --INVITE-->                                   UAS (answer)
// UAS      ==RTP (WAV speech)==>                         Jambonz (recognize)
// Jambonz  --POST /action/gather {speech.transcript:...}-> Webhook // assert
// Jambonz  --BYE-->                                      UAS
func TestVerb_Gather_Speech(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "load-ground-truth")
	wavPath, truthPath := resolveFixture(t, speechWAV), resolveFixture(t, speechTranscriptTxt)
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		s.Fatalf("read truth transcript: %v", err)
	}
	truth := strings.ToLower(strings.TrimSpace(string(truthBytes)))
	s.Logf("ground truth: %q", truth)
	s.Done()

	s = Step(t, "script-gather-speech")
	actionURL := webhookSrv.PublicURL() + "/action/gather"
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL),
		V("hangup"),
	}))
	sess.ScriptActionHook("gather", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Send 1.5s of silence first to establish the outbound RTP stream and
	// let jambonz's STT recognizer fully prime. Without enough prelude the
	// first words of the WAV get clipped (e.g. "The sun is shining" →
	// "Shining"). Empirically 500ms clipped most of the phrase; 1500ms
	// captures all of it.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	// Resume silence after the speech so jambonz's STT has trailing
	// "non-speech" audio to trigger its end-of-utterance detector. Without
	// trailing silence Google STT often waits the full gather timeout
	// before emitting a final transcript.
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
	// Payload shape (jambonz):
	//   { speech: { is_final: true, alternatives: [{transcript: "...", confidence: ...}] }, reason: "speechDetected" }
	// or under some configs the transcript is at the top level.
	transcript := extractTranscript(cb.JSON)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	// Distinctive words from the pinned truth. "sun", "shining" together
	// are content-bearing and unlikely to coincide; "the ... is" is
	// filler. Require both.
	normalized := strings.ToLower(transcript)
	for _, want := range []string{"sun", "shining"} {
		if !strings.Contains(normalized, want) {
			s.Errorf("transcript %q missing %q (truth=%q)", transcript, want, truth)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// extractTranscript pulls the recognized string from a gather actionHook
// payload, tolerating the two shapes jambonz emits (top-level `speech`
// object with alternatives[], or a flat top-level `speech` string).
func extractTranscript(m map[string]any) string {
	if m == nil {
		return ""
	}
	if s, ok := m["speech"].(string); ok && s != "" {
		return s
	}
	sp, ok := m["speech"].(map[string]any)
	if !ok {
		return ""
	}
	if t, ok := sp["transcript"].(string); ok && t != "" {
		return t
	}
	alts, ok := sp["alternatives"].([]any)
	if !ok || len(alts) == 0 {
		return ""
	}
	a0, ok := alts[0].(map[string]any)
	if !ok {
		return ""
	}
	if t, ok := a0["transcript"].(string); ok {
		return t
	}
	return ""
}

// resolveFixture returns the absolute path of a file under tests/testdata/.
// Go test binaries run with CWD set to the package dir, so "testdata/foo"
// from tests/verbs resolves correctly.
func resolveFixture(t *testing.T, rel string) string {
	t.Helper()
	abs, err := filepath.Abs(rel)
	if err != nil {
		recordFailure(t, "resolve-fixture", fmt.Sprintf("abs(%s): %v", rel, err))
		t.Fatalf("abs(%s): %v", rel, err)
	}
	if _, err := os.Stat(abs); err != nil {
		recordFailure(t, "resolve-fixture", fmt.Sprintf("fixture %s: %v", rel, err))
		t.Fatalf("fixture %s: %v", rel, err)
	}
	return abs
}
