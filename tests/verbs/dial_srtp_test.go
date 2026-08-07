// Test for the `dial` verb's srtpEncryption option over a sips:/TLS SIP URI.
//
// The jambonz-side contract we verify: dial srtpEncryption:"sdes" makes the
// feature-server emit X-Jambonz-SRTP: sdes, and sbc-outbound then offers SRTP
// (RTP/SAVP + a=crypto) on the outbound INVITE — versus a plain RTP/AVP offer
// when the option is absent. That contract lives entirely in the SDP of the
// INVITE jambonz sends, so we assert on the received offer, not on media flow
// (media encryption itself is rtpengine's job, tested upstream).
//
// Reaching the (NAT'd) harness for a type:sip forward: the harness stands up a
// SIP-over-TLS listener locally and fronts it with an ngrok TCP tunnel (raw
// TCP passthrough — TLS is terminated at the harness). We dial the tunnel's
// public tcp host:port, so sbc-outbound connects straight through to our
// listener and we read the offer off the wire. No public IP, no pcap.
//
// The probe listener rejects the INVITE after capturing it (488) — no media is
// exchanged (ngrok TCP can't carry RTP/UDP), which is fine: the proof is the
// SDP offer. Gated on ngrok (like the other Phase-2 tests); if a TCP tunnel
// can't be opened (ngrok plan/session limits) the test skips with the reason.
package verbs

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"
)

func TestVerb_Dial_Sip_SRTP_TLS(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	// Local SIP-over-TLS probe listener (self-signed). Inbound INVITEs land on
	// probeInbound; the stack does not register (the ngrok tunnel, not a
	// registration, is how the cluster reaches it).
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

	// Positive: dialing a sips:/TLS URI with srtpEncryption:"sdes" must produce
	// BOTH — TLS on the signalling plane (because the URI is sips:/transport=tls)
	// and SRTP on the media plane (because of srtpEncryption). Asserted on the
	// same INVITE.
	pos := probeDial(t, ctx, probeInbound, host, pubPort, true)
	s = Step(t, "assert-tls-and-srtp")
	if pos.sdp == "" {
		s.Fatal("no INVITE captured from probe with srtpEncryption=sdes (dial never reached it)")
	}
	// signalling plane: sips:/transport=tls must be honored end-to-end.
	if !strings.EqualFold(pos.transport, "tls") {
		s.Errorf("sips: INVITE arrived over %q, want TLS (transport not respected)", pos.transport)
	}
	// media plane: SRTP via SDES.
	if !strings.Contains(pos.sdp, "RTP/SAVP") {
		s.Errorf("srtpEncryption offer did not use SRTP transport (want RTP/SAVP); offer:\n%s", pos.sdp)
	}
	if !strings.Contains(pos.sdp, "a=crypto:") {
		s.Errorf("srtpEncryption offer had no a=crypto SDES line; offer:\n%s", pos.sdp)
	}
	if !strings.Contains(pos.sdp, "AES_CM_128_HMAC_SHA1_80") {
		s.Errorf("srtpEncryption offer missing expected SDES suite AES_CM_128_HMAC_SHA1_80; offer:\n%s", pos.sdp)
	}
	if strings.Contains(pos.sdp, "RTP/AVP ") {
		s.Errorf("srtpEncryption offer unexpectedly also contained a plain RTP/AVP m-line; offer:\n%s", pos.sdp)
	}
	s.Done()

	// Negative control: same sips:/TLS dial WITHOUT srtpEncryption must still be
	// TLS on signalling, but plain RTP/AVP on media — proving SRTP comes from the
	// option, not from sips:/TLS.
	neg := probeDial(t, ctx, probeInbound, host, pubPort, false)
	s = Step(t, "assert-tls-and-plain-rtp")
	if neg.sdp == "" {
		s.Fatal("no INVITE captured from probe without srtpEncryption")
	}
	if !strings.EqualFold(neg.transport, "tls") {
		s.Errorf("sips: INVITE arrived over %q, want TLS (transport not respected)", neg.transport)
	}
	if strings.Contains(neg.sdp, "RTP/SAVP") || strings.Contains(neg.sdp, "a=crypto:") {
		s.Errorf("offer without srtpEncryption unexpectedly used SRTP; offer:\n%s", neg.sdp)
	}
	if !strings.Contains(neg.sdp, "RTP/AVP") {
		s.Errorf("offer without srtpEncryption did not use plain RTP/AVP; offer:\n%s", neg.sdp)
	}
	s.Done()
}

// probeOffer is what the probe observed on the INVITE jambonz forwarded:
// the SDP body (media plane) and the transport it arrived on (signalling
// plane) — so one dial can assert both TLS and SRTP.
type probeOffer struct {
	sdp       string
	transport string
}

// probeDial places a caller call whose app dials a sips:/TLS URI pointing at
// the probe tunnel (optionally with srtpEncryption:"sdes"), captures the SDP
// offer and transport on the INVITE the probe receives, rejects it, and
// returns what it saw.
func probeDial(t *testing.T, ctx context.Context, probeInbound <-chan *jsip.Call,
	host string, port int, srtp bool) probeOffer {
	t.Helper()
	label := "plain"
	if srtp {
		label = "sdes"
	}
	callerUAS := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-dial-"+label)
	actionURL := SessionURL(sess, "dial")
	sipURI := fmt.Sprintf("sips:probe@%s:%d;transport=tls", host, port)
	kv := []any{
		"target", []any{map[string]any{"type": "sip", "sipUri": sipURI}},
		"timeout", 12,
		"actionHook", actionURL,
	}
	if srtp {
		kv = append(kv, "srtpEncryption", "sdes")
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial", kv...),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	// Capture the probe's inbound INVITE concurrently with the caller leg
	// (which blocks until the dial resolves — i.e. until we reject here).
	// Bounded so a send-out that never reaches the probe fails fast rather
	// than hanging until the whole-test deadline.
	offCh := make(chan probeOffer, 1)
	go func() {
		select {
		case c := <-probeInbound:
			var off probeOffer
			if inv := c.ReceivedByMethod("INVITE"); len(inv) > 0 && inv[0].RawRequest != nil {
				off.sdp = string(inv[0].RawRequest.Body())
				off.transport = inv[0].RawRequest.Transport()
			}
			_ = c.Reject(488, "Not Acceptable Here")
			offCh <- off
		case <-time.After(30 * time.Second):
			offCh <- probeOffer{}
		case <-ctx.Done():
			offCh <- probeOffer{}
		}
	}()

	s = Step(t, "place-caller-"+label)
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(30))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	return <-offCh
}

// freeTCPPort asks the OS for an unused TCP port on loopback.
func freeTCPPort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

// startSIPTCPTunnel opens an ngrok TCP endpoint that forwards raw TCP to the
// harness's local TLS SIP listener, and returns the public host+port. Skips
// the test if a TCP tunnel can't be established (e.g. ngrok plan/session
// limits alongside the webhook tunnel).
func startSIPTCPTunnel(t *testing.T, localPort int) (string, int, func()) {
	t.Helper()
	backend, err := url.Parse(fmt.Sprintf("tcp://127.0.0.1:%d", localPort))
	if err != nil {
		t.Fatalf("startSIPTCPTunnel: parse backend: %v", err)
	}
	tunCtx, cancel := context.WithCancel(context.Background())
	fwd, err := ngrok.ListenAndForward(tunCtx, backend, config.TCPEndpoint(), ngrok.WithAuthtokenFromEnv())
	if err != nil {
		cancel()
		t.Skipf("ngrok TCP tunnel unavailable (%v); this test needs an ngrok token/plan that "+
			"allows a TCP endpoint alongside the webhook tunnel", err)
	}
	host, portStr, err := net.SplitHostPort(strings.TrimPrefix(fwd.URL(), "tcp://"))
	if err != nil {
		_ = fwd.Close()
		cancel()
		t.Fatalf("startSIPTCPTunnel: parse tunnel url %q: %v", fwd.URL(), err)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		_ = fwd.Close()
		cancel()
		t.Fatalf("startSIPTCPTunnel: bad tunnel port %q: %v", portStr, err)
	}
	return host, port, func() { _ = fwd.Close(); cancel() }
}
