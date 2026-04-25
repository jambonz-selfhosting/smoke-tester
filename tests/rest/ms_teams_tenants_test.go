package rest

import (
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestMsTeamsTenant_CRUD creates + lists + deletes an MS Teams tenant under
// the test account. Covers Tier 1 row 1.9. Uses a synthetic test domain so
// this doesn't claim a real tenant name.
//
// Steps:
//  1. create-ms-teams-tenant — POST /MsTeamsTenants via managed helper (auto cleanup)
//  2. list-and-find — GET /MsTeamsTenants and confirm sid is in the list
func TestMsTeamsTenant_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-ms-teams-tenant")
	// swagger requires service_provider_sid — use the SP that owns our account.
	fqdn := fmt.Sprintf("jambonz-it-%s.example.invalid", provision.RunID())
	sid := client.ManagedMsTeamsTenant(t, ctx, provision.MsTeamsTenantCreate{
		ServiceProviderSID: cfg.SPSID,
		AccountSID:         cfg.AccountSID,
		TenantFQDN:         fqdn,
	})
	s.Logf("created ms_teams_tenant sid=%s fqdn=%s", sid, fqdn)
	s.Done()

	s = Step(t, "list-and-find")
	tenants, err := client.ListMsTeamsTenants(ctx)
	if err != nil {
		s.Fatalf("list ms_teams_tenants: %v", err)
	}
	var found bool
	for _, x := range tenants {
		if x.MsTeamsTenantSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d tenants)", sid, len(tenants))
	}
	s.Done()
}
