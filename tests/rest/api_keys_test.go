package rest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestApiKey_Create_AccountScope creates an account-scoped API key + deletes
// it. Covers Tier 1 row 1.8.
//
// Steps:
//  1. create-api-key — POST /ApiKeys via managed helper (auto cleanup)
//  2. assert-sid-and-token-non-empty — both returned, token is non-blank
func TestApiKey_Create_AccountScope(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-api-key")
	sid, token := client.ManagedApiKey(t, ctx, provision.ApiKeyCreate{
		AccountSID: cfg.AccountSID,
	})
	s.Done()

	s = Step(t, "assert-sid-and-token-non-empty")
	if sid == "" || token == "" {
		s.Fatalf("empty sid/token: sid=%q token=%q", sid, token)
	}
	if strings.TrimSpace(token) == "" {
		s.Errorf("api key token is blank")
	}
	s.Logf("created api_key sid=%s", sid) // don't log the token
	s.Done()
}

// TestApiKey_RoundTrip creates a fresh API key, builds a new client with it,
// and verifies that key actually authenticates subsequent requests. This is
// the round-trip we were previously missing: create != usable.
//
// Steps:
//  1. mint-api-key — POST /ApiKeys via managed helper; capture token
//  2. build-minted-client — new provision.Client backed by the minted token
//  3. call-with-minted-client — minted client fetches its own account
func TestApiKey_RoundTrip(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "mint-api-key")
	_, token := client.ManagedApiKey(t, ctx, provision.ApiKeyCreate{
		AccountSID: cfg.AccountSID,
	})
	if token == "" {
		s.Fatal("token is empty")
	}
	s.Done()

	s = Step(t, "build-minted-client")
	v, err := contract.New(mustSchemasRoot(t))
	if err != nil {
		s.Fatalf("contract new: %v", err)
	}
	minted := provision.New(cfg.APIBaseURL, token, cfg.AccountSID, v,
		provision.WithLabel("minted"))
	s.Done()

	s = Step(t, "call-with-minted-client")
	acct, err := minted.GetAccount(ctx, cfg.AccountSID)
	if err != nil {
		s.Fatalf("minted key GET /Accounts/{sid}: %v", err)
	}
	if acct.AccountSID != cfg.AccountSID {
		s.Errorf("minted key returned wrong account: got %q want %q",
			acct.AccountSID, cfg.AccountSID)
	}
	s.Done()
}

// TestApiKey_Create_SP creates an SP-scoped API key and verifies the one-time
// token is returned.
//
// Steps:
//  1. create-api-key-sp — POST /ApiKeys via SP client (managed helper → auto cleanup)
//  2. assert-sid-and-token-non-empty — both returned by the SP-scope create
func TestApiKey_Create_SP(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-api-key-sp")
	sid, token := spClient.ManagedApiKey(t, ctx, provision.ApiKeyCreate{
		ServiceProviderSID: cfg.SPSID,
	})
	s.Done()

	s = Step(t, "assert-sid-and-token-non-empty")
	if sid == "" || token == "" {
		s.Fatalf("empty sid/token: sid=%q token=%q", sid, token)
	}
	s.Logf("sp created api_key sid=%s", sid)
	s.Done()
}

func mustSchemasRoot(t *testing.T) string {
	t.Helper()
	p, err := contract.ResolveSchemasRoot()
	if err != nil {
		recordFailure(t, "resolve-schemas", fmt.Sprintf("resolve schemas: %v", err))
		t.Fatalf("resolve schemas: %v", err)
	}
	return p
}
