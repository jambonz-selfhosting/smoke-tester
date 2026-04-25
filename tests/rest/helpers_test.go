// Shared helpers for the rest package test suite.
package rest

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// stepTracker remembers the last Step(name) called per *testing.T so the
// watchdog (see WithTimeout) can report which step was running when the
// budget was exceeded.
//
// timedOut records whether the watchdog has already fired for this test
// (so Done() can suppress a misleading "ok" if the step happens to finish
// after the timeout has been logged).
var (
	stepTrackerMu sync.Mutex
	stepTracker   = map[*testing.T]string{}
	timedOut      = map[*testing.T]bool{}
	// failures captures every Errorf/Fatalf/Fatal/timeout from this run so
	// TestMain can print a one-line-per-failure summary after m.Run() —
	// otherwise, under -parallel and without -v, a failing step's message
	// is buried in interleaved log noise from concurrent tests.
	failures []failureRecord
)

type failureRecord struct {
	testName string
	step     string
	message  string
}

func recordFailure(t *testing.T, step, msg string) {
	stepTrackerMu.Lock()
	failures = append(failures, failureRecord{
		testName: t.Name(),
		step:     step,
		message:  msg,
	})
	stepTrackerMu.Unlock()
}

// PrintFailureSummary writes a one-line-per-failure block. Called from
// TestMain after m.Run() so operators see exactly which test, which step,
// and why. Bypasses go test's stdout/stderr capture by writing to
// /dev/tty so it's visible without -v. See ttyOut for the rationale.
func PrintFailureSummary() int {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	if len(failures) == 0 {
		return 0
	}
	fmt.Fprintf(ttyOut, "\n=== FAILURE SUMMARY (%d) ===\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(ttyOut, "  FAIL %s [step:%s] %s\n", f.testName, f.step, f.message)
	}
	fmt.Fprintln(ttyOut, "============================")
	return len(failures)
}

// ttyOut bypasses go test's output capture (which would otherwise hide our
// heartbeat + summary unless -v is set or a test fails). See the verbs
// package's ttyOut for the full rationale.
var ttyOut = func() io.Writer {
	if f, err := os.OpenFile("/dev/tty", os.O_WRONLY, 0); err == nil {
		return f
	}
	return os.Stderr
}()

// --- heartbeat ------------------------------------------------------------
// Mirror of the verbs package's heartbeat. See verbs/helpers_test.go for
// the full design rationale.

var (
	heartbeatActive    = map[string]time.Time{}
	heartbeatCompleted int
	heartbeatPassed    int
	heartbeatFailed    int
)

func heartbeatTestStarted(t *testing.T) {
	stepTrackerMu.Lock()
	heartbeatActive[t.Name()] = time.Now()
	stepTrackerMu.Unlock()
}

func heartbeatTestFinished(t *testing.T) {
	stepTrackerMu.Lock()
	delete(heartbeatActive, t.Name())
	heartbeatCompleted++
	if t.Failed() {
		heartbeatFailed++
	} else {
		heartbeatPassed++
	}
	stepTrackerMu.Unlock()
}

// StartHeartbeat begins printing a status line to /dev/tty every interval
// until the returned stop function is called. Call BEFORE m.Run() and
// stop AFTER.
func StartHeartbeat(interval time.Duration) (stop func()) {
	start := time.Now()
	done := make(chan struct{})
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				printHeartbeat(time.Since(start))
			}
		}
	}()
	return func() { close(done) }
}

func printHeartbeat(elapsed time.Duration) {
	stepTrackerMu.Lock()
	running := len(heartbeatActive)
	completed := heartbeatCompleted
	passed := heartbeatPassed
	failed := heartbeatFailed
	type active struct {
		name string
		age  time.Duration
	}
	actives := make([]active, 0, running)
	now := time.Now()
	for name, started := range heartbeatActive {
		actives = append(actives, active{name: name, age: now.Sub(started)})
	}
	// Snapshot step names while we hold the lock.
	steps := make(map[string]string, len(actives))
	for _, a := range actives {
		for tt, step := range stepTracker {
			if tt.Name() == a.name {
				steps[a.name] = step
				break
			}
		}
	}
	stepTrackerMu.Unlock()

	for i := 0; i < len(actives); i++ {
		for j := i + 1; j < len(actives); j++ {
			if actives[j].age > actives[i].age {
				actives[i], actives[j] = actives[j], actives[i]
			}
		}
	}

	var nowSuffix string
	if len(actives) > 0 {
		var parts []string
		for _, a := range actives {
			step := steps[a.name]
			if step == "" {
				step = "(no step)"
			}
			parts = append(parts, fmt.Sprintf("%s[step:%s]@%s",
				a.name, step, a.age.Round(time.Second)))
		}
		nowSuffix = " | now: " + strings.Join(parts, ", ")
	}

	fmt.Fprintf(ttyOut, "[heartbeat %s] running=%d done=%d (%d pass, %d fail)%s\n",
		elapsed.Round(time.Second), running, completed, passed, failed, nowSuffix)
}

func recordCurrentStep(t *testing.T, name string) {
	stepTrackerMu.Lock()
	stepTracker[t] = name
	stepTrackerMu.Unlock()
}

func clearCurrentStep(t *testing.T) {
	stepTrackerMu.Lock()
	delete(stepTracker, t)
	stepTrackerMu.Unlock()
}

func lookupCurrentStep(t *testing.T) string {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	if n, ok := stepTracker[t]; ok {
		return n
	}
	return "(none — test hadn't entered any step)"
}

func markTimedOut(t *testing.T) {
	stepTrackerMu.Lock()
	timedOut[t] = true
	stepTrackerMu.Unlock()
}

func isTimedOut(t *testing.T) bool {
	stepTrackerMu.Lock()
	defer stepTrackerMu.Unlock()
	return timedOut[t]
}

func forgetTest(t *testing.T) {
	stepTrackerMu.Lock()
	delete(stepTracker, t)
	delete(timedOut, t)
	stepTrackerMu.Unlock()
}

// StepCtx is a scoped failure-reporting wrapper returned by Step. Call its
// Fatalf / Errorf methods instead of t.Fatalf / t.Errorf so the resulting
// failure line carries the step name. Call Done() at the end of the step
// to emit "[step:<name>] ok (Xms)".
//
//	s := rest.Step(t, "create-application")
//	sid := client.ManagedApplication(t, ctx, body)
//	if sid == "" {
//	    s.Fatal("create returned empty SID")
//	}
//	s.Done()
type StepCtx struct {
	t     *testing.T
	name  string
	start time.Time
	ended bool
}

// Step starts a named step: logs "[step:<name>] start" and returns a
// StepCtx whose Done() closes the step with "[step:<name>] ok (Xms)".
//
// Any failure reported via s.Fatalf / s.Errorf is logged as
// "[step:<name>] FAILED: <message>" before the test fails — so an operator
// reading the failure line immediately sees which step broke and why, with
// no need to scan for the last "start" without an "ok".
//
// The step name must match the corresponding bullet in the test's top-of-
// file "Steps:" comment so the operator doesn't need to open the test file
// to understand the flow.
//
// Naming rules (keep this tight so the logs stay greppable):
//   - kebab-case, lowercase ASCII
//   - verb-first, short (e.g. "list-and-find", not "list-then-iterate")
//   - include discriminating values inline when useful ("get-by-sid")
func Step(t *testing.T, name string) *StepCtx {
	t.Helper()
	recordCurrentStep(t, name)
	t.Logf("[step:%s] start", name)
	return &StepCtx{t: t, name: name, start: time.Now()}
}

// Done closes the step and logs "[step:<name>] ok (Xms)". Safe to call
// after a failure (it will silently no-op if the step is already ended by
// a Fatalf). Call inline at the end of the step, NOT via defer — defer
// would reorder every "ok" to the end of the test and wrong-duration each
// one.
//
// If the test's watchdog already fired (see WithTimeout) we suppress the
// "ok" — the test has already been marked FAILED and a trailing "ok"
// would be misleading.
func (s *StepCtx) Done() {
	if s.ended {
		return
	}
	s.ended = true
	clearCurrentStep(s.t)
	if isTimedOut(s.t) {
		return
	}
	s.t.Helper()
	s.t.Logf("[step:%s] ok (%s)", s.name, time.Since(s.start).Round(time.Millisecond))
}

// Fatalf logs "[step:<name>] FAILED: <msg>" and calls t.Fatalf. Use instead
// of t.Fatalf so the failure line names the step.
func (s *StepCtx) Fatalf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Fatalf("[step:%s] FAILED: %s", s.name, msg)
}

// Errorf logs "[step:<name>] FAILED: <msg>" and calls t.Errorf. Use instead
// of t.Errorf so the failure line names the step. Test continues; call
// Done() at the end of the step as usual — but since this step has already
// been marked failed, Done() will no-op (no misleading "ok" after a FAILED).
func (s *StepCtx) Errorf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Errorf("[step:%s] FAILED: %s", s.name, msg)
}

// Fatal is Fatalf with a plain message.
func (s *StepCtx) Fatal(args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprint(args...)
	recordFailure(s.t, s.name, msg)
	s.t.Fatalf("[step:%s] FAILED: %s", s.name, msg)
}

// Logf passes through to t.Logf without step decoration. Use for
// information-only mid-step logs (payload dumps, recording paths, etc.).
func (s *StepCtx) Logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf(format, args...)
}

// WithTimeout is the single source of truth for per-test budget. It
// returns a context whose deadline is `budget` from now and arms a
// watchdog that hard-fails the test if the budget + a small safety margin
// is exceeded. Cleanup runs on test exit (pass or fail), cancelling the
// context and stopping the watchdog.
//
//	func TestApplication_CRUD(t *testing.T) {
//	    ctx := WithTimeout(t, 30*time.Second)
//	    ...
//	}
//
// (Name is WithTimeout, not TestTimeout, because go-test treats any
// top-level func whose name starts with "Test" as a test function and
// would reject a signature that doesn't match func(*testing.T).)
//
// When the watchdog fires (i.e. a test blocked past its budget):
//   - the failing line reads
//     "[test-timeout] FAILED: exceeded 30s (last step: list-and-find)"
//   - the test is marked FAIL via t.Errorf
//   - subsequent Done() calls no-op so no misleading "ok" appears after
//     the FAILED line.
//
// The watchdog's safety margin (2s) covers the common case where a
// context-aware call returns just after the deadline; we only consider
// the test actually-stuck when it hasn't finished by budget+safety.
//
// NOTE: the watchdog cannot force-kill a stuck test goroutine (Go has no
// such primitive). If a test is wedged in a non-context-aware syscall,
// go-test's own -timeout will eventually kill the binary. The watchdog at
// least guarantees the failure reason appears in the log at
// budget+safetyMargin rather than at the 10-minute alarm.
func WithTimeout(t *testing.T, budget time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)

	heartbeatTestStarted(t)

	const safetyMargin = 2 * time.Second
	watchdog := time.AfterFunc(budget+safetyMargin, func() {
		markTimedOut(t)
		lastStep := lookupCurrentStep(t)
		msg := fmt.Sprintf("exceeded %s (last step: %s)", budget, lastStep)
		recordFailure(t, lastStep, "[test-timeout] "+msg)
		t.Errorf("[test-timeout] FAILED: %s", msg)
	})

	t.Cleanup(func() {
		watchdog.Stop()
		cancel()
		heartbeatTestFinished(t)
		forgetTest(t)
	})
	return ctx
}
