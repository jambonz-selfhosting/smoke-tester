package rest

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestVoipCarrier_CRUD — account-scope create/read/delete of a VoipCarrier.
// Covers Tier 1 row 1.4.
//
// Steps:
//  1. create-voip-carrier — POST /VoipCarriers (cleanup via managed helper)
//  2. get-voip-carrier — GET /VoipCarriers/{sid}; assert sid + name match
//  3. list-and-find — GET /VoipCarriers; confirm sid is present
func TestVoipCarrier_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-voip-carrier")
	name := provision.Name("carrier")
	sid := client.ManagedVoipCarrier(t, ctx, provision.VoipCarrierCreate{
		Name:        name,
		Description: "smoke-tester test carrier",
		AccountSID:  suite.AccountSID,
	})
	s.Logf("created voip_carrier sid=%s", sid)
	s.Done()

	s = Step(t, "get-voip-carrier")
	got, err := client.GetVoipCarrier(ctx, sid)
	if err != nil {
		s.Fatalf("get voip_carrier: %v", err)
	}
	if got.VoipCarrierSID != sid {
		s.Fatalf("sid mismatch: got %q want %q", got.VoipCarrierSID, sid)
	}
	if got.Name != name {
		s.Errorf("name mismatch: got %q want %q", got.Name, name)
	}
	if got.Description != "smoke-tester test carrier" {
		s.Errorf("description round-trip mismatch: got %q want %q",
			got.Description, "smoke-tester test carrier")
	}
	if got.AccountSID != suite.AccountSID {
		s.Errorf("account_sid mismatch: got %q want %q", got.AccountSID, suite.AccountSID)
	}
	s.Done()

	s = Step(t, "list-and-find")
	carriers, err := client.ListVoipCarriers(ctx)
	if err != nil {
		s.Fatalf("list voip_carriers: %v", err)
	}
	var found bool
	for _, c := range carriers {
		if c.VoipCarrierSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d carriers)", sid, len(carriers))
	}
	s.Done()
}

// TestVoipCarrier_CRUD_SP exercises the SP-scoped nested endpoint
// `/ServiceProviders/{sid}/VoipCarriers`. This is a distinct route from
// the flat `/VoipCarriers` — different authorization path on the server.
//
// Steps:
//  1. create-voip-carrier-under-sp — POST /ServiceProviders/{sid}/VoipCarriers
//  2. list-and-find-under-sp — GET /ServiceProviders/{sid}/VoipCarriers and find sid
func TestVoipCarrier_CRUD_SP(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-voip-carrier-under-sp")
	name := provision.Name("carrier-sp")
	sid, err := spClient.CreateVoipCarrierUnderSP(ctx, cfg.SPSID, provision.VoipCarrierCreate{
		Name: name,
	})
	if err != nil {
		s.Fatalf("sp create carrier: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = spClient.DeleteVoipCarrier(cctx, sid)
	})
	s.Logf("sp created voip_carrier sid=%s", sid)
	s.Done()

	s = Step(t, "list-and-find-under-sp")
	carriers, err := spClient.ListVoipCarriersUnderSP(ctx, cfg.SPSID)
	if err != nil {
		s.Fatalf("sp list carriers: %v", err)
	}
	var found bool
	for _, c := range carriers {
		if c.VoipCarrierSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("sp list did not include sid %q (found %d)", sid, len(carriers))
	}
	s.Done()
}

// TestVoipCarrier_PUT exercises the update path. Covers Tier 2 row 2.1.
//
// Steps:
//  1. create-voip-carrier — POST /VoipCarriers (cleanup via managed helper)
//  2. update-voip-carrier — PUT /VoipCarriers/{sid} with a new description
//  3. get-and-assert-updated — GET /VoipCarriers/{sid} confirms the new description
func TestVoipCarrier_PUT(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-voip-carrier")
	sid := client.ManagedVoipCarrier(t, ctx, provision.VoipCarrierCreate{
		Name:       provision.Name("carrier-put"),
		AccountSID: suite.AccountSID,
	})
	s.Done()

	s = Step(t, "update-voip-carrier")
	if err := client.UpdateVoipCarrier(ctx, sid, provision.VoipCarrierUpdate{
		Description: "updated by smoke-tester",
	}); err != nil {
		s.Fatalf("update voip_carrier: %v", err)
	}
	s.Done()

	s = Step(t, "get-and-assert-updated")
	after, err := client.GetVoipCarrier(ctx, sid)
	if err != nil {
		s.Fatalf("get after update: %v", err)
	}
	if after.Description != "updated by smoke-tester" {
		s.Errorf("description not updated: got %q", after.Description)
	}
	s.Done()
}
