// Package siprec implements just enough of a SIPREC recording server (SRS) to
// prove a jambonz SIPREC session keeps receiving media.
//
// It answers the SIPREC INVITE the SBC sends, writes each forked stream's µ-law
// payload to disk for transcript assertions, and keeps a packet timeline so a
// test can ask "did the media stop, and for how long". The SBC has to be able
// to reach it, so the test that uses it runs inside the cluster's network.
package siprec

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	bucket      = 250 * time.Millisecond
	rtpHdrBytes = 12
)

// Event is something the SBC did to the recording session.
type Event struct {
	Kind   string // invite, reinvite, bye, extra-invite, bad-invite
	At     time.Time
	Detail string
}

// Window summarises one stream over a time range.
type Window struct {
	Label   string
	Packets int
	Rate    float64 // packets/sec
}

type stream struct {
	label   string
	port    int
	conn    *net.UDPConn
	file    *os.File
	buckets []struct {
		t time.Time
		n int
	}
	packets int
}

// Recorder is a single-session SRS: it accepts one SIPREC call at a time, which
// is all a test needs and makes a second INVITE for the same call a failure the
// test can see.
type Recorder struct {
	advertiseIP net.IP
	sipPort     int
	rtpPortBase int
	dir         string

	mu      sync.Mutex
	conn    *net.UDPConn
	toTag   string
	callID  string
	version int
	streams []*stream
	events  []Event
	closed  bool
}

// New builds a recorder. A zero sipPort or rtpPortBase means "pick free ports",
// which is what tests want; URL() reports the port actually bound after Start.
func New(advertiseIP net.IP, sipPort, rtpPortBase int, dir string) *Recorder {
	return &Recorder{
		advertiseIP: advertiseIP,
		sipPort:     sipPort,
		rtpPortBase: rtpPortBase,
		dir:         dir,
		toTag:       fmt.Sprintf("srs-%d", time.Now().UnixNano()%1e9),
		version:     1,
	}
}

func (r *Recorder) Start() error {
	if err := os.MkdirAll(r.dir, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", r.dir, err)
	}
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: r.sipPort})
	if err != nil {
		return fmt.Errorf("bind sip udp %d: %w", r.sipPort, err)
	}
	r.conn = conn
	r.sipPort = conn.LocalAddr().(*net.UDPAddr).Port
	go r.serve()
	return nil
}

func (r *Recorder) Stop() {
	r.mu.Lock()
	r.closed = true
	conn := r.conn
	streams := r.streams
	r.mu.Unlock()

	if conn != nil {
		_ = conn.Close()
	}
	for _, s := range streams {
		_ = s.conn.Close()
		_ = s.file.Close()
	}
}

// URL is what to hand jambonz as siprecServerURL.
func (r *Recorder) URL() string {
	return fmt.Sprintf("sip:%s:%d", r.advertiseIP, r.sipPort)
}

func (r *Recorder) Events() []Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Event(nil), r.events...)
}

func (r *Recorder) Saw(kind string) bool {
	for _, e := range r.Events() {
		if e.Kind == kind {
			return true
		}
	}
	return false
}

func (r *Recorder) Count(kind string) int {
	n := 0
	for _, e := range r.Events() {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

// StreamPath is the raw µ-law capture for a stream label ("1" is the caller
// side, "2" the party jambonz bridged them to).
func (r *Recorder) StreamPath(label string) string {
	return filepath.Join(r.dir, "stream-"+label+".ulaw")
}

// Window reports per-stream packet counts and rates over [from, to).
func (r *Recorder) Window(from, to time.Time) []Window {
	r.mu.Lock()
	defer r.mu.Unlock()
	secs := to.Sub(from).Seconds()
	if secs <= 0 {
		secs = 1
	}
	out := make([]Window, 0, len(r.streams))
	for _, s := range r.streams {
		n := 0
		for _, b := range s.buckets {
			if b.t.Add(bucket).After(from) && b.t.Before(to) {
				n += b.n
			}
		}
		out = append(out, Window{Label: s.label, Packets: n, Rate: float64(n) / secs})
	}
	return out
}

// LongestGap is the longest stretch inside [from, to) with no packet on the
// busiest stream - the number that tells a "went silent at the transfer" bug
// from a healthy recording.
func (r *Recorder) LongestGap(from, to time.Time) time.Duration {
	r.mu.Lock()
	defer r.mu.Unlock()
	var worst time.Duration
	for _, s := range r.streams {
		last := from
		var gap time.Duration
		for _, b := range s.buckets {
			if !b.t.Add(bucket).After(from) || !b.t.Before(to) || b.n == 0 {
				continue
			}
			if d := b.t.Sub(last); d > gap {
				gap = d
			}
			last = b.t.Add(bucket)
		}
		if d := to.Sub(last); d > gap {
			gap = d
		}
		if gap > worst {
			worst = gap
		}
	}
	return worst
}

func (r *Recorder) TotalPackets(from, to time.Time) int {
	n := 0
	for _, w := range r.Window(from, to) {
		n += w.Packets
	}
	return n
}

func (r *Recorder) serve() {
	buf := make([]byte, 65535)
	for {
		n, addr, err := r.conn.ReadFromUDP(buf)
		if err != nil {
			return // closed
		}
		r.handle(string(buf[:n]), addr)
	}
}

var (
	reAudioM   = regexp.MustCompile(`(?m)^m=audio (\d+) [^ ]+ ([^\r\n]+)`)
	reHeaderLn = regexp.MustCompile(`^([^:]+):\s*(.*)$`)
)

type request struct {
	method  string
	uri     string
	raw     []string
	headers map[string]string
	body    string
}

func parse(msg string) request {
	req := request{headers: map[string]string{}}
	head := msg
	if i := strings.Index(msg, "\r\n\r\n"); i >= 0 {
		head, req.body = msg[:i], msg[i+4:]
	}
	lines := strings.Split(head, "\r\n")
	if len(lines) == 0 {
		return req
	}
	parts := strings.Fields(lines[0])
	if len(parts) > 0 {
		req.method = parts[0]
	}
	if len(parts) > 1 {
		req.uri = parts[1]
	}
	req.raw = lines[1:]
	for _, l := range req.raw {
		if m := reHeaderLn.FindStringSubmatch(l); m != nil {
			k := strings.ToLower(strings.TrimSpace(m[1]))
			if _, dup := req.headers[k]; !dup {
				req.headers[k] = strings.TrimSpace(m[2])
			}
		}
	}
	return req
}

// sdpOf returns the SDP part of a (possibly multipart SIPREC) body.
func sdpOf(body string) string {
	i := strings.Index(body, "v=0")
	if i < 0 {
		return ""
	}
	rest := body[i:]
	if j := strings.Index(rest, "\n--"); j >= 0 {
		return rest[:j]
	}
	return rest
}

func (r *Recorder) handle(msg string, addr *net.UDPAddr) {
	req := parse(msg)
	if strings.HasPrefix(req.method, "SIP/2.0") {
		return
	}
	switch req.method {
	case "INVITE":
		r.onInvite(req, addr)
	case "ACK":
	case "BYE":
		r.record("bye", "")
		r.reply(req, addr, 200, "OK", "", "")
	case "OPTIONS":
		r.reply(req, addr, 200, "OK", "", "")
	default:
		r.reply(req, addr, 200, "OK", "", "")
	}
}

func (r *Recorder) onInvite(req request, addr *net.UDPAddr) {
	sdp := sdpOf(req.body)
	if sdp == "" {
		r.record("bad-invite", "no sdp in body")
		r.reply(req, addr, 488, "Not Acceptable Here", "", "")
		return
	}
	offers := reAudioM.FindAllStringSubmatch(sdp, -1)
	callID := req.headers["call-id"]

	r.mu.Lock()
	switch {
	case r.callID == "":
		r.callID = callID
		r.mu.Unlock()
		if err := r.bindStreams(len(offers)); err != nil {
			r.record("bad-invite", err.Error())
			r.reply(req, addr, 500, "Server Internal Error", "", "")
			return
		}
		r.record("invite", fmt.Sprintf("%d stream(s), %d bytes of metadata", len(offers), len(req.body)))
	case r.callID == callID:
		r.mu.Unlock()
		r.record("reinvite", fmt.Sprintf("%d stream(s)", len(offers)))
	default:
		r.mu.Unlock()
		r.record("extra-invite", "second siprec session for one call: "+callID)
		r.reply(req, addr, 486, "Busy Here", "", "")
		return
	}

	r.reply(req, addr, 200, "OK", r.answerSDP(offers), "application/sdp")
}

func (r *Recorder) bindStreams(count int) error {
	if count < 1 {
		count = 1
	}
	for i := 0; i < count; i++ {
		label := strconv.Itoa(i + 1)
		port := 0
		if r.rtpPortBase != 0 {
			port = r.rtpPortBase + (i * 2)
		}
		conn, err := net.ListenUDP("udp4", &net.UDPAddr{Port: port})
		if err != nil {
			return fmt.Errorf("bind rtp %d: %w", port, err)
		}
		port = conn.LocalAddr().(*net.UDPAddr).Port
		f, err := os.Create(r.StreamPath(label))
		if err != nil {
			_ = conn.Close()
			return fmt.Errorf("create capture: %w", err)
		}
		s := &stream{label: label, port: port, conn: conn, file: f}
		r.mu.Lock()
		r.streams = append(r.streams, s)
		r.mu.Unlock()
		go r.readRTP(s)
	}
	return nil
}

func (r *Recorder) readRTP(s *stream) {
	buf := make([]byte, 2048)
	for {
		n, err := s.conn.Read(buf)
		if err != nil {
			return
		}
		if n <= rtpHdrBytes {
			continue
		}
		payload := buf[rtpHdrBytes:n]
		now := time.Now().Truncate(bucket)

		r.mu.Lock()
		s.packets++
		if len(s.buckets) > 0 && s.buckets[len(s.buckets)-1].t.Equal(now) {
			s.buckets[len(s.buckets)-1].n++
		} else {
			s.buckets = append(s.buckets, struct {
				t time.Time
				n int
			}{now, 1})
		}
		f := s.file
		r.mu.Unlock()

		if f != nil {
			_, _ = f.Write(payload)
		}
	}
}

func (r *Recorder) answerSDP(offers [][]string) string {
	r.mu.Lock()
	ver := r.version
	r.version++
	streams := r.streams
	r.mu.Unlock()

	b := &strings.Builder{}
	fmt.Fprintf(b, "v=0\r\no=- %d %d IN IP4 %s\r\ns=jambonz smoke srs\r\nc=IN IP4 %s\r\nt=0 0\r\n",
		time.Now().Unix(), ver, r.advertiseIP, r.advertiseIP)
	for i, m := range offers {
		port := 0
		if i < len(streams) {
			port = streams[i].port
		}
		pt, extra := answerPayloads(m[2])
		fmt.Fprintf(b, "m=audio %d RTP/AVP %s\r\n", port, strings.Join(append([]string{pt}, extra...), " "))
		fmt.Fprintf(b, "a=rtpmap:%s %s/8000\r\n", pt, codecName(pt))
		for _, e := range extra {
			fmt.Fprintf(b, "a=rtpmap:%s telephone-event/8000\r\n", e)
		}
		fmt.Fprintf(b, "a=recvonly\r\na=label:%d\r\n", i+1)
	}
	return b.String()
}

func answerPayloads(fmtList string) (string, []string) {
	offered := strings.Fields(fmtList)
	pt := ""
	for _, want := range []string{"0", "8", "18", "9"} {
		for _, o := range offered {
			if o == want {
				pt = want
				break
			}
		}
		if pt != "" {
			break
		}
	}
	if pt == "" && len(offered) > 0 {
		pt = offered[0]
	}
	var extra []string
	for _, o := range offered {
		if o == "101" {
			extra = append(extra, "101")
		}
	}
	return pt, extra
}

func codecName(pt string) string {
	switch pt {
	case "8":
		return "PCMA"
	case "18":
		return "G729"
	case "9":
		return "G722"
	default:
		return "PCMU"
	}
}

func (r *Recorder) record(kind, detail string) {
	r.mu.Lock()
	r.events = append(r.events, Event{Kind: kind, At: time.Now(), Detail: detail})
	r.mu.Unlock()
}

func (r *Recorder) reply(req request, addr *net.UDPAddr, status int, reason, body, contentType string) {
	var b strings.Builder
	fmt.Fprintf(&b, "SIP/2.0 %d %s\r\n", status, reason)
	for _, l := range req.raw {
		name := strings.ToLower(strings.TrimSpace(strings.SplitN(l, ":", 2)[0]))
		switch name {
		case "via", "from", "call-id", "cseq", "record-route":
			b.WriteString(l + "\r\n")
		}
	}
	to := req.headers["to"]
	if !strings.Contains(strings.ToLower(to), ";tag=") {
		to += ";tag=" + r.toTag
	}
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Contact: <sip:%s:%d>\r\n", r.advertiseIP, r.sipPort)
	b.WriteString("User-Agent: jambonz-smoke-srs\r\n")
	if body != "" {
		fmt.Fprintf(&b, "Content-Type: %s\r\nContent-Length: %d\r\n\r\n%s", contentType, len(body), body)
	} else {
		b.WriteString("Content-Length: 0\r\n\r\n")
	}
	_, _ = r.conn.WriteToUDP([]byte(b.String()), addr)
}
