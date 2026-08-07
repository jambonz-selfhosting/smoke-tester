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
	Type     string            `json:"type"` // "user" | "phone" | "sip"
	Name     string            `json:"name,omitempty"`
	Number   string            `json:"number,omitempty"`
	Trunk    string            `json:"trunk,omitempty"`
	SipURI   string            `json:"sipUri,omitempty"`
	AuthUser string            `json:"auth_user,omitempty"`
	AuthPass string            `json:"auth_password,omitempty"`
	Headers  map[string]string `json:"headers,omitempty"`
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

	From           string            `json:"from"`
	FromHost       string            `json:"fromHost,omitempty"`
	To             CallTarget        `json:"to"`
	Timeout        int               `json:"timeout,omitempty"`
	TimeLimit      int               `json:"timeLimit,omitempty"`
	Tag            map[string]any    `json:"tag,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	AnswerOnBridge bool              `json:"answerOnBridge,omitempty"`

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

// UpdateCall issues a Live Call Control command against an in-progress call:
// POST /Accounts/{AccountSid}/Calls/{CallSid} with an arbitrary command body
// (e.g. {transfer: {...}}, {whisper: ...}, {call_status: "completed"}). jambonz
// acknowledges with 202 Accepted; the command is applied asynchronously by the
// feature-server's CallSession.updateCall. body is any JSON-serializable value.
func (c *Client) UpdateCall(ctx context.Context, callSID string, body any) error {
	if c.accountSID == "" {
		return fmt.Errorf("UpdateCall requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/Calls/" + callSID
	_, err := c.Request(ctx, http.MethodPost, path, body, "", http.StatusAccepted)
	return err
}

// DeleteCall removes the call's record from jambonz's Redis store (204).
// It does NOT hang up a live dialog — the api-server's delete handler only
// touches Redis and never signals the feature-server. To actually end a call
// use HangupCall. Idempotent on 404.
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

// CallSummary is the subset of a listed live call the harness needs.
type CallSummary struct {
	CallSID    string `json:"call_sid"`
	CallStatus string `json:"call_status"`
	Direction  string `json:"direction"`
	From       string `json:"from"`
	To         string `json:"to"`
}

// liveCallStatuses are the jambonz call_status values that mean the call is
// still up. Completed calls linger in Redis for up to an hour
// (MAX_CALL_LIFETIME_AFTER_COMPLETED) and are still returned by ListLiveCalls,
// so callers must filter.
var liveCallStatuses = map[string]bool{
	"trying": true, "ringing": true, "early-media": true,
	"in-progress": true, "queued": true,
}

// IsLive reports whether the call is still up. An empty call_status is
// treated as live: better to send a redundant hangup than to skip a leak.
func (c CallSummary) IsLive() bool {
	if c.CallStatus == "" {
		return true
	}
	return liveCallStatuses[c.CallStatus]
}

// ListLiveCalls GETs /Accounts/{AccountSid}/Calls. Despite the method name,
// jambonz returns recent calls in ANY call_status, not only live ones:
// @jambonz/realtimedb-helpers' list-calls.js filters by callStatus only when
// the caller passes that query param (which this client does not), and
// update-call-status.js leaves a completed call in Redis's CALL_SET for
// MAX_CALL_LIFETIME_AFTER_COMPLETED (default 3600s) instead of removing it.
// So a whole run's already-COMPLETED calls come back too. Callers MUST
// filter with CallSummary.IsLive() — see SuiteAccount.Teardown for the
// pattern this endpoint requires. Requires an account-scoped client.
func (c *Client) ListLiveCalls(ctx context.Context) ([]CallSummary, error) {
	if c.accountSID == "" {
		return nil, fmt.Errorf("ListLiveCalls requires an account-scoped client")
	}
	path := "/Accounts/" + c.accountSID + "/Calls"
	raw, err := c.Request(ctx, http.MethodGet, path, nil,
		"rest/calls/listCalls.response.200.json", http.StatusOK)
	if err != nil {
		return nil, err
	}
	var calls []CallSummary
	if err := json.Unmarshal(raw, &calls); err != nil {
		return nil, fmt.Errorf("decode listCalls: %w", err)
	}
	return calls, nil
}

// HangupCall ends a live call the way jambonz actually supports it: a Live
// Call Control POST with call_status=completed, which the api-server relays
// to the feature-server's updateCall handler (DELETE only drops the Redis
// record). 404 is treated as success so cleanup is idempotent. Best-effort:
// jambonz acknowledges with 202 and applies the hangup asynchronously.
func (c *Client) HangupCall(ctx context.Context, callSID string) error {
	err := c.UpdateCall(ctx, callSID, map[string]any{"call_status": "completed"})
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
