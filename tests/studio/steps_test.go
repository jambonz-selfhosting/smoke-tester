//go:build studio

package studiotests

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// Step / WithTimeout / failure summary, as in tests/rest (CLAUDE.md failure-fast pattern).

var (
	stepMu   sync.Mutex
	stepNow  = map[*testing.T]string{}
	timedOut = map[*testing.T]bool{}
	failures []failureRecord
)

type failureRecord struct{ test, step, msg string }

func recordFailure(t *testing.T, step, msg string) {
	stepMu.Lock()
	defer stepMu.Unlock()
	failures = append(failures, failureRecord{t.Name(), step, msg})
}

// PrintFailureSummary prints one line per failure; TestMain calls it after m.Run.
func PrintFailureSummary() {
	stepMu.Lock()
	defer stepMu.Unlock()
	if len(failures) == 0 {
		return
	}
	fmt.Fprintf(os.Stderr, "\n=== FAILURE SUMMARY (%d) ===\n", len(failures))
	for _, f := range failures {
		fmt.Fprintf(os.Stderr, "%s [step:%s] %s\n", f.test, f.step, f.msg)
	}
}

type StepCtx struct {
	t     *testing.T
	name  string
	start time.Time
	ended bool
}

func Step(t *testing.T, name string) *StepCtx {
	t.Helper()
	stepMu.Lock()
	stepNow[t] = name
	stepMu.Unlock()
	t.Logf("[step:%s] start", name)
	return &StepCtx{t: t, name: name, start: time.Now()}
}

func (s *StepCtx) Done() {
	if s.ended {
		return
	}
	s.ended = true
	stepMu.Lock()
	out := timedOut[s.t]
	delete(stepNow, s.t)
	stepMu.Unlock()
	if !out {
		s.t.Logf("[step:%s] ok (%s)", s.name, time.Since(s.start).Round(time.Millisecond))
	}
}

func (s *StepCtx) Errorf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Errorf("[step:%s] FAILED: %s", s.name, msg)
}

func (s *StepCtx) Fatalf(format string, args ...any) {
	s.t.Helper()
	s.ended = true
	msg := fmt.Sprintf(format, args...)
	recordFailure(s.t, s.name, msg)
	s.t.Fatalf("[step:%s] FAILED: %s", s.name, msg)
}

func (s *StepCtx) Logf(format string, args ...any) {
	s.t.Helper()
	s.t.Logf(format, args...)
}

// WithTimeout returns the test's context and fails the test, naming the last step, if it overruns the budget.
func WithTimeout(t *testing.T, budget time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	watchdog := time.AfterFunc(budget+2*time.Second, func() {
		stepMu.Lock()
		timedOut[t] = true
		last := stepNow[t]
		stepMu.Unlock()
		msg := fmt.Sprintf("exceeded %s (last step: %s)", budget, last)
		recordFailure(t, last, "[test-timeout] "+msg)
		t.Errorf("[test-timeout] FAILED: %s", msg)
	})
	t.Cleanup(func() {
		watchdog.Stop()
		cancel()
		stepMu.Lock()
		delete(stepNow, t)
		delete(timedOut, t)
		stepMu.Unlock()
	})
	return ctx
}
