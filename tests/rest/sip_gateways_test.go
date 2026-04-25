package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestSipGateway_CRUD — creates a parent VoipCarrier, then a SipGateway
// nested under it. Covers Tier 1 row 1.5.
//
// Steps:
//  1. create-voip-carrier — POST /VoipCarriers (cleanup via managed helper)
//  2. create-sip-gateway — POST /SipGateways bound to that carrier
//  3. get-sip-gateway — GET /SipGateways/{sid}; assert sid + carrier match
//  4. list-and-find — GET /SipGateways?voip_carrier_sid=...; confirm gw sid present
func TestSipGateway_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-voip-carrier")
	carrierSID := client.ManagedVoipCarrier(t, ctx, provision.VoipCarrierCreate{
		Name:       provision.Name("carrier-for-gw"),
		AccountSID: cfg.AccountSID,
	})
	s.Done()

	s = Step(t, "create-sip-gateway")
	port := 5060
	inbound := true
	outbound := true
	gwSID := client.ManagedSipGateway(t, ctx, provision.SipGatewayCreate{
		VoipCarrierSID: carrierSID,
		IPv4:           "192.0.2.1", // RFC 5737 — documentation / test IP, never routable
		Port:           &port,
		Inbound:        &inbound,
		Outbound:       &outbound,
	})
	s.Logf("created sip_gateway sid=%s", gwSID)
	s.Done()

	s = Step(t, "get-sip-gateway")
	got, err := client.GetSipGateway(ctx, gwSID)
	if err != nil {
		s.Fatalf("get sip_gateway: %v", err)
	}
	if got.SipGatewaySID != gwSID {
		s.Fatalf("sid mismatch: got %q want %q", got.SipGatewaySID, gwSID)
	}
	if got.VoipCarrierSID != carrierSID {
		s.Errorf("voip_carrier_sid mismatch: got %q want %q", got.VoipCarrierSID, carrierSID)
	}
	s.Done()

	s = Step(t, "list-and-find")
	gateways, err := client.ListSipGateways(ctx, carrierSID)
	if err != nil {
		s.Fatalf("list sip_gateways: %v", err)
	}
	if len(gateways) == 0 {
		s.Fatalf("list returned empty for carrier %s", carrierSID)
	}
	var found bool
	for _, g := range gateways {
		if g.SipGatewaySID == gwSID {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include gateway %q under carrier %q", gwSID, carrierSID)
	}
	s.Done()
}
