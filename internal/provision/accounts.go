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

// AccountCreate is the request body for POST /Accounts.
type AccountCreate struct {
	Name               string   `json:"name"`
	ServiceProviderSID string   `json:"service_provider_sid"`
	SIPRealm           string   `json:"sip_realm,omitempty"`
	RegistrationHook   *Webhook `json:"registration_hook,omitempty"`
	QueueEventHook     *Webhook `json:"queue_event_hook,omitempty"`
}

// Account mirrors the server's response shape.
type Account struct {
	AccountSID                  string   `json:"account_sid"`
	Name                        string   `json:"name"`
	ServiceProviderSID          string   `json:"service_provider_sid"`
	SIPRealm                    string   `json:"sip_realm,omitempty"`
	RegistrationHook            *Webhook `json:"registration_hook,omitempty"`
	QueueEventHook              *Webhook `json:"queue_event_hook,omitempty"`
	DeviceCallingApplicationSID string   `json:"device_calling_application_sid,omitempty"`
	WebhookSecret               string   `json:"webhook_secret,omitempty"`
}

// CreateAccount POSTs /Accounts. Requires SP-scoped token.
func (c *Client) CreateAccount(ctx context.Context, body AccountCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/Accounts", body,
		"rest/accounts/createAccount.response.201.json", http.StatusCreated)
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

// GetAccount retrieves a single account by SID.
func (c *Client) GetAccount(ctx context.Context, sid string) (*Account, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Accounts/"+sid, nil,
		"rest/accounts/getAccount.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var a Account
	if err := json.Unmarshal(raw, &a); err != nil {
		return nil, fmt.Errorf("decode Account: %w", err)
	}
	return &a, nil
}

// ListAccounts returns accounts visible to this token — all of them under an
// SP if the token is SP-scoped, or just the one for an account-scoped token.
func (c *Client) ListAccounts(ctx context.Context) ([]Account, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Accounts", nil,
		"rest/accounts/listAccounts.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out []Account
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode accounts: %w", err)
	}
	return out, nil
}

// AccountUpdate holds fields patchable on an Account. Most tenants only
// update name / sip_realm / hooks — keep the struct narrow.
type AccountUpdate struct {
	Name             string   `json:"name,omitempty"`
	SIPRealm         string   `json:"sip_realm,omitempty"`
	RegistrationHook *Webhook `json:"registration_hook,omitempty"`
	QueueEventHook   *Webhook `json:"queue_event_hook,omitempty"`
}

// UpdateAccount reassigns configuration. 204 on success.
func (c *Client) UpdateAccount(ctx context.Context, sid string, body AccountUpdate) error {
	_, err := c.Request(ctx, http.MethodPut, "/Accounts/"+sid, body, "", http.StatusNoContent)
	return err
}

// DeleteAccount removes an account; idempotent on 404.
func (c *Client) DeleteAccount(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/Accounts/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// ManagedAccount creates an Account and registers a t.Cleanup that deletes it.
// Must be called with an SP-scoped client.
func (c *Client) ManagedAccount(t *testing.T, ctx context.Context, body AccountCreate) (sid string) {
	t.Helper()
	sid, err := c.CreateAccount(ctx, body)
	if err != nil {
		t.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteAccount(cleanupCtx, sid); err != nil {
			t.Logf("cleanup: delete account %s: %v", sid, err)
		}
	})
	return sid
}
