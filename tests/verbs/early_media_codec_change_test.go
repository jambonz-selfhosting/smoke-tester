// Reproduction for the sbc-outbound "missing payload type in m= line" bug.
//
// Background (outbound-error-payload-type-missing.log, captured on the
// jambonz.cloud SBC 2026-06-24): a single outbound INVITE received THREE
// `183 Session Progress` responses whose answered codec changed between them
// (PCMA -> PCMU -> PCMA). The first 183 was transcoded correctly; every
// subsequent 183 produced an SDP toward the feature server whose media line
// had NO payload type:
//
//	m=audio 56450 RTP/AVP        <-- invalid, empty format list
//	a=rtpmap:0 PCMU/8000         <-- rtpmap survives, only m= list is blanked
//
// Root cause (validated against rtpengine 11.5 on a live cluster): it is NOT a
// codec-flag issue. sbc-outbound's localSdpA callback (lib/call-session.js)
// calls the rtpengine `answer` command once per B-leg provisional response
// WITHOUT re-issuing the `offer`. rtpengine cannot process a 2nd+ `answer`
// whose codec differs from the first for the same call — it drops the audio
// codec, leaving an m= line with only telephone-event (RTP/AVP 101) which
// sbc-outbound relays verbatim (`return response.sdp`). No answer-side flag
// combination changes this; re-issuing the `offer` before each answer resets
// the negotiation and fixes it. The downstream effect of the bug is a broken
// outbound leg: the dial fails and no audio bridges.
//
// Two scenarios drive this end-to-end against a live cluster:
//
//   - CodecChange (bug): 183 PCMA -> 183 PCMU -> 183 PCMA. Forces rtpengine to
//     re-answer with a changed codec. Expected to complete once fixed; fails
//     against a cluster that answers per-183 without re-offering.
//   - SameCodec (control): 183 PCMA -> 183 PCMA -> 183 PCMA. Same number of
//     provisional responses, NO codec change, so the re-answer stays valid.
//     Isolates the trigger: if this passes while CodecChange fails, the defect
//     is specifically the codec change, not early media / the harness / the
//     advertised RTP address.
//
// Phase-2 tests; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// earlyMediaSDPHost / Port name the RTP endpoint advertised in the callee's
// 183 SDP. Routability is irrelevant: the bug is a signaling/SDP-transform
// defect (rtpengine rewrites the m= line regardless of whether media flows),
// and the callee's final 200 OK carries its real RTP address, so post-answer
// audio still bridges. TEST-NET-3 (RFC 5737) is a documentation range.
const (
	earlyMediaSDPHost = "203.0.113.20"
	earlyMediaSDPPort = 40000
)

// TestVerb_Dial_EarlyMedia_CodecChange reproduces the multi-183 codec-change
// scenario that breaks sbc-outbound's SDP transform. Passes once sbc-outbound
// re-issues the rtpengine offer before each answer; fails against a cluster
// that answers per-183 without re-offering.
func TestVerb_Dial_EarlyMedia_CodecChange(t *testing.T) {
	runEarlyMediaDial(t, []string{"PCMA", "PCMU", "PCMA"})
}

// TestVerb_Dial_EarlyMedia_SameCodec is the control: identical flow, identical
// number of 183s, but the codec never changes. Proves the harness, early media
// and the advertised RTP address are all fine — so a CodecChange failure is
// attributable to the codec change alone.
func TestVerb_Dial_EarlyMedia_SameCodec(t *testing.T) {
	runEarlyMediaDial(t, []string{"PCMA", "PCMA", "PCMA"})
}

// runEarlyMediaDial places an outbound call (jambonz -> callee via
// sbc-outbound), has the callee emit a sequence of 183 Session Progress
// responses (one per codec in codecSeq) before answering and streaming the
// reference WAV, then asserts the dial completed and audio bridged.
//
// The caller leg is answered before dial, so jambonz bridges the callee's
// early media as RTP rather than relaying 183s upstream; the externally
// observable signal is therefore the dial OUTCOME + bridged audio, not the raw
// broken SDP (which lives on the internal SBC<->FS hop and is reprocessed by
// the feature server's own rtpengine before it could reach any external leg).
func runEarlyMediaDial(t *testing.T, codecSeq []string) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-dial-to-callee")
	actionURL := SessionURL(sess, "dial")
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"timeout", 30,
			"actionHook", actionURL,
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	const interProgressGap = 2 * time.Second
	calleeDone := make(chan struct{})
	var calleeCall *jsip.Call
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			calleeCall = c
			t.Logf("[callee:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			for i, codec := range codecSeq {
				if i > 0 {
					time.Sleep(interProgressGap)
				}
				sdp, err := jsip.EarlyMediaSDP(codec, earlyMediaSDPHost, earlyMediaSDPPort, 1000+i)
				if err != nil {
					GoroutineFailf(t, "callee:build-sdp", "EarlyMediaSDP(%s): %v", codec, err)
					return
				}
				t.Logf("[callee:183 #%d] sending Session Progress codec=%s", i+1, codec)
				if err := c.SendEarlyMedia183(sdp); err != nil {
					GoroutineFailf(t, "callee:183", "SendEarlyMedia183(%s #%d): %v", codec, i+1, err)
					return
				}
			}
			time.Sleep(interProgressGap)
			t.Logf("[callee:answer] start")
			if err := c.Answer(); err != nil {
				// If the cluster tore the leg down during the broken early
				// media, Answer fails here — record it, don't abort the test.
				t.Logf("[callee:answer] FAILED (leg may have been torn down): %v", err)
				return
			}
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
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("early-media-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-callee-done")
	<-calleeDone
	s.Done()

	s = Step(t, "assert-callee-sip-wire")
	if calleeCall == nil {
		s.Fatal("callee call was never handed to the handler")
	}
	RequireRecvMethods(s, calleeCall, "INVITE")
	s.Logf("callee received methods: %v", calleeCall.MethodsReceived())
	s.Done()

	s = Step(t, "wait-action-dial-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/dial")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/dial: %v", err)
	}
	s.Logf("action/dial body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-dial-status")
	// A run of 183s must NOT break the call, whether or not the codec changes
	// between them. When sbc-outbound re-answers per 183 without re-offering, a
	// codec change corrupts the outbound leg's media (m= line loses its audio
	// payload type) and the dial fails; a fixed cluster still completes.
	dcs := cb.String("dial_call_status")
	dss := cb.Int("dial_sip_status")
	s.Logf("codec sequence=%v -> dial_call_status=%q dial_sip_status=%d", codecSeq, dcs, dss)
	if dcs != "completed" {
		s.Errorf("dial_call_status: got %q want %q (dial_sip_status=%d) — early media sequence %v broke the call",
			dcs, "completed", dss, codecSeq)
	}
	s.Done()

	s = Step(t, "assert-bridge-audio")
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	// Post-answer audio must still bridge. If the early-media sequence left
	// rtpengine in a bad state, the bridge audio is silence (rms≈0) and
	// Deepgram finds nothing.
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}
