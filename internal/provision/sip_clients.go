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

// SIPClient is jambonz's "Clients" resource — a username/password pair
// that can register over SIP under an account. Top-level POST /Clients
// (NOT nested under /Accounts/{sid}); see jambonz docs:
// https://docs.jambonz.org/reference/rest-platform-management/clients/create-client
//
// We only model the create + delete path the test harness needs. Clients
// also support GET / PUT / list — add as needed.
type SIPClient struct {
	ClientSID  string `json:"client_sid"`
	AccountSID string `json:"account_sid"`
	Username   string `json:"username"`
	// IsActive is `0`/`1` on the wire (drift: the swagger declares bool;
	// the running api-server emits integer). Use IntField so unmarshal
	// works regardless.
	IsActive IntField `json:"is_active,omitempty"`
}

// SIPClientCreate is the POST /Clients body. Required: account_sid,
// username, password. is_active defaults to "1" on the server side per
// docs; we set it explicitly for clarity.
type SIPClientCreate struct {
	AccountSID              string `json:"account_sid"`
	Username                string `json:"username"`
	Password                string `json:"password"`
	IsActive                string `json:"is_active,omitempty"`
	AllowDirectAppCalling   string `json:"allow_direct_app_calling,omitempty"`
	AllowDirectQueueCalling string `json:"allow_direct_queue_calling,omitempty"`
	AllowDirectUserCalling  string `json:"allow_direct_user_calling,omitempty"`
}

// CreateSIPClient POSTs /Clients. Returns the new SID. Contract-validated
// against schemas/rest/clients/createClient.response.201.json.
func (c *Client) CreateSIPClient(ctx context.Context, body SIPClientCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/Clients", body,
		"rest/clients/createClient.response.201.json", http.StatusCreated)
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

// ListSIPClients fetches every Client visible to the current scope. Used
// by account-teardown to enumerate-then-delete clients of THIS account
// before deleting the account itself (the upstream `DELETE /Accounts/<sid>`
// handler doesn't cascade `clients` and otherwise fails with an FK
// constraint error).
//
// IMPORTANT: the upstream `GET /Clients` endpoint ignores any `account_sid`
// query parameter — it returns every client visible to the bearer token,
// across all accounts under the same SP. Callers MUST filter the returned
// slice by AccountSID client-side; relying on the query string risks
// deleting clients that belong to siblings under the same SP. (See
// HANDOFF for the post-mortem of the run that learned this the hard way.)
func (c *Client) ListSIPClients(ctx context.Context) ([]SIPClient, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Clients", nil, "", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var clients []SIPClient
	if err := json.Unmarshal(raw, &clients); err != nil {
		return nil, fmt.Errorf("decode clients: %w", err)
	}
	return clients, nil
}

// ListSIPClientsForAccount lists clients server-side then filters by
// AccountSID client-side. This is the safe wrapper to use whenever the
// caller plans to mutate the returned set — see ListSIPClients for why.
func (c *Client) ListSIPClientsForAccount(ctx context.Context, accountSID string) ([]SIPClient, error) {
	all, err := c.ListSIPClients(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]SIPClient, 0, len(all))
	for _, cl := range all {
		if cl.AccountSID == accountSID {
			out = append(out, cl)
		}
	}
	return out, nil
}

// DeleteSIPClient removes a Client. 404 is treated as success so cleanup
// is idempotent.
func (c *Client) DeleteSIPClient(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/Clients/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// ManagedSIPClient creates a Client and registers a t.Cleanup that deletes
// it when the test ends. Returns (sid, username, password) — the username
// + password are NOT returned by the API on create, so we generate them
// here and include them in the request. The caller registers a SIP UAS
// using the same credentials.
//
// Username pattern: "it-<runID>-<hash8>". Password is a 16-byte random
// hex string. Both stay within jambonz's name validation (alnum + dash).
//
// Note: CreateSIPClient blocks until jambonz acknowledges the row;
// per-cluster propagation lag (if any) before REGISTER works is the
// caller's problem to handle (typically a single retry suffices).
func (c *Client) ManagedSIPClient(t *testing.T, ctx context.Context) (sid, username, password string) {
	t.Helper()
	username = uniqueUsername("uas")
	password = randomPassword()
	sid, err := c.CreateSIPClient(ctx, SIPClientCreate{
		AccountSID: c.accountSID,
		Username:   username,
		Password:   password,
		IsActive:   "1",
	})
	if err != nil {
		t.Fatalf("create SIP client: %v", err)
	}
	t.Cleanup(func() {
		// Use a fresh ctx — the test's may have been cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteSIPClient(cleanupCtx, sid); err != nil {
			t.Logf("cleanup: delete SIP client %s: %v", sid, err)
		}
	})
	return sid, username, password
}
