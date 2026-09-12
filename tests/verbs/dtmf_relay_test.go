// Reproduction for the customer-reported "DTMF not reaching the callee"
// issue seen in rfc2833.pcap: caller sends RFC 2833, jambonz bridges to an
// outbound leg, callee receives nothing.
//
// The question these tests answer: is the break in DTMF *relay* itself, or
// is it a function of whether the feature server stays anchored in the
// media path? Same flow three times, differing only in the dial verb's
// media-path option:
//
//	default        — let jambonz choose
//	anchorMedia    — feature server stays in the media path (customer's case)
//	exitMediaPath  — feature server drops out after the bridge
//
// Topology matches the capture: our caller UAS -> SBC -> feature server ->
// SBC -> our callee UAS. Digits are the customer's own 16-digit string.
package verbs

import (
	"context"
	"fmt"
	"net"
	"sync/atomic"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// dtmfRelayDigits is the sequence from the customer capture (16 digits).
const dtmfRelayDigits = "0999633429620926"

func TestVerb_Dial_DTMFRelay_Default(t *testing.T) {
	requirePublicRTPAddress(t)
	expectDTMFRelay(t, "dtmf-relay-default", nil)
}

// requirePublicRTPAddress skips a test that depends on jambonz choosing to
// release media. With media released the SBC sends RTP to the address in our
// SDP verbatim; from behind NAT that is an RFC 1918 address the cluster cannot
// reach, so the digits leave the SBC and go nowhere. Anchored media works
// either way because rtpengine latches onto the source it actually sees.
func requirePublicRTPAddress(t *testing.T) {
	t.Helper()
	c, err := net.Dial("udp", "8.8.8.8:80")
	if err != nil {
		t.Skipf("cannot determine the local RTP address: %v", err)
	}
	defer c.Close()
	ip := c.LocalAddr().(*net.UDPAddr).IP
	if ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		t.Skipf("harness advertises %s in SDP; a released-media path needs a publicly "+
			"reachable RTP address, so this would fail on NAT rather than on jambonz", ip)
	}
}

func TestVerb_Dial_DTMFRelay_AnchorMedia(t *testing.T) {
	expectDTMFRelay(t, "dtmf-relay-anchored", []any{"anchorMedia", true})
}

func TestVerb_Dial_DTMFRelay_ExitMediaPath(t *testing.T) {
	expectDTMFRelay(t, "dtmf-relay-released", []any{"exitMediaPath", true})
}

// expectDTMFRelay bridges two UASes via `dial`, sends RFC 2833 from the
// caller leg, and reports what the callee leg actually received.
//
// Steps:
//  1. script-dial-to-callee   — [dial target=callee-uas (+dialExtra), hangup]
//  2. spawn-callee-goroutine  — answer, record (arms the 2833 decoder), hold
//  3. place-caller            — POST /Calls, answer caller leg, send silence
//  4. wait-callee-answered    — both legs up before any digit is sent
//  5. send-dtmf               — 16 digits, 100ms per tone
//  6. hangup                  — drain, tear down, join the goroutine
//  7. assert-dtmf-relayed     — compare callee's decoded digits to what we sent
func expectDTMFRelay(t *testing.T, tag string, dialExtra []any) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-dial-to-callee")
	actionURL := SessionURL(sess, "dial")
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	args := append([]any{
		"target", []any{map[string]any{"type": "user", "name": target}},
		"timeout", 20,
		"actionHook", actionURL,
	}, dialExtra...)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial", args...),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	calleeAnswered := make(chan struct{})
	var calleeCall *jsip.Call
	// tearingDown separates "the test is finishing" from "the INVITE never
	// arrived", so an abort elsewhere does not record a second, invented
	// failure against the callee.
	var tearingDown atomic.Bool
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			calleeCall = c
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "callee:ringing", "Ringing: %v", err)
				return
			}
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			// StartRecording is what attaches diago's DTMFReader, so this
			// is how the callee observes incoming 2833 at all.
			rec := t.TempDir() + "/" + tag + "-callee.pcm"
			if err := c.StartRecording(rec); err != nil {
				GoroutineFailf(t, "callee:record", "StartRecording: %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence", "SendSilence: %v", err)
				return
			}
			close(calleeAnswered)
			// Hold the leg open; the caller tears the call down.
			select {
			case <-c.Done():
			case <-calleeCtx.Done():
			}
			t.Logf("[callee] done, dtmf=%d", len(c.ReceivedDTMF()))
		case <-calleeCtx.Done():
			if !tearingDown.Load() {
				GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
			}
		}
	}()
	t.Cleanup(func() {
		tearingDown.Store(true)
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-callee-answered")
	select {
	case <-calleeAnswered:
	case <-calleeDone:
		// The goroutine exited without answering; it already logged why.
		s.Fatal("callee goroutine exited before answering")
	case <-ctx.Done():
		s.Fatalf("callee never answered: %v", ctx.Err())
	}
	s.Done()

	s = Step(t, "wait-bridge-up")
	// Wait for media to actually reach the callee rather than sleeping a fixed
	// pad: caller->callee is the direction the digits travel, so inbound bytes
	// on the callee leg are the signal that the bridge is carrying it. A pad
	// that is merely long enough yields received="" and the test would blame
	// jambonz for what is really a timing artifact.
	deadline := time.Now().Add(20 * time.Second)
	for calleeCall == nil || calleeCall.PCMBytesIn() < 8000 { // 0.5s of 8kHz PCM16
		if time.Now().After(deadline) {
			s.Fatalf("no media reached the callee leg within 20s (bridge never came up)")
		}
		select {
		case <-calleeDone:
			s.Fatal("callee goroutine exited before the bridge came up")
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.Done()

	s = Step(t, "send-dtmf")
	if err := call.SendDTMFWithDuration(dtmfRelayDigits, 100*time.Millisecond); err != nil {
		s.Fatalf("SendDTMF: %v", err)
	}
	s.Done()

	s = Step(t, "hangup")
	// Trailing slack so the last end-of-event packet is decoded before BYE.
	time.Sleep(2 * time.Second)
	_ = call.Hangup()
	calleeCancel()
	<-calleeDone
	s.Done()

	s = Step(t, "assert-dtmf-relayed")
	if calleeCall == nil {
		s.Fatal("callee call was never handed to the handler")
	}
	got := ""
	for _, e := range calleeCall.ReceivedDTMF() {
		got += e.Digit
	}
	s.Logf("%s: sent=%q received=%q (%d/%d digits), callee codec=%s rms=%.1f",
		tag, dtmfRelayDigits, got, len(got), len(dtmfRelayDigits),
		calleeCall.Codec(), calleeCall.RMS())
	if got != dtmfRelayDigits {
		s.Errorf("%s: callee received %q, want %q", tag, got, dtmfRelayDigits)
	}
	s.Done()
}
