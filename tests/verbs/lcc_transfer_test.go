// Tests for the Live Call Control (LCC) `transfer` command — CallSession.
// _lccTransfer in the feature-server. An in-progress call, sitting in some
// long-running verb, is redirected mid-call to a `transfer` verb via the REST
// updateCall API (POST /Accounts/{sid}/Calls/{callSid} with {transfer: {...}}).
// The feature-server replaceApplication()s the running app with the transfer.
//
// This differs from the standalone `transfer` verb test (transfer_test.go):
// there the transfer is in the call's original script; here it is injected
// from OUTSIDE, mid-call, by an API client — the LCC path.
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

// TestLCC_Transfer_BlindDial — Live Call Control transfer (CallSession._lccTransfer).
// The caller is parked in a long pause; an out-of-band updateCall injects a blind
// transfer (blindMethod:dial) to a target UAS. The target answering (100/180/200)
// proves the LCC transfer replaced the running app and the dial bridged.
func TestLCC_Transfer_BlindDial(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 90*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-caller-park")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// Park the caller in a long pause so the call is firmly mid-app when the
	// out-of-band updateCall lands. The trailing hangup is what would run if the
	// pause ever completed; the LCC transfer preempts it via replaceApplication.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("pause", "length", 60),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	go answerAndIdleTarget(t, ctx, targetUAS, targetDone, &targetCall)()
	s.Done()

	s = Step(t, "place-caller")
	callSID, call := placeWebhookCallToWithSID(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "lcc-transfer")
	// Let the call settle into the pause, then inject the transfer out-of-band.
	WaitFor(t, "settle-into-pause", BridgeSettleDelay)
	body := map[string]any{
		"transfer": map[string]any{
			"mode":        "blind",
			"blindMethod": "dial",
			"target": []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"timeout": 20,
		},
	}
	if err := client.UpdateCall(ctx, callSID, body); err != nil {
		s.Fatalf("UpdateCall(transfer) sid=%s: %v", callSID, err)
	}
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-answered")
	// The human leg answering (100/180/200) proves the LCC transfer dialed and
	// bridged — _lccTransfer replaced the parked pause with the transfer verb.
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler (LCC transfer did not dial the target)")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()
}

// TestLCC_Redirect_DuringWarmTransfer — regression for the transfer kill gap:
// a live-call-control call_hook redirect arriving while a warm/parked transfer
// is ringing must abort the transfer IMMEDIATELY — cancel the ringing human
// leg, stop the onHoldHook loop, and run the replacement verbs promptly.
//
// Pre-fix behavior: TaskTransfer.kill() only killed blind-mode sub-tasks, so a
// redirect during a warm transfer left the strategy running — the hold loop
// kept firing onHoldHook webhooks and playing hold audio, the replacement
// app's verbs were stalled until the transfer's ring timeout (here 30s), and
// an answering target could still bridge to the caller AFTER the kill. The
// elapsed-time assertion (<20s from updateCall to call end) is the
// discriminator: pre-fix the redirect say can't play before the 30s timeout.
//
// Steps:
//  1. script-transfer-with-onholdhook — call hook: [transfer warm/parked timeout=30 onHoldHook]; onhold scripted with a say
//  2. spawn-target-goroutine          — async: ring, NEVER answer, await the CANCEL the abort must send
//  3. place-caller-and-record         — POST /Calls, answer caller, record from answer
//  4. wait-onhold-callback            — hold loop is live (proves we redirect mid-park)
//  5. lcc-redirect                    — re-script call hook to [say "redirected", hangup], updateCall{call_hook}
//  6. wait-call-ended                 — block until the caller leg ends; measure elapsed since updateCall
//  7. assert-redirect-prompt          — elapsed <20s AND caller recording contains "redirected"
//  8. wait-target-done                — target goroutine saw the CANCEL / leg end
//  9. assert-target-canceled          — target received INVITE, rang, never sent 200
func TestLCC_Redirect_DuringWarmTransfer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transfer-with-onholdhook")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	sess.ScriptActionHook("onhold", webhook.Script{
		V("say", "text", "Please hold while we transfer you to an agent."),
	})
	// timeout:30 on purpose — the pre-fix stall lasts the full ring timeout,
	// which is what the <20s elapsed assertion discriminates against.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transfer",
			"mode", "warm",
			"callerPresent", false,
			"target", []any{map[string]any{
				"type": "user",
				"name": target,
			}},
			"onHoldHook", SessionURL(sess, "onhold"),
			"timeout", 30,
			"anchorMedia", true,
			"actionHook", SessionURL(sess, "transfer")),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transfer")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	// Ring but NEVER answer: the redirect's abort must CANCEL this leg. If the
	// abort is broken the leg instead rings until jambonz's 30s ring timeout.
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
			t.Logf("[target:awaiting-cancel] start")
			select {
			case <-c.Done():
				t.Logf("[target] leg ended (canceled)")
			case <-targetCtx.Done():
				t.Logf("[target] ctx done while awaiting cancel")
			}
		case <-targetCtx.Done():
			if targetCtx.Err() == context.DeadlineExceeded {
				GoroutineFailf(t, "target", "never received INVITE: %v", targetCtx.Err())
			}
		}
	}()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid.
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	callSID, call := placeWebhookCallToWithSID(ctx, t, callerUAS, sess, withTimeLimit(60))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "lcc-redirect-caller.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-onhold-callback")
	// Redirecting only makes sense once the transfer is actually parked and
	// the hold loop is running — the hook's first hit is that signal.
	waitCtx0, wcancel0 := context.WithTimeout(ctx, 20*time.Second)
	defer wcancel0()
	hcb, err := sess.WaitCallbackFor(waitCtx0, "action/onhold")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/onhold: %v", err)
	}
	s.Logf("action/onhold body: %s", string(hcb.Body))
	s.Done()

	s = Step(t, "lcc-redirect")
	// Re-script the session's call hook: the redirect's session:redirect
	// request hits /hook (correlated via the call's tag/customerData) and now
	// returns the replacement app instead of the original transfer script.
	sess.ScriptCallHook(webhook.Script{
		V("say", "text", "You have been redirected successfully."),
		V("hangup"),
	})
	redirectStart := time.Now()
	body := map[string]any{
		"call_hook": map[string]any{
			"url":    webhookSrv.PublicURL() + "/hook",
			"method": "POST",
		},
	}
	if err := client.UpdateCall(ctx, callSID, body); err != nil {
		s.Fatalf("UpdateCall(call_hook) sid=%s: %v", callSID, err)
	}
	s.Done()

	s = Step(t, "wait-call-ended")
	select {
	case <-call.Done():
	case <-ctx.Done():
		s.Fatalf("caller leg never ended after redirect: %v", ctx.Err())
	}
	elapsed := time.Since(redirectStart)
	s.Logf("updateCall → call-end elapsed: %s", elapsed)
	s.Done()

	s = Step(t, "assert-redirect-prompt")
	// The discriminator: post-fix the replacement say+hangup runs within a few
	// seconds; pre-fix the killed transfer stalled the replacement app until
	// the 30s ring timeout.
	if elapsed >= 20*time.Second {
		s.Errorf("redirect-to-hangup took %s, want <20s — killed transfer stalled the replacement app", elapsed)
	}
	AssertTranscriptContains(s, ctx, recPath, "redirected")
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-canceled")
	if targetCall == nil {
		s.Fatal("target never received INVITE (transfer did not dial the human)")
	}
	RequireRecvMethods(s, targetCall, "INVITE")
	sent := StatusesOf(targetCall.Sent())
	if slices.Contains(sent, 200) {
		s.Errorf("target answered (sent 200) but the redirect abort must CANCEL the ringing leg; statuses = %v", sent)
	}
	s.Done()
}
