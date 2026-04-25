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

// CallTarget mirrors the Target schema used as `to` in POST /Calls.
//
// Three common shapes:
//   - registered user: {type:"user", name:"caller-uas@sip.jambonz.me"}
//   - phone number via carrier: {type:"phone", number:"+15551234", trunk:"..."}
//   - arbitrary SIP URI: {type:"sip", sipUri:"sip:foo@example.com"}
type CallTarget struct {
	Type       string `json:"type"` // "user" | "phone" | "sip"
	Name       string `json:"name,omitempty"`
	Number     string `json:"number,omitempty"`
	Trunk      string `json:"trunk,omitempty"`
	SipURI     string `json:"sipUri,omitempty"`
	AuthUser   string `json:"auth_user,omitempty"`
	AuthPass   string `json:"auth_password,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
}

// CallCreate is the body of POST /Accounts/{sid}/Calls.
type CallCreate struct {
	// Either application_sid OR (call_hook + call_status_hook) OR app_json is
	// required. For this phase we use app_json so no webhook is needed.
	ApplicationSID string   `json:"application_sid,omitempty"`
	CallHook       *Webhook `json:"call_hook,omitempty"`
	CallStatusHook *Webhook `json:"call_status_hook,omitempty"`

	// app_json is a JSON-encoded verb array. Takes precedence over call_hook.
	// The *value* of this field is itself a JSON string (not a sub-object) —
	// jambonz re-parses it server-side.
	AppJSON string `json:"app_json,omitempty"`

	From       string            `json:"from"`
	FromHost   string            `json:"fromHost,omitempty"`
	To         CallTarget        `json:"to"`
	Timeout    int               `json:"timeout,omitempty"`
	TimeLimit  int               `json:"timeLimit,omitempty"`
	Tag        map[string]any    `json:"tag,omitempty"`
	Headers    map[string]string `json:"headers,omitempty"`
	AnswerOnBridge bool          `json:"answerOnBridge,omitempty"`

	// Speech overrides. `*_label` selects a specific provisioned
	// SpeechCredential when the account has multiple under the same
	// vendor — feature-server reads these via the merged
	// `{...application, ...req.body}` shape (see middleware.js).
	SpeechSynthesisVendor    string `json:"speech_synthesis_vendor,omitempty"`
	SpeechSynthesisLabel     string `json:"speech_synthesis_label,omitempty"`
	SpeechSynthesisLanguage  string `json:"speech_synthesis_language,omitempty"`
	SpeechSynthesisVoice     string `json:"speech_synthesis_voice,omitempty"`
	SpeechRecognizerVendor   string `json:"speech_recognizer_vendor,omitempty"`
	SpeechRecognizerLabel    string `json:"speech_recognizer_label,omitempty"`
	SpeechRecognizerLanguage string `json:"speech_recognizer_language,omitempty"`
}

// CreateCall POSTs /Accounts/{AccountSid}/Calls. Returns the new call_sid.
// Requires an account-scoped client.
func (c *Client) CreateCall(ctx context.Context, body CallCreate) (string, error) {
	if c.accountSID == "" {
		return "", fmt.Errorf("CreateCall requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/Calls"
	raw, err := c.Request(ctx, http.MethodPost, path, body,
		"rest/calls/createCall.response.201.json", http.StatusCreated)
	if err != nil {
		return "", err
	}
	var ok struct {
		SID string `json:"sid"`
	}
	if err := json.Unmarshal(raw, &ok); err != nil {
		return "", fmt.Errorf("decode createCall: %w", err)
	}
	return ok.SID, nil
}

// DeleteCall ends a live call (204). Idempotent on 404.
func (c *Client) DeleteCall(ctx context.Context, callSID string) error {
	if c.accountSID == "" {
		return fmt.Errorf("DeleteCall requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/Calls/" + callSID
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

// ManagedCall creates a call and registers a t.Cleanup that hangs it up if
// still active when the test ends.
func (c *Client) ManagedCall(t *testing.T, ctx context.Context, body CallCreate) string {
	t.Helper()
	sid, err := c.CreateCall(ctx, body)
	if err != nil {
		t.Fatalf("create call: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := c.DeleteCall(cctx, sid); err != nil {
			t.Logf("cleanup: delete call %s: %v", sid, err)
		}
	})
	return sid
}
