package sip

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// drainable is the subset of *Call that drainCalls needs. It exists so the
// drain logic is testable without a live SIP stack.
type drainable interface {
	CallID() string
	Hangup() error
	Done() <-chan struct{}
}

// DrainResult reports what happened to one call during a drain.
type DrainResult struct {
	CallID string
	// HangupErr is the error returned by Hangup, if any.
	HangupErr error
	// Ended is true if the call reached its terminal state (Done closed)
	// within the drain budget.
	Ended bool
}

// drainCalls hangs up every call concurrently and waits for each to reach its
// terminal state. Each call gets at most perCall to finish; the whole drain
// gets at most total. Returns one DrainResult per input call, in input order.
// A nil/empty slice returns an empty (non-nil) slice immediately.
// drainCalls never panics and never blocks longer than `total` — the Hangup
// call itself is bounded by `total` too, not just the wait for Done(), so a
// wedged Hangup() can't stall the drain past the deadline.
func drainCalls(calls []drainable, perCall, total time.Duration) []DrainResult {
	results := make([]DrainResult, len(calls))
	if len(calls) == 0 {
		return results
	}

	// perCall/total <= 0 means "no waiting": Hangup still fires on every
	// call, but we never block on Done() — only already-closed channels
	// count as Ended.
	noWait := perCall <= 0 || total <= 0

	ctx, cancel := context.WithTimeout(context.Background(), total)
	defer cancel()

	var wg sync.WaitGroup
	for i, call := range calls {
		wg.Add(1)
		go func(i int, call drainable) {
			defer wg.Done()
			// Each goroutine writes only its own slot; disjoint slice
			// elements are safe to write concurrently without a race.
			results[i] = drainOne(ctx, call, perCall, noWait)
		}(i, call)
	}
	wg.Wait()
	return results
}

// drainOne hangs up a single call and waits for it to end, bounded by
// whichever comes first: perCall or the shared overall deadline (ctx). The
// Hangup call itself is also bounded by ctx: it runs in its own goroutine so
// a wedged Hangup() (Call.Hangup has its own internal timeout, which can
// exceed a short `total`) can never make drainOne block past the overall
// deadline.
func drainOne(ctx context.Context, call drainable, perCall time.Duration, noWait bool) DrainResult {
	res := DrainResult{CallID: call.CallID()}
	done := call.Done()

	// Already ended: report promptly, and call Hangup synchronously — this
	// is not the wedged-BYE scenario the goroutine-bounding below exists
	// for (the call is already in its terminal state), so there's nothing
	// to gain from the extra bookkeeping, and doing it synchronously keeps
	// "Hangup invoked before we return" a hard guarantee rather than a race
	// against a freshly-spawned goroutine.
	select {
	case <-done:
		res.HangupErr = safeHangup(call)
		res.Ended = true
		return res
	default:
	}

	if noWait {
		// noWait's contract is "Hangup still fires, but we never wait" — so
		// Hangup is expected to return immediately here; call it
		// synchronously so its (immediate) error is never lost.
		res.HangupErr = safeHangup(call)
		select {
		case <-done:
			res.Ended = true
		default:
		}
		return res
	}

	// Run Hangup in its own goroutine so a wedged Hangup() (Call.Hangup has
	// its own internal timeout, which can exceed a short `total`) can never
	// make drainOne block past the overall deadline. hangupCh is buffered
	// so the goroutine's send never blocks, even if drainOne has already
	// returned via the ctx/timer paths below.
	hangupCh := make(chan error, 1)
	go func() {
		hangupCh <- safeHangup(call)
	}()

	timer := time.NewTimer(perCall)
	defer timer.Stop()

	var hangupSeen bool
	for {
		select {
		case err := <-hangupCh:
			res.HangupErr = err
			hangupSeen = true
		case <-done:
			res.Ended = true
			// Closed channels stay perpetually ready; nil the local
			// reference so this case can't fire again and spin the loop
			// while we wait out the remaining time for the hangup result.
			done = nil
		case <-timer.C:
			return res
		case <-ctx.Done():
			return res
		}
		if hangupSeen && res.Ended {
			return res
		}
	}
}

// safeHangup calls Hangup and converts any panic into an error so one wedged
// call can't crash the whole drain.
func safeHangup(call drainable) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("hangup panicked: %v", r)
		}
	}()
	return call.Hangup()
}
