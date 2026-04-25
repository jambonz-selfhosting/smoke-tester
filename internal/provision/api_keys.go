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

type ApiKeyCreate struct {
	AccountSID         string `json:"account_sid,omitempty"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
	ExpirySecs         *int   `json:"expiry_secs,omitempty"`
}

type ApiKey struct {
	APIKeySID          string `json:"api_key_sid"`
	Token              string `json:"token"`
	AccountSID         string `json:"account_sid,omitempty"`
	ServiceProviderSID string `json:"service_provider_sid,omitempty"`
	ExpiresAt          string `json:"expires_at,omitempty"`
	CreatedAt          string `json:"created_at,omitempty"`
	LastUsed           string `json:"last_used,omitempty"`
}

type apiKeyAddResponse struct {
	SID   string `json:"sid"`
	Token string `json:"token"`
}

// CreateApiKey returns (sid, token). Token is one-time — capture it now.
func (c *Client) CreateApiKey(ctx context.Context, body ApiKeyCreate) (string, string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/ApiKeys", body,
		"rest/api_keys/createApikey.response.201.json", http.StatusCreated)
	if err != nil {
		return "", "", err
	}
	var ok apiKeyAddResponse
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", "", fmt.Errorf("decode SuccessfulApiKeyAdd: %w", err)
	}
	return ok.SID, ok.Token, nil
}

// DeleteApiKey: the swagger documents 200 for success (not 204).
func (c *Client) DeleteApiKey(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/ApiKeys/"+sid, nil, "", http.StatusOK)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusNoContent) {
			return nil
		}
		return err
	}
	return nil
}

func (c *Client) ManagedApiKey(t *testing.T, ctx context.Context, body ApiKeyCreate) (sid, token string) {
	t.Helper()
	sid, token, err := c.CreateApiKey(ctx, body)
	if err != nil {
		t.Fatalf("create api_key: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteApiKey(cctx, sid); err != nil {
			t.Logf("cleanup: delete api_key %s: %v", sid, err)
		}
	})
	return sid, token
}
