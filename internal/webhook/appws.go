// jambonz WebSocket **application** protocol (the "AsyncAPI").
//
// This is distinct from the generic /ws/<id> capture transport in ws.go.
// That one is a passive sink for listen/stream/llm sub-connections jambonz
// dials out to mid-call. THIS one is the top-level application protocol:
// jambonz fetches the entire verb script over a socket instead of HTTP POST,
// which is what turns on feature-server's `appIsUsingWebsockets`. Some verbs
// only work in that mode — notably `say` with `stream: true`
// (feature-server lib/tasks/say.js: "streaming say verb requires
// applications to use the websocket API").
//
// Endpoint:  (GET) /appws/<testID>
// The Application's call_hook is provisioned as wss://<public>/appws/<id>,
// so feature-server builds a WsRequestor (lib/middleware.js) and drives the
// whole call over this socket.
//
// Wire protocol (mirrors feature-server lib/utils/ws-requestor.js):
//   - jambonz connects with WS subprotocol "ws.jambonz.org"; we MUST echo it
//     in the handshake or the upgrade is rejected.
//   - jambonz → us:  {"type":"session:new","msgid":"..","call_sid":"..","data":{..}}
//                    {"type":"verb:status", ...}   (no ack expected)
//                    {"type":"call:status",  ...}  (no ack expected)
//     …and other MTYPE_WANTS_ACK types that do NOT want an ack.
//   - us → jambonz:  {"type":"ack","msgid":"<their msgid>","data":<verb array>}
//     The ack to session:new carries the verb script in `data` — the same
//     Script a test registers via Session.ScriptCallHook. Later hooks
//     (verb:hook for a queued action) are acked with the matching
//     action-hook script.
//
// Only session:new / verb:hook require a reply in the flows we drive today;
// everything else is captured for assertions and acked when jambonz set a
// msgid (harmless to ack an unknown one — jambonz discards it).
package webhook

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"
)

// jambonzWSSubprotocol is the WS subprotocol feature-server's WsRequestor
// opens with (lib/utils/ws-requestor.js: `new Websocket(url, ['ws.jambonz.org'])`).
// The upgrader must offer it back or gorilla returns no negotiated protocol
// and the ws client rejects the handshake.
const jambonzWSSubprotocol = "ws.jambonz.org"

// appWSUpgrader echoes the jambonz subprotocol.
var appWSUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
	Subprotocols:    []string{jambonzWSSubprotocol},
}

// inboundAppMsg is the shape jambonz sends us on the app socket. Only the
// fields we route on are decoded; `data` stays raw for capture.
type inboundAppMsg struct {
	Type    string          `json:"type"`
	Msgid   string          `json:"msgid"`
	CallSid string          `json:"call_sid"`
	Hook    string          `json:"hook"`
	Data    json.RawMessage `json:"data"`
}

// handleAppWS upgrades GET /appws/<testID> and speaks the jambonz app
// protocol: on session:new it replies with the session's call-hook verb
// script; on a verb:hook it replies with the matching action-hook script.
// Every inbound message is also captured as a Callback so tests can assert
// on it exactly like an HTTP hook.
func (s *Server) handleAppWS(w http.ResponseWriter, r *http.Request) {
	testID := strings.TrimPrefix(r.URL.Path, "/appws/")
	if testID == "" {
		http.Error(w, "test id required in /appws/<id>", http.StatusBadRequest)
		return
	}
	// A single shared WS Application serves every WS-driven test, so its
	// wss:// call_hook path can't carry a per-test id. The real testID
	// arrives inside session:new (data.customerData.x_test_id, set from the
	// POST /Calls `tag`). Until we've seen it we route to a placeholder
	// session; once resolved we switch to the right one. A per-test URL
	// path (/appws/<realID>) also works and short-circuits this.
	conn, err := appWSUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("webhook: appWS upgrade failed", "err", err)
		return
	}
	defer conn.Close()
	s.logger.Debug("webhook: appWS connected", "path_id", testID, "subproto", conn.Subprotocol())

	var sess *Session // resolved lazily on the first frame that names a test

	for {
		mt, raw, err := conn.ReadMessage()
		if err != nil {
			s.logger.Debug("webhook: appWS read ended", "id", testID, "err", err)
			return
		}
		if mt != websocket.TextMessage {
			// The app protocol is JSON text only. jambonz treats an inbound
			// binary frame from the app as a malicious client and closes;
			// we simply ignore non-text here.
			continue
		}
		var msg inboundAppMsg
		if err := json.Unmarshal(raw, &msg); err != nil {
			s.logger.Warn("webhook: appWS non-JSON frame", "id", testID, "err", err)
			continue
		}

		// Resolve the session for this connection. Prefer an explicit path
		// id; otherwise the x_test_id carried in the frame; otherwise fall
		// back to the call_sid binding, then a placeholder.
		if sess == nil {
			sess = s.resolveAppWSSession(testID, msg)
		}

		// Bind call_sid → testID on the first message that carries it, so
		// any callSid-keyed assertions resolve (parity with HTTP hooks).
		if msg.CallSid != "" {
			s.Registry.BindCallSid(msg.CallSid, sess.ID())
		}

		// Capture every inbound app message as a Callback for assertions.
		s.captureCallback(sess, Callback{
			Hook:      appHookLabel(msg.Type),
			Transport: TransportWS,
			Received:  time.Now(),
			Method:    "WS",
			Body:      raw,
			JSON:      decodeJSON(raw),
			TestID:    testID,
		})

		// Reply where the protocol expects a verb script.
		switch msg.Type {
		case "session:new":
			// Note: no schema validation here — the WS envelope
			// ({type,msgid,call_sid,data}) differs from the HTTP session-new
			// callback body, so the callbacks/session-new schema doesn't
			// apply to this frame. The verb-array we return is the contract
			// that matters, and that's shared with the HTTP path.
			out := sess.outcomeForCallHook()
			s.ackVerbs(conn, testID, msg.Msgid, out)
		case "verb:hook":
			// A queued action hook (e.g. gather/dial) arriving over WS. The
			// hook URL path tells us which verb's action script to return.
			verb := verbFromHookURL(msg.Hook)
			out := sess.outcomeForActionHook(verb)
			s.ackVerbs(conn, testID, msg.Msgid, out)
		default:
			// call:status / verb:status / jambonz:error etc. These don't
			// want an ack (feature-server's MTYPE_WANTS_ACK). Nothing to do.
		}
	}
}

// resolveAppWSSession picks the Session for a WS connection. Priority:
//  1. explicit path id (/appws/<realID>) that already has a session
//  2. x_test_id carried in the frame's data.customerData (the POST /Calls tag)
//  3. call_sid previously bound to a test
//  4. a placeholder session under the path id (last resort)
func (s *Server) resolveAppWSSession(pathID string, msg inboundAppMsg) *Session {
	if pathID != "" && pathID != wsSharedPathID {
		if sess, ok := s.Registry.Lookup(pathID); ok {
			return sess
		}
	}
	if id := testIDFromAppData(msg.Data); id != "" {
		if sess, ok := s.Registry.Lookup(id); ok {
			return sess
		}
		s.logger.Warn("webhook: appWS x_test_id has no session; creating", "id", id)
		return s.Registry.New(id)
	}
	if msg.CallSid != "" {
		if sess, ok := s.Registry.LookupByCallSid(msg.CallSid); ok {
			return sess
		}
	}
	s.logger.Warn("webhook: appWS no correlation; using path session", "path_id", pathID)
	return s.Registry.New(pathID)
}

// testIDFromAppData digs the x_test_id out of a session:new `data` payload.
// feature-server snake_cases the POST /Calls `tag` into
// data.customer_data.<CorrelationKey> (customerData is preserved as-is by
// snakeCaseKeys, so the key stays "x_test_id"). We check both the
// snake_cased and camelCased container names defensively.
func testIDFromAppData(data json.RawMessage) string {
	if len(data) == 0 {
		return ""
	}
	var d map[string]any
	if json.Unmarshal(data, &d) != nil {
		return ""
	}
	for _, container := range []string{"customerData", "customer_data"} {
		if cd, ok := d[container].(map[string]any); ok {
			if v, ok := cd[CorrelationKey].(string); ok && v != "" {
				return v
			}
		}
	}
	// Some payloads surface the tag at the top level too.
	if v, ok := d[CorrelationKey].(string); ok && v != "" {
		return v
	}
	return ""
}

// wsSharedPathID is the path segment used by the single shared WS
// Application (/appws/<wsSharedPathID>). It's a sentinel, not a real test —
// the actual test is resolved from the frame's x_test_id.
const wsSharedPathID = "shared"

// ackVerbs writes an {type:ack, msgid, data:<verbs>} reply. The verb script
// goes in `data` — that is exactly what feature-server's WsRequestor
// _recvAck delivers to the session as the new verb list.
func (s *Server) ackVerbs(conn *websocket.Conn, testID, msgid string, out HookOutcome) {
	var data any
	switch {
	case len(out.Body) > 0:
		// Raw body override — decode so it rides as JSON, not a string.
		var v any
		if json.Unmarshal(out.Body, &v) == nil {
			data = v
		} else {
			data = out.Body
		}
	case out.Verbs != nil:
		data = out.Verbs
	default:
		data = Script{}
	}
	ack := map[string]any{"type": "ack", "msgid": msgid, "data": data}
	b, err := json.Marshal(ack)
	if err != nil {
		s.logger.Error("webhook: appWS marshal ack failed", "id", testID, "err", err)
		return
	}
	if err := conn.WriteMessage(websocket.TextMessage, b); err != nil {
		s.logger.Debug("webhook: appWS write ack failed", "id", testID, "err", err)
	}
}

// appHookLabel maps a jambonz app-message type to the Hook label used on
// captured Callbacks, so WS tests read the same way as HTTP tests
// (session:new → "call_hook", matching handleCallHook).
func appHookLabel(msgType string) string {
	switch msgType {
	case "session:new":
		return "call_hook"
	case "call:status":
		return "call_status_hook"
	case "verb:hook":
		return "verb_hook"
	default:
		return msgType
	}
}

// verbFromHookURL pulls the verb name from an action-hook URL of the form
// ".../action/<verb>". Returns "" if the shape doesn't match.
func verbFromHookURL(hookURL string) string {
	i := strings.Index(hookURL, "/action/")
	if i < 0 {
		return ""
	}
	rest := hookURL[i+len("/action/"):]
	if q := strings.IndexAny(rest, "?#"); q >= 0 {
		rest = rest[:q]
	}
	return rest
}
