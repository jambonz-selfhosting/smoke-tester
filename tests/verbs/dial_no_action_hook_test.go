// Tests for what happens *after* a `dial` verb finishes on a WebSocket
// application.
//
// An http application that runs out of verbs ends the call: the task list is
// empty, the session tears down, jambonz BYEs the caller. A websocket
// application is different — feature-server parks the session in
// CallSession._awaitCommandsOrHangup() waiting for the app to push more verbs
// over the socket. That is the right behaviour when the app *was* asked for
// verbs and is thinking about it, but not when it was never asked at all:
//
//   - `dial` with no `actionHook` — nothing was ever sent to the app, so
//     nothing is coming back.
//   - `dial` whose actionHook request failed — over WS the only failure mode
//     is the app never acking, which feature-server gives up on after
//     JAMBONES_WS_API_MSG_RESPONSE_TIMEOUT (5s).
//
// In both cases the B leg is gone and the caller sits in dead air until some
// unrelated timer (timeLimit, media timeout) eventually fires. These tests
// pin the fix: the caller gets a BYE promptly instead.
//
// The discriminator is timing, so the calls are placed with a deliberately
// long timeLimit — long enough that a regression cannot be rescued by the
// max-duration timer and quietly pass.
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

// dialEndBYEBudget is how long after the B leg hangs up we allow jambonz to
// BYE the caller when the application has no follow-on verbs. Teardown is a
// couple of round trips, so this is generous; the failure it guards against
// is an indefinite park, which overshoots by a minute or more.
const dialEndBYEBudget = 10 * time.Second

// dialCallTimeLimit is the POST /Calls timeLimit for these tests. It must
// comfortably exceed dialEndBYEBudget (plus the WS ack timeout in the
// no-ack case) so that a parked call is caught by the assertion rather than
// being torn down by the max-duration timer, which would mask the bug.
const dialCallTimeLimit = 90

// TestVerb_Dial_WS_NoActionHook_EndsCall — `dial` with no `actionHook` on a
// WebSocket application. The app was never asked for follow-on verbs, so
// once the callee hangs up the call must end rather than park.
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
		tag:        "dial-ws-no-actionhook",
		scriptStep: "script-dial-no-actionhook",
		// No actionHook on the verb at all.
		withActionHook: false,
		byeBudget:      dialEndBYEBudget,
	})
}

// TestVerb_Dial_WS_ActionHookNoAck_EndsCall — `dial` with an `actionHook`
// that the WebSocket application never acks. feature-server's WsRequestor
// rejects the pending verb:hook after JAMBONES_WS_API_MSG_RESPONSE_TIMEOUT
// (5s by default); a failed actionHook is no better a reason to keep the
// caller on the line than no actionHook at all, so the call must end.
//
// Withholding the ack is the only way to fail a hook over WS transport —
// there is no status code to return — see Session.ScriptActionHookNoAck.
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
		expectVerbHook: true,
		// feature-server waits out its 5s ack timer before the hook fails,
		// and only then ends the call.
		byeBudget: 5*time.Second + dialEndBYEBudget,
	})
}

// dialEndsCallCase is the per-test knobs for runDialWSEndsCall.
type dialEndsCallCase struct {
	tag        string
	scriptStep string
	// withActionHook wires a relative actionHook on the dial verb whose ack
	// the WS app then withholds.
	withActionHook bool
	// expectVerbHook asserts the hook actually travelled over the app socket
	// before the BYE assertion runs.
	expectVerbHook bool
	byeBudget      time.Duration
}

// runDialWSEndsCall drives a WS-application call whose only verb is a `dial`
// — deliberately with no trailing `hangup`, so the only thing that can end
// the call is feature-server deciding there are no follow-on verbs coming.
// The callee hangs up the B leg; the assertion is that the caller's BYE
// follows within c.byeBudget.
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
		// anchorMedia keeps both legs' RTP inside the cluster data plane —
		// same reason as TestVerb_Dial_User_Bridge.
		"anchorMedia", true,
	}
	if c.withActionHook {
		// RELATIVE, not SessionURL(). feature-server's WsRequestor short-circuits
		// an absolute http(s) hook to a plain HTTP webhook
		// (lib/utils/ws-requestor.js: "if we have an absolute url, and it is http
		// then do a standard webhook"), which would never reach the app socket and
		// so could never have its ack withheld. A relative path is sent as a
		// verb:hook frame — the thing under test. Correlation still works: the WS
		// connection is already bound to this session.
		dial = append(dial, "actionHook", "/action/dial")
		sess.ScriptActionHookNoAck("dial")
	}
	// NOTE: no `hangup` after the dial. That is the whole point — if the
	// script ended the call itself these tests would pass either way.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{V("dial", dial...)}))
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	// calleeByeAt is written by the goroutine and read by the main test
	// goroutine after calleeDone closes; atomic keeps the race detector
	// quiet if a future change reads it earlier.
	var calleeByeAt atomic.Int64
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
			// Hold the bridge up long enough to be unambiguously
			// established before tearing it down, so a BYE that races the
			// answer can't be mistaken for the teardown under test.
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
	// Join the goroutine even if a later Step fatals — registered after
	// spawn so it runs (LIFO) before WithTimeout's ctx-cancel cleanup.
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
	// Budget is measured from the B-leg BYE, not from now, so time already
	// spent joining the goroutine counts against it.
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
