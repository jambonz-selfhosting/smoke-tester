package provision

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// AccountSweeper deletes orphaned ephemeral test accounts left behind by
// previous (crashed) runs. Only accounts whose `name` field starts with
// `NamePrefix` ("it-") are considered. The current run's accounts —
// `it-<protectRunID>-*` — are protected.
//
// Safety properties (audited 2026-05-01 after a destructive incident):
//
//   1. Never deletes an account whose `name` does not start with `it-`.
//      We DO NOT trust upstream filters; both the prefix check and the
//      protectRunID exclusion are evaluated client-side.
//   2. Deletes the account's clients first, because the upstream
//      `DELETE /Accounts/<sid>` handler doesn't cascade `clients` and
//      otherwise fails with a foreign-key constraint error. Client
//      enumeration uses ListSIPClientsForAccount, which filters
//      client-side (the upstream `GET /Clients?account_sid=X` endpoint
//      ignores its query parameter).
//   3. Per-client double-check: only deletes a client whose AccountSID
//      matches the account we are about to delete. Belt-and-braces.
//   4. The sweeper only runs with an SP-scoped client; it has no
//      reach outside the SP (no admin scope), so worst-case scope is
//      exactly "accounts under our SP".
//   5. Age gate: only accounts older than MinAge (ORPHAN_TTL_HOURS) are
//      swept. Concurrent test packages in one `make test` run get
//      distinct runIDs when RUN_ID isn't shared, and without the age
//      gate whichever package started second would delete the other's
//      live suite account (cascading its api_keys → mid-run 401s).
//      An account whose created_at is missing or unparseable is never
//      swept — we can't establish its age, so we leave it alone.
type AccountSweeper struct {
	C *Client
	// MinAge is the minimum account age before it is considered an
	// orphan. Thread cfg.OrphanTTL here. Zero means no age gate.
	MinAge time.Duration
}

func (s *AccountSweeper) Name() string { return "accounts" }

func (s *AccountSweeper) Sweep(protectRunID string) (int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	accts, err := s.C.ListAccounts(ctx)
	if err != nil {
		return 0, fmt.Errorf("list: %w", err)
	}
	currentPrefix := fmt.Sprintf("%s%s-", NamePrefix, protectRunID)

	// Pre-list every client once; per-account filter happens client-side
	// (see comment on Safety property 2).
	allClients, err := s.C.ListSIPClients(ctx)
	if err != nil {
		// Sweep is best-effort — continue without clients; account delete
		// may then fail for accounts that have orphan clients but the
		// remaining empty-account deletes will still succeed.
		allClients = nil
	}

	now := time.Now()
	var swept int
	for _, a := range accts {
		if !shouldSweep(a, currentPrefix, s.MinAge, now) {
			continue
		}
		// Enumerate clients of THIS account (filter by AccountSID, never
		// trust upstream query). Delete each one, then the account.
		for _, cl := range allClients {
			if cl.AccountSID != a.AccountSID {
				continue
			}
			_ = s.C.DeleteSIPClient(ctx, cl.ClientSID)
		}
		if err := s.C.DeleteAccount(ctx, a.AccountSID); err != nil {
			// Likely a remaining FK we missed; leave for next run / manual
			// cleanup rather than retry-loop here.
			continue
		}
		swept++
	}
	return swept, nil
}

// shouldSweep decides whether one account is a sweepable orphan. Split out
// so the filter is unit-testable without an HTTP client.
//
//   - Hard guard: never touch an account whose name doesn't start with
//     `it-`, even if a server-side mistake or response shape drift returned
//     a non-`it-` account.
//   - Never touch the current run's accounts (currentPrefix).
//   - Age gate: with minAge > 0, only sweep accounts whose created_at is
//     parseable AND older than minAge. Unparseable/missing created_at means
//     we can't establish age → don't sweep.
func shouldSweep(a Account, currentPrefix string, minAge time.Duration, now time.Time) bool {
	if !strings.HasPrefix(a.Name, NamePrefix) {
		return false
	}
	if strings.HasPrefix(a.Name, currentPrefix) {
		return false
	}
	if minAge > 0 {
		created, err := parseAPITime(a.CreatedAt)
		if err != nil {
			return false
		}
		if now.Sub(created) < minAge {
			return false
		}
	}
	return true
}

// parseAPITime parses the API's created_at. mysql2 returns DATETIME columns
// as JS Dates, which JSON-serialize to ISO 8601 ("2026-08-11T14:00:00.000Z");
// the plain MySQL layout is accepted as a fallback for deployments running
// with dateStrings enabled.
func parseAPITime(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02 15:04:05", s)
}
