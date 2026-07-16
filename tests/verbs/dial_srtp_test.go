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
	}, func(_ context.Context, call *jsip.Call) error {
		select {
		case probeInbound <- call:
		default:
			_ = call.Reject(486, "Busy Here")
			return nil
		}
		<-call.Done()
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
	s.Logf("tunnel tcp://%s:%d -> 127.0.0.1:%d", host, pubPort, localPort)
	s.Done()

	// Positive: with srtpEncryption:"sdes" the offer must be SRTP.
	posSDP := probeDial(t, ctx, probeInbound, host, pubPort, true)
	s = Step(t, "assert-srtp-offer")
	if posSDP == "" {
		s.Fatal("no INVITE SDP captured from probe with srtpEncryption=sdes (dial never reached it)")
	}
	if !strings.Contains(posSDP, "RTP/SAVP") {
		s.Errorf("srtpEncryption offer did not use SRTP transport (want RTP/SAVP); offer:\n%s", posSDP)
	}
	if !strings.Contains(posSDP, "a=crypto:") {
		s.Errorf("srtpEncryption offer had no a=crypto SDES line; offer:\n%s", posSDP)
	}
	if !strings.Contains(posSDP, "AES_CM_128_HMAC_SHA1_80") {
		s.Errorf("srtpEncryption offer missing expected SDES suite AES_CM_128_HMAC_SHA1_80; offer:\n%s", posSDP)
	}
	s.Done()

	// Negative control: without srtpEncryption the same dial must be plain RTP.
	// This proves the SRTP offer is caused by the option, not by sips:/TLS.
	negSDP := probeDial(t, ctx, probeInbound, host, pubPort, false)
	s = Step(t, "assert-plain-offer")
	if negSDP == "" {
		s.Fatal("no INVITE SDP captured from probe without srtpEncryption")
	}
	if strings.Contains(negSDP, "RTP/SAVP") || strings.Contains(negSDP, "a=crypto:") {
		s.Errorf("offer without srtpEncryption unexpectedly used SRTP; offer:\n%s", negSDP)
	}
	if !strings.Contains(negSDP, "RTP/AVP") {
		s.Errorf("offer without srtpEncryption did not use plain RTP/AVP; offer:\n%s", negSDP)
	}
	s.Done()
}

// probeDial places a caller call whose app dials a sips:/TLS URI pointing at
// the probe tunnel (optionally with srtpEncryption:"sdes"), captures the SDP
// offer on the INVITE the probe receives, rejects it, and returns the offer.
func probeDial(t *testing.T, ctx context.Context, probeInbound <-chan *jsip.Call,
	host string, port int, srtp bool) string {
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
	sdpCh := make(chan string, 1)
	go func() {
		select {
		case c := <-probeInbound:
			sdp := ""
			if inv := c.ReceivedByMethod("INVITE"); len(inv) > 0 && inv[0].RawRequest != nil {
				sdp = string(inv[0].RawRequest.Body())
			}
			_ = c.Reject(488, "Not Acceptable Here")
			sdpCh <- sdp
		case <-time.After(30 * time.Second):
			sdpCh <- ""
		case <-ctx.Done():
			sdpCh <- ""
		}
	}()

	s = Step(t, "place-caller-"+label)
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(30))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	return <-sdpCh
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
