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
	"fmt"
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
