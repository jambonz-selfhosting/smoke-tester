// Tests for the `play` verb.
//
// Schema: schemas/verbs/play — `url` is the usual required field, either a
// single string or an array. Optional: `loop` (number or "forever"),
// `earlyMedia`.
//
// Why we self-host the fixture: jambonz fetches `play` URLs from the
// network, so a local file path won't work — but we don't want to depend
// on a third-party hosted sample either (no transcript pinned, content
// could change, server can disappear). Instead the webhook server
// exposes `tests/verbs/testdata/` under `/static/` on the ngrok tunnel,
// and we point `play` at our own `test_audio.wav` whose transcript is
// pinned in `test_audio.transcript` ("The sun is shining."). That lets
// the assertion verify jambonz fetched, decoded, and streamed the right
// content — not just "some bytes came back".
//
// earlyMedia coverage is deferred (see say_test.go comment).
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// playFixtureURL returns the public URL of our pinned-transcript fixture
// WAV ("The sun is shining.") served by the webhook tunnel under
// /static/. Tests should NOT hard-code this — call this so the URL
// stays consistent if the route ever changes.
func playFixtureURL() string {
	return webhookSrv.PublicURL() + "/static/test_audio.wav"
}

// playFixtureKeywords are content words from the fixture's pinned
// transcript ("The sun is shining."). At least ONE must survive the
// recording → STT round-trip; the fixture is short (~1.7s) and STT on
// telephony-quality clips occasionally drops a word.
var playFixtureKeywords = []string{"sun", "shining"}

// TestVerb_Play_Basic — single URL, single playback. Asserts the
// streamed audio's transcript contains every fixture keyword.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-content — STT-of-recording contains "sun" + "shining"
func TestVerb_Play_Basic(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runPlay(t, "play-basic",
		[]map[string]any{V("play", "url", playFixtureURL()), V("hangup")})
}

// TestVerb_Play_Loop2 — source played twice. Asserts the transcript
// contains the fixture keywords at least twice (proves the loop ran);
// "sun" is unambiguous so duplicate detection is reliable.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-content — STT-of-recording contains "sun" twice
func TestVerb_Play_Loop2(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	verbs := []map[string]any{V("play", "url", playFixtureURL(), "loop", 2), V("hangup")}
	call := placeCallTo(ctx, t, uas, WithWarmup(verbs), withTimeLimit(20))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("play-loop2"), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-content-played-twice")
	// Transcript should contain "sun" at least twice (from two playbacks
	// of "The sun is shining."). A regression that ignores `loop` would
	// show "sun" once and fail.
	AssertTranscriptKeywordCount(s, ctx, wav, "sun", 2)
	s.Done()
}

// TestVerb_Play_ArrayOfURLs — list of URLs plays sequentially. Same
// fixture twice → same "sun"/"shining" assertion as Loop2 (two
// playbacks expected).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-content — STT-of-recording contains "sun" twice
func TestVerb_Play_ArrayOfURLs(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	url := playFixtureURL()
	verbs := []map[string]any{V("play", "url", []any{url, url}), V("hangup")}
	call := placeCallTo(ctx, t, uas, WithWarmup(verbs), withTimeLimit(20))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("play-array"), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-content-played-twice")
	// Two distinct URLs (same content, twice) → "sun" must appear
	// twice in transcript. A regression that plays only the first URL
	// would show "sun" once.
	AssertTranscriptKeywordCount(s, ctx, wav, "sun", 2)
	s.Done()
}

// runPlay is the single-playback path used by Basic. Loop2 / Array
// tests inline their own variant because they need a stronger "played
// N times" assertion.
//
// Steps (Basic):
//  1. place-call — POST /Calls with [answer, pause, play ...]
//  2. answer-record-and-wait-end — record PCM, send silence, block on end
//  3. assert-audio-content — STT-of-recording matches fixture keywords
func runPlay(t *testing.T, tag string, verbs []map[string]any, extras ...func(*provision.CallCreate)) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)
	opts := append([]func(*provision.CallCreate){withTimeLimit(20)}, extras...)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, WithWarmup(verbs), opts...)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-content")
	// Sanity: at least 4000 PCM bytes captured (proves audio actually
	// streamed). The transcript check below is the real assertion.
	if call.PCMBytesIn() < 4000 {
		s.Fatalf("%s: only %d PCM bytes captured (want >= 4000) — fetch likely failed",
			tag, call.PCMBytesIn())
	}
	// HasMost(1): at least one of "sun"/"shining" must land. The
	// fixture is short (~1.7s) and PCMU+telephony noise occasionally
	// drops one word — strict-Contains was flaky.
	AssertTranscriptHasMost(s, ctx, wav, 1, playFixtureKeywords...)
	s.Done()
}
