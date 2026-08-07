// Reproduction test for the drachtio NAT route-rewrite TLS->UDP ACK defect.
//
// Symptom in production (see fd_issue/tls-ack-root-cause-*.md): ~25% of
// outbound `dial` calls to a `type:sip` target over TLS
// (sips:...;transport=tls) fail. The remote SBC answers 200 OK, but jambonz'
// final ACK never arrives on the TLS dialog; the SBC retransmits 200 OK and
// eventually clears the call with BYE cause=408.
//
// Root cause: when the 200 OK's Contact host differs from the packet source
// IP and there is no Record-Route, drachtio's UAC NAT detection
// (SipDialogController::processResponseOutsideDialog) rebuilds the in-dialog
// route as a BARE "sip:<source-ip>:<port>" — dropping the sips scheme and
// ;transport=tls. The ACK then defaults to UDP and is sent to the source IP
// over UDP, which the TLS-only far end never receives.
//
// How this test forces the bug deterministically:
//   - It stands up a SIP-over-TLS listener locally and fronts it with an
//     ngrok TCP tunnel (raw TCP passthrough; TLS terminates at the harness),
//     exactly like dial_srtp_test.go.
//   - jambonz dials sips:...;transport=tls at the tunnel, so the INVITE
//     arrives on our listener over TLS.
//   - Our listener ANSWERS 200 OK. Its Contact host is the listener bind
//     address (127.0.0.1), which never equals the ngrok public IP drachtio
//     sees as the packet source, and diago emits no Record-Route — so the
//     NAT-detection branch fires every time.
//   - We then wait for the ACK. On a buggy drachtio it goes out over UDP to
//     the ngrok public IP and never reaches this TLS listener, so no ACK is
//     observed -> test fails (bug reproduced). On a fixed drachtio the ACK is
//     sent over the TLS dialog and arrives -> test passes.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN (needs a TCP tunnel).
package verbs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func TestVerb_Dial_Sip_NAT_TLS_ACK(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	// 1. Local SIP-over-TLS probe listener (self-signed). Inbound INVITEs are
	// handed to the test via probeInbound; the handler blocks on call.Done()
	// (or the stack's serve ctx, see below) so diago keeps the dialog alive
	// while we wait for the ACK.
	s := Step(t, "start-tls-probe")
	localPort := freeTCPPort(t)
	probeInbound := make(chan *jsip.Call, 4)
	probe, err := jsip.Start(context.Background(), jsip.Config{
		TLSBindHost: "127.0.0.1",
		TLSBindPort: localPort,
		LogLevel:    cfg.LogLevel,
		Owner:       t.Name(),
	}, func(hctx context.Context, call *jsip.Call) error {
		select {
		case probeInbound <- call:
		default:
			_ = call.Reject(486, "Busy Here")
			return nil
		}
		// Honour the stack's serve ctx: when Stack.Stop cancels it, return so
		// dispatchInbound's safety-net Hangup runs on a leg the test
		// abandoned. Blocking unconditionally leaves the leg until jambonz's
		// own timers fire.
		select {
		case <-call.Done():
		case <-hctx.Done():
		}
		return nil
	})
	if err != nil {
		s.Fatalf("start TLS probe stack: %v", err)
	}
	t.Cleanup(probe.Stop)
	s.Done()

	// 2. ngrok TCP tunnel fronting the TLS listener so the NAT'd cluster can
	// reach it (skips the test if a TCP endpoint can't be opened).
	s = Step(t, "open-ngrok-tcp-tunnel")
	host, pubPort, closeTun := startSIPTCPTunnel(t, localPort)
	t.Cleanup(closeTun)
	// LIFO: this runs before closeTun, so Stack.Stop's drain can still reach
	// the cluster through the tunnel to BYE any leg the probe still holds.
	// The earlier t.Cleanup(probe.Stop) above stays as the safety net for a
	// Fatalf during tunnel setup; Stop is idempotent (sync.Once), so the
	// second invocation is a no-op.
	t.Cleanup(probe.Stop)
	s.Logf("tunnel tcp://%s:%d -> 127.0.0.1:%d", host, pubPort, localPort)
	s.Done()

	callerUAS := claimUAS(t, ctx)
	_, sess := claimSession(t)

	// 3. App script: dial the sips:/TLS URI pointing at the probe tunnel.
	s = Step(t, "script-dial-tls")
	actionURL := SessionURL(sess, "dial")
	sipURI := fmt.Sprintf("sips:probe@%s:%d;transport=tls", host, pubPort)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{"type": "sip", "sipUri": sipURI}},
			"timeout", 20,
			"actionHook", actionURL,
		),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	// 4. Probe goroutine: answer the forwarded INVITE, then wait for the ACK.
	type probeResult struct {
		gotINVITE bool
		transport string // transport the INVITE arrived on (want "tls")
		gotACK    bool
		noACK     bool // failure signature is a missing ACK (the bug), not a setup error
		err       error
	}
	resCh := make(chan probeResult, 1)
	go func() {
		select {
		case c := <-probeInbound:
			res := probeResult{gotINVITE: true}
			_ = c.Trying()
			_ = c.Ringing()
			if inv := c.ReceivedByMethod("INVITE"); len(inv) > 0 && inv[0].RawRequest != nil {
				res.transport = inv[0].RawRequest.Transport()
			}
			// diago's Answer() blocks until the ACK confirms the INVITE server
			// transaction. On a buggy drachtio the ACK is misrouted (UDP/TCP to
			// the ngrok public IP) and never reaches this TLS listener, so
			// Answer() returns "transaction terminated" (~32s). Distinguish that
			// no-ACK signature from an unrelated Answer failure (e.g. bad
			// SDP/codec), which is an env problem and not this bug.
			if err := c.Answer(); err != nil {
				res.err = err
				res.noACK = isNoACKSignature(err)
				_ = c.Hangup() // unblock the probe handler deterministically
				resCh <- res
				return
			}
			// Answer() returned (ACK already seen, or fast path): still require an
			// observed ACK. 15s is well past T1 backoff.
			ackCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			_, aerr := c.AwaitReceivedRequest(ackCtx, "ACK")
			cancel()
			res.gotACK = aerr == nil
			res.err = aerr
			res.noACK = aerr != nil // deadline waiting for the ACK == the bug
			_ = c.Hangup()
			resCh <- res
		case <-time.After(45 * time.Second):
			resCh <- probeResult{err: fmt.Errorf("probe never received the forwarded INVITE")}
		case <-ctx.Done():
			resCh <- probeResult{err: ctx.Err()}
		}
	}()

	// 5. Place the caller leg — this drives jambonz to run the app and dial
	// the probe. The caller leg stays up (answered, silence) until jambonz
	// tears it down after the dial resolves / the time limit hits.
	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(45))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	// 6. Assertions.
	s = Step(t, "assert-ack-over-tls")
	res := <-resCh
	if !res.gotINVITE {
		s.Fatalf("dial never reached the TLS probe (setup/env problem, not the bug): %v", res.err)
	}
	// An Answer() failure that is NOT the missing-ACK signature is an env/setup
	// problem (e.g. SDP/codec) — don't misattribute it to the drachtio bug.
	if res.err != nil && !res.gotACK && !res.noACK {
		s.Fatalf("probe could not answer the forwarded INVITE for a non-ACK reason "+
			"(setup/env problem, not the drachtio NAT bug): %v", res.err)
	}
	if res.transport != "" && !strings.EqualFold(res.transport, "tls") {
		s.Errorf("outbound INVITE arrived over %q, want TLS (sips:/transport=tls not honored)", res.transport)
	}
	if !res.gotACK {
		s.Fatalf("BUG REPRODUCED — far end answered 200 OK over TLS but jambonz never "+
			"delivered the ACK.\n"+
			"This is the drachtio NAT route-rewrite defect: on a 200 OK whose Contact "+
			"host != packet source IP and with no Record-Route, drachtio rebuilds the "+
			"in-dialog route as bare sip:<ip>:<port> (dropping ;transport=tls) and sends "+
			"the ACK over the wrong transport, which never reaches this TLS-only listener. "+
			"In production the remote SBC then retransmits 200 OK and clears the call with "+
			"BYE cause=408 (~25%% of type:sip TLS dials). err=%v", res.err)
	}
	s.Logf("ACK received over TLS — dialog completed correctly (drachtio fix present)")
	s.Done()
}

// isNoACKSignature reports whether an Answer() error indicates the 200 OK was
// never ACKed (the INVITE server transaction timed out / terminated) rather
// than an unrelated failure to build the answer. Kept string-based because
// diago surfaces these as plain errors, not typed sentinels.
func isNoACKSignature(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "transaction terminated") ||
		strings.Contains(msg, "deadline exceeded") ||
		strings.Contains(msg, "timeout")
}
