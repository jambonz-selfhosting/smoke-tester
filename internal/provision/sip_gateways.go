package provision

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

type SipGatewayCreate struct {
	VoipCarrierSID string `json:"voip_carrier_sid"`
	IPv4           string `json:"ipv4"`
	Port           *int   `json:"port,omitempty"`
	Netmask        *int   `json:"netmask,omitempty"`
	Inbound        *bool  `json:"inbound,omitempty"`
	Outbound       *bool  `json:"outbound,omitempty"`
	IsActive       *bool  `json:"is_active,omitempty"`
	Protocol       string `json:"protocol,omitempty"`
}

// SipGateway mirrors the server-returned shape. Boolean flags like `inbound`
// and `outbound` come back as 0/1 integers in live responses (the swagger
// declares them `boolean`); decode as int so either serialization works.
type SipGateway struct {
	SipGatewaySID  string `json:"sip_gateway_sid"`
	VoipCarrierSID string `json:"voip_carrier_sid"`
	IPv4           string `json:"ipv4"`
	Port           int    `json:"port,omitempty"`
	Netmask        int    `json:"netmask,omitempty"`
	Inbound        int    `json:"inbound,omitempty"`
	Outbound       int    `json:"outbound,omitempty"`
}

func (c *Client) CreateSipGateway(ctx context.Context, body SipGatewayCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/SipGateways", body,
		"rest/sip_gateways/createSipGateway.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

func (c *Client) GetSipGateway(ctx context.Context, sid string) (*SipGateway, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/SipGateways/"+sid, nil,
		"rest/sip_gateways/getSipGateway.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var g SipGateway
	if err := json.Unmarshal(raw, &g); err != nil {
		return nil, fmt.Errorf("decode SipGateway: %w", err)
	}
	return &g, nil
}

// ListSipGateways requires voip_carrier_sid as a query param.
func (c *Client) ListSipGateways(ctx context.Context, voipCarrierSID string) ([]SipGateway, error) {
	q := url.Values{}
	q.Set("voip_carrier_sid", voipCarrierSID)
	raw, err := c.Request(ctx, http.MethodGet, "/SipGateways?"+q.Encode(), nil,
		"rest/sip_gateways/listSipGateways.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []SipGateway
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode sip_gateways: %w", err)
	}
	return out, nil
}

func (c *Client) DeleteSipGateway(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/SipGateways/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) ManagedSipGateway(t *testing.T, ctx context.Context, body SipGatewayCreate) string {
	t.Helper()
	sid, err := c.CreateSipGateway(ctx, body)
	if err != nil {
		t.Fatalf("create sip gateway: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteSipGateway(cctx, sid); err != nil {
			t.Logf("cleanup: delete sip_gateway %s: %v", sid, err)
		}
	})
	return sid
}
