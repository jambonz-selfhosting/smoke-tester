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

// CreateAccountLlmCredential POSTs an LLM Services credential under an account
// and returns its SID. body carries vendor, label and the vendor's fields as the
// portal submits them (api_key, host, access_key_id, service_key, ...).
func (c *Client) CreateAccountLlmCredential(ctx context.Context, accountSID string, body map[string]any) (string, error) {
	path := fmt.Sprintf("/Accounts/%s/LlmCredentials", accountSID)
	raw, err := c.Request(ctx, http.MethodPost, path, body, "", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode SuccessfulAdd: %w", err)
	}
	return ok.SID, nil
}

// DeleteAccountLlmCredential removes a credential; 404 and 401 mean it is already gone.
func (c *Client) DeleteAccountLlmCredential(ctx context.Context, accountSID, sid string) error {
	path := fmt.Sprintf("/Accounts/%s/LlmCredentials/%s", accountSID, sid)
	_, err := c.Request(ctx, http.MethodDelete, path, nil, "", http.StatusNoContent)
	var apiErr *APIError
	if err != nil && errors.As(err, &apiErr) &&
		(apiErr.Status == http.StatusNotFound || apiErr.Status == http.StatusUnauthorized) {
		return nil
	}
	return err
}

// ManagedAccountLlmCredential creates a credential and deletes it when the test ends.
func (c *Client) ManagedAccountLlmCredential(t *testing.T, ctx context.Context, accountSID string, body map[string]any) string {
	t.Helper()
	sid, err := c.CreateAccountLlmCredential(ctx, accountSID, body)
	if err != nil {
		t.Fatalf("create %v llm credential: %v", body["vendor"], err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteAccountLlmCredential(cctx, accountSID, sid); err != nil {
			t.Logf("cleanup llm credential %s: %v", sid, err)
		}
	})
	return sid
}
