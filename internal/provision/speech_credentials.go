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
	// SpeechmaticsSTTURI is required by the API when vendor is
	// "speechmatics" — the realtime host, e.g. "eu2.rt.speechmatics.com".
	SpeechmaticsSTTURI string `json:"speechmatics_stt_uri,omitempty"`
	// ModelID is required by the API for some vendors (openai) and optional
	// for others; it is the default model the credential recognizes with,
	// overridable per-verb via the vendor's recognizer options.
	ModelID string `json:"model_id,omitempty"`
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

// SpeechCredentialTestResult is the body of GET
// /Accounts/{account_sid}/SpeechCredentials/{sid}/test — per-direction
// {status: "ok" | "fail" | "not tested", reason}.
type SpeechCredentialTestResult struct {
	TTS struct {
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
	} `json:"tts"`
	STT struct {
		Status string `json:"status"`
		Reason string `json:"reason,omitempty"`
	} `json:"stt"`
}

// TestAccountSpeechCredential exercises a credential against its vendor and
// records the outcome on the row (stt_tested_ok / tts_tested_ok).
//
// This is MANDATORY for google, and only for google: feature-server's
// getSpeechCredentials refuses a google credential whose stt_tested_ok (or
// tts_tested_ok) is not set — see lib/session/call-session.js — so a freshly
// provisioned google credential is invisible to the verbs until it has been
// tested, and the call fails with "stt using google requested but creds not
// supplied". Other vendors have no such gate.
func (c *Client) TestAccountSpeechCredential(ctx context.Context, accountSID, sid string) (SpeechCredentialTestResult, error) {
	var out SpeechCredentialTestResult
	path := fmt.Sprintf("/Accounts/%s/SpeechCredentials/%s/test", accountSID, sid)
	raw, err := c.Request(ctx, http.MethodGet, path, nil, "", http.StatusOK)
	if err != nil {
		return out, err
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, fmt.Errorf("decode speech credential test result: %w", err)
	}
	return out, nil
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
