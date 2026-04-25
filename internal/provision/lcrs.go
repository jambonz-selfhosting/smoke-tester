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

type LcrCreate struct {
	Name               string `json:"name"`
	AccountSID         string `json:"account_sid,omitempty"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
}

type Lcr struct {
	LcrSID                    string `json:"lcr_sid"`
	Name                      string `json:"name"`
	Description               string `json:"description,omitempty"`
	AccountSID                string `json:"account_sid,omitempty"`
	ServiceProviderSID        string `json:"service_provider_sid,omitempty"`
	DefaultCarrierSetEntrySID string `json:"default_carrier_set_entry_sid,omitempty"`
}

func (c *Client) CreateLcr(ctx context.Context, body LcrCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/Lcrs", body,
		"rest/lcrs/createLcr.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct{ SID string `json:"sid"` }
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

func (c *Client) GetLcr(ctx context.Context, sid string) (*Lcr, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Lcrs/"+sid, nil,
		"rest/lcrs/getLcr.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var l Lcr
	if err := json.Unmarshal(raw, &l); err != nil {
		return nil, fmt.Errorf("decode Lcr: %w", err)
	}
	return &l, nil
}

func (c *Client) ListLcrs(ctx context.Context) ([]Lcr, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Lcrs", nil,
		"rest/lcrs/listLcrs.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []Lcr
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode lcrs: %w", err)
	}
	return out, nil
}

func (c *Client) DeleteLcr(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/Lcrs/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) ManagedLcr(t *testing.T, ctx context.Context, body LcrCreate) string {
	t.Helper()
	sid, err := c.CreateLcr(ctx, body)
	if err != nil {
		t.Fatalf("create lcr: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteLcr(cctx, sid); err != nil {
			t.Logf("cleanup: delete lcr %s: %v", sid, err)
		}
	})
	return sid
}
