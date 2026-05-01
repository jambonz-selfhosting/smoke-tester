package sip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"sync"
	"time"
)

func timeNowPlus() time.Time { return time.Now().Add(500 * time.Millisecond) }

func isClosedErr(err error) bool {
	return errors.Is(err, net.ErrClosed)
}

// StaticResolver is a *net.Resolver that answers any A query for a hostname
// in `Hosts` with the configured IP, and forwards everything else to the
// system resolver. It runs an in-process UDP DNS listener on 127.0.0.1 and
// hands sipgo's UserAgent a *net.Resolver wired to it via PreferGo + Dial.
//
// Why this dance: sipgo's transport layer always calls
// `dnsResolver.LookupIPAddr` before opening a SIP socket — we can't bypass
// that path. The synthetic per-account sip_realm we provision (e.g.
// `it-<runID>-verbs.smoke.test`) doesn't have real DNS records, so without
// an override sipgo would fail with NXDOMAIN. *net.Resolver is a concrete
// struct, not an interface, so we can't subclass; the smallest workaround
// is to drive it via a tiny in-process DNS server.
//
// All other lookups fall through to the system resolver (e.g. ngrok hosts,
// any cluster DNS the harness might issue).
type StaticResolver struct {
	// Hosts maps lowercase hostname → IP. Trailing dots are stripped from
	// the request before lookup.
	Hosts map[string]net.IP

	mu       sync.Mutex
	listener *net.UDPConn
	addr     string
	stop     chan struct{}
	stopped  bool
}

// NewStaticResolver builds and starts a resolver. Call Close when done.
func NewStaticResolver(hosts map[string]string) (*StaticResolver, error) {
	parsed := make(map[string]net.IP, len(hosts))
	for h, ip := range hosts {
		p := net.ParseIP(ip)
		if p == nil {
			return nil, fmt.Errorf("StaticResolver: %q is not a valid IP", ip)
		}
		parsed[strings.ToLower(strings.TrimSuffix(h, "."))] = p.To4()
	}
	r := &StaticResolver{Hosts: parsed, stop: make(chan struct{})}
	if err := r.start(); err != nil {
		return nil, err
	}
	return r, nil
}

func (r *StaticResolver) start() error {
	addr := &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0}
	l, err := net.ListenUDP("udp", addr)
	if err != nil {
		return fmt.Errorf("StaticResolver listen: %w", err)
	}
	r.listener = l
	r.addr = l.LocalAddr().String()

	go r.serve()
	return nil
}

// Resolver returns a *net.Resolver wired to this server. Pass to
// sipgo.WithUserAgentDNSResolver.
func (r *StaticResolver) Resolver() *net.Resolver {
	return &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			// Force the Go resolver to talk to OUR listener. Network is
			// "udp" or "tcp" — we only handle UDP, but Go's resolver
			// will fall back to UDP if needed.
			d := net.Dialer{}
			return d.DialContext(ctx, "udp", r.addr)
		},
	}
}

// Close stops the listener.
func (r *StaticResolver) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stopped {
		return nil
	}
	r.stopped = true
	close(r.stop)
	if r.listener != nil {
		return r.listener.Close()
	}
	return nil
}

// serve reads DNS queries and answers A records from Hosts; everything else
// is forwarded to the system stub resolver via net.LookupIP.
func (r *StaticResolver) serve() {
	buf := make([]byte, 1500)
	for {
		select {
		case <-r.stop:
			return
		default:
		}
		_ = r.listener.SetReadDeadline(timeNowPlus())
		n, src, err := r.listener.ReadFromUDP(buf)
		if err != nil {
			if isClosedErr(err) {
				return
			}
			continue
		}
		req := append([]byte(nil), buf[:n]...)
		go r.handle(req, src)
	}
}

func (r *StaticResolver) handle(req []byte, src *net.UDPAddr) {
	if len(req) < 12 {
		return
	}
	id := binary.BigEndian.Uint16(req[0:2])
	qdcount := binary.BigEndian.Uint16(req[4:6])
	if qdcount == 0 {
		return
	}
	name, qtype, qend := parseQuestion(req, 12)
	if name == "" {
		return
	}
	low := strings.ToLower(strings.TrimSuffix(name, "."))

	resp := buildResponse(id, req[12:qend])

	// Only intercept A records (1) and AAAA (28); for AAAA on our hosts
	// answer with NOERROR no-records so the resolver moves on to A.
	switch qtype {
	case 1: // A
		if ip, ok := r.Hosts[low]; ok && ip != nil {
			resp = appendARecord(resp, req[12:qend], ip)
			binary.BigEndian.PutUint16(resp[6:8], 1) // ANCOUNT=1
			_, _ = r.listener.WriteToUDP(resp, src)
			return
		}
		// fallthrough: let system resolve
	case 28: // AAAA
		if _, ok := r.Hosts[low]; ok {
			// Synthetic host has no AAAA → empty answer.
			_, _ = r.listener.WriteToUDP(resp, src)
			return
		}
	}

	// Forward to system resolver.
	ips, err := net.LookupIP(low)
	if err != nil {
		// NXDOMAIN
		resp[3] |= 3 // RCODE=3
		_, _ = r.listener.WriteToUDP(resp, src)
		return
	}
	// Match the question type.
	var matched int
	for _, ip := range ips {
		switch qtype {
		case 1:
			if v4 := ip.To4(); v4 != nil {
				resp = appendARecord(resp, req[12:qend], v4)
				matched++
			}
		case 28:
			if ip.To4() == nil {
				resp = appendAAAARecord(resp, req[12:qend], ip)
				matched++
			}
		}
	}
	binary.BigEndian.PutUint16(resp[6:8], uint16(matched))
	_, _ = r.listener.WriteToUDP(resp, src)
}

// buildResponse copies the question from the request into a new response
// header. Caller fills ANCOUNT after appending answers.
func buildResponse(id uint16, qsection []byte) []byte {
	hdr := make([]byte, 12)
	binary.BigEndian.PutUint16(hdr[0:2], id)
	hdr[2] = 0x81 // QR=1, RD=1
	hdr[3] = 0x80 // RA=1
	binary.BigEndian.PutUint16(hdr[4:6], 1) // QDCOUNT=1
	// ANCOUNT/NSCOUNT/ARCOUNT zero by default
	return append(hdr, qsection...)
}

// parseQuestion reads the QNAME, QTYPE, QCLASS starting at off in req.
// Returns name (with trailing dot), qtype, and end offset (one past QCLASS).
func parseQuestion(req []byte, off int) (string, uint16, int) {
	var sb strings.Builder
	for off < len(req) {
		l := int(req[off])
		off++
		if l == 0 {
			break
		}
		if off+l > len(req) {
			return "", 0, 0
		}
		sb.Write(req[off : off+l])
		sb.WriteByte('.')
		off += l
	}
	if off+4 > len(req) {
		return "", 0, 0
	}
	qtype := binary.BigEndian.Uint16(req[off : off+2])
	return sb.String(), qtype, off + 4
}

// appendARecord writes an A RR using the same QNAME (via name-pointer).
func appendARecord(resp, qsection []byte, ip net.IP) []byte {
	out := make([]byte, 0, len(resp)+16)
	out = append(out, resp...)
	out = append(out, 0xc0, 0x0c) // pointer to QNAME at offset 12
	out = append(out, 0x00, 0x01) // TYPE=A
	out = append(out, 0x00, 0x01) // CLASS=IN
	out = append(out, 0x00, 0x00, 0x00, 0x3c) // TTL=60
	out = append(out, 0x00, 0x04) // RDLENGTH=4
	out = append(out, ip.To4()...)
	return out
}

// appendAAAARecord writes a AAAA RR (rare in our flows, but supports
// fallthrough for non-overridden hosts).
func appendAAAARecord(resp, qsection []byte, ip net.IP) []byte {
	out := make([]byte, 0, len(resp)+28)
	out = append(out, resp...)
	out = append(out, 0xc0, 0x0c)
	out = append(out, 0x00, 0x1c)
	out = append(out, 0x00, 0x01)
	out = append(out, 0x00, 0x00, 0x00, 0x3c)
	out = append(out, 0x00, 0x10)
	out = append(out, ip.To16()...)
	return out
}
