package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
)

type MsTeamsTenantCreate struct {
	ServiceProviderSID string `json:"service_provider_sid"`
	AccountSID         string `json:"account_sid,omitempty"`
	ApplicationSID     string `json:"application_sid,omitempty"`
	TenantFQDN         string `json:"tenant_fqdn"`
}

type MsTeamsTenant struct {
	MsTeamsTenantSID   string `json:"ms_teams_tenant_sid"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
	AccountSID         string `json:"account_sid,omitempty"`
	ApplicationSID     string `json:"application_sid,omitempty"`
	TenantFQDN         string `json:"tenant_fqdn"`
}

func (c *Client) CreateMsTeamsTenant(ctx context.Context, body MsTeamsTenantCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/MicrosoftTeamsTenants", body,
		"rest/ms_teams_tenants/createMsTeamsTenant.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

func (c *Client) ListMsTeamsTenants(ctx context.Context) ([]MsTeamsTenant, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/MicrosoftTeamsTenants", nil,
		"rest/ms_teams_tenants/listMsTeamsTenants.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []MsTeamsTenant
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode ms_teams_tenants: %w", err)
	}
	return out, nil
}

func (c *Client) DeleteMsTeamsTenant(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/MicrosoftTeamsTenants/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) ManagedMsTeamsTenant(t *testing.T, ctx context.Context, body MsTeamsTenantCreate) string {
	t.Helper()
	sid, err := c.CreateMsTeamsTenant(ctx, body)
	if err != nil {
		t.Fatalf("create ms_teams_tenant: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteMsTeamsTenant(cctx, sid); err != nil {
			t.Logf("cleanup: delete ms_teams_tenant %s: %v", sid, err)
		}
	})
	return sid
}
