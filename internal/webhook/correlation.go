package webhook

import (
	"encoding/json"
	"net/http"
	"strings"
)

// CorrelationHeader is the SIP / HTTP header the harness uses to thread a
// test ID from the outbound call all the way back to the webhook. jambonz
// passes custom SIP headers through as HTTP headers on call_hook.
const CorrelationHeader = "X-Test-Id"

// CorrelationKey is the key we set inside the POST /Calls `tag` field, which
// feature-server emits back to us as `customerData.<CorrelationKey>` on
// every webhook payload. Preferred over CorrelationHeader because api-server
// overrides per-call `call_hook` with the Application's default — URL-based
// correlation never reaches feature-server.
const CorrelationKey = "x_test_id"

// extractTestID pulls the test ID from (in order) header, query param, JSON
// body field "x_test_id" or "X-Test-Id", or SIP sub-object body.
func extractTestID(r *http.Request, body []byte) string {
	if v := r.Header.Get(CorrelationHeader); v != "" {
		return v
	}
	if v := r.URL.Query().Get(CorrelationHeader); v != "" {
		return v
	}
	// action hooks + call_hook payloads carry the INVITE headers under
	// "sip.headers" — inspect them if the body parses as JSON.
	if len(body) == 0 {
		return ""
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return ""
	}
	if v, ok := m[CorrelationHeader].(string); ok && v != "" {
		return v
	}
	if v, ok := m["x_test_id"].(string); ok && v != "" {
		return v
	}
	// jambonz call_hook payload has sip.headers map
	if sip, ok := m["sip"].(map[string]any); ok {
		if hdrs, ok := sip["headers"].(map[string]any); ok {
			if v, ok := hdrs[CorrelationHeader].(string); ok && v != "" {
				return v
			}
			// case-insensitive fallback
			for k, v := range hdrs {
				if strings.EqualFold(k, CorrelationHeader) {
					if s, ok := v.(string); ok {
						return s
					}
				}
			}
		}
	}
	// customerData carries the POST /Calls `tag` field on every webhook
	// payload. This is our primary correlation path.
	if cd, ok := m["customerData"].(map[string]any); ok {
		if v, ok := cd[CorrelationKey].(string); ok && v != "" {
			return v
		}
		if v, ok := cd[CorrelationHeader].(string); ok && v != "" {
			return v
		}
	}
	return ""
}
