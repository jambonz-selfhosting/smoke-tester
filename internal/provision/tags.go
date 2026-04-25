package provision

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
)

// NamePrefix is stamped on every resource smoke-tester creates. The orphan
// sweep in TestMain relies on this to find leaked resources from crashed
// prior runs. See ADR-0008.
const NamePrefix = "it-"

var (
	runIDOnce sync.Once
	runID     string
)

// RunID returns a short process-local identifier used in every resource name.
// Env var RUN_ID overrides for debugging.
func RunID() string {
	runIDOnce.Do(func() {
		if v := getenv("RUN_ID"); v != "" {
			runID = sanitise(v)
			return
		}
		b := make([]byte, 4)
		_, _ = rand.Read(b)
		runID = hex.EncodeToString(b) // 8 hex chars — short and unique enough
	})
	return runID
}

// Name returns "it-<runID>-<suffix>". Always use this when naming a resource
// we create — violating this breaks orphan sweeps.
func Name(suffix string) string {
	return fmt.Sprintf("%s%s-%s", NamePrefix, RunID(), suffix)
}

// uniqueUsername returns a per-test SIP username with the harness prefix +
// runID + a random suffix. Used by ManagedSIPClient so concurrent tests
// (and re-runs of the same test) never collide on the username uniqueness
// constraint server-side. Pattern: "it-<runID>-<role>-<hex8>".
func uniqueUsername(role string) string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return fmt.Sprintf("%s%s-%s-%s", NamePrefix, RunID(), role, hex.EncodeToString(b))
}

// randomPassword returns a 32-hex-char random string suitable as a SIP
// digest auth password. Long enough that brute-force is irrelevant in a
// short-lived test run.
func randomPassword() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// IsHarnessResource reports whether a resource name looks like one created by
// any smoke-tester run — used by the orphan sweeper.
func IsHarnessResource(name string) bool {
	return strings.HasPrefix(name, NamePrefix)
}

// sanitise keeps only [a-zA-Z0-9_-] from user-supplied RUN_ID, so bad input
// can't construct surprising resource names.
func sanitise(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		}
	}
	if b.Len() == 0 {
		return "run"
	}
	return b.String()
}

// getenv is split out so tests can override. Currently direct os-env.
func getenv(k string) string {
	return osGetenv(k)
}
