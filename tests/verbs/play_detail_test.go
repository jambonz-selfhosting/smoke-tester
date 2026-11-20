//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
