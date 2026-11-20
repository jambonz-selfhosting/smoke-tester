//go:build detail

package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

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
