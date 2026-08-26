// What happens after a `dial` finishes on a WebSocket application.
//
// A websocket app that runs out of verbs is parked in
// CallSession._awaitCommandsOrHangup() instead of ending the call. That is
// wrong when the app was never asked for verbs — no `actionHook`, or one that
// failed — and leaves the caller in dead air until timeLimit. These tests pin
// the caller's BYE arriving promptly instead.
//
// Phase-2 tests; skipped without NGROK_AUTHTOKEN. Require both UASes
// registered (JAMBONZ_SIP_USER + JAMBONZ_SIP_CALLEE_USER).
package verbs

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Generous: teardown is a couple of round trips, the bug overshoots by minutes.
const dialEndBYEBudget = 10 * time.Second

// Must exceed the budget, or the max-duration timer would mask a regression.
const dialCallTimeLimit = 90

// TestVerb_Dial_WS_NoActionHook_EndsCall — the app was never asked for
// follow-on verbs, so once the callee hangs up the call must end, not park.
//
// Steps:
//  1. script-dial-no-actionhook — [answer, pause, dial target=callee] over the WS app
//  2. spawn-callee-goroutine — async: answer, hold briefly, hang up the B leg
//  3. place-caller-and-answer — place against wsApp, answer, latch RTP
//  4. wait-callee-done — wait for the callee goroutine to finish
//  5. assert-caller-byed — jambonz BYEs the caller within dialEndBYEBudget of the B-leg BYE
func TestVerb_Dial_WS_NoActionHook_EndsCall(t *testing.T) {
	t.Parallel()
	runDialWSEndsCall(t, dialEndsCallCase{
		tag:            "dial-ws-no-actionhook",
		scriptStep:     "script-dial-no-actionhook",
		withActionHook: false,
		byeBudget:      dialEndBYEBudget,
	})
}

// TestVerb_Dial_WS_ActionHookNoAck_EndsCall — an `actionHook` the app never
// acks. WsRequestor gives up after 5s; a failed hook is no better a reason to
// keep the caller on the line than no hook at all.
//
// Steps:
//  1. script-dial-actionhook-noack — [answer, pause, dial actionHook=/action/dial] with the ack withheld
//  2. spawn-callee-goroutine — async: answer, hold briefly, hang up the B leg
//  3. place-caller-and-answer — place against wsApp, answer, latch RTP
//  4. wait-callee-done — wait for the callee goroutine to finish
//  5. assert-ws-got-dial-hook — the actionHook really arrived on the socket (so withholding the ack fails it)
//  6. assert-caller-byed — jambonz BYEs the caller within the ack timeout + budget
func TestVerb_Dial_WS_ActionHookNoAck_EndsCall(t *testing.T) {
	t.Parallel()
	runDialWSEndsCall(t, dialEndsCallCase{
		tag:            "dial-ws-actionhook-noack",
		scriptStep:     "script-dial-actionhook-noack",
		withActionHook: true,
		noAck:          true,
		expectVerbHook: true,
		byeBudget:      5*time.Second + dialEndBYEBudget, // 5s ack timer, then teardown
	})
}

// TestVerb_Dial_WS_ActionHookEmpty_EndsCall — an `actionHook` the app acks with
// `[]`. The hook succeeded but yielded no verbs, which is no better a reason to
// hold the caller than a hook that failed, so the call must end.
//
// This deliberately overrides the ws "ack now, send verbs later" pattern for
// `dial`: an app that means to push verbs afterwards must return them here.
//
// Steps:
//  1. script-dial-actionhook-empty — [answer, pause, dial actionHook=/action/dial] acked with []
//  2. spawn-callee-goroutine — async: answer, hold briefly, hang up the B leg
//  3. place-caller-and-answer — place against wsApp, answer, latch RTP
//  4. wait-callee-done — wait for the callee goroutine to finish
//  5. assert-ws-got-dial-hook — the actionHook really arrived on the socket
//  6. assert-caller-byed — jambonz BYEs the caller within dialEndBYEBudget of the B-leg BYE
func TestVerb_Dial_WS_ActionHookEmpty_EndsCall(t *testing.T) {
	t.Parallel()
	runDialWSEndsCall(t, dialEndsCallCase{
		tag:            "dial-ws-actionhook-empty",
		scriptStep:     "script-dial-actionhook-empty",
		withActionHook: true,
		expectVerbHook: true,
		byeBudget:      dialEndBYEBudget, // acked at once, no ack timer to wait out
	})
}

type dialEndsCallCase struct {
	tag            string
	scriptStep     string
	withActionHook bool // wire a relative actionHook on the dial verb
	noAck          bool // withhold the ack so the hook fails, rather than acking []
	expectVerbHook bool // assert the hook really travelled over the socket
	byeBudget      time.Duration
}

// runDialWSEndsCall drives a WS call whose only verb is a `dial`, with no
// trailing `hangup` so nothing but the fix can end it. Callee hangs up the
// B leg; the caller's BYE must follow within c.byeBudget.
func runDialWSEndsCall(t *testing.T, c dialEndsCallCase) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, c.scriptStep)
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	dial := []any{
		"target", []any{map[string]any{"type": "user", "name": target}},
		"timeout", 20,
		"anchorMedia", true, // keep RTP in the cluster data plane, as in Dial_User_Bridge
	}
	if c.withActionHook {
		/* RELATIVE, not SessionURL(): WsRequestor short-circuits an absolute
		   http(s) hook to a plain HTTP webhook, which never reaches the socket
		   and so can never have its ack withheld. */
		dial = append(dial, "actionHook", "/action/dial")
		if c.noAck {
			sess.ScriptActionHookNoAck("dial")
		} else {
			SessionAckEmpty(sess, "dial")
		}
	}
	// No `hangup`: it would end the call itself and the tests would pass either way.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{V("dial", dial...)}))
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	var calleeByeAt atomic.Int64 // written by the goroutine, read after calleeDone
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case cc := <-calleeUAS.Inbound:
			t.Logf("[callee:answer] start")
			if err := cc.Answer(); err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			if err := cc.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence", "SendSilence: %v", err)
				return
			}
			// Let the bridge establish, so a BYE racing the answer isn't
			// mistaken for the teardown under test.
			time.Sleep(2 * time.Second)
			t.Logf("[callee:hangup] start")
			calleeByeAt.Store(time.Now().UnixNano())
			if err := cc.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
				return
			}
			<-cc.Done()
			t.Logf("[callee] done")
		case <-calleeCtx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
		}
	}()
	// Join even if a later Step fatals; LIFO puts this before the ctx-cancel cleanup.
	t.Cleanup(func() {
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-caller-and-answer")
	call := placeWSCallTo(ctx, t, callerUAS, sess, withTimeLimit(dialCallTimeLimit))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Outbound RTP opens the NAT pinhole so the bridge latches (ADR-0014).
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-callee-done")
	select {
	case <-calleeDone:
	case <-ctx.Done():
		s.Fatalf("callee goroutine never finished: %v", ctx.Err())
	}
	byeAt := time.Unix(0, calleeByeAt.Load())
	if calleeByeAt.Load() == 0 {
		s.Fatal("callee never hung up the B leg; nothing to assert")
	}
	s.Done()

	if c.expectVerbHook {
		s = Step(t, "assert-ws-got-dial-hook")
		hookCtx, hcancel := context.WithTimeout(ctx, 5*time.Second)
		cb, err := sess.WaitCallbackFor(hookCtx, "verb_hook")
		hcancel()
		if err != nil {
			s.Fatalf("dial actionHook never arrived on the app socket "+
				"(so the ack was never withheld and this test proves nothing): %v", err)
		}
		s.Logf("dial verb_hook on socket: %s", string(cb.Body))
		s.Done()
	}

	s = Step(t, "assert-caller-byed")
	// Measured from the B-leg BYE, so time spent joining the goroutine counts.
	deadline := byeAt.Add(c.byeBudget)
	byeCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	if _, err := call.AwaitReceivedRequest(byeCtx, "BYE"); err != nil {
		s.Errorf("%s: caller leg got no BYE within %s of the B-leg hangup "+
			"(the session parked waiting for verbs the application was never going to send): %v",
			c.tag, c.byeBudget, err)
		// Tear the caller leg down ourselves so the run doesn't hold a live
		// call for the rest of timeLimit.
		HangupAndWaitEnded(t, ctx, call)
		s.Done()
		return
	}
	s.Logf("%s: caller BYE arrived %s after the B-leg hangup (budget %s)",
		c.tag, time.Since(byeAt).Round(time.Millisecond), c.byeBudget)
	if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
		s.Errorf("caller call did not reach ended state: %v", err)
	}
	s.Done()
}
