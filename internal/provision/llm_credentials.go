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

// LlmCredentialCreate is the body for POST /Accounts/{sid}/LlmCredentials
// (api-key vendors only).
type LlmCredentialCreate struct {
	Vendor string `json:"vendor"`
	Label  string `json:"label,omitempty"`
	APIKey string `json:"api_key"`
}

// CreateAccountLlmCredential POSTs an LLM credential under an account and
// returns the new SID.
func (c *Client) CreateAccountLlmCredential(ctx context.Context, accountSID string, body LlmCredentialCreate) (string, error) {
	path := fmt.Sprintf("/Accounts/%s/LlmCredentials", accountSID)
	raw, err := c.Request(ctx, http.MethodPost, path, body,
		"rest/llm_credentials/createLlmCredential.response.201.json", http.StatusCreated)
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

// DeleteAccountLlmCredential removes an LLM credential; 404/401 mean it is
// already gone (see DeleteAccountSpeechCredential).
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

// ManagedAccountLlmCredential creates an LLM credential deleted when the test ends.
func (c *Client) ManagedAccountLlmCredential(t *testing.T, ctx context.Context, accountSID string, body LlmCredentialCreate) string {
	t.Helper()
	sid, err := c.CreateAccountLlmCredential(ctx, accountSID, body)
	if err != nil {
		t.Fatalf("create llm credential: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteAccountLlmCredential(cleanupCtx, accountSID, sid); err != nil {
			t.Logf("cleanup: delete llm credential %s: %v", sid, err)
		}
	})
	return sid
}
