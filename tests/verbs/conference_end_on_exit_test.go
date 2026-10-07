// Regression test for `conference` with endConferenceOnExit.
//
// When the member that joined with endConferenceOnExit hangs up, the room
// must end and every other member must be released; a member whose app has
// no verbs after `conference` must then get a BYE from jambonz. Reported in
// the field as "the other leg stays connected after the caller hung up".
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

// TestVerb_Conference_EndOnExit — caller joins with endConferenceOnExit,
// agent joins plainly; the caller hangs up and the agent must be hung up.
//
// Steps (per join order):
//  1. script-conference — agent [conference], caller [conference endConferenceOnExit]
//  2. first-joins — first leg answered and in the room
//  3. second-joins — second leg answered and in the room
//  4. caller-hangup — caller sends BYE after both have been bridged for 3s
//  5. agent-gets-bye — agent leg hung up by jambonz within 5s
func TestVerb_Conference_EndOnExit(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, agentUAS := claimUAS2(t, ctx)

	for _, agentFirst := range []bool{true, false} {
		name := "caller-first"
		if agentFirst {
			name = "agent-first"
		}
		ok := t.Run(name, func(t *testing.T) {
			runConferenceEndOnExit(t, ctx, callerUAS, agentUAS, agentFirst)
		})
		if !ok {
			return
		}
	}
}

func runConferenceEndOnExit(t *testing.T, ctx context.Context, callerUAS, agentUAS *UAS, agentFirst bool) {
	s := Step(t, "script-conference")
	room := fmt.Sprintf("jambonz-it-eoe-%d", time.Now().UnixNano())
	agentID, callerID := t.Name()+"-agent", t.Name()+"-caller"
	agentSess, callerSess := webhookReg.New(agentID), webhookReg.New(callerID)
	t.Cleanup(func() {
		webhookReg.Release(agentID)
		webhookReg.Release(callerID)
	})
	agentSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("conference", "name", room),
	}))
	callerSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("conference", "name", room, "endConferenceOnExit", true),
	}))
	s.Done()

	join := func(s *StepCtx, uas *UAS, sess *webhook.Session) *jsip.Call {
		c := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
		if err := c.Answer(); err != nil {
			s.Fatalf("Answer: %v", err)
		}
		_ = c.SendSilence()
		t.Cleanup(func() { _ = c.Hangup() })
		// warmup answer+pause, then the conference join
		time.Sleep(2 * time.Second)
		return c
	}

	var agent, caller *jsip.Call
	s = Step(t, "first-joins")
	if agentFirst {
		agent = join(s, agentUAS, agentSess)
	} else {
		caller = join(s, callerUAS, callerSess)
	}
	s.Done()

	s = Step(t, "second-joins")
	if agentFirst {
		caller = join(s, callerUAS, callerSess)
	} else {
		agent = join(s, agentUAS, agentSess)
	}
	s.Done()

	s = Step(t, "caller-hangup")
	time.Sleep(3 * time.Second)
	if agent.State() == jsip.StateEnded || caller.State() == jsip.StateEnded {
		s.Fatalf("a leg ended before the caller hung up (agent=%s caller=%s)", agent.State(), caller.State())
	}
	if err := caller.Hangup(); err != nil {
		s.Fatalf("caller Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "agent-gets-bye")
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := agent.WaitState(wctx, jsip.StateEnded); err != nil {
		s.Errorf("agent leg %s still up 5s after the endConferenceOnExit member hung up", agent.CallID())
	}
	s.Done()
}
