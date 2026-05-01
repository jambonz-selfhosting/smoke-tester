// Tests for the `dial` verb — multi-leg.
//
// Schema: schemas/verbs/dial — required `target` (array of at least one
// target object). This test drives a real end-to-end bridge and verifies
// not just SIP signalling but that jambonz-bridged media actually reaches
// the caller:
//
//   1. Jambonz INVITEs our primary UAS (caller leg).
//   2. `dial` sends a second INVITE to our callee UAS.
//   3. Callee answers, streams the reference WAV
//      (tests/verbs/testdata/test_audio.wav, pinned transcript
//      "The sun is shining.") over its outbound RTP.
//   4. Caller records its inbound RTP — that's whatever made it through
//      jambonz's bridge — to a PCM16 file.
//   5. Callee hangs up, dial actionHook fires, caller's recording is sent
//      through Deepgram and asserted to contain the expected words.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN. Requires both UASes
// registered (JAMBONZ_SIP_USER + JAMBONZ_SIP_CALLEE_USER).
package verbs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Dial_User_Bridge — two legs bridged via `dial`. Callee streams
// a reference WAV; caller records whatever jambonz's bridge passes through
// and Deepgram verifies the content.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. resolve-fixture — resolve testdata/test_audio.wav path
//  3. script-dial-to-callee — [dial target=callee-uas, hangup] + empty action ack
//  4. claim-callee-channel — reserve callee-uas inbound channel before INVITE lands
//  5. spawn-callee-goroutine — async: answer, stream WAV, hang up callee leg
//  6. place-caller-and-record — POST /Calls, answer caller leg, record bridge audio
//  7. wait-callee-done — wait for callee goroutine to finish
//  8. assert-callee-sip-wire — callee received INVITE, sent 100/180/200
//  9. wait-action-dial-callback — block on /action/dial HTTP callback
// 10. assert-dial-status-completed — dial_call_status=="completed", dial_sip_status==200
// 11. assert-bridge-audio-transcript — Deepgram transcript contains "sun" + "shining"
//
// Test     --POST /Calls [tag.x_test_id, to=caller-uas]-->            Jambonz
// Jambonz  --GET /hook-->                                          Webhook
// Webhook  --[answer, pause, dial target=callee-uas, hangup]-->      Jambonz
// Jambonz  --INVITE (caller leg)-->                                UAS(caller-uas)
// UAS      --200 OK-->                                             Jambonz
// Jambonz  --INVITE (callee leg)-->                                UAS(callee-uas)
// UAS2     --200 OK-->                                             Jambonz
//                     (RTP bridged both directions)
// UAS2     ==silence + test_audio.wav + silence==>                 Jambonz ==> UAS
// UAS                                                     records PCM16 from bridge
// UAS2     --BYE-->                                                Jambonz
// Jambonz  --POST /action/dial {dial_call_status:"completed"}-->   Webhook  // assert
// Jambonz  --BYE-->                                                UAS
//                                                                  // Deepgram: assert
//                                                                  //   transcript has
//                                                                  //   "sun" + "shining"
func TestVerb_Dial_User_Bridge(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS := claimUAS(t, ctx)
	calleeUAS := claimUAS(t, ctx)

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
			"timeout", 20,
			"actionHook", actionURL,
			// anchorMedia=true forces FreeSWITCH to relay RTP between the
			// two legs instead of brokering a peer-to-peer SDP exchange.
			// Without this the cluster sometimes hands each leg the OTHER
			// leg's private NAT'd RTP address (10.x.x.x), which neither
			// side can reach — so the bridge "completes" SIP-wise but no
			// audio crosses. Anchored media keeps every packet inside the
			// cluster's data plane, which we can reach via the SBC public IP.
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "claim-callee-channel")
	// calleeUAS.Inbound is already a per-test channel (claimed at function
	// top). The previous singleton-claim step is unnecessary now; this
	// no-op step is kept so the Steps: block in the doc comment still
	// matches the body 1:1.
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	// Callee goroutine: answer, stream the reference WAV, hang up. Key
	// ordering: 1.5s of silence first so the bridge's RTP path stabilizes
	// before the WAV starts; trailing silence so the recording captures
	// the tail of the phrase before BYE tears down media.
	calleeDone := make(chan struct{})
	var calleeCall *jsip.Call
	go func() {
		defer close(calleeDone)
		// sub-step prefix [callee:*] identifies the goroutine's steps
		// distinct from the main test goroutine's [step:*] lines.
		select {
		case c := <-calleeUAS.Inbound:
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
			// Let the bridge settle + the caller's recording pipeline fully
			// latch before speech starts. Same pattern as gather_speech.
			time.Sleep(RecognizerArmDelay)
			t.Logf("[callee:send-wav] start")
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "callee:send-wav", "SendWAV: %v", err)
				return
			}
			t.Logf("[callee:silence-trail] start")
			// Trailing silence so Deepgram sees a proper utterance boundary
			// and the caller's recording captures the full phrase before
			// BYE closes the media socket.
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-trail", "SendSilence: %v", err)
				return
			}
			time.Sleep(500 * time.Millisecond)
			t.Logf("[callee:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[callee] done")
		case <-ctx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", ctx.Err())
		}
	}()
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("dial-caller"), WithSilence())
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
	// The real proof: audio actually flowed through the bridge. The caller
	// recorded what came back from jambonz; if dial didn't connect the
	// media streams, the recording would be silence and Deepgram would
	// find nothing. Expected substrings come from the pinned transcript
	// of testdata/test_audio.wav ("The sun is shining.").
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}
