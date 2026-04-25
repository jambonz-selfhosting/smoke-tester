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

type VoipCarrierCreate struct {
	Name                string `json:"name"`
	Description         string `json:"description,omitempty"`
	AccountSID          string `json:"account_sid,omitempty"`
	ApplicationSID      string `json:"application_sid,omitempty"`
	ServiceProviderSID  string `json:"service_provider_sid,omitempty"`
	E164LeadingPlus     *bool  `json:"e164_leading_plus,omitempty"`
	RequiresRegister    *bool  `json:"requires_register,omitempty"`
	RegisterUsername    string `json:"register_username,omitempty"`
	RegisterSIPRealm    string `json:"register_sip_realm,omitempty"`
	RegisterPassword    string `json:"register_password,omitempty"`
	TechPrefix          string `json:"tech_prefix,omitempty"`
	InboundAuthUsername string `json:"inbound_auth_username,omitempty"`
	InboundAuthPassword string `json:"inbound_auth_password,omitempty"`
	IsActive            *bool  `json:"is_active,omitempty"`
}

type VoipCarrier struct {
	VoipCarrierSID     string `json:"voip_carrier_sid"`
	Name               string `json:"name"`
	Description        string `json:"description,omitempty"`
	AccountSID         string `json:"account_sid,omitempty"`
	ApplicationSID     string `json:"application_sid,omitempty"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
}

func (c *Client) CreateVoipCarrier(ctx context.Context, body VoipCarrierCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/VoipCarriers", body,
		"rest/voip_carriers/createVoipCarrier.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

func (c *Client) GetVoipCarrier(ctx context.Context, sid string) (*VoipCarrier, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/VoipCarriers/"+sid, nil,
		"rest/voip_carriers/getVoipCarrier.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var v VoipCarrier
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, fmt.Errorf("decode VoipCarrier: %w", err)
	}
	return &v, nil
}

func (c *Client) ListVoipCarriers(ctx context.Context) ([]VoipCarrier, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/VoipCarriers", nil,
		"rest/voip_carriers/listVoipCarriers.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []VoipCarrier
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode voip_carriers: %w", err)
	}
	return out, nil
}

// VoipCarrierUpdate excludes the primary-key field.
type VoipCarrierUpdate struct {
	Name                string `json:"name,omitempty"`
	Description         string `json:"description,omitempty"`
	E164LeadingPlus     *bool  `json:"e164_leading_plus,omitempty"`
	RequiresRegister    *bool  `json:"requires_register,omitempty"`
	RegisterUsername    string `json:"register_username,omitempty"`
	RegisterSIPRealm    string `json:"register_sip_realm,omitempty"`
	RegisterPassword    string `json:"register_password,omitempty"`
	TechPrefix          string `json:"tech_prefix,omitempty"`
	InboundAuthUsername string `json:"inbound_auth_username,omitempty"`
	InboundAuthPassword string `json:"inbound_auth_password,omitempty"`
	IsActive            *bool  `json:"is_active,omitempty"`
}

// UpdateVoipCarrier replaces the carrier's configuration. 204 on success.
func (c *Client) UpdateVoipCarrier(ctx context.Context, sid string, body VoipCarrierUpdate) error {
	_, err := c.Request(ctx, http.MethodPut, "/VoipCarriers/"+sid, body, "", http.StatusNoContent)
	return err
}

func (c *Client) DeleteVoipCarrier(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/VoipCarriers/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// CreateVoipCarrierUnderSP creates a carrier via the SP-scoped endpoint
// `/ServiceProviders/{sid}/VoipCarriers`. Requires an SP-scope client.
func (c *Client) CreateVoipCarrierUnderSP(ctx context.Context, spSID string, body VoipCarrierCreate) (string, error) {
	path := "/ServiceProviders/" + spSID + "/VoipCarriers"
	raw, err := c.Request(ctx, http.MethodPost, path, body,
		"rest/voip_carriers/createCarrierForServiceProvider.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

// ListVoipCarriersUnderSP lists carriers belonging to a specific SP via the
// nested endpoint.
func (c *Client) ListVoipCarriersUnderSP(ctx context.Context, spSID string) ([]VoipCarrier, error) {
	path := "/ServiceProviders/" + spSID + "/VoipCarriers"
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/voip_carriers/getServiceProviderCarriers.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []VoipCarrier
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode voip_carriers: %w", err)
	}
	return out, nil
}

func (c *Client) ManagedVoipCarrier(t *testing.T, ctx context.Context, body VoipCarrierCreate) string {
	t.Helper()
	sid, err := c.CreateVoipCarrier(ctx, body)
	if err != nil {
		t.Fatalf("create voip carrier: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteVoipCarrier(cctx, sid); err != nil {
			t.Logf("cleanup: delete voip_carrier %s: %v", sid, err)
		}
	})
	return sid
}
