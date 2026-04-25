// Package webhook hosts an HTTP + WebSocket server, exposes it publicly via
// an ngrok tunnel, and routes jambonz callbacks to the test currently in
// flight. Every inbound payload is validated against schemas/callbacks/*;
// every outbound verb array is validated against schemas/jambonz-app.
//
// See ADR-0015 (contract testing) and schemas/callbacks/ for the shapes.
package webhook

import (
	"encoding/json"
	"net/http"
	"time"
)

// Transport identifies which webhook protocol an Application is configured
// for. jambonz supports either; tests run both by default.
type Transport string

const (
	TransportHTTP Transport = "http"
	TransportWS   Transport = "ws"
)

// Script is the verb array a test wants the server to return when jambonz
// calls its `call_hook`. The value must validate against the root
// jambonz-app schema.
type Script []any

// Callback captures one incoming webhook hit (request + decoded body).
type Callback struct {
	Hook       string            // "call_hook" / "call_status_hook" / "action/<verb>" / "tool_hook" / "auth_hook"
	Transport  Transport         // http or ws
	Received   time.Time
	Method     string            // http method or ws message kind ("verb-response"/"event"/etc.)
	Headers    map[string]string
	Body       json.RawMessage
	JSON       map[string]any    // decoded body, best-effort
	TestID     string            // X-Test-Id resolved from header/query/body
	Transport_ Transport         // reserved for future SP-vs-account routing
}

// HookOutcome says what the test wants the server to return for a specific
// hook invocation. Scripts get turned into this by the registry when a
// call_hook/action_hook is handled.
type HookOutcome struct {
	Status int   // HTTP status code; default 200
	Verbs  Script // verb array written back as response body (if non-nil)
	Body   []byte // raw body override (wins over Verbs if set)
}

// ContextWithError is used internally when a handler encounters an
// unrecoverable error and wants to surface it to the test's Wait* methods.
type hookError struct {
	TestID string
	Err    error
}

// httpStatus helper: 200 by default, explicit codes for 4xx/5xx scenarios.
func httpStatusOf(o HookOutcome) int {
	if o.Status == 0 {
		return http.StatusOK
	}
	return o.Status
}
