//go:build detail

package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Conference_RejoinAfterEndOnExit — the agent dropped by an
// endConferenceOnExit member runs a second [conference] and must get in,
// not be hung up because its endpoint still looks "in a room".
//
// Steps:
//  1. script-conference — agent [conference] whose actionHook returns [conference second],
//     caller [conference endConferenceOnExit]
//  2. first-joins / second-joins — agent then caller in the room
//  3. caller-hangup — caller sends BYE
//  4. agent-leaves-first — the first conference's actionHook fires
//  5. agent-stays-up — agent is in the second room, still up 4s later
func TestVerb_Conference_RejoinAfterEndOnExit(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, agentUAS := claimUAS2(t, ctx)

	s := Step(t, "script-conference")
	room := fmt.Sprintf("jambonz-it-eoe-%d", time.Now().UnixNano())
	agentID, callerID := t.Name()+"-agent", t.Name()+"-caller"
	agentSess, callerSess := webhookReg.New(agentID), webhookReg.New(callerID)
	t.Cleanup(func() {
		webhookReg.Release(agentID)
		webhookReg.Release(callerID)
	})
	agentSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("conference", "name", room, "actionHook", SessionURL(agentSess, "conf-first")),
	}))
	agentSess.ScriptActionHook("conf-first", webhook.Script{
		V("conference", "name", room+"-second"),
	})
	callerSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("conference", "name", room, "endConferenceOnExit", true),
	}))
	s.Done()

	s = Step(t, "first-joins")
	agent := placeWebhookCallTo(ctx, t, agentUAS, agentSess, withTimeLimit(60))
	if err := agent.Answer(); err != nil {
		s.Fatalf("agent Answer: %v", err)
	}
	_ = agent.SendSilence()
	t.Cleanup(func() { _ = agent.Hangup() })
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "second-joins")
	caller := placeWebhookCallTo(ctx, t, callerUAS, callerSess, withTimeLimit(60))
	if err := caller.Answer(); err != nil {
		s.Fatalf("caller Answer: %v", err)
	}
	_ = caller.SendSilence()
	t.Cleanup(func() { _ = caller.Hangup() })
	time.Sleep(5 * time.Second)
	s.Done()

	s = Step(t, "caller-hangup")
	if err := caller.Hangup(); err != nil {
		s.Fatalf("caller Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "agent-leaves-first")
	actx, acancel := context.WithTimeout(ctx, 5*time.Second)
	defer acancel()
	if _, err := agentSess.WaitCallbackFor(actx, "action/conf-first"); err != nil {
		s.Fatalf("agent never left the first conference: %v", err)
	}
	s.Done()

	s = Step(t, "agent-stays-up")
	wctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if err := agent.WaitState(wctx, jsip.StateEnded); err == nil {
		s.Errorf("agent leg %s was hung up instead of joining the second conference", agent.CallID())
	}
	s.Done()
}
