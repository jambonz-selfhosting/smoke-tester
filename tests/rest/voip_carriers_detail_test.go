//go:build detail

package rest

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

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
