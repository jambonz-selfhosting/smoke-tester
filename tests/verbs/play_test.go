// Tests for the `play` verb.
//
// Schema: schemas/verbs/play — `url` is the usual required field, either a
// single string or an array. Optional: `loop` (number or "forever"),
// `earlyMedia`.
//
// earlyMedia coverage is deferred (see say_test.go comment).
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

const (
	// Short stable public WAV (~3.2 seconds). jambonz fetches + transcodes.
	// Picked short enough that array/loop tests complete in reasonable time.
	playSampleA = "https://samplelib.com/lib/preview/wav/sample-3s.wav"
	playSampleB = "https://samplelib.com/lib/preview/wav/sample-3s.wav"
)

// TestVerb_Play_Basic — single URL, single playback. 3s sample.
func TestVerb_Play_Basic(t *testing.T) {
	t.Parallel()
	runPlay(t, "play-basic",
		[]map[string]any{V("play", "url", playSampleA), V("hangup")})
}

// TestVerb_Play_Loop2 — source played twice (~6s).
func TestVerb_Play_Loop2(t *testing.T) {
	t.Parallel()
	runPlay(t, "play-loop2",
		[]map[string]any{V("play", "url", playSampleA, "loop", 2), V("hangup")})
}

// TestVerb_Play_ArrayOfURLs — list of URLs plays sequentially (~6s total).
func TestVerb_Play_ArrayOfURLs(t *testing.T) {
	t.Parallel()
	runPlay(t, "play-array",
		[]map[string]any{V("play", "url", []any{playSampleA, playSampleB}), V("hangup")})
}

// runPlay places the call, answers, records, sends silence, waits for BYE,
// then asserts a minimum amount of PCM arrived.
//
// Steps (shared by all TestVerb_Play_* variants):
//  1. place-call — POST /Calls with [answer, pause, play ...]
//  2. answer-record-and-wait-end — record PCM, send silence, block on end
//  3. assert-audio-bytes — at least 4000 PCM bytes captured, RMS non-trivial
func runPlay(t *testing.T, tag string, verbs []map[string]any, extras ...func(*provision.CallCreate)) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)
	opts := append([]func(*provision.CallCreate){withTimeLimit(20)}, extras...)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, WithWarmup(verbs), opts...)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-bytes")
	AssertAudioBytes(s, call, 4000, tag)
	s.Done()
}
