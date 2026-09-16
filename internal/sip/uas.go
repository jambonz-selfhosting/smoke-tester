// Package sip wraps sipgo + diago as a test-friendly SIP stack with
// symmetrical UAC + UAS control. Tests drive Calls step-by-step: Trying,
// Ringing, Answer, Reject, Hangup, and observe every SIP message + media
// frame that came and went.
package sip

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Config drives SIP stack construction.
type Config struct {
	SIPDomain string // e.g. "sip.jambonz.me"
	User      string // e.g. "caller-uas"
	Pass      string
	Transport string // "tcp" (default) or "udp"
	LogLevel  string // "info" | "debug"

	// Owner names the test that owns this stack (t.Name()). Every *Call
	// born on the stack — inbound via dispatch, outbound via Invite —
	// inherits it, so per-leg recording archives (ADR-0016) know which
	// test a recording belongs to without any per-test wiring. Empty is
	// fine: archiving is skipped for ownerless calls.
	Owner string

	// Resolver, if non-nil, replaces the default *net.Resolver in sipgo's
	// transport layer. Use this to make synthetic SIP realms (no real DNS)
	// resolve to the cluster's SBC public IP. See
	// internal/sip/resolver.go's StaticResolver.
	Resolver *net.Resolver

	// TLSBindPort, when non-zero, adds a SIP-over-TLS listener on
	// TLSBindHost:TLSBindPort using a generated self-signed certificate (in
	// addition to the ephemeral tcp/udp transports). Used by the SRTP/TLS
	// offer test, which fronts this listener with an ngrok TCP tunnel so the
	// cluster can reach it through NAT and the harness can inspect the SDP
	// offer on the received INVITE.
	TLSBindHost string
	TLSBindPort int
}

// InboundHandler is invoked synchronously for every incoming INVITE. The
// handler owns the call's lifetime: it must drive the state (Trying/Ringing/
// Answer/Reject) and eventually Hangup. When the handler returns, the Call
// is torn down.
type InboundHandler func(ctx context.Context, call *Call) error

// Stack is a sipgo UA + diago transaction user that can act as both UAS and
// UAC. Use Start to construct.
type Stack struct {
	cfg Config
	ua  *sipgo.UserAgent
	dg  *diago.Diago

	ctx    context.Context
	cancel context.CancelFunc

	// regTx is the live REGISTER transaction. Held so Stop() can send the
	// de-REGISTER over the *same* connection (and thus same local source
	// port) the registration was created on, before the serve context is
	// cancelled and the connection torn down. Deregistering after cancel
	// would dial a fresh ephemeral port the registrar can't match to the
	// existing binding -> 403 Forbidden.
	regTx    *diago.RegisterTransaction
	stopOnce sync.Once

	handlerMu sync.RWMutex
	handler   InboundHandler

	// calls tracks every Call born on this stack (UAC via Invite, UAS via
	// dispatchInbound) so Stop can drain any that are still live.
	callsMu sync.Mutex
	calls   []*Call
}

// Start constructs a Stack, registers (if SIPDomain/User/Pass are set), and
// serves incoming calls in the background via the provided handler.
//
// Pass handler = nil for outbound-only usage; inbound INVITEs are then
// rejected with 480 Temporarily Unavailable.
func Start(ctx context.Context, cfg Config, handler InboundHandler) (*Stack, error) {
	if cfg.Transport == "" {
		cfg.Transport = "tcp"
	}
	if cfg.LogLevel == "debug" {
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})))
	}

	uaOpts := []sipgo.UserAgentOption{
		sipgo.WithUserAgent(nonEmpty(cfg.User, "jambonz-it")),
	}
	if cfg.Resolver != nil {
		uaOpts = append(uaOpts, sipgo.WithUserAgentDNSResolver(cfg.Resolver))
	}
	ua, err := sipgo.NewUA(uaOpts...)
	if err != nil {
		return nil, fmt.Errorf("sipgo NewUA: %w", err)
	}

	opts := []diago.DiagoOption{
		diago.WithTransport(diago.Transport{Transport: "tcp", BindHost: "0.0.0.0", BindPort: 0}),
		diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "0.0.0.0", BindPort: 0}),
	}
	if cfg.TLSBindPort != 0 {
		tlsConf, err := selfSignedTLSConfig(cfg.TLSBindHost)
		if err != nil {
			return nil, err
		}
		opts = append(opts, diago.WithTransport(diago.Transport{
			Transport: "tls",
			BindHost:  nonEmpty(cfg.TLSBindHost, "0.0.0.0"),
			BindPort:  cfg.TLSBindPort,
			TLSConf:   tlsConf,
		}))
	}
	opts = append(opts, diago.WithServerRequestMiddleware(observeRequestMiddleware))
	dg := diago.NewDiago(ua, opts...)

	serveCtx, cancel := context.WithCancel(ctx)
	s := &Stack{
		cfg:     cfg,
		ua:      ua,
		dg:      dg,
		ctx:     serveCtx,
		cancel:  cancel,
		handler: handler,
	}

	// Serve incoming calls.
	go func() {
		_ = dg.Serve(serveCtx, s.dispatchInbound)
	}()

	// REGISTER if credentials were provided.
	if cfg.SIPDomain != "" && cfg.User != "" && cfg.Pass != "" {
		if err := s.register(); err != nil {
			cancel()
			return nil, err
		}
	}
	return s, nil
}

// SetHandler replaces the inbound handler. Useful for per-test routing.
func (s *Stack) SetHandler(h InboundHandler) {
	s.handlerMu.Lock()
	s.handler = h
	s.handlerMu.Unlock()
}

// track records a live call on this stack so Stop can drain it.
func (s *Stack) track(c *Call) {
	if c == nil {
		return
	}
	s.callsMu.Lock()
	s.calls = append(s.calls, c)
	s.callsMu.Unlock()
}

// liveCalls snapshots the calls that have not reached a terminal state.
func (s *Stack) liveCalls() []drainable {
	s.callsMu.Lock()
	calls := make([]*Call, len(s.calls))
	copy(calls, s.calls)
	s.callsMu.Unlock()

	live := make([]drainable, 0, len(calls))
	for _, c := range calls {
		select {
		case <-c.Done():
			// already ended; nothing to drain
		default:
			live = append(live, c)
		}
	}
	return live
}

// Stop drains any calls still live on this stack, deregisters (over the live
// registration connection), then cancels the serve loop and closes the UA.
// Safe to call more than once.
//
// Order matters: the drain must run BEFORE the de-REGISTER, because sending
// a BYE for a live dialog needs the same live transport the de-REGISTER
// needs — draining after would race the connection teardown below. The
// de-REGISTER itself must go out BEFORE s.cancel() tears down the transport,
// so sipgo reuses the same TCP connection — and thus the same local source
// port — that the original REGISTER created. The registrar matches the
// de-REGISTER to that binding only when the source port matches;
// deregistering on a fresh ephemeral port gets a 403.
func (s *Stack) Stop() {
	s.stopOnce.Do(func() {
		// Drain first: a test that hit t.Fatalf mid-call, or whose callee
		// goroutine abandoned a leg, never sends BYE. Without this, jambonz's
		// inbound BYE arrives at a UA we've already closed and the session
		// leaks server-side.
		if live := s.liveCalls(); len(live) > 0 {
			slog.Debug("sip: draining live calls at stack stop", "user", s.cfg.User, "count", len(live))
			results := drainCalls(live, 5*time.Second, 10*time.Second)
			for _, r := range results {
				if !r.Ended || r.HangupErr != nil {
					slog.Warn("sip: call did not end cleanly at stack stop",
						"user", s.cfg.User, "callID", r.CallID, "ended", r.Ended, "err", r.HangupErr)
				}
			}
		}

		if s.regTx != nil {
			// Fresh, short ctx: s.ctx may already be near its deadline, and
			// the deregister is best-effort cleanup either way.
			deregCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if err := s.regTx.Unregister(deregCtx); err != nil {
				slog.Debug("sip: deregister failed (cleanup, best-effort)",
					"user", s.cfg.User, "err", err)
			} else {
				slog.Debug("sip: deregistered", "user", s.cfg.User)
			}
			cancel()
		}
		s.cancel()
		if s.ua != nil {
			_ = s.ua.Close()
		}
	})
}

// registerAttempts bounds the retry loop in register(). Under heavy parallel
// stack churn, sipgo's TCP transport occasionally closes a connection right
// after the REGISTER goes out on it (the "TCP ref went negative" refcount
// bug — drachtio sees our FIN arrive 0.2ms behind the REGISTER), so the 401
// challenge has no path back and the attempt dies on Timer_B after ~32s.
// The poisoned connection is already gone by then; a fresh transaction on a
// fresh connection succeeds, so a bounded retry converts a hard claimUAS
// fatal into a logged hiccup. SIP-level rejections (401 with bad creds,
// 403) are returned immediately — retrying can't fix credentials, and some
// tests assert on them.
const registerAttempts = 3

func (s *Stack) register() error {
	var lastErr error
	for attempt := 1; attempt <= registerAttempts; attempt++ {
		err := s.registerOnce()
		if err == nil {
			if attempt > 1 {
				slog.Info("sip: register succeeded after retry",
					"user", s.cfg.User, "attempt", attempt)
			}
			return nil
		}
		lastErr = err
		var rejected *diago.RegisterResponseError
		if errors.As(err, &rejected) {
			// The registrar answered with a final non-2xx: a real rejection,
			// not a transport failure.
			return err
		}
		if s.ctx.Err() != nil {
			return err
		}
		if attempt < registerAttempts {
			slog.Warn("sip: register attempt failed, retrying",
				"user", s.cfg.User, "attempt", attempt, "err", err)
			time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
		}
	}
	return lastErr
}

func (s *Stack) registerOnce() error {
	params := sip.NewParams()
	params.Add("transport", s.cfg.Transport)
	regURI := sip.Uri{User: s.cfg.User, Host: s.cfg.SIPDomain, UriParams: params}
	slog.Debug("sip: registering", "uri", regURI.String(), "transport", s.cfg.Transport)

	// Use RegisterTransaction (not dg.Register) so we own the transaction
	// handle. dg.Register buries an Unregister in a defer that only fires
	// after its qualify loop exits — i.e. after s.cancel() has already closed
	// the connection — which sends the de-REGISTER from a new ephemeral port
	// the registrar can't match (403). We instead deregister explicitly in
	// Stop(), on the live connection, before cancelling.
	tx, err := s.dg.RegisterTransaction(s.ctx, regURI, diago.RegisterOptions{
		Username: s.cfg.User,
		Password: s.cfg.Pass,
		Expiry:   300 * time.Second,
	})
	if err != nil {
		return fmt.Errorf("register: build transaction: %w", err)
	}

	if err := tx.Register(s.ctx); err != nil {
		return fmt.Errorf("register: %w", err)
	}
	s.regTx = tx

	// Keep the registration fresh in the background until the serve ctx ends.
	// QualifyLoop blocks re-sending REGISTER on the expiry schedule; it
	// returns when s.ctx is cancelled (in Stop) or on a fatal register error.
	go func() {
		if err := tx.QualifyLoop(s.ctx); err != nil &&
			s.ctx.Err() == nil {
			slog.Debug("sip: register qualify loop ended", "user", s.cfg.User, "err", err)
		}
	}()

	slog.Info("sip: registered", "user", s.cfg.User, "domain", s.cfg.SIPDomain, "transport", s.cfg.Transport)
	return nil
}

// dispatchInbound wraps each inbound dialog in a *Call and hands it to the
// configured handler.
func (s *Stack) dispatchInbound(d *diago.DialogServerSession) {
	call := newInboundCall(d, s.cfg.Owner)
	s.track(call)
	slog.Info("sip: inbound call",
		"call_id", call.CallID(),
		"from", call.From(),
		"to", call.To())

	s.handlerMu.RLock()
	h := s.handler
	s.handlerMu.RUnlock()
	if h == nil {
		_ = call.Reject(480, "Temporarily Unavailable")
		return
	}

	handlerCtx, cancel := context.WithCancel(s.ctx)
	defer cancel()
	if err := h(handlerCtx, call); err != nil {
		slog.Warn("sip: inbound handler error", "err", err, "call_id", call.CallID())
	}
	if call.State() != StateEnded {
		slog.Debug("sip: handler exited without hangup; closing call", "call_id", call.CallID())
		_ = call.Hangup()
	}
}

func nonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
