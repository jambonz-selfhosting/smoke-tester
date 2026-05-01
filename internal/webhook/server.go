package webhook

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
)

// Server hosts the HTTP handler. It does not open its own tunnel — callers
// (usually TestMain) pair it with a Tunnel implementation that exposes it
// publicly. Exported PublicURL is set by the caller after the tunnel comes
// up.
type Server struct {
	Registry  *Registry
	Validator *contract.Validator

	mu        sync.RWMutex
	publicURL string
	staticDir string

	httpSrv  *http.Server
	listener net.Listener
	logger   *slog.Logger
}

// staticHandler serves files from Server.staticDir. Bound to /static/.
// Used so play/dub tests can host their own fixture WAV at the public
// tunnel URL with a known/pinned transcript instead of relying on a
// third-party hosted sample.
type staticHandler struct{ srv *Server }

func (h staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.srv.mu.RLock()
	dir := h.srv.staticDir
	h.srv.mu.RUnlock()
	if dir == "" {
		http.Error(w, "static dir not configured", http.StatusNotFound)
		return
	}
	http.FileServer(http.Dir(dir)).ServeHTTP(w, r)
}

// SetStaticDir registers an absolute filesystem path whose contents are
// exposed under the public URL at /static/<file>. Set this once at
// suite startup if any test needs to host a fixture.
func (s *Server) SetStaticDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staticDir = dir
}

// New constructs a Server bound to a local ephemeral port. Call Serve in a
// goroutine and SetPublicURL once the tunnel is up.
func New(registry *Registry, v *contract.Validator) (*Server, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("webhook listen: %w", err)
	}
	s := &Server{
		Registry:  registry,
		Validator: v,
		listener:  l,
		logger:    slog.Default(),
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/hook", s.handleCallHook)
	mux.HandleFunc("/status", s.handleStatusHook)
	mux.HandleFunc("/action/", s.handleActionHook) // /action/<verb>
	mux.HandleFunc("/ws/", s.handleWS)             // /ws/<session-id>
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok"))
	})
	// /static/<file> serves files from staticDir (set via SetStaticDir).
	// Used by play/dub tests to host fixtures with a pinned transcript at
	// a public URL — without this, jambonz would have to fetch from a
	// third-party server with unknown content.
	mux.Handle("/static/", http.StripPrefix("/static/", staticHandler{srv: s}))
	s.httpSrv = &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// LocalAddr returns the loopback address the server listens on.
func (s *Server) LocalAddr() string { return s.listener.Addr().String() }

// LocalPort returns just the numeric port.
func (s *Server) LocalPort() int {
	_, p, _ := net.SplitHostPort(s.listener.Addr().String())
	n, _ := strconv.Atoi(p)
	return n
}

// Serve blocks until the server is closed.
func (s *Server) Serve() error {
	return s.httpSrv.Serve(s.listener)
}

// Stop gracefully shuts the server down.
func (s *Server) Stop(ctx context.Context) error {
	return s.httpSrv.Shutdown(ctx)
}

// SetPublicURL records the externally-reachable base URL (e.g. ngrok https).
// Callers should set this before any test uses it to provision an Application.
func (s *Server) SetPublicURL(u string) {
	s.mu.Lock()
	s.publicURL = u
	s.mu.Unlock()
}

// PublicURL returns the currently-set public base URL.
func (s *Server) PublicURL() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.publicURL
}

// --- request handlers --------------------------------------------------

func (s *Server) handleCallHook(w http.ResponseWriter, r *http.Request) {
	body := readAll(r)
	testID := extractTestID(r, body)

	s.validateInbound("session-new", body)
	sess := s.sessionFor(testID, extractCallSid(body))
	s.captureCallback(sess, Callback{
		Hook:      "call_hook",
		Transport: TransportHTTP,
		Received:  time.Now(),
		Method:    r.Method,
		Headers:   flattenHeaders(r.Header),
		Body:      body,
		JSON:      decodeJSON(body),
		TestID:    testID,
	})
	out := sess.outcomeForCallHook()
	s.writeOutcome(w, out, "call_hook")
}

func (s *Server) handleStatusHook(w http.ResponseWriter, r *http.Request) {
	body := readAll(r)
	testID := extractTestID(r, body)

	s.validateInbound("call-status", body)
	sess := s.sessionFor(testID, extractCallSid(body))
	s.captureCallback(sess, Callback{
		Hook:      "call_status_hook",
		Transport: TransportHTTP,
		Received:  time.Now(),
		Method:    r.Method,
		Headers:   flattenHeaders(r.Header),
		Body:      body,
		JSON:      decodeJSON(body),
		TestID:    testID,
	})
	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleActionHook(w http.ResponseWriter, r *http.Request) {
	// Path format: /action/<verb>
	verb := r.URL.Path[len("/action/"):]
	if verb == "" {
		http.Error(w, "verb required in path", http.StatusBadRequest)
		return
	}
	body := readAll(r)
	testID := extractTestID(r, body)

	s.validateInbound(verb, body)
	sess := s.sessionFor(testID, extractCallSid(body))
	s.captureCallback(sess, Callback{
		Hook:      "action/" + verb,
		Transport: TransportHTTP,
		Received:  time.Now(),
		Method:    r.Method,
		Headers:   flattenHeaders(r.Header),
		Body:      body,
		JSON:      decodeJSON(body),
		TestID:    testID,
	})
	out := sess.outcomeForActionHook(verb)
	s.writeOutcome(w, out, "action/"+verb)
}

// --- helpers ---

// sessionFor routes an incoming webhook to the right session, in priority
// order:
//  1. Primary: explicit testID (from header / `tag.x_test_id` / SIP header).
//     Bind-side-effect: when both testID and call_sid are present, record
//     the mapping so future hooks for this call route correctly even when
//     correlation is lost (see Registry.BindCallSid).
//  2. Secondary: callSid → previously-bound testID. Covers `tag` verb
//     (replaces customerData), transcribe's transcriptionHook, and other
//     hooks that drop our correlation key.
//  3. Fallback: shared `_anon` session, with a warning.
//
// callSid is the "call_sid" field at the top level of the webhook body
// (every jambonz hook for an active call carries this).
func (s *Server) sessionFor(testID, callSid string) *Session {
	if testID != "" {
		if sess, ok := s.Registry.Lookup(testID); ok {
			s.Registry.BindCallSid(callSid, testID)
			return sess
		}
		s.logger.Warn("webhook: unknown test id", "test_id", testID)
		sess := s.Registry.New(testID)
		s.Registry.BindCallSid(callSid, testID)
		return sess
	}
	// No testID — try call_sid lookup.
	if sess, ok := s.Registry.LookupByCallSid(callSid); ok {
		return sess
	}
	// Last resort: anon session. Log loud because under parallel runs anon
	// is a shared bag and contention there causes flaky drains.
	s.logger.Warn("webhook: no correlation; using anon session", "call_sid", callSid)
	return s.Registry.New("_anon")
}

// extractCallSid pulls the top-level call_sid string from a JSON body.
// Returns "" if absent or body isn't JSON.
func extractCallSid(body []byte) string {
	m := decodeJSON(body)
	if m == nil {
		return ""
	}
	if s, ok := m["call_sid"].(string); ok {
		return s
	}
	return ""
}

func (s *Server) captureCallback(sess *Session, cb Callback) {
	sess.record(cb)
}

func (s *Server) writeOutcome(w http.ResponseWriter, out HookOutcome, hookLabel string) {
	if len(out.Body) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatusOf(out))
		_, _ = w.Write(out.Body)
		return
	}
	if out.Verbs != nil {
		b, err := json.Marshal(out.Verbs)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// TODO: outbound verb-array contract validation. The
		// jambonz-app.schema.json we vendored uses absolute URL $refs (e.g.
		// https://jambonz.org/schema/verbs/answer) which santhosh-tekuri
		// can't resolve from disk without a custom Loader. Skipping for
		// now; individual verb schemas are still useful for per-verb checks
		// when we author tests that return a single-verb response.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(httpStatusOf(out))
		_, _ = w.Write(b)
		return
	}
	// Default empty 200
	w.WriteHeader(httpStatusOf(out))
}

// validateInbound looks up schemas/callbacks/<name>.schema.json and
// validates body against it. No schema for that name = debug log (expected
// for hooks we haven't authored yet); real violation = error log.
func (s *Server) validateInbound(name string, body []byte) {
	if s.Validator == nil || len(body) == 0 {
		return
	}
	rel := "callbacks/" + name + ".schema.json"
	err := s.Validator.ValidateResponse(rel, body)
	if err == nil {
		return
	}
	if errors.Is(err, contract.ErrNoSchema) {
		s.logger.Debug("webhook: no schema for inbound", "hook", name)
		return
	}
	// Vendored @jambonz/schema uses absolute URL $refs — santhosh-tekuri
	// needs a custom Loader to resolve them. Treat as "no schema" for now.
	if strings.Contains(err.Error(), "no Loader found") ||
		strings.Contains(err.Error(), "compilation failed") {
		s.logger.Debug("webhook: schema uses URL $refs; skipping", "hook", name)
		return
	}
	s.logger.Error("webhook: inbound failed contract", "hook", name, "err", err)
}

func readAll(r *http.Request) []byte {
	if r.Body == nil {
		return nil
	}
	defer r.Body.Close()
	b, _ := io.ReadAll(r.Body)
	return b
}

func decodeJSON(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return nil
	}
	return m
}

func flattenHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		if len(vs) > 0 {
			out[k] = vs[0]
		}
	}
	return out
}
