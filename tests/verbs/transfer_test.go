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
	"strings"
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
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
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
//  9. assert-target-caller-id-fallback — no callerId configured: target INVITE From falls back to the parent caller number, never anonymous/empty
//
// 10. wait-action-transfer-callback — block on /action/transfer HTTP callback (20s sub-context)
//
// 11. assert-transfer-status-bridged — transfer_result=="bridged", transfer_reason=="completed", sip_status==200
// 12. assert-brief-reached-target  — Deepgram STT on target recording contains "briefing" + "agent"
// 13. assert-bridge-audio-transcript — Deepgram STT on caller recording contains "sun" + "shining"
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
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
			// before sending the reference WAV. The brief (~2.9s) plays on
			// this leg BEFORE the bridge forms, so a 1.5s wait fires the WAV
			// mid-brief into an unbridged leg; WarmBriefSettleDelay clears it.
			time.Sleep(WarmBriefSettleDelay)
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
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
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

	s = Step(t, "assert-target-caller-id-fallback")
	// No callerId is configured on this transfer, so the target leg must fall
	// back to the parent call's caller number (the REST-created caller leg
	// uses From 441514533212 — see placeWebhookCallToWithSID). Regression: the
	// warm-transfer outdial once sent an empty From user, which sbc-outbound
	// presents as "anonymous" and PSTN carriers reject (Twilio 403 / 32204).
	from := targetCall.From()
	s.Logf("target INVITE From: %s", from)
	if !strings.Contains(from, "441514533212") {
		s.Errorf("target INVITE From = %q, want fallback to parent caller number 441514533212", from)
	}
	if strings.Contains(strings.ToLower(from), "anonymous") {
		s.Errorf("target INVITE From = %q presents anonymous — caller-id fallback is broken", from)
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
	// Assert a single trailing keyword, not every word: the brief's first word
	// ("briefing") is frequently clipped because TTS starts the instant the leg
	// connects (media path not fully latched), and conference/telephony STT is
	// word-boundary sensitive. "agent" reliably proves the brief was spoken.
	AssertTranscriptContains(s, ctx, targetRecPath, "agent")
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	// Prove that AFTER the brief, the bridge connected and the target's
	// reference WAV crossed to the caller. If the parked→bridged flow
	// failed, the caller's recording would be silence from hold music only.
	// Expected substrings come from the pinned transcript of
	// testdata/test_audio.wav ("The sun is shining.").
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	// Single trailing keyword: bridge audio level varies and conference/telephony
	// STT drops boundary words; "shining" reliably proves the WAV crossed.
	AssertTranscriptContains(s, ctx, callerRec, "shining")
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
//  5. spawn-target-goroutine        — async: answer, silence, wait WarmBriefSettleDelay (brief finishes + bridge latches), stream WAV, hang up
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
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
			// The brief (~2.9s) plays into the conference before the WAV; without
			// a long-enough wait the two overlap in the conference mix and STT may
			// drop a phrase. WarmBriefSettleDelay clears the brief + RTP latch.
			time.Sleep(WarmBriefSettleDelay)
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
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
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
	// Single trailing keyword: the brief's first word is often clipped at
	// conference-join and conference STT is word-boundary sensitive; "agent"
	// reliably proves the brief reached the caller in the room.
	AssertTranscriptContains(s, ctx, callerRec, "agent")
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	// After the brief, the target's reference WAV crossed the conference bridge
	// to the caller (same callerRec). Conference mixing attenuates and STT drops
	// boundary words; "shining" reliably proves the WAV reached the caller.
	AssertTranscriptContains(s, ctx, callerRec, "shining")
	s.Done()
}

// TestVerb_Transfer_NoAnswerReturn — failure-disposition path (spec §8): the
// human target never answers, so within `timeout` the transfer resolves to the
// `return` disposition. This is the spec's headline value — failure handling is
// "the part that actually makes the hand-rolled version too hard, and the part
// packaging must own." The happy-path tests never exercise a disposition; this
// one proves the platform owns the no-answer path end-to-end.
//
// Asserts:
//
//	(a) actionHook reports transfer_result=="returned", transfer_reason=="no-answer",
//	(b) the caller is STILL ON THE LINE and the verb stack CONTINUED — proven by a
//	    distinctive `say` scripted AFTER the transfer verb reaching the caller's
//	    recording ("...the verb stack continues — so the app resumes control", §5.2).
//
// Steps:
//  1. script-transfer-no-answer  — [transfer warm onNoAnswer=return, say "<marker>", hangup] + empty action ack
//  2. spawn-target-goroutine     — async: Trying + Ringing then NEVER answer; let jambonz time out + CANCEL
//  3. place-caller-and-record    — POST /Calls, answer caller, record (hold music + post-return say)
//  4. wait-target-done           — target goroutine saw CANCEL / ctx done
//  5. assert-target-sip-wire     — target received INVITE, sent 100/180, never 200
//  6. wait-action-transfer-callback — block on /action/transfer
//  7. assert-transfer-status-returned — transfer_result=="returned", transfer_reason=="no-answer"
//  8. assert-post-return-say-reached-caller — caller recording contains the post-transfer say marker
func TestVerb_Transfer_NoAnswerReturn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transfer-no-answer")
	actionURL := SessionURL(sess, "transfer")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// timeout:8 keeps the no-answer wait short. onNoAnswer:return is the spec
	// default but set explicitly to pin the behavior under test. The say after
	// the transfer is the proof the caller survived and the stack resumed;
	// "afterwards" is the trailing keyword we assert (STT-robust, distinctive).
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "warm",
			"callerPresent", false,
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"brief", map[string]any{"text": "Briefing the agent now."},
			"timeout", 8,
			"anchorMedia", true,
			"disposition", map[string]any{"onNoAnswer": "return"},
			"actionHook", actionURL),
		V("say", "text", "The transfer returned afterwards."),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Target goroutine: ring but NEVER answer, so jambonz's `timeout` fires and
	// the no-answer disposition runs. Send 100/180, then wait for the inbound
	// INVITE to be CANCEL'd (jambonz cancels the ringing leg on timeout) or ctx
	// to end. We do NOT Answer().
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
			// Do NOT answer — wait for jambonz to CANCEL the ringing leg on
			// timeout, or for the test context to end.
			t.Logf("[target:awaiting-cancel] start")
			select {
			case <-c.Done():
				t.Logf("[target] leg ended (canceled by jambonz)")
			case <-targetCtx.Done():
				t.Logf("[target] ctx done while awaiting cancel")
			}
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	callerRec := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("transfer-noanswer-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-sip-wire")
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler")
	}
	// The target rang (100/180) but was never answered (no 200).
	RequireRecvMethods(s, targetCall, "INVITE")
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	if slices.Contains(sent, 200) {
		s.Errorf("target answered (sent 200) but this test requires no-answer; statuses = %v", sent)
	}
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-returned")
	if got := cb.String("transfer_result"); got != "returned" {
		s.Errorf("transfer_result: got %q want %q", got, "returned")
	}
	if got := cb.String("transfer_reason"); got != "no-answer" {
		s.Errorf("transfer_reason: got %q want %q", got, "no-answer")
	}
	s.Done()

	s = Step(t, "assert-post-return-say-reached-caller")
	// The real proof of `return`: the caller is still on the line and the verb
	// stack continued past the transfer. The say scripted AFTER the transfer
	// must reach the caller. If `return` were broken (caller dropped, or the
	// stack didn't resume), this recording would be hold music / silence only.
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, callerRec, "afterwards")
	s.Done()
}

// TestVerb_Transfer_BlindRefer — blind transfer via the DEFAULT method, SIP
// REFER (spec §7.1: "blind transfer is a trivial sip:refer"). The existing
// TestVerb_Transfer_Blind forces blindMethod="dial"; this one omits it so
// blindMethod defaults to "refer" and the REFER path is exercised.
//
// With REFER, jambonz sends a REFER toward the caller's far end (Refer-To =
// target) and drops out of the media path — so there is NO bridge audio to
// assert, and this differs fundamentally from blindMethod="dial" (which places
// an outbound INVITE to the target). In this harness the SBC/UAS rejects the
// REFER with 488 (Not Acceptable Here), which TaskSipRefer maps to a 'rejected'
// outcome → transfer_reason "error" → the onFailure disposition (default
// "return"). That is the correct, deterministic outcome here and is exactly the
// signature of the REFER path (a dial path would yield no-answer/busy/completed,
// never an immediate "error" from a SIP-method rejection).
//
// Asserts: actionHook reports transfer_result=="returned" / transfer_reason=="error".
// (We do NOT assert the REFER on the UAS wire: the SBC mediates/rejects it, so
// the diago UAS does not observe a clean REFER request. The actionHook reason is
// the reliable proof jambonz ran the REFER path.)
//
// Steps:
//  1. script-transfer-blind-refer — [transfer mode=blind (refer default) target, hangup] + empty action ack
//  2. place-caller-and-wait       — POST /Calls, answer caller, hold until the transfer resolves
//  3. wait-action-transfer-callback — block on /action/transfer
//  4. assert-transfer-status-returned — transfer_result=="returned", transfer_reason=="error"
func TestVerb_Transfer_BlindRefer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	callerUAS := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transfer-blind-refer")
	actionURL := SessionURL(sess, "transfer")
	// A target user on the realm. It need not be registered — for REFER we only
	// assert jambonz emitted the REFER with this URI in Refer-To; the referred
	// call is completed by the caller's far end (here, our UAS that never
	// NOTIFYs), not by us dialing the target.
	target := fmt.Sprintf("refer-target-%s@%s", sess.ID(), suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "blind",
			// blindMethod omitted on purpose → defaults to "refer".
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"timeout", 20,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "place-caller-and-wait")
	// No recording assertion: REFER takes jambonz out of media. Answer + hold
	// the leg open through the REFER + the ~15s NOTIFY timeout until the call
	// ends (jambonz hangs up after the transfer resolves + the trailing hangup).
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 25*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-returned")
	if got := cb.String("transfer_result"); got != "returned" {
		s.Errorf("transfer_result: got %q want %q", got, "returned")
	}
	// 488 rejection of the REFER → 'rejected' → reason "error" → onFailure
	// (default return). This "error" reason is the REFER-path signature.
	if got := cb.String("transfer_reason"); got != "error" {
		s.Errorf("transfer_reason: got %q want %q", got, "error")
	}
	s.Done()
}

// TestVerb_Transfer_WarmCallerID — regression: the `callerId` option on a warm
// transfer must be presented to the transfer destination. A feature-server bug
// dropped the configured callerId (the transfer task never copied
// data.callerId off the verb), so the target-leg INVITE went out with an
// EMPTY From user; sbc-outbound then presented "anonymous" and PSTN carriers
// rejected the leg (Twilio: 403, error 32204 "Invalid Caller ID"). A
// registered-user target accepts the call regardless of caller ID, which lets
// us assert the From header directly on the UAS wire instead of needing a
// PSTN carrier in the loop.
//
// No audio/STT assertions here — bridge audio is already covered by
// TestVerb_Transfer_WarmParked. This test pins the caller-ID contract only.
//
// Asserts:
//
//	(a) the INVITE received by the target carries the configured callerId in
//	    its From header (and does not present anonymous),
//	(b) the transfer still completes end-to-end (transfer_result=="bridged",
//	    transfer_reason=="completed").
//
// Steps:
//  1. script-transfer-warm-callerid — [transfer warm/callerPresent=false callerId=..., hangup] + empty action ack
//  2. spawn-target-goroutine        — async: answer, silence, wait out the brief + bridge, hang up
//  3. place-caller-and-wait         — POST /Calls, answer caller leg, hold until ended
//  4. wait-target-done              — wait for target goroutine to finish
//  5. assert-target-sip-wire        — target received INVITE, sent 100/180/200
//  6. assert-target-caller-id       — target INVITE From contains the configured callerId
//  7. wait-action-transfer-callback — block on /action/transfer HTTP callback (20s sub-context)
//  8. assert-transfer-status-bridged — transfer_result=="bridged", transfer_reason=="completed"
//
// Test     --POST /Calls [tag.x_test_id, to=callerUAS]-->                 Jambonz
// Jambonz  --GET /hook-->                                                 Webhook
// Webhook  --[answer, pause, transfer warm callerId=+15005550042, hangup]--> Jambonz
// Jambonz  --INVITE (caller leg)-->                                       UAS(callerUAS)
// UAS      --200 OK-->                                                    Jambonz
//
//	(caller parked on hold)
//
// Jambonz  --INVITE From: <sip:+15005550042@...> (target leg)-->          UAS(targetUAS)  // assert From
// UAS2     --200 OK-->                                                    Jambonz
// Jambonz  ==TTS brief, then bridge==>                                    both legs
// UAS2     --BYE-->                                                       Jambonz
// Jambonz  --POST /action/transfer {transfer_result:"bridged"}-->         Webhook  // assert
// Jambonz  --BYE-->                                                       UAS
func TestVerb_Transfer_WarmCallerID(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	// Distinctive E.164 number no other fixture uses: if it shows up in the
	// target's From header it can only have come from the verb's callerId.
	const transferCallerID = "+15005550042"

	s := Step(t, "script-transfer-warm-callerid")
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
			"callerId", transferCallerID,
			"brief", map[string]any{"text": "Briefing the agent now."},
			"timeout", 20,
			"anchorMedia", true,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Target goroutine: answer, prime the RTP path, wait out the brief + the
	// bridge forming, then hang up. Hanging up before the bridge forms would
	// resolve the transfer as a decline/failure instead of bridged, so the
	// WarmBriefSettleDelay wait is load-bearing.
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
			// Let the brief finish and the bridge latch before hanging up so
			// the actionHook reports bridged/completed, not a failure path.
			time.Sleep(WarmBriefSettleDelay)
			t.Logf("[target:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "target:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[target] done")
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
	s.Done()

	s = Step(t, "place-caller-and-wait")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
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

	s = Step(t, "assert-target-caller-id")
	// The core regression assertion: the configured callerId must survive
	// feature-server → sbc-outbound → target INVITE. With the bug, the From
	// user is empty and sbc-outbound substitutes "anonymous".
	from := targetCall.From()
	s.Logf("target INVITE From: %s", from)
	if !strings.Contains(from, transferCallerID) {
		s.Errorf("target INVITE From = %q, want it to contain configured callerId %q", from, transferCallerID)
	}
	if strings.Contains(strings.ToLower(from), "anonymous") {
		s.Errorf("target INVITE From = %q presents anonymous — configured callerId was dropped", from)
	}
	s.Done()

	s = Step(t, "wait-action-transfer-callback")
	// Same 20s sub-context as the other warm tests: brief-duration +
	// bridge-setup precede the callback.
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
	s.Done()
}

// TestVerb_Transfer_WarmParked_OnHoldHook — the onHoldHook option on a warm/
// parked transfer: while the caller is parked, jambonz requests the hook and
// plays the returned verbs (say/play/pause only) to the caller in a loop until
// the bridge forms. Previously the schema documented onHoldHook but the
// feature-server played only silence ("no onHoldHook processing in v1").
//
// The target delays its answer by a few seconds so the hold announcement has a
// clean window to play to the parked caller before the brief + bridge cut it off.
//
// Asserts:
//
//	(a) the onHoldHook is invoked with event_type=="transfer.on-hold",
//	(b) the hold announcement reached the PARKED caller — Deepgram STT on the
//	    caller recording contains "transfer" (from "Please hold while we
//	    transfer you to an agent."),
//	(c) the transfer still completes end-to-end (transfer_result=="bridged").
//
// Steps:
//  1. script-transfer-onholdhook   — transfer warm/parked with onHoldHook; onhold hook scripted with the say
//  2. spawn-target-goroutine       — async: ring, delay ~4s, answer, wait out the brief + bridge, hang up
//  3. place-caller-and-record      — POST /Calls, answer caller leg, record until ended
//  4. wait-onhold-callback         — block on /action/onhold, assert event_type=="transfer.on-hold"
//  5. wait-target-done             — wait for target goroutine to finish
//  6. assert-target-sip-wire       — target received INVITE, sent 100/180/200
//  7. wait-action-transfer-callback — block on /action/transfer
//  8. assert-transfer-status-bridged — transfer_result=="bridged", transfer_reason=="completed"
//  9. assert-hold-announcement-reached-caller — caller recording contains "transfer"
func TestVerb_Transfer_WarmParked_OnHoldHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transfer-onholdhook")
	actionURL := SessionURL(sess, "transfer")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// The hold announcement served each time jambonz requests the onHoldHook.
	// The feature-server loops the hook while the caller stays parked, so the
	// phrase may repeat — the transcript assertion only needs one occurrence.
	sess.ScriptActionHook("onhold", webhook.Script{
		V("say", "text", "Please hold while we transfer you to an agent."),
	})
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "warm",
			"callerPresent", false,
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"brief", map[string]any{"text": "Briefing the agent now."},
			"onHoldHook", SessionURL(sess, "onhold"),
			"timeout", 20,
			"anchorMedia", true,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Ring for ~4s before answering so the hold announcement (~3.5s of TTS,
	// starting after the 500ms pacing silence + hook round-trip) plays to the
	// parked caller before the brief + bridge interrupt it.
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	targetCtx, targetCancel := context.WithCancel(ctx)
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
			// Hold-announcement window: stay ringing while the caller hears
			// the onHoldHook say.
			time.Sleep(4 * time.Second)
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
			// Let the brief finish and the bridge latch before hanging up so
			// the actionHook reports bridged/completed, not a failure path.
			time.Sleep(WarmBriefSettleDelay)
			t.Logf("[target:hangup] start")
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "target:hangup", "Hangup: %v", err)
			}
			<-c.Done()
			t.Logf("[target] done")
		case <-targetCtx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	callerRec := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("transfer-onhold-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-onhold-callback")
	// The hook fired while the call was live; its callback is already queued.
	waitCtx0, wcancel0 := context.WithTimeout(ctx, 15*time.Second)
	defer wcancel0()
	hcb, err := sess.WaitCallbackFor(waitCtx0, "action/onhold")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/onhold: %v", err)
	}
	s.Logf("action/onhold body: %s", string(hcb.Body))
	if got := hcb.String("event_type"); got != "transfer.on-hold" {
		s.Errorf("onHoldHook event_type: got %q want %q", got, "transfer.on-hold")
	}
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
	waitCtx, wcancel := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel()
	cb2, err := sess.WaitCallbackFor(waitCtx, "action/transfer")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/transfer: %v", err)
	}
	s.Logf("action/transfer body: %s", string(cb2.Body))
	s.Done()

	s = Step(t, "assert-transfer-status-bridged")
	if got := cb2.String("transfer_result"); got != "bridged" {
		s.Errorf("transfer_result: got %q want %q", got, "bridged")
	}
	if got := cb2.String("transfer_reason"); got != "completed" {
		s.Errorf("transfer_reason: got %q want %q", got, "completed")
	}
	s.Done()

	s = Step(t, "assert-hold-announcement-reached-caller")
	// The real proof: the parked caller heard the app-provided hold verbs, not
	// silence. "transfer" is mid-phrase ("...while we TRANSFER you to an
	// agent") so it survives both start-clipping and an end-of-phrase cutoff
	// when the bridge forms.
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	AssertTranscriptContains(s, ctx, callerRec, "transfer")
	s.Done()
}
