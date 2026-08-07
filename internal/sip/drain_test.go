// Internal-package tests for drainCalls, the helper that hangs up a batch of
// live calls concurrently and waits (with bounded budgets) for each to reach
// its terminal state. This file lives in package sip (not sip_test) because
// drainCalls, DrainResult, and drainable are unexported.
//
// drainFake below is a hand-rolled drainable used only by these tests; it is
// never used to fake production behaviour, only to pin drainCalls' contract:
// concurrency, per-call/total budgets, error/panic surfacing, and result
// ordering.
package sip

import (
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainFake is a configurable drainable for testing drainCalls.
type drainFake struct {
	id string

	done chan struct{}

	hangupDelay time.Duration
	hangupErr   error
	doPanic     bool

	// autoCloseOnHangup, if true, closes done from within Hangup after
	// hangupDelay elapses (simulating a call that ends as a result of the
	// hangup completing).
	autoCloseOnHangup bool

	hangupCount int32

	closeOnce sync.Once
}

func newDrainFake(id string) *drainFake {
	return &drainFake{id: id, done: make(chan struct{})}
}

func (f *drainFake) CallID() string       { return f.id }
func (f *drainFake) Done() <-chan struct{} { return f.done }

func (f *drainFake) Hangup() error {
	atomic.AddInt32(&f.hangupCount, 1)
	if f.hangupDelay > 0 {
		time.Sleep(f.hangupDelay)
	}
	if f.autoCloseOnHangup {
		f.closeDone()
	}
	if f.doPanic {
		panic("drainFake: simulated Hangup panic")
	}
	return f.hangupErr
}

func (f *drainFake) closeDone() {
	f.closeOnce.Do(func() { close(f.done) })
}

func (f *drainFake) hangupCalls() int32 {
	return atomic.LoadInt32(&f.hangupCount)
}

// timing budgets, kept small (tens of ms) so the whole file runs well under
// 3s, but with generous multipliers in assertions so a loaded CI box doesn't
// flake.
const (
	tinyDelay  = 20 * time.Millisecond
	smallDelay = 60 * time.Millisecond
	medDelay   = 120 * time.Millisecond
)

// --- 1. nil / empty input --------------------------------------------------

func TestDrainCalls_NilAndEmptyInput(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		calls []drainable
	}{
		{"nil slice", nil},
		{"empty slice", []drainable{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			got := drainCalls(tc.calls, 5*time.Second, 5*time.Second)
			elapsed := time.Since(start)

			if got == nil {
				t.Fatalf("drainCalls(%s) returned nil, want non-nil empty slice", tc.name)
			}
			if len(got) != 0 {
				t.Fatalf("drainCalls(%s) returned %d results, want 0", tc.name, len(got))
			}
			if elapsed > 200*time.Millisecond {
				t.Errorf("drainCalls(%s) took %v, want effectively immediate", tc.name, elapsed)
			}
		})
	}
}

// --- 2. Hangup invoked exactly once per call; results positional, not keyed
//        by CallID (covers duplicate CallIDs) -----------------------------

func TestDrainCalls_HangupInvokedOnce_ResultsPositional(t *testing.T) {
	t.Parallel()

	// Duplicate ids ("dup") to prove results are positional, not id-keyed.
	fakes := []*drainFake{
		newDrainFake("dup"),
		newDrainFake("b"),
		newDrainFake("dup"),
		newDrainFake("c"),
	}
	for _, f := range fakes {
		f.autoCloseOnHangup = true // ends immediately
	}

	calls := make([]drainable, len(fakes))
	for i, f := range fakes {
		calls[i] = f
	}

	results := drainCalls(calls, 500*time.Millisecond, 2*time.Second)

	if len(results) != len(fakes) {
		t.Fatalf("got %d results, want %d", len(results), len(fakes))
	}
	for i, f := range fakes {
		if results[i].CallID != f.CallID() {
			t.Errorf("results[%d].CallID = %q, want %q (input order)", i, results[i].CallID, f.CallID())
		}
		if !results[i].Ended {
			t.Errorf("results[%d].Ended = false, want true", i)
		}
		if got := f.hangupCalls(); got != 1 {
			t.Errorf("fake[%d] (id=%q) Hangup called %d times, want exactly 1", i, f.id, got)
		}
	}
}

// --- 3. Hangup() error surfaced in DrainResult.HangupErr -------------------

func TestDrainCalls_HangupErrorSurfaced(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("boom")

	okFake := newDrainFake("ok")
	okFake.autoCloseOnHangup = true

	errFake := newDrainFake("err")
	errFake.autoCloseOnHangup = true
	errFake.hangupErr = errBoom

	calls := []drainable{okFake, errFake}
	results := drainCalls(calls, 500*time.Millisecond, 2*time.Second)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].HangupErr != nil {
		t.Errorf("results[0].HangupErr = %v, want nil", results[0].HangupErr)
	}
	if !errors.Is(results[1].HangupErr, errBoom) {
		t.Errorf("results[1].HangupErr = %v, want %v", results[1].HangupErr, errBoom)
	}
}

// --- 4/6. Done() closes within perCall (already-closed on entry must be
//          prompt, not wait out perCall) -----------------------------------

func TestDrainCalls_AlreadyClosedDoneIsPrompt(t *testing.T) {
	t.Parallel()

	f := newDrainFake("already-done")
	f.closeDone() // closed before drainCalls even sees it

	perCall := 300 * time.Millisecond
	start := time.Now()
	results := drainCalls([]drainable{f}, perCall, 2*time.Second)
	elapsed := time.Since(start)

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].Ended {
		t.Errorf("Ended = false, want true for already-closed Done()")
	}
	if elapsed >= perCall/2 {
		t.Errorf("took %v to report an already-ended call, want well under perCall (%v)", elapsed, perCall)
	}
	if got := f.hangupCalls(); got != 1 {
		t.Errorf("Hangup called %d times, want 1", got)
	}
}

// --- 5. Done() never closes -> Ended=false after ~perCall, does not block
//        other calls in the batch -----------------------------------------

func TestDrainCalls_NeverEndingDoesNotBlockOthers(t *testing.T) {
	t.Parallel()

	fast := newDrainFake("fast")
	fast.autoCloseOnHangup = true // ends immediately

	wedged := newDrainFake("wedged")
	// Hangup returns immediately but Done() is never closed.

	perCall := 100 * time.Millisecond
	start := time.Now()
	results := drainCalls([]drainable{fast, wedged}, perCall, 2*time.Second)
	elapsed := time.Since(start)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if !results[0].Ended {
		t.Errorf("fast call: Ended = false, want true")
	}
	if results[1].Ended {
		t.Errorf("wedged call: Ended = true, want false (Done() never closed)")
	}
	// The wedged call must not force the whole drain to wait for `total`;
	// it should resolve at ~perCall.
	if elapsed > 3*perCall {
		t.Errorf("drain took %v, want roughly bounded by perCall (%v), not total", elapsed, perCall)
	}
}

// --- 8. Whole drain returns within ~total even when calls are wedged with
//        perCall > total ---------------------------------------------------

func TestDrainCalls_TotalBudgetCapsWedgedDrain(t *testing.T) {
	t.Parallel()

	wedged := []drainable{
		newDrainFake("w1"),
		newDrainFake("w2"),
		newDrainFake("w3"),
	}
	// None of these ever close Done(); Hangup returns immediately.

	perCall := 2 * time.Second // larger than total
	total := 100 * time.Millisecond

	start := time.Now()
	results := drainCalls(wedged, perCall, total)
	elapsed := time.Since(start)

	if len(results) != 3 {
		t.Fatalf("got %d results, want 3", len(results))
	}
	for i, r := range results {
		if r.Ended {
			t.Errorf("results[%d].Ended = true, want false (wedged call)", i)
		}
	}
	if elapsed > 3*total {
		t.Errorf("drain took %v, want capped by total (%v) despite perCall (%v)", elapsed, total, perCall)
	}
}

// --- 2 (again, load-bearing). Hangups happen concurrently, not serially ---

func TestDrainCalls_ConcurrentNotSerial(t *testing.T) {
	t.Parallel()

	const n = 5
	fakes := make([]*drainFake, n)
	calls := make([]drainable, n)
	for i := 0; i < n; i++ {
		f := newDrainFake(fmt.Sprintf("c%d", i))
		f.hangupDelay = medDelay
		f.autoCloseOnHangup = true
		fakes[i] = f
		calls[i] = f
	}

	perCall := 5 * medDelay
	total := 5 * time.Second

	start := time.Now()
	results := drainCalls(calls, perCall, total)
	elapsed := time.Since(start)

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for i, r := range results {
		if !r.Ended {
			t.Errorf("results[%d].Ended = false, want true", i)
		}
	}
	// Serial would take ~n*medDelay; concurrent should take ~medDelay.
	// Generous 3x bound to avoid flaking on a loaded box while still
	// clearly distinguishing concurrent from serial (n=5).
	if elapsed > 3*medDelay {
		t.Errorf("drain of %d calls (each Hangup blocking %v) took %v, want ~%v (concurrent), not ~%v (serial)",
			n, medDelay, elapsed, medDelay, n*medDelay)
	}
}

// --- 9. perCall<=0 or total<=0 means "no waiting" --------------------------

func TestDrainCalls_ZeroOrNegativeBudgets_NoWaiting(t *testing.T) {
	t.Parallel()

	budgetCases := []struct {
		name    string
		perCall time.Duration
		total   time.Duration
	}{
		{"zero perCall, large total", 0, 5 * time.Second},
		{"negative perCall and total", -1 * time.Millisecond, -1 * time.Millisecond},
	}

	for _, bc := range budgetCases {
		t.Run(bc.name, func(t *testing.T) {
			preClosed := newDrainFake("pre-closed")
			preClosed.closeDone()

			neverEnds := newDrainFake("never-ends")
			// Hangup returns immediately; Done never closes.

			start := time.Now()
			results := drainCalls([]drainable{preClosed, neverEnds}, bc.perCall, bc.total)
			elapsed := time.Since(start)

			if len(results) != 2 {
				t.Fatalf("got %d results, want 2", len(results))
			}
			if !results[0].Ended {
				t.Errorf("pre-closed call: Ended = false, want true")
			}
			if results[1].Ended {
				t.Errorf("never-ends call: Ended = true, want false (no waiting means it can't have ended)")
			}
			for _, f := range []*drainFake{preClosed, neverEnds} {
				if got := f.hangupCalls(); got != 1 {
					t.Errorf("fake %q: Hangup called %d times, want 1 (must still be invoked)", f.id, got)
				}
			}
			if elapsed > 200*time.Millisecond {
				t.Errorf("drain with budgets %v/%v took %v, want effectively immediate", bc.perCall, bc.total, elapsed)
			}
		})
	}
}

// --- 11. A panicking Hangup() is recovered, not process-fatal --------------

func TestDrainCalls_PanicInHangupRecovered(t *testing.T) {
	t.Parallel()

	panicker := newDrainFake("panicker")
	panicker.doPanic = true

	survivor := newDrainFake("survivor")
	survivor.autoCloseOnHangup = true

	calls := []drainable{panicker, survivor}
	results := drainCalls(calls, 300*time.Millisecond, 2*time.Second)

	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
	if results[0].HangupErr == nil {
		t.Errorf("panicking call: HangupErr = nil, want non-nil (recovered panic)")
	}
	if !results[1].Ended {
		t.Errorf("survivor call: Ended = false, want true; a sibling panic must not affect it")
	}
	if got := survivor.hangupCalls(); got != 1 {
		t.Errorf("survivor Hangup called %d times, want 1", got)
	}
}

// --- Mixed batch: every documented outcome in one drain, asserted
//     independently per slot ------------------------------------------------

func TestDrainCalls_MixedBatch(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("hangup failed")

	immediate := newDrainFake("immediate")
	immediate.autoCloseOnHangup = true

	delayed := newDrainFake("delayed")
	delayed.hangupDelay = smallDelay
	delayed.autoCloseOnHangup = true

	wedged := newDrainFake("wedged")
	// never closes Done()

	erroring := newDrainFake("erroring")
	erroring.autoCloseOnHangup = true
	erroring.hangupErr = errBoom

	panicking := newDrainFake("panicking")
	panicking.doPanic = true

	fakes := []*drainFake{immediate, delayed, wedged, erroring, panicking}
	calls := make([]drainable, len(fakes))
	for i, f := range fakes {
		calls[i] = f
	}

	perCall := 3 * smallDelay
	total := 3 * time.Second

	start := time.Now()
	results := drainCalls(calls, perCall, total)
	elapsed := time.Since(start)

	if len(results) != len(fakes) {
		t.Fatalf("got %d results, want %d", len(results), len(fakes))
	}

	for i, f := range fakes {
		if results[i].CallID != f.CallID() {
			t.Errorf("results[%d].CallID = %q, want %q", i, results[i].CallID, f.CallID())
		}
		if got := f.hangupCalls(); got != 1 {
			t.Errorf("fake %q: Hangup called %d times, want 1", f.id, got)
		}
	}

	if !results[0].Ended || results[0].HangupErr != nil {
		t.Errorf("immediate: got Ended=%v HangupErr=%v, want Ended=true HangupErr=nil", results[0].Ended, results[0].HangupErr)
	}
	if !results[1].Ended || results[1].HangupErr != nil {
		t.Errorf("delayed: got Ended=%v HangupErr=%v, want Ended=true HangupErr=nil", results[1].Ended, results[1].HangupErr)
	}
	if results[2].Ended {
		t.Errorf("wedged: got Ended=true, want false")
	}
	if !results[3].Ended || !errors.Is(results[3].HangupErr, errBoom) {
		t.Errorf("erroring: got Ended=%v HangupErr=%v, want Ended=true HangupErr=%v", results[3].Ended, results[3].HangupErr, errBoom)
	}
	if results[4].HangupErr == nil {
		t.Errorf("panicking: HangupErr = nil, want non-nil (recovered panic)")
	}

	// The wedged call resolves at ~perCall; the whole batch must not be
	// dragged out anywhere near `total`.
	if elapsed > 3*perCall {
		t.Errorf("mixed-batch drain took %v, want bounded by perCall (%v), not total (%v)", elapsed, perCall, total)
	}
}

// --- Edge case: Done() closes while Hangup() is still blocked -------------

func TestDrainCalls_DoneClosesWhileHangupBlocked(t *testing.T) {
	t.Parallel()

	errBoom := errors.New("hangup error while done raced closed")

	f := newDrainFake("racy")
	f.hangupDelay = smallDelay
	f.hangupErr = errBoom
	// Close Done() independently, part-way through the Hangup() block,
	// simulating the dialog ending for a reason unrelated to our own
	// Hangup() call still being in flight.
	time.AfterFunc(tinyDelay, f.closeDone)

	perCall := 5 * smallDelay
	results := drainCalls([]drainable{f}, perCall, 2*time.Second)

	if len(results) != 1 {
		t.Fatalf("got %d results, want 1", len(results))
	}
	if !results[0].Ended {
		t.Errorf("Ended = false, want true (Done() closed within perCall)")
	}
	if !errors.Is(results[0].HangupErr, errBoom) {
		t.Errorf("HangupErr = %v, want %v", results[0].HangupErr, errBoom)
	}
}

// --- Large batch: goroutine-explosion / ordering bug detector -------------

func TestDrainCalls_LargeBatch(t *testing.T) {
	t.Parallel()

	const n = 200
	fakes := make([]*drainFake, n)
	calls := make([]drainable, n)
	for i := 0; i < n; i++ {
		f := newDrainFake(fmt.Sprintf("call-%d", i))
		f.autoCloseOnHangup = true
		fakes[i] = f
		calls[i] = f
	}

	perCall := 500 * time.Millisecond
	total := 3 * time.Second

	start := time.Now()
	results := drainCalls(calls, perCall, total)
	elapsed := time.Since(start)

	if len(results) != n {
		t.Fatalf("got %d results, want %d", len(results), n)
	}
	for i, f := range fakes {
		if results[i].CallID != f.CallID() {
			t.Errorf("results[%d].CallID = %q, want %q", i, results[i].CallID, f.CallID())
		}
		if !results[i].Ended {
			t.Errorf("results[%d].Ended = false, want true", i)
		}
		if got := f.hangupCalls(); got != 1 {
			t.Errorf("fake[%d] Hangup called %d times, want 1", i, got)
		}
	}
	if elapsed > 1*time.Second {
		t.Errorf("draining %d immediately-ending calls took %v, want well under a second", n, elapsed)
	}
}
