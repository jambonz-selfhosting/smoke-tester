package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestLcr_CRUD — creates + reads + deletes a Least-Cost-Routing table.
// Covers Tier 1 row 1.11.
//
// Steps:
//  1. create-lcr — POST /Lcrs via managed helper (auto cleanup)
//  2. get-lcr — GET /Lcrs/{sid} and assert sid + name match
//  3. list-and-find — GET /Lcrs and confirm sid is in the list
func TestLcr_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-lcr")
	name := provision.Name("lcr")
	sid := client.ManagedLcr(t, ctx, provision.LcrCreate{
		Name:       name,
		AccountSID: suite.AccountSID,
	})
	s.Logf("created lcr sid=%s", sid)
	s.Done()

	s = Step(t, "get-lcr")
	got, err := client.GetLcr(ctx, sid)
	if err != nil {
		s.Fatalf("get lcr: %v", err)
	}
	if got.LcrSID != sid {
		s.Fatalf("sid mismatch: got %q want %q", got.LcrSID, sid)
	}
	if got.Name != name {
		s.Errorf("name mismatch: got %q want %q", got.Name, name)
	}
	s.Done()

	s = Step(t, "list-and-find")
	lcrs, err := client.ListLcrs(ctx)
	if err != nil {
		s.Fatalf("list lcrs: %v", err)
	}
	var found bool
	for _, l := range lcrs {
		if l.LcrSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d lcrs)", sid, len(lcrs))
	}
	s.Done()
}
