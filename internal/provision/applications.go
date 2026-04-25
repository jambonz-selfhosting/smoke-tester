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

// Webhook mirrors the swagger's Webhook component.
type Webhook struct {
	URL      string `json:"url"`
	Method   string `json:"method,omitempty"`   // "get" | "post"
	Username string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
}

// ApplicationCreate is the request body for POST /Applications.
//
// `*_label` fields aren't documented in swagger but are consumed by
// feature-server (see feature-server/lib/middleware.js, call-session.js)
// — they pick a specific SpeechCredential row when the account has multiple
// under the same vendor.
type ApplicationCreate struct {
	Name                     string                 `json:"name"`
	AccountSID               string                 `json:"account_sid"`
	CallHook                 Webhook                `json:"call_hook"`
	CallStatusHook           Webhook                `json:"call_status_hook"`
	MessagingHook            *Webhook               `json:"messaging_hook,omitempty"`
	AppJSON                  string                 `json:"app_json,omitempty"`
	SpeechSynthesisVendor    string                 `json:"speech_synthesis_vendor,omitempty"`
	SpeechSynthesisLabel     string                 `json:"speech_synthesis_label,omitempty"`
	SpeechSynthesisVoice     string                 `json:"speech_synthesis_voice,omitempty"`
	SpeechRecognizerVendor   string                 `json:"speech_recognizer_vendor,omitempty"`
	SpeechRecognizerLabel    string                 `json:"speech_recognizer_label,omitempty"`
	SpeechRecognizerLanguage string                 `json:"speech_recognizer_language,omitempty"`
	EnvVars                  map[string]any         `json:"env_vars,omitempty"`
}

// Application is the server-returned shape (matches the swagger Application
// component — all required fields are present, nullable fields decoded as zero-
// values or *T).
type Application struct {
	ApplicationSID           string  `json:"application_sid"`
	Name                     string  `json:"name"`
	AccountSID               string  `json:"account_sid"`
	CallHook                 Webhook `json:"call_hook"`
	CallStatusHook           Webhook `json:"call_status_hook"`
	MessagingHook            *Webhook `json:"messaging_hook,omitempty"`
	SpeechSynthesisVendor    string  `json:"speech_synthesis_vendor,omitempty"`
	SpeechSynthesisVoice     string  `json:"speech_synthesis_voice,omitempty"`
	SpeechRecognizerVendor   string  `json:"speech_recognizer_vendor,omitempty"`
	SpeechRecognizerLanguage string  `json:"speech_recognizer_language,omitempty"`
}

// CreateApplication POSTs /Applications. Returns the new SID.
func (c *Client) CreateApplication(ctx context.Context, body ApplicationCreate) (string, error) {
	raw, err := c.Request(ctx, http.MethodPost, "/Applications", body,
		"rest/applications/createApplication.response.201.json", http.StatusCreated)
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

// GetApplication retrieves a single application by SID.
func (c *Client) GetApplication(ctx context.Context, sid string) (*Application, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Applications/"+sid, nil,
		"rest/applications/getApplication.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var app Application
	if err := json.Unmarshal(raw, &app); err != nil {
		return nil, fmt.Errorf("decode Application: %w", err)
	}
	return &app, nil
}

// ListApplications returns all applications (scoped by token).
func (c *Client) ListApplications(ctx context.Context) ([]Application, error) {
	raw, err := c.Request(ctx, http.MethodGet, "/Applications", nil,
		"rest/applications/listApplications.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var apps []Application
	if err := json.Unmarshal(raw, &apps); err != nil {
		return nil, fmt.Errorf("decode applications: %w", err)
	}
	return apps, nil
}

// ApplicationUpdate is the PUT body — primary-key / immutable fields are
// excluded so the server doesn't reject them with "is immutable".
type ApplicationUpdate struct {
	Name                     string   `json:"name,omitempty"`
	CallHook                 *Webhook `json:"call_hook,omitempty"`
	CallStatusHook           *Webhook `json:"call_status_hook,omitempty"`
	MessagingHook            *Webhook `json:"messaging_hook,omitempty"`
	AppJSON                  string   `json:"app_json,omitempty"`
	SpeechSynthesisVendor    string   `json:"speech_synthesis_vendor,omitempty"`
	SpeechSynthesisVoice     string   `json:"speech_synthesis_voice,omitempty"`
	SpeechRecognizerVendor   string   `json:"speech_recognizer_vendor,omitempty"`
	SpeechRecognizerLanguage string   `json:"speech_recognizer_language,omitempty"`
}

// UpdateApplication replaces the application's configuration. 204 No Content
// on success — no body, no contract check.
func (c *Client) UpdateApplication(ctx context.Context, sid string, body ApplicationUpdate) error {
	_, err := c.Request(ctx, http.MethodPut, "/Applications/"+sid, body, "", http.StatusNoContent)
	return err
}

// DeleteApplication removes an application; swallows 404 so cleanup is
// idempotent (per ADR-0008). 204 responses have no body, so no contract
// check is needed.
func (c *Client) DeleteApplication(ctx context.Context, sid string) error {
	_, err := c.Request(ctx, http.MethodDelete, "/Applications/"+sid, nil, "", http.StatusNoContent)
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.Status == http.StatusNotFound {
			return nil
		}
		return err
	}
	return nil
}

// ManagedApplication creates an Application and registers a t.Cleanup that
// deletes it when the test ends. The returned SID is the server-assigned one.
// Use this as the default path; if you need raw control, call CreateApplication
// directly and wire cleanup yourself.
func (c *Client) ManagedApplication(t *testing.T, ctx context.Context, body ApplicationCreate) (sid string) {
	t.Helper()
	sid, err := c.CreateApplication(ctx, body)
	if err != nil {
		t.Fatalf("create application: %v", err)
	}
	t.Cleanup(func() {
		// Use a fresh context — the test's ctx may have been cancelled.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := c.DeleteApplication(cleanupCtx, sid); err != nil {
			t.Logf("cleanup: delete application %s: %v", sid, err)
		}
	})
	return sid
}
