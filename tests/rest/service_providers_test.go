package rest

import (
	"testing"
	"time"
)

// TestServiceProviders_Read exercises list + get against the SP-scope token.
// Covers Tier 1 row 1.15 (read-only subset).
//
// Steps:
//  1. list-service-providers — GET /ServiceProviders via SP client; non-empty
//  2. get-service-provider — GET /ServiceProviders/{sid}; sid + name sanity
func TestServiceProviders_Read(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "list-service-providers")
	sps, err := spClient.ListServiceProviders(ctx)
	if err != nil {
		s.Fatalf("list sps: %v", err)
	}
	if len(sps) == 0 {
		s.Fatal("SP-scoped token returned empty SP list")
	}
	s.Logf("SP token can see %d service provider(s)", len(sps))
	s.Done()

	s = Step(t, "get-service-provider")
	sp, err := spClient.GetServiceProvider(ctx, cfg.SPSID)
	if err != nil {
		s.Fatalf("get sp: %v", err)
	}
	if sp.ServiceProviderSID != cfg.SPSID {
		s.Errorf("sid mismatch: got %q want %q", sp.ServiceProviderSID, cfg.SPSID)
	}
	if sp.Name == "" {
		s.Errorf("SP has empty name: %+v", sp)
	}
	s.Done()
}
