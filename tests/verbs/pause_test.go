// Tests for the `pause` verb.
//
// Schema: schemas/verbs/pause — required field `length` (seconds to pause
// before continuing to the next verb). Produces silence during the pause.
package verbs

import (
	"testing"
	"time"
)

// TestVerb_Pause_1s — shortest useful pause.
//
// Steps (shared with TestVerb_Pause_3s via runPauseTest):
//  1. place-call — POST /Calls with [pause length=N]
//  2. answer-record-and-wait-end — 200 OK, record, send silence, block on end
//  3. assert-duration-and-silence — duration ≈ N seconds, RMS is low (silent)
func TestVerb_Pause_1s(t *testing.T) { runPauseTest(t, 1) }

// TestVerb_Pause_3s — longer pause to be sure duration is actually honored.
func TestVerb_Pause_3s(t *testing.T) { runPauseTest(t, 3) }

func runPauseTest(t *testing.T, seconds int) {
	t.Helper()
	ctx := WithTimeout(t, time.Duration(seconds+20)*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, []map[string]any{V("pause", "length", seconds)})
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("pause"), WithSilence())
	s.Done()

	s = Step(t, "assert-duration-and-silence")
	want := time.Duration(seconds) * time.Second
	d := call.Duration()
	// Tolerance: ±1.5s for network jitter + verb teardown.
	if d < want-500*time.Millisecond || d > want+3*time.Second {
		s.Errorf("duration out of window: got %s want ~%s", d, want)
	}
	if call.RMS() > 200 {
		s.Errorf("pause audio not silent: rms=%.1f", call.RMS())
	}
	s.Logf("pause(%ds): duration=%s rms=%.1f", seconds, d, call.RMS())
	s.Done()
}
