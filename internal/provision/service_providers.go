package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ServiceProvider is read-only from our perspective — creating/deleting real
// SPs on a shared cluster is too destructive. Tier 1 only lists and gets.
type ServiceProvider struct {
	ServiceProviderSID string   `json:"service_provider_sid"`
	Name               string   `json:"name"`
	Description        string   `json:"description,omitempty"`
	RootDomain         string   `json:"root_domain,omitempty"`
	MsTeamsFqdn        string   `json:"ms_teams_fqdn,omitempty"`
	RegistrationHook   *Webhook `json:"registration_hook,omitempty"`
}

func (c *Client) ListServiceProviders(ctx context.Context) ([]ServiceProvider, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/ServiceProviders", nil,
		"rest/service_providers/listServiceProviders.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []ServiceProvider
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode service_providers: %w", err)
	}
	return out, nil
}

func (c *Client) GetServiceProvider(ctx context.Context, sid string) (*ServiceProvider, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/ServiceProviders/"+sid, nil,
		"rest/service_providers/getServiceProvider.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var sp ServiceProvider
	if err := json.Unmarshal(raw, &sp); err != nil {
		return nil, fmt.Errorf("decode ServiceProvider: %w", err)
	}
	return &sp, nil
}
