package sip

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/emiago/diago"
	"github.com/emiago/diago/audio"
	"github.com/emiago/diago/media"
	"github.com/emiago/sipgo/sip"
)

// Call is the harness-side handle for a single call leg. Both UAS (inbound)
// and UAC (outbound) use the same type; methods that don't apply to the
// current direction return an error.
//
// Every SIP message sent and received is recorded on the Call for inspection.
// Media (recording, silence, DTMF) is opt-in — nothing runs automatically.
// Each method returns an error; invalid transitions never panic.
type Call struct {
	direction Direction
	created   time.Time

	// exactly one of these is non-nil
	in  *diago.DialogServerSession
	out *diago.DialogClientSession

	mu         sync.Mutex
	state      State
	answeredAt time.Time
	endedAt    time.Time
	endReason  string
	err        error

	// SIP message history
	sent []Message
	recv []Message

	// state waiters: broadcast on every state change
	stateCh chan struct{}
	doneCh  chan struct{} // closed on StateEnded

	// media (opt-in)
	mediaMu       sync.Mutex
	recording     *recordingSession
	silenceCancel context.CancelFunc
	codec         string
	pcmBytesIn    int64
	samplesIn     int64
	sumSqIn       float64
	rmsIn         float64
	rawPCMPath    string
	dtmf          []DTMFEvent

	// owner is the name of the test that owns this call (ADR-0016).
	// Inherited from Stack.Config.Owner at construction — immutable, so
	// reads need no lock. When the package-level ArchiveHook is installed,
	// each finalized recording is archived under <owner>/<role>.wav where
	// role is the recording file's basename.
	owner string
}

type recordingSession struct {
	file     *os.File
	path     string
	cancel   context.CancelFunc
	done     chan struct{}
	archived bool // guards double-archive across StopRecording + stopMedia
}

// ArchiveHook, when set, is invoked once per finalized recording with the
// owning test's name, the leg's role (the recording file's basename, e.g.
// "dial-caller"), and the path to the captured raw PCM. It is installed by
// TestMain (see internal/recording) so the sip package needs no dependency
// on config/recording. nil = no archiving (release-gate default).
var ArchiveHook func(test, role, pcmPath string)

// SetArchiveHook installs the per-leg recording archive hook. Pass nil to
// disable. Not safe to call concurrently with active recordings — call once
// in TestMain before any test runs.
func SetArchiveHook(fn func(test, role, pcmPath string)) { ArchiveHook = fn }

// --- construction (called by UAS/UAC internals) ---

func newInboundCall(d *diago.DialogServerSession, owner string) *Call {
	c := &Call{
		direction: Inbound,
		created:   time.Now(),
		in:        d,
		state:     StateInit,
		stateCh:   make(chan struct{}, 1),
		doneCh:    make(chan struct{}),
		owner:     owner,
	}
	c.recordReceived(newRequestMsg(MsgRecv, d.InviteRequest))
	// Route in-dialog requests (BYE, INFO, re-INVITE, ...) back to this Call
	// via the middleware in observer.go.
	registry.register(c.CallID(), c)
	d.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			c.setState(StateEnded, "remote-bye")
		}
	})
	return c
}

func newOutboundCall(d *diago.DialogClientSession, owner string) *Call {
	c := &Call{
		direction: Outbound,
		created:   time.Now(),
		out:       d,
		state:     StateInit,
		stateCh:   make(chan struct{}, 1),
		doneCh:    make(chan struct{}),
		owner:     owner,
	}
	// Record the INVITE we sent.
	if d.InviteRequest != nil {
		c.recordSent(newRequestMsg(MsgSent, d.InviteRequest))
	}
	registry.register(c.CallID(), c)
	d.OnState(func(s sip.DialogState) {
		if s == sip.DialogStateEnded {
			c.setState(StateEnded, "remote-bye")
		}
	})
	return c
}

func (c *Call) setState(s State, reason string) {
	c.mu.Lock()
	if s == c.state {
		c.mu.Unlock()
		return
	}
	c.state = s
	if s == StateAnswered && c.answeredAt.IsZero() {
		c.answeredAt = time.Now()
	}
	endedNow := false
	if s == StateEnded && c.endedAt.IsZero() {
		c.endedAt = time.Now()
		c.endReason = reason
		close(c.doneCh)
		endedNow = true
	}
	c.mu.Unlock()
	if endedNow {
		registry.unregister(c.CallID())
		// Finalize media on remote-initiated teardown too (BYE from
		// jambonz). Local Hangup() already ran stopMedia before signalling;
		// this path covers calls the peer ends, where nothing else closes
		// the recording file — without it the recording is never flushed/
		// archived (ADR-0016). Async: setState runs on diago's dialog-state
		// callback and stopMedia blocks until the audio pump drains.
		go c.stopMedia()
	}
	// wake all WaitState watchers
	for i := 0; i < 4; i++ {
		select {
		case c.stateCh <- struct{}{}:
		default:
		}
	}
}

func (c *Call) recordSent(m Message)     { c.mu.Lock(); c.sent = append(c.sent, m); c.mu.Unlock() }
func (c *Call) recordReceived(m Message) { c.mu.Lock(); c.recv = append(c.recv, m); c.mu.Unlock() }

// --- state transitions ---

// Trying sends 100 Trying. Inbound only.
func (c *Call) Trying() error {
	if c.direction != Inbound {
		return fmt.Errorf("Trying: inbound only")
	}
	if s := c.State(); s != StateInit {
		return invalidState("Trying", s, StateInit)
	}
	if err := c.in.Trying(); err != nil {
		return fmt.Errorf("Trying: %w", err)
	}
	c.setState(StateTrying, "")
	return nil
}

// Ringing sends 180 Ringing. Inbound only.
func (c *Call) Ringing() error {
	if c.direction != Inbound {
		return fmt.Errorf("Ringing: inbound only")
	}
	if s := c.State(); s != StateInit && s != StateTrying {
		return invalidState("Ringing", s, StateInit, StateTrying)
	}
	if err := c.in.Ringing(); err != nil {
		return fmt.Errorf("Ringing: %w", err)
	}
	c.setState(StateRinging, "")
	return nil
}

// SendEarlyMedia183 sends a raw 183 Session Progress carrying `sdp` as the
// message body. Inbound (UAS) only, and only before the call is answered. Safe
// to call more than once on the same call — each invocation emits another 183
// on the same early dialog (the To-tag stays constant), so a test can drive a
// far end that re-sends session progress with a CHANGING codec.
//
// Why raw instead of diago's ProgressMedia: diago locks its media session to
// the codecs of the FIRST 183 (initMediaSessionFromConf is a no-op once a
// session exists), so it cannot emit a sequence of 183s whose offered codec
// changes between them. That changing-codec sequence is exactly what triggers
// the sbc-outbound "empty m= payload type" regression (see
// outbound-error-payload-type-missing.log: PCMA -> PCMU -> PCMA across three
// successive 183s). Sending the SDP verbatim gives the test control over every
// m= line.
//
// No local RTP session is created — early-media *audio* is out of scope for
// this signaling reproduction; rtpengine performs its SDP transform regardless
// of whether media actually flows. Build the body with EarlyMediaSDP. Leaves
// the call in StateRinging; follow with Answer() or Hangup().
func (c *Call) SendEarlyMedia183(sdp []byte) error {
	if c.direction != Inbound {
		return fmt.Errorf("SendEarlyMedia183: inbound only")
	}
	if s := c.State(); s != StateInit && s != StateTrying && s != StateRinging {
		return invalidState("SendEarlyMedia183", s, StateInit, StateTrying, StateRinging)
	}
	hdrs := []sip.Header{sip.NewHeader("Content-Type", "application/sdp")}
	if err := c.in.Respond(183, "Session Progress", sdp, hdrs...); err != nil {
		return fmt.Errorf("SendEarlyMedia183: %w", err)
	}
	c.setState(StateRinging, "")
	return nil
}

// EarlyMediaSDP builds a minimal single-audio-codec SDP body for a 183
// early-media response, mirroring the shape real carriers send (one G.711
// codec + telephone-event). codec is "PCMU" or "PCMA". host/port name the RTP
// endpoint; routability is irrelevant to a signaling reproduction. sessionID
// varies the origin line so successive bodies aren't byte-identical (some
// stacks ignore a repeated o= version).
func EarlyMediaSDP(codec, host string, port, sessionID int) ([]byte, error) {
	var pt int
	up := strings.ToUpper(codec)
	switch up {
	case "PCMU":
		pt = 0
	case "PCMA":
		pt = 8
	default:
		return nil, fmt.Errorf("EarlyMediaSDP: unsupported codec %q (want PCMU or PCMA)", codec)
	}
	body := fmt.Sprintf(
		"v=0\r\n"+
			"o=SmokeTester %d %d IN IP4 %s\r\n"+
			"s=SmokeTester Early Media\r\n"+
			"c=IN IP4 %s\r\n"+
			"t=0 0\r\n"+
			"m=audio %d RTP/AVP %d 101\r\n"+
			"a=rtpmap:%d %s/8000\r\n"+
			"a=rtpmap:101 telephone-event/8000\r\n"+
			"a=fmtp:101 0-16\r\n"+
			"a=ptime:20\r\n"+
			"a=sendrecv\r\n",
		sessionID, sessionID, host, host, port, pt, pt, up)
	return []byte(body), nil
}

// Answer accepts the call with 200 OK (inbound) or marks an already-answered
// outbound call (UAC.Invite blocks until 200 OK; calling Answer is a no-op
// transition for outbound, provided for symmetry).
func (c *Call) Answer() error {
	if s := c.State(); s == StateAnswered || s == StateEnded {
		return invalidState("Answer", s, StateInit, StateTrying, StateRinging)
	}
	if c.direction == Inbound {
		if err := c.in.Answer(); err != nil {
			return fmt.Errorf("Answer: %w", err)
		}
	}
	c.setState(StateAnswered, "")
	// Capture negotiated codec from the audio reader.
	m := diago.MediaProps{}
	if c.direction == Inbound {
		_, _ = c.in.AudioReader(diago.WithAudioReaderMediaProps(&m))
	} else {
		_, _ = c.out.AudioReader(diago.WithAudioReaderMediaProps(&m))
	}
	c.mediaMu.Lock()
	c.codec = m.Codec.Name
	c.mediaMu.Unlock()
	return nil
}

// Reject sends a final failure response (4xx/5xx/6xx). Inbound only.
func (c *Call) Reject(code int, reason string) error {
	if c.direction != Inbound {
		return fmt.Errorf("Reject: inbound only")
	}
	if code < 400 || code > 699 {
		return fmt.Errorf("Reject: code %d is not a failure response", code)
	}
	if s := c.State(); s == StateAnswered || s == StateEnded {
		return invalidState("Reject", s, StateInit, StateTrying, StateRinging)
	}
	if err := c.in.Respond(code, reason, nil); err != nil {
		return fmt.Errorf("Reject: %w", err)
	}
	c.setState(StateEnded, fmt.Sprintf("rejected %d %s", code, reason))
	return nil
}

// Hangup sends BYE (or CANCEL if not yet answered). Both directions.
// Idempotent: safe to call after the call has already ended.
// SendInfo sends an in-dialog INFO request. The call must be answered.
// Returns the peer's response. Call must be answered.
//
// Typical uses:
//   - application/dtmf-relay body for out-of-band DTMF
//   - custom application/x-* content types
//
// The sent request and received response are both captured in Sent()/Received()
// via the observer middleware.
func (c *Call) SendInfo(ctx context.Context, contentType string, body []byte, extra ...sip.Header) (*sip.Response, error) {
	return c.doInDialog(ctx, sip.INFO, contentType, body, extra...)
}

// SendMessage sends an in-dialog MESSAGE request. The call must be answered.
// Typical body is text/plain with the message text.
func (c *Call) SendMessage(ctx context.Context, contentType string, body []byte, extra ...sip.Header) (*sip.Response, error) {
	return c.doInDialog(ctx, sip.MESSAGE, contentType, body, extra...)
}

// SendRefer sends a REFER to the peer. `target` is the URI to transfer to.
// diago handles the resulting NOTIFY subscription internally.
func (c *Call) SendRefer(ctx context.Context, target sip.Uri) error {
	if s := c.State(); s != StateAnswered {
		return invalidState("SendRefer", s, StateAnswered)
	}
	if c.direction == Inbound {
		return c.in.Refer(ctx, target)
	}
	return c.out.Refer(ctx, target)
}

// SendReinvite sends an in-dialog re-INVITE carrying custom headers, waits
// for the final response, and ACKs a 2xx. Outbound (UAC) calls only.
//
// diago's DialogClientSession only auto-ACKs re-INVITEs it RECEIVES
// in-dialog (see handleReInviteACK); a re-INVITE this side SENDS is not
// auto-ACKed, so the 2xx here must be ACKed explicitly. We also can't use
// diago's own ReInvite/Ack: ReInvite can't carry arbitrary headers, and Ack
// re-ACKs the ORIGINAL InviteResponse (finalizing media again) rather than
// this re-INVITE's response.
//
// Callers must NOT pass a "Content-Type" header — it's set automatically to
// application/sdp. For RFC 4028 session-timer refresh, callers MUST include
// a Session-Expires header: drachtio cancels its session-expiry timer on
// any received re-INVITE and only re-arms it from that header, so omitting
// it disables refresh.
//
// After a successful refresh, AnsweredResponse()/AnsweredStatus() report the
// LAST INVITE 200 OK (this re-INVITE's), not the original negotiation —
// read the initial answer before calling SendReinvite if you need it.
func (c *Call) SendReinvite(ctx context.Context, headers H) (*sip.Response, error) {
	if c.direction != Outbound {
		return nil, fmt.Errorf("SendReinvite: outbound only")
	}
	if s := c.State(); s != StateAnswered {
		return nil, invalidState("SendReinvite", s, StateAnswered)
	}
	contact := c.out.RemoteContact()
	if contact == nil {
		return nil, fmt.Errorf("SendReinvite: no remote contact on dialog")
	}
	sdp := c.mediaSession().LocalSDP()
	if len(sdp) == 0 {
		return nil, fmt.Errorf("SendReinvite: no local SDP on dialog")
	}

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(sip.HeaderClone(c.out.InviteRequest.Contact()))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}
	req.SetBody(sdp)

	c.recordSent(newRequestMsg(MsgSent, req))
	res, err := c.out.Do(ctx, req)
	if res != nil {
		c.recordReceived(newResponseMsg(MsgRecv, res))
	}
	if err != nil {
		return res, fmt.Errorf("SendReinvite: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Non-2xx final responses are auto-ACKed by the transaction layer;
		// nothing more to do here.
		return res, fmt.Errorf("reinvite rejected: %d %s", res.StatusCode, res.Reason)
	}

	ackTo := res.Contact()
	if ackTo == nil {
		ackTo = contact
	}
	ack := sip.NewRequest(sip.ACK, ackTo.Address)
	c.recordSent(newRequestMsg(MsgSent, ack))
	return res, c.out.WriteRequest(ack)
}

// SendReinviteWithSDP sends an in-dialog re-INVITE carrying a caller-supplied
// SDP offer (rather than reusing the current local SDP, which the far end
// treats as a session-timer refresh and never renegotiates media for). Use it
// to reproduce a media-changing re-INVITE — e.g. a carrier moving its RTP port
// mid-call — which forces the far end to renegotiate its media endpoint.
//
// Same UAC/ACK contract as SendReinvite: outbound calls only, 2xx is ACKed
// here, non-2xx is auto-ACKed by the transaction layer. The returned response
// is the final response (200 on success; the offending status on rejection).
func (c *Call) SendReinviteWithSDP(ctx context.Context, sdp []byte, headers H) (*sip.Response, error) {
	if c.direction != Outbound {
		return nil, fmt.Errorf("SendReinviteWithSDP: outbound only")
	}
	if s := c.State(); s != StateAnswered {
		return nil, invalidState("SendReinviteWithSDP", s, StateAnswered)
	}
	if len(sdp) == 0 {
		return nil, fmt.Errorf("SendReinviteWithSDP: empty sdp")
	}
	contact := c.out.RemoteContact()
	if contact == nil {
		return nil, fmt.Errorf("SendReinviteWithSDP: no remote contact on dialog")
	}

	req := sip.NewRequest(sip.INVITE, contact.Address)
	req.AppendHeader(sip.HeaderClone(c.out.InviteRequest.Contact()))
	req.AppendHeader(sip.NewHeader("Content-Type", "application/sdp"))
	for k, v := range headers {
		req.AppendHeader(sip.NewHeader(k, v))
	}
	req.SetBody(sdp)

	c.recordSent(newRequestMsg(MsgSent, req))
	res, err := c.out.Do(ctx, req)
	if res != nil {
		c.recordReceived(newResponseMsg(MsgRecv, res))
	}
	if err != nil {
		return res, fmt.Errorf("SendReinviteWithSDP: %w", err)
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		// Non-2xx final responses are auto-ACKed by the transaction layer.
		return res, nil
	}

	ackTo := res.Contact()
	if ackTo == nil {
		ackTo = contact
	}
	ack := sip.NewRequest(sip.ACK, ackTo.Address)
	c.recordSent(newRequestMsg(MsgSent, ack))
	return res, c.out.WriteRequest(ack)
}

func (c *Call) doInDialog(ctx context.Context, method sip.RequestMethod, contentType string, body []byte, extra ...sip.Header) (*sip.Response, error) {
	if s := c.State(); s != StateAnswered {
		return nil, invalidState(method.String(), s, StateAnswered)
	}
	var contact *sip.ContactHeader
	if c.direction == Inbound {
		contact = c.in.RemoteContact()
	} else {
		contact = c.out.RemoteContact()
	}
	if contact == nil {
		return nil, fmt.Errorf("%s: no remote contact on dialog", method.String())
	}
	req := sip.NewRequest(method, contact.Address)
	if contentType != "" {
		req.AppendHeader(sip.NewHeader("Content-Type", contentType))
	}
	for _, h := range extra {
		req.AppendHeader(h)
	}
	if body != nil {
		req.SetBody(body)
	}
	c.recordSent(newRequestMsg(MsgSent, req))
	var res *sip.Response
	var err error
	if c.direction == Inbound {
		res, err = c.in.Do(ctx, req)
	} else {
		res, err = c.out.Do(ctx, req)
	}
	if res != nil {
		c.recordReceived(newResponseMsg(MsgRecv, res))
	}
	return res, err
}

func (c *Call) Hangup() error {
	if s := c.State(); s == StateEnded {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	c.stopMedia()
	var err error
	if c.direction == Inbound {
		err = c.in.Hangup(ctx)
	} else {
		err = c.out.Hangup(ctx)
	}
	if err != nil {
		c.setState(StateEnded, "hangup-err: "+err.Error())
		return nil
	}
	c.setState(StateEnded, "local-hangup")
	return nil
}

// --- accessors ---

func (c *Call) Direction() Direction { return c.direction }

func (c *Call) State() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state
}

func (c *Call) CallID() string {
	if c.direction == Inbound {
		return c.in.InviteRequest.CallID().Value()
	}
	return c.out.InviteRequest.CallID().Value()
}

func (c *Call) From() string {
	if c.direction == Inbound {
		return c.in.InviteRequest.From().String()
	}
	return c.out.InviteRequest.From().String()
}

func (c *Call) To() string {
	if c.direction == Inbound {
		return c.in.InviteRequest.To().String()
	}
	return c.out.InviteRequest.To().String()
}

// Header reads a header from the initial INVITE (the request we received for
// inbound, the request we sent for outbound).
func (c *Call) Header(name string) string {
	var req *sip.Request
	if c.direction == Inbound {
		req = c.in.InviteRequest
	} else {
		req = c.out.InviteRequest
	}
	h := req.GetHeader(name)
	if h == nil {
		return ""
	}
	return h.Value()
}

// Duration returns elapsed time from Answer to Ended (or zero if not yet
// answered / not yet ended).
func (c *Call) Duration() time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.answeredAt.IsZero() {
		return 0
	}
	end := c.endedAt
	if end.IsZero() {
		end = time.Now()
	}
	return end.Sub(c.answeredAt)
}

func (c *Call) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

func (c *Call) EndReason() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.endReason
}

// Sent returns every SIP message the harness emitted for this call.
func (c *Call) Sent() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.sent))
	copy(out, c.sent)
	return out
}

// ReceivedByMethod returns all received requests whose Method matches.
// Empty result if none match. Method comparison is case-sensitive (SIP
// standard); pass the canonical upper-case name.
func (c *Call) ReceivedByMethod(method string) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Message
	for _, m := range c.recv {
		if m.Method == method {
			out = append(out, m)
		}
	}
	return out
}

// SentByStatus returns all sent responses whose StatusCode matches.
func (c *Call) SentByStatus(status int) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Message
	for _, m := range c.sent {
		if m.StatusCode == status {
			out = append(out, m)
		}
	}
	return out
}

// ReceivedByStatus returns all received responses whose StatusCode matches.
// Mirror of SentByStatus for the UAC side — useful for asserting on the
// final response to an outbound INVITE (e.g. answer verb → 200 OK).
func (c *Call) ReceivedByStatus(status int) []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	var out []Message
	for _, m := range c.recv {
		if m.StatusCode == status {
			out = append(out, m)
		}
	}
	return out
}

// AnsweredStatus returns the final 2xx status code of the INVITE this call
// was opened with. For UAC outbound calls, the response code from jambonz
// (typically 200). For UAS inbound calls, the code we sent in Answer
// (also typically 200). Returns 0 if the call was never answered.
func (c *Call) AnsweredStatus() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.direction == Outbound {
		// Walk recv backwards: the last 2xx for INVITE is the answer.
		for i := len(c.recv) - 1; i >= 0; i-- {
			m := c.recv[i]
			if m.StatusCode >= 200 && m.StatusCode < 300 && m.Method == "INVITE" {
				return m.StatusCode
			}
		}
		return 0
	}
	// Inbound: walk sent backwards.
	for i := len(c.sent) - 1; i >= 0; i-- {
		m := c.sent[i]
		if m.StatusCode >= 200 && m.StatusCode < 300 && m.Method == "INVITE" {
			return m.StatusCode
		}
	}
	return 0
}

// AwaitReceivedRequest polls the recorded-received-messages slice every
// 200ms (under the same mutex the other recording accessors use) until a
// recorded message matches one of `methods`, or ctx is cancelled/expires.
//
// StatusCode == 0 is the only reliable request/response discriminator here:
// Message.Method is populated for RESPONSES too (copied from the CSeq
// header, see newResponseMsg in messages.go), so a Method-only match would
// also match the request's own reply (e.g. matching "INVITE" would match
// the 200 OK we send back to it). Filtering StatusCode == 0 restricts the
// match to actual requests.
func (c *Call) AwaitReceivedRequest(ctx context.Context, methods ...string) (Message, error) {
	match := func() (Message, bool) {
		c.mu.Lock()
		defer c.mu.Unlock()
		for _, m := range c.recv {
			if m.StatusCode != 0 {
				continue
			}
			for _, method := range methods {
				if strings.EqualFold(m.Method, method) {
					return m, true
				}
			}
		}
		return Message{}, false
	}
	if m, ok := match(); ok {
		return m, nil
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return Message{}, ctx.Err()
		case <-ticker.C:
			if m, ok := match(); ok {
				return m, nil
			}
		}
	}
}

// AnsweredResponse returns the recorded INVITE 200 OK for this call, if any.
// bool is false if the call hasn't been answered (mirrors AnsweredStatus's
// direction-aware scan of sent/recv).
func (c *Call) AnsweredResponse() (Message, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.direction == Outbound {
		for i := len(c.recv) - 1; i >= 0; i-- {
			m := c.recv[i]
			if m.StatusCode == 200 && m.Method == "INVITE" {
				return m, true
			}
		}
		return Message{}, false
	}
	for i := len(c.sent) - 1; i >= 0; i-- {
		m := c.sent[i]
		if m.StatusCode == 200 && m.Method == "INVITE" {
			return m, true
		}
	}
	return Message{}, false
}

// Received returns every SIP message the harness received for this call.
func (c *Call) Received() []Message {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Message, len(c.recv))
	copy(out, c.recv)
	return out
}

// MethodsReceived returns the SIP method of every received in-dialog
// request, in arrival order. Convenience wrapper for the common
// "did we see a BYE / INFO / REFER?" assertion. Method-less entries
// (responses) are skipped.
func (c *Call) MethodsReceived() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.recv))
	for _, m := range c.recv {
		if m.Method != "" {
			out = append(out, m.Method)
		}
	}
	return out
}

// --- state waiting ---

// Done returns a channel closed when the call reaches StateEnded.
func (c *Call) Done() <-chan struct{} { return c.doneCh }

// WaitState blocks until the call reaches target state, or ctx expires.
// Returns an error if the state is unreachable from the current one.
func (c *Call) WaitState(ctx context.Context, target State) error {
	for {
		s := c.State()
		if s == target {
			return nil
		}
		if s == StateEnded && target != StateEnded {
			return invalidState("WaitState", s, target)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.stateCh:
			continue
		}
	}
}

// --- media: recording ---

// StartRecording begins writing inbound audio to path (PCM16 mono 8 kHz)
// AND starts decoding incoming RFC 2833 DTMF events into ReceivedDTMF().
// Must be called after Answer. Auto-stops when the call ends.
func (c *Call) StartRecording(path string) error {
	if s := c.State(); s != StateAnswered {
		return invalidState("StartRecording", s, StateAnswered)
	}
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	if c.recording != nil {
		return errors.New("StartRecording: already recording")
	}
	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("StartRecording: %w", err)
	}

	var dm *diago.DialogMedia
	if c.direction == Inbound {
		dm = &c.in.DialogMedia
	} else {
		dm = &c.out.DialogMedia
	}

	// Attach a DTMF reader to the audio pipeline. RFC 2833 telephone-events
	// come in-band over RTP; diago's DTMFReader scans incoming RTP packets
	// and invokes OnDTMF for each completed event.
	dtmfReader := &diago.DTMFReader{}
	dtmfReader.OnDTMF(func(digit rune) error {
		ev := DTMFEvent{Digit: string(digit), Timestamp: time.Now()}
		c.mediaMu.Lock()
		c.dtmf = append(c.dtmf, ev)
		c.mediaMu.Unlock()
		return nil
	})

	props := diago.MediaProps{}
	reader, err := dm.AudioReader(
		diago.WithAudioReaderMediaProps(&props),
		diago.WithAudioReaderDTMF(dtmfReader),
	)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("StartRecording: %w", err)
	}
	decoder, err := audio.NewPCMDecoderReader(props.Codec.PayloadType, reader)
	if err != nil {
		_ = f.Close()
		return fmt.Errorf("StartRecording: %w", err)
	}
	c.codec = props.Codec.Name

	ctx, cancel := context.WithCancel(context.Background())
	sess := &recordingSession{file: f, path: path, cancel: cancel, done: make(chan struct{})}
	c.recording = sess
	c.rawPCMPath = path

	go c.pumpAudio(ctx, decoder, f, sess.done)
	return nil
}

// StopRecording closes the active recording. Idempotent.
func (c *Call) StopRecording() {
	c.mediaMu.Lock()
	sess := c.recording
	c.recording = nil
	c.mediaMu.Unlock()
	if sess == nil {
		return
	}
	sess.cancel()
	<-sess.done
	_ = sess.file.Close()
	c.finalizeRecording(sess)
}

// finalizeRecording archives the just-closed recording as WAV (ADR-0016),
// exactly once per session. Identity needs no per-test wiring: the owning
// test comes from the Stack the call was born on, and the role is the
// recording file's basename (e.g. ".../dial-caller.pcm" → "dial-caller"),
// which every recording site already names meaningfully. Must be called
// only after sess.file is closed so the PCM is fully flushed to disk.
func (c *Call) finalizeRecording(sess *recordingSession) {
	if sess == nil || ArchiveHook == nil || c.owner == "" || sess.path == "" {
		return
	}
	c.mediaMu.Lock()
	if sess.archived {
		c.mediaMu.Unlock()
		return
	}
	sess.archived = true
	c.mediaMu.Unlock()

	role := filepath.Base(sess.path)
	role = strings.TrimSuffix(role, filepath.Ext(role))
	ArchiveHook(c.owner, role, sess.path)
}

func (c *Call) pumpAudio(ctx context.Context, r io.Reader, w io.Writer, done chan<- struct{}) {
	defer close(done)
	buf := make([]byte, media.RTPBufSize)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		n, rerr := r.Read(buf)
		if n > 0 {
			_, _ = w.Write(buf[:n])
			c.mediaMu.Lock()
			c.pcmBytesIn += int64(n)
			for i := 0; i+1 < n; i += 2 {
				s := int16(binary.LittleEndian.Uint16(buf[i : i+2]))
				c.sumSqIn += float64(s) * float64(s)
				c.samplesIn++
			}
			if c.samplesIn > 0 {
				c.rmsIn = sqrt(c.sumSqIn / float64(c.samplesIn))
			}
			c.mediaMu.Unlock()
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				c.mu.Lock()
				if c.err == nil {
					c.err = rerr
				}
				c.mu.Unlock()
			}
			return
		}
	}
}

// --- media: silence writer ---

// SendDTMF transmits an out-of-band DTMF sequence (RFC 2833) to the peer
// with 250ms per tone. Valid characters: 0-9, *, #, A-D. Call must be
// answered. Blocks until the whole sequence has been written.
func (c *Call) SendDTMF(digits string) error {
	return c.SendDTMFWithDuration(digits, 250*time.Millisecond)
}

// SendDTMFWithDuration is SendDTMF with an explicit per-tone duration.
//
// Why we don't use diago's DTMFWriter: it writes all 7 packets for a digit
// with RTP timestamp held at 0 via WriteSamples(..., 0, ...). Multi-digit
// sequences therefore share a single timestamp, and receivers (freeswitch,
// jambonz) dedup adjacent same-timestamp events and mis-decode the stream —
// e.g. "1234" is reported as "2223". We bypass DTMFWriter and drive
// RTPPacketWriter directly, incrementing the timestamp between digits so
// each event is distinguishable per RFC 2833 §3.6.
//
// Per-digit layout at 20ms ptime (8000Hz clock):
//   - N interim packets, one every 20ms, `duration` growing 160, 320, ...
//   - 3 redundant end-of-event packets (same final duration, E-bit set)
//   - Advance the RTP timestamp by (duration + ~40ms gap) so the next digit
//     starts on its own timestamp.
func (c *Call) SendDTMFWithDuration(digits string, perTone time.Duration) error {
	if s := c.State(); s != StateAnswered {
		return invalidState("SendDTMF", s, StateAnswered)
	}
	if perTone < 40*time.Millisecond {
		return fmt.Errorf("SendDTMF: perTone %v too short (min 40ms)", perTone)
	}
	var dm *diago.DialogMedia
	if c.direction == Inbound {
		dm = &c.in.DialogMedia
	} else {
		dm = &c.out.DialogMedia
	}
	pw := dm.RTPPacketWriter
	if pw == nil {
		return fmt.Errorf("SendDTMF: no RTP packet writer")
	}

	const (
		sampleRate   = 8000
		samplesPer20 = 160 // 20ms @ 8kHz
		gapSamples   = 320 // 40ms inter-digit silence (timestamp-only)
		payloadType  = 101 // CodecTelephoneEvent8000
	)
	// Tone is N packets of 20ms.
	nPackets := int(perTone / (20 * time.Millisecond))
	if nPackets < 2 {
		nPackets = 2
	}
	totalDuration := uint16(nPackets) * samplesPer20

	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()

	for _, r := range digits {
		eventCode, ok := dtmfEventCode(r)
		if !ok {
			return fmt.Errorf("SendDTMF: unsupported digit %q", r)
		}

		// Interim packets 1..N. First packet of each new DTMF event sets
		// the RTP marker bit (RFC 4733 §2.5.1.3).
		for i := 0; i < nPackets; i++ {
			ev := media.DTMFEvent{
				Event:      eventCode,
				EndOfEvent: false,
				Volume:     10,
				Duration:   uint16(i+1) * samplesPer20,
			}
			if i == 0 {
				// First packet of the event owns the digit's starting
				// timestamp; subsequent packets share it (timestamp_increment=0).
				if _, err := pw.WriteSamples(media.DTMFEncode(ev), 0, true, payloadType); err != nil {
					return fmt.Errorf("SendDTMF(%q) write: %w", digits, err)
				}
			} else {
				<-ticker.C
				if _, err := pw.WriteSamples(media.DTMFEncode(ev), 0, false, payloadType); err != nil {
					return fmt.Errorf("SendDTMF(%q) write: %w", digits, err)
				}
			}
		}
		// Single end-of-event packet with E-bit set. RFC 4733 §2.5.1.4
		// recommends 3 retransmissions for loss resilience, but in practice
		// freeswitch (and thus jambonz) treats each incoming E-packet as a
		// new "event complete" notification rather than deduping by (ts,
		// duration), so 3 copies of "1" end up as three detected "1"s.
		// Send one, advance timestamp, move on.
		<-ticker.C
		endEv := media.DTMFEvent{
			Event:      eventCode,
			EndOfEvent: true,
			Volume:     10,
			Duration:   totalDuration,
		}
		tsAdvance := uint32(totalDuration) + gapSamples
		if _, err := pw.WriteSamples(media.DTMFEncode(endEv), tsAdvance, false, payloadType); err != nil {
			return fmt.Errorf("SendDTMF(%q) end: %w", digits, err)
		}
	}
	return nil
}

// dtmfEventCode maps a DTMF rune to its RFC 2833 event code.
func dtmfEventCode(r rune) (uint8, bool) {
	switch {
	case r >= '0' && r <= '9':
		return uint8(r - '0'), true
	case r == '*':
		return 10, true
	case r == '#':
		return 11, true
	case r >= 'A' && r <= 'D':
		return uint8(r-'A') + 12, true
	case r >= 'a' && r <= 'd':
		return uint8(r-'a') + 12, true
	}
	return 0, false
}

// SendWAV streams the audio content of a RIFF WAVE file out of band as
// the test's outbound media. Only accepts 16-bit linear-PCM mono 8 kHz
// (the common "telephony-quality" WAV flavor; matches what jambonz's own
// api-server/data/test_audio.wav uses). Other formats return an error.
//
// Blocks until every 20ms frame has been emitted. If a SendSilence loop
// was running, it's stopped first so silence doesn't interleave with the
// speech frames; callers that need silence afterwards can call SendSilence
// again once SendWAV returns.
//
// Typical use: streaming a known phrase as the test's voice so jambonz's
// `gather input=[speech]` can recognize it and return a transcript via
// actionHook.
func (c *Call) SendWAV(path string) error {
	if s := c.State(); s != StateAnswered {
		return invalidState("SendWAV", s, StateAnswered)
	}
	c.mediaMu.Lock()
	if c.silenceCancel != nil {
		c.silenceCancel()
		c.silenceCancel = nil
	}
	var dm *diago.DialogMedia
	if c.direction == Inbound {
		dm = &c.in.DialogMedia
	} else {
		dm = &c.out.DialogMedia
	}
	c.mediaMu.Unlock()

	lpcm, err := readTelephonyWAV(path)
	if err != nil {
		return fmt.Errorf("SendWAV(%s): %w", path, err)
	}
	w, err := dm.AudioWriter()
	if err != nil {
		return fmt.Errorf("SendWAV: %w", err)
	}

	const samplesPerFrame = 160 // 20 ms @ 8 kHz
	const bytesPerFrame = samplesPerFrame * 2
	ulaw := make([]byte, samplesPerFrame)

	// The underlying RTPPacketWriter paces itself via its clockTicker —
	// each Write blocks on the next tick after sending. No extra ticker
	// needed here; adding one double-paces frames and stretches audio.
	frames := 0
	start := time.Now()
	for off := 0; off+bytesPerFrame <= len(lpcm); off += bytesPerFrame {
		if _, err := audio.EncodeUlawTo(ulaw, lpcm[off:off+bytesPerFrame]); err != nil {
			return fmt.Errorf("SendWAV encode: %w", err)
		}
		if _, err := w.Write(ulaw); err != nil {
			return fmt.Errorf("SendWAV write: %w", err)
		}
		frames++
	}
	slog.Debug("sip: SendWAV complete", "file", path, "frames", frames,
		"lpcm_bytes", len(lpcm), "wall", time.Since(start))
	return nil
}

// readTelephonyWAV loads a RIFF WAVE file and returns the raw LPCM bytes
// in little-endian signed 16-bit format. Rejects anything other than
// PCM / 1 channel / 8000 Hz / 16-bit — telephony tests shouldn't be
// silently resampling or down-mixing.
func readTelephonyWAV(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(blob) < 44 || string(blob[0:4]) != "RIFF" || string(blob[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	// Walk the sub-chunks: find fmt (validate) + data (extract payload).
	var (
		haveFmt     bool
		numChannels uint16
		sampleRate  uint32
		bitsPer     uint16
		data        []byte
	)
	i := 12
	for i+8 <= len(blob) {
		id := string(blob[i : i+4])
		size := int(binary.LittleEndian.Uint32(blob[i+4 : i+8]))
		body := blob[i+8 : i+8+size]
		switch id {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("fmt chunk too small (%d bytes)", size)
			}
			format := binary.LittleEndian.Uint16(body[0:2])
			if format != 1 {
				return nil, fmt.Errorf("WAV format %d not PCM", format)
			}
			numChannels = binary.LittleEndian.Uint16(body[2:4])
			sampleRate = binary.LittleEndian.Uint32(body[4:8])
			bitsPer = binary.LittleEndian.Uint16(body[14:16])
			haveFmt = true
		case "data":
			data = body
		}
		i += 8 + size
		// RIFF chunks pad to even length.
		if size%2 == 1 {
			i++
		}
	}
	if !haveFmt {
		return nil, errors.New("fmt chunk not found")
	}
	if data == nil {
		return nil, errors.New("data chunk not found")
	}
	if numChannels != 1 || sampleRate != 8000 || bitsPer != 16 {
		return nil, fmt.Errorf("WAV must be mono 8000 Hz 16-bit; got %d ch / %d Hz / %d-bit",
			numChannels, sampleRate, bitsPer)
	}
	return data, nil
}

// SendSilence starts sending 20 ms PCMU silence frames (50 Hz). Needed for
// symmetric-RTP NAT traversal — without outbound RTP, jambonz can't return
// audio to us. Call after Answer. No-op if already running; auto-stops on
// hangup.
func (c *Call) SendSilence() error {
	if s := c.State(); s != StateAnswered {
		return invalidState("SendSilence", s, StateAnswered)
	}
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	if c.silenceCancel != nil {
		return nil
	}
	var dm *diago.DialogMedia
	if c.direction == Inbound {
		dm = &c.in.DialogMedia
	} else {
		dm = &c.out.DialogMedia
	}
	w, err := dm.AudioWriter()
	if err != nil {
		return fmt.Errorf("SendSilence: %w", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	c.silenceCancel = cancel
	go func() {
		frame := make([]byte, 160)
		for i := range frame {
			frame[i] = 0xFF
		}
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if _, err := w.Write(frame); err != nil {
					return
				}
			}
		}
	}()
	return nil
}

func (c *Call) stopMedia() {
	c.mediaMu.Lock()
	if c.silenceCancel != nil {
		c.silenceCancel()
		c.silenceCancel = nil
	}
	rec := c.recording
	c.recording = nil
	c.mediaMu.Unlock()
	if rec != nil {
		rec.cancel()
		<-rec.done
		_ = rec.file.Close()
		c.finalizeRecording(rec)
	}
}

// --- media observations ---

func (c *Call) Codec() string {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.codec
}

// --- media session accessors (available after Answer) ---

// LocalRTPAddr returns the local RTP address the harness is listening on
// (host:port). Empty string before Answer.
func (c *Call) LocalRTPAddr() string {
	if ms := c.mediaSession(); ms != nil {
		return ms.Laddr.String()
	}
	return ""
}

// RemoteRTPAddr returns the peer's RTP address as negotiated via SDP
// (host:port). Empty string before Answer.
func (c *Call) RemoteRTPAddr() string {
	if ms := c.mediaSession(); ms != nil {
		return ms.Raddr.String()
	}
	return ""
}

// LocalSDP returns the SDP we offered or answered with. nil before Answer.
func (c *Call) LocalSDP() []byte {
	if ms := c.mediaSession(); ms != nil {
		return ms.LocalSDP()
	}
	return nil
}

// mediaSession resolves to diago's MediaSession for either direction.
// Nil until Answer has run (no negotiation has happened yet).
func (c *Call) mediaSession() *media.MediaSession {
	var dm *diago.DialogMedia
	if c.direction == Inbound {
		if c.in == nil {
			return nil
		}
		dm = &c.in.DialogMedia
	} else {
		if c.out == nil {
			return nil
		}
		dm = &c.out.DialogMedia
	}
	return dm.MediaSession()
}

func (c *Call) PCMBytesIn() int64 {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.pcmBytesIn
}

func (c *Call) AudioDuration() time.Duration {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return time.Duration(c.samplesIn) * time.Second / 8000
}

func (c *Call) RMS() float64 {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.rmsIn
}

// RawPCMPath is the filesystem path of the most-recent recording.
func (c *Call) RawPCMPath() string {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	return c.rawPCMPath
}

// ReceivedDTMF returns the DTMF events captured so far (empty until we wire
// DTMF reading — not yet implemented in the stepwise API).
func (c *Call) ReceivedDTMF() []DTMFEvent {
	c.mediaMu.Lock()
	defer c.mediaMu.Unlock()
	out := make([]DTMFEvent, len(c.dtmf))
	copy(out, c.dtmf)
	return out
}

func sqrt(v float64) float64 {
	if v <= 0 {
		return 0
	}
	g := v / 2
	for i := 0; i < 6; i++ {
		g = (g + v/g) / 2
	}
	return g
}
