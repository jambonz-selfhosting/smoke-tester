package sip

import (
	"sync"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// Full-wire SIP observability.
//
// Diago hides in-dialog requests (BYE, INFO, REFER, NOTIFY, re-INVITE, ACK)
// behind package-level handlers with no per-session hook. To capture them
// we plug into sipgo's request-middleware slot that diago forwards to us via
// diago.WithServerRequestMiddleware. The middleware:
//
//  1. resolves the inbound request to its Call via the Call-ID registry,
//  2. records the request on that Call,
//  3. replaces the ServerTransaction with an observedTx whose Respond
//     records the outbound response on the same Call before handing off
//     to diago's real handler.
//
// This also covers the out-of-dialog ACK/OPTIONS/... case: a lookup miss
// just means "not a call we care about" and we pass through unchanged.

// callRegistry maps Call-ID to the harness-side *Call so an inbound request
// matched on Call-ID can be routed to its owner. Entries are inserted in
// newInboundCall / newOutboundCall (see call.go) and removed when the call
// reaches StateEnded.
type callRegistry struct {
	mu    sync.RWMutex
	calls map[string]*Call
}

var registry = &callRegistry{calls: map[string]*Call{}}

func (r *callRegistry) register(callID string, c *Call) {
	if callID == "" {
		return
	}
	r.mu.Lock()
	r.calls[callID] = c
	r.mu.Unlock()
}

func (r *callRegistry) unregister(callID string) {
	if callID == "" {
		return
	}
	r.mu.Lock()
	delete(r.calls, callID)
	r.mu.Unlock()
}

func (r *callRegistry) lookup(callID string) (*Call, bool) {
	r.mu.RLock()
	c, ok := r.calls[callID]
	r.mu.RUnlock()
	return c, ok
}

// observeRequestMiddleware is the diago-compatible middleware that records
// every inbound in-dialog request and every response sent on its transaction.
// Pass it to diago.WithServerRequestMiddleware.
//
// Two details worth pinning:
//
//  1. On INVITE the Call isn't in the registry yet — newInboundCall runs
//     inside diago's handler, AFTER this middleware. We still wrap tx with
//     observedTx so that responses sent later (diago's Trying/Ringing/
//     Answer via the stored inviteTx) route through Respond() and get
//     captured. The wrapper looks the Call up lazily on each Respond.
//
//  2. On in-dialog requests (BYE/INFO/ACK/…) the Call is already
//     registered, so we record the request immediately. We still wrap
//     tx so the response diago emits back (e.g. 200 OK to BYE) is also
//     captured on the same Call.
func observeRequestMiddleware(next sipgo.RequestHandler) sipgo.RequestHandler {
	return func(req *sip.Request, tx sip.ServerTransaction) {
		var callID string
		if cid := req.CallID(); cid != nil {
			callID = cid.Value()
		}
		if c, ok := registry.lookup(callID); ok {
			// Already-registered (in-dialog) request: record it now.
			c.recordReceived(newRequestMsg(MsgRecv, req))
		}
		// Always wrap — the Call may be registered later (during INVITE
		// handling) and its responses still need to be captured.
		next(req, &observedTx{ServerTransaction: tx, callID: callID})
	}
}

// observedTx wraps a sip.ServerTransaction to record every Respond(res)
// onto the owning Call's sent list. The Call is resolved lazily via the
// registry on each Respond — at INVITE time the Call isn't registered
// yet; by the time Trying/Ringing/Answer fire (inside the handler, after
// newInboundCall has run) it is.
type observedTx struct {
	sip.ServerTransaction
	callID string
}

func (o *observedTx) Respond(res *sip.Response) error {
	if c, ok := registry.lookup(o.callID); ok {
		c.recordSent(newResponseMsg(MsgSent, res))
	}
	return o.ServerTransaction.Respond(res)
}
