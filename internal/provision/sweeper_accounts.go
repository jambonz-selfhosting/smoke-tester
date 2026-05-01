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
type AccountSweeper struct {
	C *Client
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

	var swept int
	for _, a := range accts {
		// Hard guard: even if a server-side mistake or response shape
		// drift returned a non-`it-` account, we will not touch it.
		if !strings.HasPrefix(a.Name, NamePrefix) {
			continue
		}
		if strings.HasPrefix(a.Name, currentPrefix) {
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
