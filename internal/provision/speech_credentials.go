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

// SpeechCredentialCreate is the body for POST
// /Accounts/{account_sid}/SpeechCredentials. Vendor-specific keys
// (api_key, service_key, etc.) are passed at the top level.
//
// Swagger only enumerates {google, aws} for vendor, but the live api-server
// accepts every vendor jambonz speech-vendors.js supports — including
// `deepgram`, which is what we use across the verb suite.
//
// For deepgram, only `api_key` is required (deepgram_stt_uri / deepgram_tts_uri
// can be set if pointing at an on-prem cluster; we don't).
type SpeechCredentialCreate struct {
	Vendor     string `json:"vendor"`
	Label      string `json:"label,omitempty"`
	UseForTTS  bool   `json:"use_for_tts"`
	UseForSTT  bool   `json:"use_for_stt"`
	APIKey     string `json:"api_key,omitempty"`
	ServiceKey string `json:"service_key,omitempty"`
}

// CreateAccountSpeechCredential POSTs a credential under an account. Returns
// the new SID. Contract-validated against
// schemas/rest/speech_credentials/createSpeechCredential.response.201.json.
func (c *Client) CreateAccountSpeechCredential(ctx context.Context, accountSID string, body SpeechCredentialCreate) (string, error) {
	path := fmt.Sprintf("/Accounts/%s/SpeechCredentials", accountSID)
	raw, err := c.Request(ctx, http.MethodPost, path, body,
		"rest/speech_credentials/createSpeechCredential.response.201.json", http.StatusCreated)
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

// DeleteAccountSpeechCredential removes a credential. 404 is treated as
// success so cleanup is idempotent.
func (c *Client) DeleteAccountSpeechCredential(ctx context.Context, accountSID, sid string) error {
	path := fmt.Sprintf("/Accounts/%s/SpeechCredentials/%s", accountSID, sid)
	_, err := c.Request(ctx, http.MethodDelete, path, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// ManagedAccountSpeechCredential creates a SpeechCredential and registers a
// t.Cleanup that deletes it when the test ends. Useful when a single test
// needs its own credential; for the suite-wide Deepgram credential the verb
// TestMain provisions, see provisionDeepgramCredential in verbsmain_test.go
// (provisioned outside a *testing.T so it has its own deferred-cleanup
// shape — TestMain runs after every test).
func (c *Client) ManagedAccountSpeechCredential(t *testing.T, ctx context.Context, accountSID string, body SpeechCredentialCreate) string {
	t.Helper()
	sid, err := c.CreateAccountSpeechCredential(ctx, accountSID, body)
	if err != nil {
		t.Fatalf("create speech credential: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteAccountSpeechCredential(cleanupCtx, accountSID, sid); err != nil {
			t.Logf("cleanup: delete speech credential %s: %v", sid, err)
		}
	})
	return sid
}
