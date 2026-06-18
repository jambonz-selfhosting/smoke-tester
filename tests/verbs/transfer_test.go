// Tests for the `transfer` verb.
//
// T-03: TestVerb_Transfer_Blind — blind/dial mode: jambonz bridges the caller
// to the target via a `transfer` verb (mode=blind, blindMethod=dial). Asserts
// the actionHook reports bridged/completed and the bridge carried real audio.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN. Requires both UASes
// registered (JAMBONZ_SIP_USER + JAMBONZ_SIP_CALLEE_USER).
package verbs

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Transfer_Blind — two legs bridged via `transfer` (blind/dial mode).
// Target streams a reference WAV; caller records whatever jambonz's bridge
// passes through and Deepgram verifies the content.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. resolve-fixture — resolve testdata/test_audio.wav path
//  3. script-transfer-to-target — [transfer blind/dial target=targetUAS, hangup] + empty action ack
//  4. claim-target-channel — reserve targetUAS inbound channel before INVITE lands
//  5. spawn-target-goroutine — async: answer, stream WAV, hang up target leg
//  6. place-caller-and-record — POST /Calls, answer caller leg, record bridge audio
//  7. wait-target-done — wait for target goroutine to finish
//  8. assert-target-sip-wire — target received INVITE, sent 100/180/200
//  9. wait-action-transfer-callback — block on /action/transfer HTTP callback
//
// 10. assert-transfer-status-bridged — transfer_result=="bridged", transfer_reason=="completed", sip_status==200
// 11. assert-bridge-audio-transcript — Deepgram transcript contains "sun" + "shining"
//
// Test     --POST /Calls [tag.x_test_id, to=callerUAS]-->        Jambonz
// Jambonz  --GET /hook-->                                        Webhook
// Webhook  --[answer, pause, transfer blind/dial target=target, hangup]--> Jambonz
// Jambonz  --INVITE (caller leg)-->                              UAS(callerUAS)
// UAS      --200 OK-->                                           Jambonz
// Jambonz  --INVITE (target leg)-->                              UAS(targetUAS)
// UAS2     --200 OK-->                                           Jambonz
//
//	(RTP bridged both directions)
//
// UAS2     ==silence + test_audio.wav + silence==>               Jambonz ==> UAS
// UAS                                                   records PCM16 from bridge
// UAS2     --BYE-->                                              Jambonz
// Jambonz  --POST /action/transfer {transfer_result:"bridged"}-> Webhook  // assert
// Jambonz  --BYE-->                                              UAS
//
//	// Deepgram: assert
//	//   transcript has
//	//   "sun" + "shining"
func TestVerb_Transfer_Blind(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-transfer-to-target")
	actionURL := SessionURL(sess, "transfer")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "blind",
			"blindMethod", "dial",
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"timeout", 20,
			"anchorMedia", true,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "claim-target-channel")
	// targetUAS.Inbound is already a per-test channel (claimed at function
	// top). This no-op step is kept so the Steps: block in the doc comment
	// still matches the body 1:1.
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Target goroutine: answer, stream the reference WAV, hang up. Key
	// ordering: 1.5s of silence first so the bridge's RTP path stabilises
	// before the WAV starts; trailing silence so the recording captures
	// the tail of the phrase before BYE tears down media.
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	go func() {
		defer close(targetDone)
		// sub-step prefix [target:*] identifies the goroutine's steps
		// distinct from the main test goroutine's [step:*] lines.
		select {
		case c := <-targetUAS.Inbound:
			targetCall = c
			t.Logf("[target:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "target:trying", "Trying: %v", err)
				return
			}
			t.Logf("[target:ringing] start")
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "target:ringing", "Ringing: %v", err)
				return
			}
			t.Logf("[target:answer] start")
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "target:answer", "Answer: %v", err)
				return
			}
			t.Logf("[target:silence-prime] start")
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-prime", "SendSilence: %v", err)
				return
			}
			// Let the bridge settle + the caller's recording pipeline fully
			// latch before speech starts. Same pattern as gather_speech.
			time.Sleep(RecognizerArmDelay)
			t.Logf("[target:send-wav] start")
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "target:send-wav", "SendWAV: %v", err)
				return
			}
			t.Logf("[target:silence-trail] start")
			// Trailing silence so Deepgram sees a proper utterance boundary
			// and the caller's recording captures the full phrase before
			// BYE closes the media socket.
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-trail", "SendSilence: %v", err)
				return
			}
			t.Logf("[target:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "target:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[target] done")
		case <-ctx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", ctx.Err())
		}
	}()
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("transfer-blind-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-sip-wire")
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler")
	}
	RequireRecvMethods(s, targetCall, "INVITE")
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 15*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-bridged")
	if got := cb.String("transfer_result"); got != "bridged" {
		s.Errorf("transfer_result: got %q want %q", got, "bridged")
	}
	if got := cb.String("transfer_reason"); got != "completed" {
		s.Errorf("transfer_reason: got %q want %q", got, "completed")
	}
	if got := cb.Int("sip_status"); got != 200 {
		s.Errorf("sip_status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	// The real proof: audio actually flowed through the bridge. The caller
	// recorded what came back from jambonz; if transfer didn't connect the
	// media streams, the recording would be silence and Deepgram would
	// find nothing. Expected substrings come from the pinned transcript
	// of testdata/test_audio.wav ("The sun is shining.").
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}

// TestVerb_Transfer_WarmParked — warm transfer with callerPresent=false: caller
// is parked on hold; jambonz dials the target, plays a brief TTS on the target
// leg only ("Briefing the agent now."), then bridges caller↔target. Asserts:
//
//	(a) the brief TTS reached the target leg (Deepgram STT on target recording),
//	(b) after the brief the bridge carried the target's reference WAV to the
//	    caller (Deepgram STT on caller recording),
//	(c) actionHook reports transfer_result=="bridged", transfer_reason=="completed",
//	    sip_status==200.
//
// Steps:
//  1. register-webhook-session    — webhook.Registry.New + cleanup
//  2. resolve-fixture             — resolve testdata/test_audio.wav path
//  3. script-transfer-warm-parked — [transfer warm/callerPresent=false target=target brief=..., hangup] + empty action ack
//  4. claim-target-channel        — reserve targetUAS inbound channel before INVITE lands
//  5. spawn-target-goroutine      — async: answer, start recording immediately, stream WAV, hang up target leg
//  6. place-caller-and-record     — POST /Calls, answer caller leg, record bridge audio
//  7. wait-target-done            — wait for target goroutine to finish
//  8. assert-target-sip-wire      — target received INVITE, sent 100/180/200
//  9. wait-action-transfer-callback — block on /action/transfer HTTP callback (20s sub-context)
//
// 10. assert-transfer-status-bridged — transfer_result=="bridged", transfer_reason=="completed", sip_status==200
// 11. assert-brief-reached-target  — Deepgram STT on target recording contains "briefing" + "agent"
// 12. assert-bridge-audio-transcript — Deepgram STT on caller recording contains "sun" + "shining"
//
// Test     --POST /Calls [tag.x_test_id, to=callerUAS]-->                 Jambonz
// Jambonz  --GET /hook-->                                                 Webhook
// Webhook  --[answer, pause, transfer warm callerPresent=false ..., hangup]--> Jambonz
// Jambonz  --INVITE (caller leg)-->                                       UAS(callerUAS)
// UAS      --200 OK-->                                                    Jambonz
//
//	(caller parked on hold)
//
// Jambonz  --INVITE (target leg)-->                                       UAS(targetUAS)
// UAS2     --200 OK-->                                                    Jambonz
// Jambonz  ==TTS "Briefing the agent now." ==>                            UAS2 (target only — caller hears hold)
//
//	UAS2 records target PCM
//
// Jambonz  ==bridge established==>                                        both legs
// UAS2     ==silence + test_audio.wav + silence==>                        Jambonz ==> UAS (caller)
// UAS                                                             records PCM16 from bridge
// UAS2     --BYE-->                                                        Jambonz
// Jambonz  --POST /action/transfer {transfer_result:"bridged"}-->          Webhook  // assert
// Jambonz  --BYE-->                                                        UAS
//
//	// Deepgram: assert target recording has "briefing" + "agent"
//	// Deepgram: assert caller recording has "sun" + "shining"
func TestVerb_Transfer_WarmParked(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-transfer-warm-parked")
	actionURL := SessionURL(sess, "transfer")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "warm",
			"callerPresent", false,
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"brief", map[string]any{"text": "Briefing the agent now."},
			"timeout", 20,
			"anchorMedia", true,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "claim-target-channel")
	// targetUAS.Inbound is already a per-test channel (claimed at function
	// top). This no-op step is kept so the Steps: block in the doc comment
	// still matches the body 1:1.
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Target goroutine: answer immediately and start recording so the brief
	// TTS is captured, then stream the reference WAV after the bridge
	// settles.  Key ordering: StartRecording MUST happen right after
	// Answer() — before any sleep — so the brief audio is not missed.
	// After the brief (~1.5-2s) jambonz bridges; we wait BridgeSettleDelay
	// then send the reference WAV so the caller's recording captures it.
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	var targetRecPath string
	go func() {
		defer close(targetDone)
		// sub-step prefix [target:*] identifies the goroutine's steps
		// distinct from the main test goroutine's [step:*] lines.
		select {
		case c := <-targetUAS.Inbound:
			targetCall = c
			t.Logf("[target:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "target:trying", "Trying: %v", err)
				return
			}
			t.Logf("[target:ringing] start")
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "target:ringing", "Ringing: %v", err)
				return
			}
			t.Logf("[target:answer] start")
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "target:answer", "Answer: %v", err)
				return
			}
			// Start recording immediately after Answer so the brief TTS
			// (which jambonz plays on this leg before bridging) is captured.
			// If recording starts after any sleep the brief is missed and the
			// STT assertion fails confusingly.
			recPath := filepath.Join(t.TempDir(), "transfer-parked-target.pcm")
			t.Logf("[target:start-recording] path=%s", recPath)
			if err := c.StartRecording(recPath); err != nil {
				GoroutineFailf(t, "target:start-recording", "StartRecording: %v", err)
				return
			}
			targetRecPath = recPath
			t.Logf("[target:silence-prime] start")
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-prime", "SendSilence: %v", err)
				return
			}
			// Wait for the brief TTS to finish and the bridge to settle
			// before sending the reference WAV. BridgeSettleDelay covers
			// the brief duration (~1.5-2s) + RTP-path stabilisation.
			time.Sleep(BridgeSettleDelay)
			t.Logf("[target:send-wav] start")
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "target:send-wav", "SendWAV: %v", err)
				return
			}
			t.Logf("[target:silence-trail] start")
			// Trailing silence so Deepgram sees a proper utterance boundary
			// and the caller's recording captures the full phrase before
			// BYE closes the media socket.
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-trail", "SendSilence: %v", err)
				return
			}
			t.Logf("[target:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "target:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[target] done")
		case <-ctx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", ctx.Err())
		}
	}()
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	callerRec := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("transfer-parked-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-sip-wire")
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler")
	}
	RequireRecvMethods(s, targetCall, "INVITE")
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	// Warm mode needs a longer sub-context than blind: the brief TTS plays
	// before bridging, so the total latency is brief-duration + bridge-setup.
	// 20s is the minimum safe window (blind uses 15s).
	waitCtx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-bridged")
	if got := cb.String("transfer_result"); got != "bridged" {
		s.Errorf("transfer_result: got %q want %q", got, "bridged")
	}
	if got := cb.String("transfer_reason"); got != "completed" {
		s.Errorf("transfer_reason: got %q want %q", got, "completed")
	}
	if got := cb.Int("sip_status"); got != 200 {
		s.Errorf("sip_status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "assert-brief-reached-target")
	// Prove the brief TTS was spoken to the target leg only. The target
	// started recording immediately after Answer() so the brief is captured.
	// AssertAudioDuration is available in helpers — gate on >500ms audio so
	// a silent recording fails loudly rather than as a confusing transcript miss.
	AssertAudioDuration(s, targetCall, 500*time.Millisecond, 0, "target-brief-recording")
	AssertTranscriptContains(s, ctx, targetRecPath, "briefing", "agent")
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	// Prove that AFTER the brief, the bridge connected and the target's
	// reference WAV crossed to the caller. If the parked→bridged flow
	// failed, the caller's recording would be silence from hold music only.
	// Expected substrings come from the pinned transcript of
	// testdata/test_audio.wav ("The sun is shining.").
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, callerRec, "sun", "shining")
	s.Done()
}

// TestVerb_Transfer_WarmThreeWay — warm transfer with callerPresent=true: both
// caller and target are put into a conference. The brief TTS is played into
// the conference so BOTH parties hear it; then the bridge carries the target's
// reference WAV to the caller. Asserts:
//
//	(a) the brief TTS reached the CALLER (caller is present in three-way —
//	    unlike parked mode where caller hears hold music), proven by Deepgram
//	    STT on the caller's recording,
//	(b) after the brief the bridge carried the target's reference WAV to the
//	    caller (also on the caller's recording),
//	(c) actionHook reports transfer_result=="bridged", transfer_reason=="completed",
//	    sip_status==200.
//
// Steps:
//  1. register-webhook-session      — webhook.Registry.New + cleanup
//  2. resolve-fixture               — resolve testdata/test_audio.wav path
//  3. script-transfer-warm-3way     — [transfer warm/callerPresent=true target=target brief=..., hangup] + empty action ack
//  4. claim-target-channel          — reserve targetUAS inbound channel before INVITE lands
//  5. spawn-target-goroutine        — async: answer, silence, wait RecognizerArmDelay+BridgeSettleDelay (brief finishes), stream WAV, hang up
//  6. place-caller-and-record       — POST /Calls, answer caller leg, record full caller leg until ended
//  7. wait-target-done              — wait for target goroutine to finish
//  8. assert-target-sip-wire        — target received INVITE, sent 100/180/200
//  9. wait-action-transfer-callback — block on /action/transfer HTTP callback (20s sub-context)
//
// 10. assert-transfer-status-bridged  — transfer_result=="bridged", transfer_reason=="completed", sip_status==200
// 11. assert-brief-reached-caller     — Deepgram STT on caller recording contains "briefing" + "agent"
// 12. assert-bridge-audio-transcript  — Deepgram STT on caller recording contains "sun" + "shining"
//
// Test     --POST /Calls [tag.x_test_id, to=callerUAS]-->                     Jambonz
// Jambonz  --GET /hook-->                                                     Webhook
// Webhook  --[answer, pause, transfer warm callerPresent=true ..., hangup]--> Jambonz
// Jambonz  --INVITE (caller leg)-->                                           UAS(callerUAS)
// UAS      --200 OK-->                                                        Jambonz
// Jambonz  --INVITE (target leg)-->                                           UAS(targetUAS)
// UAS2     --200 OK-->                                                        Jambonz
// Jambonz  ==TTS "Briefing the agent now." ==>                                BOTH UAS + UAS2 (conference — caller present)
//
//	UAS records full PCM (brief + post-brief bridge audio)
//
// Jambonz  ==bridge established (already in conference)==>                    both legs
// UAS2     ==silence + test_audio.wav + silence==>                            Jambonz ==> UAS (caller)
// UAS2     --BYE-->                                                            Jambonz
// Jambonz  --POST /action/transfer {transfer_result:"bridged"}-->             Webhook  // assert
// Jambonz  --BYE-->                                                           UAS
//
//	// Deepgram: assert caller recording has "briefing" + "agent" (brief)
//	// Deepgram: assert caller recording has "sun" + "shining"  (bridge)
func TestVerb_Transfer_WarmThreeWay(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-transfer-warm-3way")
	actionURL := SessionURL(sess, "transfer")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "warm",
			"callerPresent", true,
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"brief", map[string]any{"text": "Briefing the agent now."},
			"timeout", 20,
			"anchorMedia", true,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "claim-target-channel")
	// targetUAS.Inbound is already a per-test channel (claimed at function
	// top). This no-op step is kept so the Steps: block in the doc comment
	// still matches the body 1:1.
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Target goroutine: answer and send silence (NAT latch), then wait for
	// the brief TTS to finish before streaming the reference WAV.
	//
	// In three-way mode the brief plays into the conference (both caller and
	// target hear it), so we must NOT overlap the brief with the target's WAV
	// or STT will miss one of the two phrases in the caller's recording.
	//
	// Sequencing: RecognizerArmDelay gives the conference bridge's RTP path
	// time to stabilise after Answer(); BridgeSettleDelay waits for the brief
	// TTS (~1.5-2s) to finish playing. Only after both delays does the target
	// send the reference WAV, ensuring the caller's single recording captures
	// brief first, then bridge audio, with no overlap.
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	go func() {
		defer close(targetDone)
		select {
		case c := <-targetUAS.Inbound:
			targetCall = c
			t.Logf("[target:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "target:trying", "Trying: %v", err)
				return
			}
			t.Logf("[target:ringing] start")
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "target:ringing", "Ringing: %v", err)
				return
			}
			t.Logf("[target:answer] start")
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "target:answer", "Answer: %v", err)
				return
			}
			t.Logf("[target:silence-prime] start")
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-prime", "SendSilence: %v", err)
				return
			}
			// Wait for the brief TTS to finish before sending the reference WAV.
			// RecognizerArmDelay lets the bridge RTP path stabilise; BridgeSettleDelay
			// covers the brief duration (~1.5-2s). Without this wait the WAV and
			// brief audio overlap in the conference mix and STT may drop one phrase.
			time.Sleep(RecognizerArmDelay)
			time.Sleep(BridgeSettleDelay)
			t.Logf("[target:send-wav] start")
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "target:send-wav", "SendWAV: %v", err)
				return
			}
			t.Logf("[target:silence-trail] start")
			// Trailing silence so Deepgram sees a proper utterance boundary
			// and the caller's recording captures the full phrase before
			// BYE closes the media socket.
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence-trail", "SendSilence: %v", err)
				return
			}
			t.Logf("[target:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "target:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[target] done")
		case <-ctx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", ctx.Err())
		}
	}()
	s.Done()

	s = Step(t, "place-caller-and-record")
	// The caller is present in the conference for BOTH the brief AND the
	// post-brief bridge audio. AnswerRecordAndWaitEnded records from Answer to
	// BYE in one continuous recording — this is the key recording that proves
	// both (a) the brief reached the caller and (b) the bridge carried the
	// target's reference WAV to the caller.
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	callerRec := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("transfer-3way-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-sip-wire")
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler")
	}
	RequireRecvMethods(s, targetCall, "INVITE")
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	// Three-way mode needs the same 20s sub-context as warm-parked: the brief
	// TTS plays into the conference before the target's WAV, so total latency
	// includes brief-duration + bridge-setup.
	waitCtx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-bridged")
	if got := cb.String("transfer_result"); got != "bridged" {
		s.Errorf("transfer_result: got %q want %q", got, "bridged")
	}
	if got := cb.String("transfer_reason"); got != "completed" {
		s.Errorf("transfer_reason: got %q want %q", got, "completed")
	}
	if got := cb.Int("sip_status"); got != 200 {
		s.Errorf("sip_status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "assert-brief-reached-caller")
	// In three-way mode the caller is present in the conference when the brief
	// plays, so the caller's recording must contain the brief phrase. This is
	// the key distinction from warm-parked (where callerPresent=false means the
	// caller only hears hold music during the brief).
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, callerRec, "briefing", "agent")
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	// After the brief, the target's reference WAV crossed the conference bridge
	// to the caller. Both assertions target the same callerRec — proving that
	// brief + bridge audio both reached the caller in sequence. Expected
	// substrings come from the pinned transcript of testdata/test_audio.wav
	// ("The sun is shining.").
	AssertTranscriptContains(s, ctx, callerRec, "sun", "shining")
	s.Done()
}
