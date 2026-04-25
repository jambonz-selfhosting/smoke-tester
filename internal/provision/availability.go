package provision

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

type AvailabilityType string

const (
	AvailabilityEmail     AvailabilityType = "email"
	AvailabilityPhone     AvailabilityType = "phone"
	AvailabilitySubdomain AvailabilityType = "subdomain"
)

// CheckAvailability queries whether a given value is free. Read-only probe;
// useful as a cluster-reachability sanity check.
func (c *Client) CheckAvailability(ctx context.Context, t AvailabilityType, value string) (bool, error) {
	q := url.Values{}
	q.Set("type", string(t))
	q.Set("value", value)
	raw, err := c.Request(ctx, http.MethodGet, "/Availability?"+q.Encode(), nil,
		"rest/availability/checkAvailability.response.200.json", http.StatusOK)
	if err != nil {
		return false, err
	}
	var out struct {
		Available bool `json:"available"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return false, fmt.Errorf("decode availability: %w", err)
	}
	return out.Available, nil
}
