//go:build studio

package studiotests

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

const frontPrompt = `You are the front desk of Acme Bank, on the phone. Keep every reply to one or two short sentences.
Branches are open nine to five, Monday to Friday.
When the caller has a billing question, ask for their account number, then hand them to billing.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`

const billingPrompt = `You are the billing desk of Acme Bank, on the phone. Keep every reply to one or two short sentences.
The caller's account number is {{handoff.account_id}}. In your first reply, read that account number back digit by
digit and ask the caller to confirm it.
You know nothing about branches or opening hours: for anything that is not billing, hand the caller back to the front
desk at once, without answering it yourself.`

// receives turns on receiving handoffs, with the last few turns as context and the given inputs.
func receives(description string, inputs ...map[string]any) func(map[string]any) {
	return func(c map[string]any) {
		if inputs == nil {
			inputs = []map[string]any{}
		}
		c["handoff"] = map[string]any{"accept": true, "description": description, "inputs": inputs,
			"context": map[string]any{"mode": "turns", "turns": 6}}
	}
}

func handsTo(c map[string]any, agentID, name, description string) {
	apps, _ := c["applications"].([]any)
	c["applications"] = append(apps, map[string]any{"id": "d-" + agentID[:8], "agent_id": agentID, "name": name,
		"description": description})
}

// TestStudio_Handoff: a front-desk agent hands a billing caller to a billing agent with the account number, and the
// billing agent hands them back for a question it can't answer. One call checks what a caller would notice.
//
// Steps:
//   - setup-agents: front desk (receives, published), billing (receives an account_id, hands back; published), then
//     the front desk's draft hands to billing
//   - call: billing question, account number, a branch-hours question, goodbye
//   - check-handed-over: billing reads the account number back (it was handed over, not asked again)
//   - check-returned: the hours answer came from the front desk after the return
//   - check-answered-fast: each agent that took the call spoke within a few seconds, not after the caller's silence
//   - check-no-ping-pong: exactly the two handoffs the caller asked for, no bouncing between the agents
func TestStudio_Handoff(t *testing.T) {
	ctx := WithTimeout(t, 6*time.Minute)
	t.Parallel()
	v := voice(t, ctx)

	s := Step(t, "setup-agents")
	front, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, frontPrompt), Hangup: true, Voice: v,
		Edit: receives("The caller has a general question, or wants branch information.")})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		s.Fatalf("front desk draft errors: %+v", issues)
	}
	if err := ws.Publish(ctx, front); err != nil {
		s.Fatalf("publish front desk: %v", err)
	}
	billing, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, billingPrompt), Voice: v,
		Edit: func(c map[string]any) {
			receives("The caller has a question about a charge, a bill or a payment.",
				map[string]any{"name": "account_id", "description": "The caller's account number, digits only",
					"type": "string", "required": true})(c)
			handsTo(c, front, "front desk", "The caller asks about anything that is not billing.")
		}})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		s.Fatalf("billing draft errors: %+v", issues)
	}
	if err := ws.Publish(ctx, billing); err != nil {
		s.Fatalf("publish billing: %v", err)
	}
	issues, err := ws.EditDraft(ctx, front, func(c map[string]any) { handsTo(c, billing, "billing", "") })
	if err != nil || slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		s.Fatalf("front desk handing to billing: %v %+v", err, issues)
	}
	s.Done()

	r := call(t, ctx, studio.Scenario{
		Name: "handoff", AgentID: front, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, I have a question about a charge on my bill."},
			{Say: "My account number is 4 4 8 2 1 3."},
			{Say: "Yes, that's right. Actually, first, what time do your branches open?", WaitSec: 20},
			{Say: "Thanks, that's all. Goodbye.", WaitSec: 20},
		},
		MaxSec: 180, TailSec: 15,
		Expect: &studio.Expect{Events: []string{"Handed to billing .*account_id", "(?i)billing answered", "Handed back to front desk"}},
	})
	turns := turnsOf(t, ctx, r.CallSID)

	s = Step(t, "check-handed-over")
	digits := regexp.MustCompile(`(?i)4\D{0,3}4\D{0,3}8\D{0,3}2\D{0,3}1\D{0,3}3|four,? four,? eight,? two,? one,? three`)
	after := agentAfter(turns, "account number", "what time")
	if !slices.ContainsFunc(after, digits.MatchString) {
		s.Errorf("billing never read back the handed-over account number: %q", after)
	}
	s.Done()

	s = Step(t, "check-answered-fast")
	evs, err := ws.CallEvents(ctx, r.CallSID)
	if err != nil {
		s.Fatalf("call events: %v", err)
	}
	secs := regexp.MustCompile(`([0-9.]+) s after the handoff`)
	answered, handed := 0, 0
	for _, e := range evs {
		if strings.HasPrefix(e.Text, "Handed ") {
			handed++
		}
		m := secs.FindStringSubmatch(e.Sub)
		if !strings.HasSuffix(e.Text, " answered") || m == nil {
			continue
		}
		answered++
		if v, _ := strconv.ParseFloat(m[1], 64); v > 6 {
			s.Errorf("%s %s: the caller waited in silence for the next agent", e.Text, e.Sub)
		}
	}
	if answered < 2 {
		s.Errorf("expected an answered event for the handoff and the return, got %d", answered)
	}
	if handed != 2 {
		s.Errorf("the call changed agents %d times; the caller asked for 2 (to billing, back to the front desk)", handed)
	}
	s.Done()

	s = Step(t, "check-returned")
	if hours := agentAfter(turns, "what time", "goodbye"); !anyContains(hours, "nine") && !anyContains(hours, "9") {
		s.Errorf("no opening hours after the question, so the call didn't come back to the front desk: %q", hours)
	}
	s.Done()
}
