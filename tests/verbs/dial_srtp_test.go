// Test for the `dial` verb with srtpEncryption over a sips:/TLS SIP URI —
// the end-to-end proof for encrypted-media outbound calls (e.g. to a
// LiveKit sips: endpoint).
//
// Unlike TestVerb_Dial_User_Bridge (which dials a registered user reached
// via REGISTER connection-reuse), this test dials an explicit sip: URI
// (`type:"sip"`). For that forward the cluster's sbc-outbound opens a fresh
// connection to the Request-URI host, so the harness must be reachable there:
// the test binds a TLS SIP listener on JAMBONZ_IT_SIP_TLS_PORT and advertises
// JAMBONZ_IT_SIP_PUBLIC_HOST. When either is unset the test skips (passes).
//
// The chain under test:
//  1. dial verb carries srtpEncryption:"sdes".
//  2. feature-server emits X-Jambonz-SRTP: sdes to sbc-outbound.
//  3. sbc-outbound forwards over TLS and offers SRTP (RTP/SAVP + a=crypto).
//  4. the harness's TLS UAS answers with SDES SRTP (diago MediaSRTP=1),
//     streams the reference WAV, and the caller records the bridged audio.
//
// Proof points: (a) the INVITE the callee received offered RTP/SAVP with an
// AES_CM_128_HMAC_SHA1_80 a=crypto line — i.e. media really was encrypted,
// not plain RTP; (b) the reference phrase survives the encrypted bridge and
// Deepgram transcribes it from the caller's recording.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN and without the reachable
// TLS-SIP config.
package verbs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func TestVerb_Dial_Sip_SRTP_TLS(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasReachableTLSSIP() {
		t.Skip("JAMBONZ_IT_SIP_PUBLIC_HOST / JAMBONZ_IT_SIP_TLS_PORT not set; " +
			"skipping dial-to-sips SRTP/TLS test (harness must be cluster-reachable for a type:sip forward)")
	}
	ctx := WithTimeout(t, 120*time.Second)

	// Caller leg: a normal registered UAS that jambonz INVITEs and whose
	// inbound RTP (the bridged audio) we record + transcribe.
	callerUAS := claimUAS(t, ctx)

	// Callee leg: a standalone TLS + SDES-SRTP listener on a fixed, cluster-
	// reachable port. It is NOT registered — the cluster reaches it directly
	// via the sip: URI in the dial target. MediaSRTP is enabled inside the
	// stack for the TLS transport.
	s := Step(t, "start-tls-srtp-callee")
	calleeInbound := make(chan *jsip.Call, 4)
	calleeStack, err := jsip.Start(context.Background(), jsip.Config{
		Transport:    "tls",
		TLSBindPort:  cfg.SIPTLSPort,
		ExternalHost: cfg.SIPPublicHost,
		LogLevel:     cfg.LogLevel,
		Owner:        t.Name(),
	}, func(_ context.Context, call *jsip.Call) error {
		select {
		case calleeInbound <- call:
		default:
			_ = call.Reject(486, "Busy Here")
			return nil
		}
		<-call.Done()
		return nil
	})
	if err != nil {
		s.Fatalf("start TLS/SRTP callee stack: %v", err)
	}
	t.Cleanup(calleeStack.Stop)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-dial-to-sips")
	actionURL := SessionURL(sess, "dial")
	// sips: + transport=tls so sbc-outbound uses TLS signalling; the user part
	// is arbitrary (our UAS answers any inbound INVITE on this transport).
	sipURI := fmt.Sprintf("sips:srtp-callee@%s:%d;transport=tls", cfg.SIPPublicHost, cfg.SIPTLSPort)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{
				"type":   "sip",
				"sipUri": sipURI,
			}},
			// the option under test — request encrypted media (SDES) on the
			// outbound leg.
			"srtpEncryption", "sdes",
			"timeout", 20,
			"actionHook", actionURL,
			// keep media anchored in the cluster data plane, same rationale as
			// TestVerb_Dial_User_Bridge.
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	var calleeCall *jsip.Call
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeInbound:
			calleeCall = c
			t.Logf("[callee:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			t.Logf("[callee:ringing] start")
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "callee:ringing", "Ringing: %v", err)
				return
			}
			t.Logf("[callee:answer] start")
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			t.Logf("[callee:silence-prime] start")
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-prime", "SendSilence: %v", err)
				return
			}
			time.Sleep(RecognizerArmDelay)
			t.Logf("[callee:send-wav] start")
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "callee:send-wav", "SendWAV: %v", err)
				return
			}
			t.Logf("[callee:silence-trail] start")
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-trail", "SendSilence: %v", err)
				return
			}
			t.Logf("[callee:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[callee] done")
		case <-calleeCtx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
		}
	}()
	t.Cleanup(func() {
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("dial-srtp-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-callee-done")
	<-calleeDone
	s.Done()

	s = Step(t, "assert-callee-sip-wire")
	if calleeCall == nil {
		s.Fatal("callee call was never handed to the handler")
	}
	RequireRecvMethods(s, calleeCall, "INVITE")
	sent := StatusesOf(calleeCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("callee sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "assert-invite-offered-srtp")
	// The strongest proof media was encrypted: the INVITE the callee received
	// must offer SRTP (RTP/SAVP transport + an a=crypto SDES line). A plain
	// (unencrypted) dial would offer RTP/AVP with no crypto and the callee's
	// auto-mirror would answer plain RTP — audio would still flow, so audio
	// alone does not prove encryption; the offer SDP does.
	inv := calleeCall.ReceivedByMethod("INVITE")
	if len(inv) == 0 || inv[0].RawRequest == nil {
		s.Fatal("no INVITE with raw request captured on callee")
	}
	offer := string(inv[0].RawRequest.Body())
	if !strings.Contains(offer, "RTP/SAVP") {
		s.Errorf("callee INVITE offer did not use SRTP transport (want RTP/SAVP); offer:\n%s", offer)
	}
	if !strings.Contains(offer, "a=crypto:") {
		s.Errorf("callee INVITE offer had no a=crypto SDES line; offer:\n%s", offer)
	}
	if !strings.Contains(offer, "AES_CM_128_HMAC_SHA1_80") {
		s.Errorf("callee INVITE offer did not include the expected SDES suite AES_CM_128_HMAC_SHA1_80; offer:\n%s", offer)
	}
	if tp := inv[0].RawRequest.Transport(); !strings.EqualFold(tp, "tls") {
		s.Errorf("callee INVITE arrived over %q, want TLS", tp)
	}
	s.Done()

	s = Step(t, "wait-action-dial-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/dial")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/dial: %v", err)
	}
	s.Logf("action/dial body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-dial-status-completed")
	if got := cb.String("dial_call_status"); got != "completed" {
		s.Errorf("dial_call_status: got %q want %q", got, "completed")
	}
	if got := cb.Int("dial_sip_status"); got != 200 {
		s.Errorf("dial_sip_status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}
