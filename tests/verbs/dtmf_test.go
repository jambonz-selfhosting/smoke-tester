// Tests for the `dtmf` verb.
//
// Schema: schemas/verbs/dtmf — sends digits mid-call to the peer (us).
// Required: `dtmf` (string of digits). Valid characters: 0-9, *, #, A-D,
// and 'w' for a 500ms inter-digit pause. Optional `duration` (ms per
// tone, default 500).
//
// Digits captured on our side via RFC 2833 decode in the audio pipeline
// (Call.StartRecording wires the diago DTMFReader).
//
// A trailing pause keeps the call open long enough for late RTP events to
// arrive before jambonz auto-hangs-up. It is NOT a pause-verb test.
package verbs

import (
	"testing"
	"time"
)

// TestVerb_Dtmf_SingleDigit — verb sends "5".
func TestVerb_Dtmf_SingleDigit(t *testing.T) { expectDTMF(t, "5", "5", "dtmf-single", 2) }

// TestVerb_Dtmf_MultiDigit — sends "1w2w3w4" with explicit inter-digit
// pauses; the receiver should observe "1234".
func TestVerb_Dtmf_MultiDigit(t *testing.T) { expectDTMF(t, "1w2w3w4", "1234", "dtmf-multi", 5) }

// TestVerb_Dtmf_Symbols — "*#0" with explicit inter-digit pauses.
func TestVerb_Dtmf_Symbols(t *testing.T) { expectDTMF(t, "*w#w0", "*#0", "dtmf-symbols", 3) }

// expectDTMF dials, sends the given digits via the dtmf verb, and asserts
// the receiver observed at least the expected sequence.
//
//	sent     — the `dtmf` string on the verb (may include 'w' pauses)
//	wantSeq  — digits the receiver should actually see (no 'w')
//	tailSecs — trailing `pause` length. Size it to: (tones × 500ms) +
//	           (w-separators × 500ms) + 1s slack for the last RFC 2833
//	           end-event to arrive before BYE. Too short → tail digit
//	           dropped; too long → wasted wall-clock.
//
// Steps (shared by all TestVerb_Dtmf_* variants):
//  1. place-call — POST /Calls with [answer, pause, dtmf <sent>, pause <tailSecs>]
//  2. answer-record-and-wait-end — record PCMU (DTMF arrives as RFC 2833 events)
//  3. assert-dtmf-received — events captured, received string contains wantSeq
func expectDTMF(t *testing.T, sent, wantSeq, tag string, tailSecs int) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	// Leading warmup gives our RTP-receive + DTMF-decoder pipeline time to
	// spin up before jambonz starts emitting digits (otherwise the first
	// digit can be clipped). Trailing pause per tailSecs (see doc).
	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, WithWarmup([]map[string]any{
		V("dtmf", "dtmf", sent),
		V("pause", "length", tailSecs),
	}), withTimeLimit(30))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-dtmf-received")
	events := call.ReceivedDTMF()
	got := ""
	for _, e := range events {
		got += e.Digit
	}
	s.Logf("%s: sent=%q want=%q received=%q events=%d rms=%.1f",
		tag, sent, wantSeq, got, len(events), call.RMS())
	if got == "" {
		s.Fatalf("%s: no DTMF digits received (expected %q)", tag, wantSeq)
	}
	// Strict equality (was Contains): RFC 2833 decode is deterministic,
	// so jambonz emitting EXTRA digits ("125 1234 999") would have
	// passed the old Contains check and is exactly the regression we
	// want to catch.
	if got != wantSeq {
		s.Errorf("%s: received %q != expected %q", tag, got, wantSeq)
	}
	// Length check: same number of events as digits — a regression that
	// doubles every event (re-emits the off-event as on, or splits a
	// tone across two events) would still satisfy the equality check
	// after de-duplication but would have len(events) == 2*len(wantSeq).
	if len(events) != len(wantSeq) {
		s.Errorf("%s: %d DTMF events received for %d-digit sequence %q",
			tag, len(events), len(wantSeq), wantSeq)
	}
	s.Done()
}
