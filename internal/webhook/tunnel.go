package webhook

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sync"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"
)

// Tunnel is an ngrok HTTPS tunnel that forwards to the local Server.
type Tunnel struct {
	t   ngrok.Tunnel
	url string

	mu       sync.Mutex
	closeCtx context.CancelFunc
	done     chan struct{}
}

// StartNgrok opens an HTTPS tunnel to s and forwards traffic to s.LocalAddr.
// Expects NGROK_AUTHTOKEN in the environment (via ngrok.WithAuthtokenFromEnv).
func StartNgrok(ctx context.Context, s *Server) (*Tunnel, error) {
	// Build the tunnel.
	opts := []config.HTTPEndpointOption{
		config.WithForwardsTo(s.LocalAddr()),
	}

	tun, err := ngrok.Listen(ctx, config.HTTPEndpoint(opts...),
		ngrok.WithAuthtokenFromEnv(),
	)
	if err != nil {
		return nil, fmt.Errorf("ngrok.Listen: %w", err)
	}

	t := &Tunnel{
		t:    tun,
		url:  tun.URL(),
		done: make(chan struct{}),
	}

	// Forward traffic: ngrok.Tunnel implements net.Listener, so serve our
	// mux over it.
	forwardCtx, cancel := context.WithCancel(context.Background())
	t.closeCtx = cancel
	go func() {
		defer close(t.done)
		srv := &http.Server{Handler: s.httpSrv.Handler}
		err := srv.Serve(tun)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Warn("ngrok tunnel serve error", "err", err)
		}
		_ = forwardCtx // quiet linter
	}()

	// Tell the server its public URL so tests can provision Applications.
	s.SetPublicURL(t.url)
	slog.Info("webhook: ngrok tunnel up", "public_url", t.url, "forwards_to", s.LocalAddr())
	return t, nil
}

// URL returns the tunnel's public HTTPS URL (e.g. https://abcd.ngrok-free.app).
func (t *Tunnel) URL() string { return t.url }

// Close shuts down the tunnel.
func (t *Tunnel) Close() error {
	if t.closeCtx != nil {
		t.closeCtx()
	}
	return t.t.Close()
}
