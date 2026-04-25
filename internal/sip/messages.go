package sip

import (
	"time"

	"github.com/emiago/sipgo/sip"
)

// H is a convenience header map accepted by outbound methods.
type H map[string]string

// MessageDirection distinguishes what we sent vs. what we received.
type MessageDirection int

const (
	MsgSent MessageDirection = iota
	MsgRecv
)

func (d MessageDirection) String() string {
	if d == MsgSent {
		return "sent"
	}
	return "recv"
}

// Message is a captured SIP request or response with metadata for inspection.
// Tests typically look at Method/StatusCode/Headers and not the raw *sip types.
type Message struct {
	Direction  MessageDirection
	Timestamp  time.Time
	Method     string // REGISTER/INVITE/BYE/etc. — "" for responses
	StatusCode int    // 0 for requests
	Reason     string // reason phrase for responses
	CallID     string
	CSeq       string
	Headers    H

	// Raw references — prefer the fields above. These are *sipgo types for
	// advanced inspection and may be nil if the harness synthesised the entry.
	RawRequest  *sip.Request
	RawResponse *sip.Response
}

// newRequestMsg builds a Message from a *sip.Request.
func newRequestMsg(dir MessageDirection, req *sip.Request) Message {
	m := Message{
		Direction:  dir,
		Timestamp:  time.Now(),
		Method:     req.Method.String(),
		Headers:    headerSliceToMap(req.Headers()),
		RawRequest: req,
	}
	if cid := req.CallID(); cid != nil {
		m.CallID = cid.Value()
	}
	if cs := req.CSeq(); cs != nil {
		m.CSeq = cs.Value()
	}
	return m
}

// newResponseMsg builds a Message from a *sip.Response.
func newResponseMsg(dir MessageDirection, res *sip.Response) Message {
	m := Message{
		Direction:   dir,
		Timestamp:   time.Now(),
		StatusCode:  int(res.StatusCode),
		Reason:      res.Reason,
		Headers:     headerSliceToMap(res.Headers()),
		RawResponse: res,
	}
	if cid := res.CallID(); cid != nil {
		m.CallID = cid.Value()
	}
	if cs := res.CSeq(); cs != nil {
		m.CSeq = cs.Value()
		m.Method = cs.MethodName.String() // method the response is for
	}
	return m
}

// headerSliceToMap flattens headers into a map; repeated headers concatenate.
func headerSliceToMap(hs []sip.Header) H {
	out := H{}
	for _, h := range hs {
		k := h.Name()
		v := h.Value()
		if existing, ok := out[k]; ok {
			out[k] = existing + ", " + v
		} else {
			out[k] = v
		}
	}
	return out
}

// DTMFEvent captures a single DTMF digit received during a call.
type DTMFEvent struct {
	Digit     string // "0"-"9", "*", "#", "A"-"D"
	Timestamp time.Time
	Duration  time.Duration // best-effort from RFC 2833 event
}
