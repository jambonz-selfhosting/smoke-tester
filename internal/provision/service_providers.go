package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ServiceProvider is never created or deleted — that is too destructive on a
// shared cluster. Tier 1 lists and gets; UpdateServiceProvider only flips
// flags that a test restores in t.Cleanup.
type ServiceProvider struct {
	ServiceProviderSID string   `json:"service_provider_sid"`
	Name               string   `json:"name"`
	Description        string   `json:"description,omitempty"`
	RootDomain         string   `json:"root_domain,omitempty"`
	MsTeamsFqdn        string   `json:"ms_teams_fqdn,omitempty"`
	RegistrationHook   *Webhook `json:"registration_hook,omitempty"`
	// DisableMediaCapture opts every account under the SP out of
	// troubleshooting audio capture (MySQL BOOLEAN → 0/1).
	DisableMediaCapture Flag `json:"disable_media_capture"`
}

// ServiceProviderUpdate is the narrow PUT body the harness sends; the SP
// itself is never created or deleted (see ServiceProvider).
type ServiceProviderUpdate struct {
	DisableMediaCapture *bool `json:"disable_media_capture,omitempty"`
}

// UpdateServiceProvider PUTs /ServiceProviders/{sid}. 204 on success.
func (c *Client) UpdateServiceProvider(ctx context.Context, sid string, body ServiceProviderUpdate) error {
	_, err := c.Request(ctx, http.MethodPut, "/ServiceProviders/"+sid, body, "", http.StatusNoContent)
	return err
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
