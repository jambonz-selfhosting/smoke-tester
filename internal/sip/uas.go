// Package sip wraps sipgo + diago as a test-friendly SIP stack with
// symmetrical UAC + UAS control. Tests drive Calls step-by-step: Trying,
// Ringing, Answer, Reject, Hangup, and observe every SIP message + media
// frame that came and went.
package sip

import (
	"context"
	"fmt"
	"log/slog"
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

	handlerMu sync.RWMutex
	handler   InboundHandler
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

	ua, err := sipgo.NewUA(
		sipgo.WithUserAgent(nonEmpty(cfg.User, "jambonz-it")),
	)
	if err != nil {
		return nil, fmt.Errorf("sipgo NewUA: %w", err)
	}

	dg := diago.NewDiago(ua,
		diago.WithTransport(diago.Transport{Transport: "tcp", BindHost: "0.0.0.0", BindPort: 0}),
		diago.WithTransport(diago.Transport{Transport: "udp", BindHost: "0.0.0.0", BindPort: 0}),
		diago.WithServerRequestMiddleware(observeRequestMiddleware),
	)

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

// Stop cancels the serve loop and closes the UA. Safe to call more than once.
func (s *Stack) Stop() {
	s.cancel()
	if s.ua != nil {
		_ = s.ua.Close()
	}
}

func (s *Stack) register() error {
	params := sip.NewParams()
	params.Add("transport", s.cfg.Transport)
	regURI := sip.Uri{User: s.cfg.User, Host: s.cfg.SIPDomain, UriParams: params}
	slog.Debug("sip: registering", "uri", regURI.String(), "transport", s.cfg.Transport)

	readyCh := make(chan struct{}, 1)
	regErrCh := make(chan error, 1)
	go func() {
		regErrCh <- s.dg.Register(s.ctx, regURI, diago.RegisterOptions{
			Username: s.cfg.User,
			Password: s.cfg.Pass,
			Expiry:   300 * time.Second,
			OnRegistered: func() {
				select {
				case readyCh <- struct{}{}:
				default:
				}
			},
		})
	}()

	select {
	case <-readyCh:
	case err := <-regErrCh:
		return fmt.Errorf("register: %w", err)
	case <-time.After(15 * time.Second):
		return fmt.Errorf("register: timeout after 15s")
	}
	slog.Info("sip: registered", "user", s.cfg.User, "domain", s.cfg.SIPDomain, "transport", s.cfg.Transport)
	return nil
}

// dispatchInbound wraps each inbound dialog in a *Call and hands it to the
// configured handler.
func (s *Stack) dispatchInbound(d *diago.DialogServerSession) {
	call := newInboundCall(d)
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
