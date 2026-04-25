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
	IsActive   bool   `json:"is_active,omitempty"`
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
// by the orphan sweeper at TestMain. No contract validation right now —
// the live server's response shape isn't covered by a vendored schema; if
// it drifts the sweeper is best-effort and won't break the suite.
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
