package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Sbc represents a row in the SBC address table (read-only from our
// perspective — the harness does not create SBC entries).
type Sbc struct {
	SbcAddressSID      string `json:"sbc_address_sid"`
	IPv4               string `json:"ipv4"`
	Port               int    `json:"port,omitempty"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
}

func (c *Client) ListSbcs(ctx context.Context) ([]Sbc, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Sbcs", nil,
		"rest/sbcs/listSbcs.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []Sbc
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode sbcs: %w", err)
	}
	return out, nil
}
