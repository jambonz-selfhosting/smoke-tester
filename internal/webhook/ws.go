// Generic WebSocket transport for jambonz-initiated WS connections.
//
// Several jambonz features open a WS to a URL we provide:
//   - `listen` / `stream` verbs: stream caller RTP as binary frames.
//   - `llm` / `agent` / `*_s2s` verbs: bidirectional model traffic.
//   - The AsyncAPI ("jambonz WebSocket API"): jambonz sends verb-script
//     messages over WS instead of HTTP POST.
//
// All of these share the same shape — jambonz → GET upgrade at a URL we
// advertise, with some opening JSON metadata and then mixed text/binary
// traffic for the lifetime of the session. We keep one transport
// implementation and let callers interpret frames however they need.
//
// The endpoint we expose is:   (GET) /ws/<session-id>
// Routes by session-id so multiple tests can run side by side, each
// owning its own stream.
package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

// WSMessage is one frame received over the WS. Kind distinguishes text
// (JSON / protocol messages) from binary (typically audio). Exactly one
// of Text / Binary is populated.
type WSMessage struct {
	Kind     WSKind
	Received time.Time
	Text     string // when Kind == WSText
	Binary   []byte // when Kind == WSBinary
	// JSON is eagerly decoded when Kind == WSText and the body parses as
	// a JSON object; empty otherwise. Frees callers from re-unmarshaling.
	JSON map[string]any
}

// WSKind — the two types of frame jambonz sends on a control/audio WS.
type WSKind int

const (
	WSText WSKind = iota
	WSBinary
)

// wsSession is the per-Session WS state. It buffers inbound frames so
// tests that connect before opening the call don't miss early frames,
// and exposes read helpers on Session.
type wsSession struct {
	mu sync.Mutex

	// conn is set once jambonz upgrades. Writes are serialized by mu.
	conn *websocket.Conn

	// inbound is a buffered queue of frames received so far. Tests can
	// either drain one at a time (WaitWSMessage) or collect-until-close
	// (CollectWS).
	inbound chan WSMessage

	// metadata captures the first JSON text frame jambonz sends — useful
	// because many verb types put session params there.
	metadata map[string]any

	closed   bool
	closedCh chan struct{}
}

func newWSSession() *wsSession {
	return &wsSession{
		inbound:  make(chan WSMessage, 256),
		closedCh: make(chan struct{}),
	}
}

// --- Session public API -----------------------------------------------

// WSClosed returns a channel that closes when the WS session ends (either
// jambonz hung up or an IO error terminated the read loop).
func (s *Session) WSClosed() <-chan struct{} {
	s.mu.Lock()
	if s.ws == nil {
		s.ws = newWSSession()
	}
	ch := s.ws.closedCh
	s.mu.Unlock()
	return ch
}

// WSMetadata returns the first JSON frame jambonz sent on the WS (if
// any). Many verb shapes put configuration / session context there.
// Returns nil if no JSON frame has been received yet or the first frame
// was binary.
func (s *Session) WSMetadata() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ws == nil {
		return nil
	}
	s.ws.mu.Lock()
	defer s.ws.mu.Unlock()
	out := make(map[string]any, len(s.ws.metadata))
	for k, v := range s.ws.metadata {
		out[k] = v
	}
	return out
}

// WaitWSMessage blocks for the next WS frame. Returns ctx.Err() on
// timeout; returns a zero-value WSMessage + error if the WS closed
// before another frame arrived.
func (s *Session) WaitWSMessage(ctx context.Context) (WSMessage, error) {
	s.mu.Lock()
	if s.ws == nil {
		s.ws = newWSSession()
	}
	ws := s.ws
	s.mu.Unlock()
	select {
	case m, ok := <-ws.inbound:
		if !ok {
			return WSMessage{}, errors.New("WS: stream closed")
		}
		return m, nil
	case <-ctx.Done():
		return WSMessage{}, ctx.Err()
	}
}

// CollectWS drains every remaining frame until the WS closes or ctx
// fires. Use at end-of-test to get everything jambonz sent in one pass.
func (s *Session) CollectWS(ctx context.Context) []WSMessage {
	s.mu.Lock()
	if s.ws == nil {
		s.ws = newWSSession()
	}
	ws := s.ws
	s.mu.Unlock()
	var out []WSMessage
	for {
		select {
		case m, ok := <-ws.inbound:
			if !ok {
				return out
			}
			out = append(out, m)
		case <-ws.closedCh:
			// Drain anything queued after close before returning.
			for {
				select {
				case m, ok := <-ws.inbound:
					if !ok {
						return out
					}
					out = append(out, m)
				default:
					return out
				}
			}
		case <-ctx.Done():
			return out
		}
	}
}

// BinaryConcat concatenates the Binary payloads of messages whose Kind
// is WSBinary. Helper for audio-capture tests that ultimately want one
// big byte slice.
func BinaryConcat(msgs []WSMessage) []byte {
	var n int
	for _, m := range msgs {
		if m.Kind == WSBinary {
			n += len(m.Binary)
		}
	}
	out := make([]byte, 0, n)
	for _, m := range msgs {
		if m.Kind == WSBinary {
			out = append(out, m.Binary...)
		}
	}
	return out
}

// SendWSText writes a UTF-8 text frame to jambonz. Typically used to
// reply on bidirectional protocols (verb scripts over WS, LLM turns,
// etc.). Returns an error if the WS isn't connected yet or has closed.
func (s *Session) SendWSText(b []byte) error {
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return errors.New("WS: not connected")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn == nil {
		return errors.New("WS: not connected")
	}
	return ws.conn.WriteMessage(websocket.TextMessage, b)
}

// SendWSBinary writes a binary frame to jambonz (e.g. audio reply on
// bidirectional listen).
func (s *Session) SendWSBinary(b []byte) error {
	s.mu.Lock()
	ws := s.ws
	s.mu.Unlock()
	if ws == nil {
		return errors.New("WS: not connected")
	}
	ws.mu.Lock()
	defer ws.mu.Unlock()
	if ws.conn == nil {
		return errors.New("WS: not connected")
	}
	return ws.conn.WriteMessage(websocket.BinaryMessage, b)
}

// --- server-side handler ----------------------------------------------

var wsUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	CheckOrigin:     func(*http.Request) bool { return true },
}

// handleWS upgrades GET /ws/<session-id>, attaches the connection to the
// session, and reads frames into the session's inbound queue until the
// WS closes. This handler is deliberately agnostic to content: callers
// interpret frames via the Session helpers above.
func (s *Server) handleWS(w http.ResponseWriter, r *http.Request) {
	sessID := strings.TrimPrefix(r.URL.Path, "/ws/")
	if sessID == "" {
		http.Error(w, "session id required in /ws/<id>", http.StatusBadRequest)
		return
	}
	sess, ok := s.Registry.Lookup(sessID)
	if !ok {
		s.logger.Warn("webhook: WS for unknown session; using anon", "id", sessID)
		sess = s.Registry.New("_anon")
	}
	conn, err := wsUpgrader.Upgrade(w, r, nil)
	if err != nil {
		s.logger.Error("webhook: WS upgrade failed", "err", err)
		return
	}
	defer conn.Close()

	sess.mu.Lock()
	if sess.ws == nil {
		sess.ws = newWSSession()
	}
	ws := sess.ws
	sess.mu.Unlock()

	ws.mu.Lock()
	ws.conn = conn
	ws.mu.Unlock()

	defer func() {
		ws.mu.Lock()
		if !ws.closed {
			ws.closed = true
			close(ws.closedCh)
			close(ws.inbound)
		}
		ws.mu.Unlock()
	}()

	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			s.logger.Debug("webhook: WS read ended", "id", sessID, "err", err)
			return
		}
		msg := WSMessage{Received: time.Now()}
		switch mt {
		case websocket.TextMessage:
			msg.Kind = WSText
			msg.Text = string(data)
			var j map[string]any
			if json.Unmarshal(data, &j) == nil {
				msg.JSON = j
				// First text frame often carries session metadata; stash.
				ws.mu.Lock()
				if ws.metadata == nil {
					ws.metadata = j
				}
				ws.mu.Unlock()
			}
		case websocket.BinaryMessage:
			msg.Kind = WSBinary
			// Copy — the buffer reused by gorilla/ws after this frame.
			cp := make([]byte, len(data))
			copy(cp, data)
			msg.Binary = cp
		default:
			// Ping/pong/close handled by gorilla internally; ignore other
			// message types.
			continue
		}
		select {
		case ws.inbound <- msg:
		default:
			s.logger.Warn("webhook: WS inbound queue full; dropping frame", "id", sessID)
		}
	}
}
