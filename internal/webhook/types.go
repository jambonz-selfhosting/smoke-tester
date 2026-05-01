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

// String returns a top-level string field from the decoded body, or "".
// Use NestedString for deeper paths.
func (c Callback) String(field string) string {
	if c.JSON == nil {
		return ""
	}
	if v, ok := c.JSON[field].(string); ok {
		return v
	}
	return ""
}

// Int returns a top-level numeric field as int. Returns 0 if missing or
// non-numeric.
func (c Callback) Int(field string) int {
	if c.JSON == nil {
		return 0
	}
	switch v := c.JSON[field].(type) {
	case float64:
		return int(v)
	case int:
		return v
	}
	return 0
}

// Bool returns a top-level boolean field. Returns false if missing.
func (c Callback) Bool(field string) bool {
	if c.JSON == nil {
		return false
	}
	if v, ok := c.JSON[field].(bool); ok {
		return v
	}
	return false
}

// NestedString descends a dot-separated path through nested objects /
// array indices and returns a string at the end, or "". Use for things
// like `cb.NestedString("speech.alternatives.0.transcript")`.
func (c Callback) NestedString(path string) string {
	v := c.NestedAny(path)
	if v == nil {
		return ""
	}
	if s, ok := v.(string); ok {
		return s
	}
	return ""
}

// NestedAny is the lower-level traversal: returns the value at path or nil
// when any segment is missing / wrong-typed. Numeric segments index into
// arrays; everything else is treated as a map key.
func (c Callback) NestedAny(path string) any {
	if c.JSON == nil || path == "" {
		return nil
	}
	var cur any = map[string]any(c.JSON)
	for _, seg := range splitPath(path) {
		switch typed := cur.(type) {
		case map[string]any:
			cur = typed[seg]
		case []any:
			idx := -1
			for i, ch := range seg {
				if i == 0 && ch == '-' {
					idx = 0
					continue
				}
				if ch < '0' || ch > '9' {
					return nil
				}
				if idx < 0 {
					idx = 0
				}
				idx = idx*10 + int(ch-'0')
			}
			if idx < 0 || idx >= len(typed) {
				return nil
			}
			cur = typed[idx]
		default:
			return nil
		}
		if cur == nil {
			return nil
		}
	}
	return cur
}

// CustomerData returns the body's `customerData` map, or nil. Useful when
// asserting on the correlation round-trip end-to-end.
func (c Callback) CustomerData() map[string]any {
	if c.JSON == nil {
		return nil
	}
	if m, ok := c.JSON["customerData"].(map[string]any); ok {
		return m
	}
	return nil
}

func splitPath(p string) []string {
	out := []string{}
	cur := ""
	for _, ch := range p {
		if ch == '.' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(ch)
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
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
