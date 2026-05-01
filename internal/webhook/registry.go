package webhook

import (
	"context"
	"fmt"
	"sync"
	"time"
)

// Registry owns per-test script registration and per-test callback capture.
// Tests register a "session" for a correlation ID, push verb scripts onto it,
// and read back captured callbacks.
//
// callSidIndex provides secondary correlation: when a webhook payload arrives
// without our x_test_id (e.g. jambonz's `tag` verb replaces customerData,
// transcribe's transcriptionHook drops it), we look up the call_sid in the
// payload and route to the session that originally owned it. The mapping is
// populated lazily — the first hook that DOES carry x_test_id (typically
// call_hook) records its call_sid → session.id binding, so all subsequent
// hooks for the same call land in the right session even after correlation
// is lost. This makes parallel runs safe; without it, t.Parallel() races on
// the shared `_anon` session.
type Registry struct {
	mu           sync.Mutex
	sessions     map[string]*Session
	callSidIndex map[string]string // call_sid → testID
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{
		sessions:     map[string]*Session{},
		callSidIndex: map[string]string{},
	}
}

// BindCallSid records that callSid belongs to testID. Idempotent. Used by
// the webhook server when it sees a call_hook with both x_test_id and a
// call_sid — that's the binding moment.
func (r *Registry) BindCallSid(callSid, testID string) {
	if callSid == "" || testID == "" || testID == "_anon" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.callSidIndex[callSid] = testID
}

// LookupByCallSid returns the session whose test owns callSid, or (nil, false).
func (r *Registry) LookupByCallSid(callSid string) (*Session, bool) {
	if callSid == "" {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	testID, ok := r.callSidIndex[callSid]
	if !ok {
		return nil, false
	}
	s, ok := r.sessions[testID]
	return s, ok
}

// New registers (or returns existing) a Session under testID.
func (r *Registry) New(testID string) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[testID]; ok {
		return s
	}
	s := &Session{
		id:          testID,
		callHook:    nil,
		actionHooks: map[string]HookOutcome{},
		pending:     make(chan Callback, 32),
	}
	r.sessions[testID] = s
	return s
}

// Release removes a session. Tests typically call this in t.Cleanup.
func (r *Registry) Release(testID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.sessions, testID)
}

// Lookup returns the session for testID or (nil, false).
func (r *Registry) Lookup(testID string) (*Session, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[testID]
	return s, ok
}

// Session is the per-test handle into the Registry.
type Session struct {
	id string

	mu          sync.Mutex
	callHook    *HookOutcome           // returned by the next call_hook hit
	actionHooks map[string]HookOutcome // keyed by verb name (e.g. "gather", "dial")
	pending     chan Callback          // incoming callbacks queued for WaitCallback
	errs        []error
	ws          *wsSession // populated when jambonz opens a WS to /ws/<id>
}

func (s *Session) ID() string { return s.id }

// ScriptCallHook sets the verb array the server returns when jambonz calls
// /hook with this session's test id.
func (s *Session) ScriptCallHook(verbs Script) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.callHook = &HookOutcome{Verbs: verbs}
	return s
}

// ScriptActionHook sets the verb array the server returns when jambonz calls
// the action hook for verb `verbName` (e.g. "gather", "dial"). Use an empty
// array to simply acknowledge without chaining more verbs.
func (s *Session) ScriptActionHook(verbName string, verbs Script) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionHooks[verbName] = HookOutcome{Verbs: verbs}
	return s
}

// ScriptActionHookBody sets a raw JSON body the server returns for the named
// action hook, overriding the default verb-array response. Used by hooks
// that expect a JSON object rather than a verb array — most notably the
// agent verb's toolHook, where the body becomes the tool's return value
// piped back to the LLM (see feature-server lib/tasks/agent/state-machine.js
// `_onToolCall`).
func (s *Session) ScriptActionHookBody(verbName string, body []byte) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.actionHooks[verbName] = HookOutcome{Status: 200, Body: body}
	return s
}

// outcomeForCallHook returns what the server should reply to a call_hook
// request for this session.
func (s *Session) outcomeForCallHook() HookOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.callHook == nil {
		// Default: hang up immediately. Keeps jambonz from hanging if a
		// test forgot to register a script.
		return HookOutcome{Verbs: Script{map[string]any{"verb": "hangup"}}}
	}
	return *s.callHook
}

// outcomeForActionHook returns what the server should reply to an action
// hook for verbName.
func (s *Session) outcomeForActionHook(verbName string) HookOutcome {
	s.mu.Lock()
	defer s.mu.Unlock()
	if o, ok := s.actionHooks[verbName]; ok {
		return o
	}
	// No explicit action-hook script — acknowledge with empty response.
	return HookOutcome{Status: 200, Body: []byte("[]")}
}

// record queues a callback for WaitCallback.
func (s *Session) record(cb Callback) {
	select {
	case s.pending <- cb:
	default:
		// Never block the webhook handler — drop with an error flag so
		// tests see a warning rather than a hung server.
		s.mu.Lock()
		s.errs = append(s.errs, fmt.Errorf("callback queue full, dropped %s", cb.Hook))
		s.mu.Unlock()
	}
}

// WaitCallback returns the next captured callback or ctx.Err() on timeout.
func (s *Session) WaitCallback(ctx context.Context) (Callback, error) {
	select {
	case <-ctx.Done():
		return Callback{}, ctx.Err()
	case cb := <-s.pending:
		return cb, nil
	}
}

// WaitCallbackFor returns the next callback matching hook, skipping others.
// Other callbacks are *not* re-queued — if you need them, drain first.
func (s *Session) WaitCallbackFor(ctx context.Context, hook string) (Callback, error) {
	deadline, hasDeadline := ctx.Deadline()
	for {
		remaining := 30 * time.Second
		if hasDeadline {
			remaining = time.Until(deadline)
			if remaining <= 0 {
				return Callback{}, ctx.Err()
			}
		}
		innerCtx, cancel := context.WithTimeout(ctx, remaining)
		cb, err := s.WaitCallback(innerCtx)
		cancel()
		if err != nil {
			return Callback{}, err
		}
		if cb.Hook == hook {
			return cb, nil
		}
	}
}

// Errors returns accumulated non-fatal errors (e.g. dropped callbacks).
func (s *Session) Errors() []error {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]error, len(s.errs))
	copy(out, s.errs)
	return out
}
