//go:build drachtio

package drachtio

import (
	"fmt"
	"io"
	"net"
	"sync"
	"testing"
	"time"
)

// tcpRelay is a man-in-the-middle TCP relay that can make an established
// connection to the SBC go silent the way a carrier's connection does in
// production: the socket is left open, nothing is ever written to it again,
// and everything read from it is discarded. No FIN, no RST — so the far end
// (drachtio) sees a perfectly healthy socket that simply stops answering.
//
// Why a relay rather than just closing the UAC's socket: closing sends a FIN,
// and drachtio notices a FIN. `tport_is_closed()` goes true, `checkTportState()`
// releases the dialog's pinned tport, and the bug never reproduces. The whole
// defect depends on the connection dying *without* the TCP layer saying so —
// which we can only stage from a third party sitting in the path.
//
//	UAC ──(stable downstream)──▶ relay ──(swappable upstream)──▶ SBC
//
// Reconnect() abandons the current upstream and dials a fresh one. Because the
// new upstream is a new TCP connection from this host, drachtio sees it as a
// new source port from the same IP — exactly the shape of the incident
// (10.222.30.173:11114 abandoned, 10.222.30.173:8219 established 10s later).
//
// The downstream socket to the UAC is untouched throughout, so sipgo keeps its
// dialog state and never learns a reconnect happened. That mirrors production:
// the carrier's SIP layer also carried on regardless — a SIP dialog is matched
// by Call-ID and tags, never by connection.
type tcpRelay struct {
	t        *testing.T
	ln       net.Listener
	upstream string // "host:port" of the real SBC

	mu        sync.Mutex
	sessions  []*relaySession
	closed    bool
	dialCount int
}

type relaySession struct {
	r    *tcpRelay
	down net.Conn

	mu        sync.Mutex
	up        net.Conn   // current upstream; nil once the session is torn down
	abandoned []net.Conn // deliberately held open, never closed — the "dead" sockets
}

// newTCPRelay starts a relay listening on listenAddr and forwarding to
// upstream. Callers point the SIP stack's resolver at the listen address.
func newTCPRelay(t *testing.T, listenAddr, upstream string) *tcpRelay {
	t.Helper()

	ln, err := net.Listen("tcp", listenAddr)
	if err != nil {
		t.Fatalf("tcpRelay: listen on %s: %v (is something else bound to it? the SIP "+
			"request-uri carries no port, so the relay has to own the default SIP port)",
			listenAddr, err)
	}
	r := &tcpRelay{t: t, ln: ln, upstream: upstream}

	go r.acceptLoop()
	t.Cleanup(r.close)
	t.Logf("tcpRelay: listening on %s, forwarding to %s", ln.Addr(), upstream)
	return r
}

func (r *tcpRelay) acceptLoop() {
	for {
		down, err := r.ln.Accept()
		if err != nil {
			return // listener closed
		}
		up, err := net.DialTimeout("tcp", r.upstream, 10*time.Second)
		if err != nil {
			r.t.Logf("tcpRelay: dial upstream %s: %v", r.upstream, err)
			_ = down.Close()
			continue
		}

		r.mu.Lock()
		r.dialCount++
		n := r.dialCount
		r.mu.Unlock()
		r.t.Logf("tcpRelay: session up — UAC %s ⇄ relay ⇄ SBC (local port %s, upstream conn #%d)",
			down.RemoteAddr(), localPort(up), n)

		s := &relaySession{r: r, down: down, up: up}
		r.mu.Lock()
		r.sessions = append(r.sessions, s)
		r.mu.Unlock()

		go s.pumpDownToUp()
		go s.pumpUpToDown(up)
	}
}

// Reconnect abandons every live upstream socket and dials a replacement for
// each, leaving the old ones open and unread-from. This is the reconnect
// itself: it is purely a transport-layer event, with nothing at the SIP layer
// announcing it — which is precisely why drachtio cannot correlate the old
// connection with the new one on its own.
//
// Returns the number of connections swapped.
func (r *tcpRelay) Reconnect() int {
	r.mu.Lock()
	sessions := append([]*relaySession(nil), r.sessions...)
	r.mu.Unlock()

	n := 0
	for _, s := range sessions {
		if s.reconnect() {
			n++
		}
	}
	return n
}

func (s *relaySession) reconnect() bool {
	s.mu.Lock()
	old := s.up
	s.mu.Unlock()
	if old == nil {
		return false
	}

	up, err := net.DialTimeout("tcp", s.r.upstream, 10*time.Second)
	if err != nil {
		s.r.t.Logf("tcpRelay: reconnect dial failed: %v", err)
		return false
	}

	s.r.mu.Lock()
	s.r.dialCount++
	n := s.r.dialCount
	s.r.mu.Unlock()

	s.mu.Lock()
	s.up = up
	s.abandoned = append(s.abandoned, old)
	s.mu.Unlock()

	s.r.t.Logf("tcpRelay: RECONNECT — abandoned upstream local port %s (left open, no FIN/RST), "+
		"new upstream local port %s (conn #%d)", localPort(old), localPort(up), n)

	// Keep draining the abandoned socket into the void. If we stopped reading,
	// its receive buffer would fill and TCP would eventually stop ACKing, which
	// drachtio could notice as backpressure. Draining keeps drachtio's writes
	// succeeding exactly as they did in production, where it logged 685-byte
	// REFER sends onto the dead connection for over nine minutes.
	go func() { _, _ = io.Copy(io.Discard, old) }()

	go s.pumpUpToDown(up)
	return true
}

// pumpDownToUp forwards UAC bytes to whichever upstream is current at the
// moment of the write, so it survives a Reconnect without the UAC noticing.
func (s *relaySession) pumpDownToUp() {
	buf := make([]byte, 32*1024)
	for {
		n, err := s.down.Read(buf)
		if n > 0 {
			s.mu.Lock()
			up := s.up
			s.mu.Unlock()
			if up == nil {
				return
			}
			if _, werr := up.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			s.teardown()
			return
		}
	}
}

// pumpUpToDown forwards one specific upstream's bytes to the UAC. After a
// Reconnect the goroutine for the abandoned socket is replaced by the
// io.Discard drain, so nothing from the dead connection ever reaches the UAC.
func (s *relaySession) pumpUpToDown(up net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := up.Read(buf)
		if n > 0 {
			s.mu.Lock()
			current := s.up
			s.mu.Unlock()
			if current != up {
				return // superseded by a reconnect; drop whatever arrives late
			}
			if _, werr := s.down.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			return
		}
	}
}

func (s *relaySession) teardown() {
	s.mu.Lock()
	up := s.up
	s.up = nil
	abandoned := s.abandoned
	s.abandoned = nil
	s.mu.Unlock()

	if up != nil {
		_ = up.Close()
	}
	for _, c := range abandoned {
		_ = c.Close() // only at teardown — during the test they stay open on purpose
	}
	_ = s.down.Close()
}

func (r *tcpRelay) close() {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.closed = true
	sessions := append([]*relaySession(nil), r.sessions...)
	r.mu.Unlock()

	_ = r.ln.Close()
	for _, s := range sessions {
		s.teardown()
	}
}

// Addr is the relay's listen address.
func (r *tcpRelay) Addr() string { return r.ln.Addr().String() }

func localPort(c net.Conn) string {
	if c == nil {
		return "?"
	}
	_, port, err := net.SplitHostPort(c.LocalAddr().String())
	if err != nil {
		return c.LocalAddr().String()
	}
	return port
}

// relayListenAddr is where the relay binds. It must be the default SIP port:
// the INVITE's request-uri is sip:app-<sid>@<realm> with no port, so sipgo
// resolves <realm> and connects to 5060.
const relayListenAddr = "127.0.0.1:5060"

func sbcAddr() string { return fmt.Sprintf("%s:5060", cfg.SBCPublicIP.String()) }
