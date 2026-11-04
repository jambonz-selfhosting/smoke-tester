// Package studio drives Agent Studio agents for smoke tests: it signs in, builds
// agents, plays a scripted caller through a Studio test session and checks the
// call against Studio's own record of it.
package studio

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

type Turn struct {
	Say string `json:"say"`
	// WaitAgent: wait for the agent to finish speaking first (default true); false barges in once it starts.
	WaitAgent *bool `json:"wait_agent,omitempty"`
}

type Scenario struct {
	Name      string            `json:"name"`
	AgentID   string            `json:"agent_id"`
	Direction string            `json:"direction"`     // inbound | outbound | inbound-did
	DID       string            `json:"did,omitempty"` // inbound-did: number routed to the published agent
	Language  string            `json:"language"`      // for the offline transcript
	Voice     string            `json:"voice"`         // Deepgram voice for the caller
	Env       map[string]string `json:"env,omitempty"`
	Turns     []Turn            `json:"turns"`
	MaxSec    int               `json:"max_sec,omitempty"`
	TailSec   int               `json:"tail_sec,omitempty"` // wait after the last turn; 0 means 25
	Expect    *Expect           `json:"expect,omitempty"`
}

// Expect: what Studio's call record must show for the scenario to pass. Unset fields are not checked.
type Expect struct {
	CallerTurns  int      `json:"caller_turns,omitempty"`  // caller turns with text, at least
	LatencyTurns int      `json:"latency_turns,omitempty"` // agent turns with a latency record, at least
	ToolsOK      []string `json:"tools_ok,omitempty"`      // tools that must have run and succeeded
	AgentHangup  *bool    `json:"agent_hangup,omitempty"`  // who must end the call
	Completion   string   `json:"completion,omitempty"`    // regexp on cdr.completion_reason
	Events       []string `json:"events,omitempty"`        // regexps each matching some event (text + sub)
	Heard        []string `json:"heard,omitempty"`         // regexps on what the caller heard
	Interrupted  bool     `json:"interrupted,omitempty"`   // an agent turn was cut off by the caller
}

func LoadScenario(path string) (Scenario, error) {
	var sc Scenario
	raw, err := os.ReadFile(path)
	if err != nil {
		return sc, err
	}
	if err := json.Unmarshal(raw, &sc); err != nil {
		return sc, fmt.Errorf("%s: %w", path, err)
	}
	return sc, nil
}

type callDetail struct {
	Cdr struct {
		CompletionReason string `json:"completion_reason"`
		Ended            string `json:"ended"`
	} `json:"cdr"`
	Turns []struct {
		Role        string          `json:"role"`
		Text        string          `json:"text"`
		Latency     json.RawMessage `json:"latency"`
		Interrupted bool            `json:"interrupted"`
		ToolCalls   []struct {
			Name string `json:"name"`
			OK   bool   `json:"ok"`
		} `json:"tool_calls"`
	} `json:"turns"`
	Events []struct {
		Text string `json:"text"`
		Sub  string `json:"sub"`
	} `json:"events"`
}

// Check returns one line per expectation that the call did not meet.
func Check(e *Expect, raw json.RawMessage, endedByAgent bool, heard string) []string {
	var fails []string
	var d callDetail
	if len(raw) == 0 || json.Unmarshal(raw, &d) != nil {
		return []string{"no Studio call record"}
	}
	caller, latency, interrupted := 0, 0, false
	tools := map[string]bool{}
	for _, t := range d.Turns {
		if t.Role == "caller" && strings.TrimSpace(t.Text) != "" {
			caller++
		}
		if t.Role == "agent" && len(t.Latency) > 0 && string(t.Latency) != "null" {
			latency++
		}
		interrupted = interrupted || t.Interrupted
		for _, c := range t.ToolCalls {
			tools[c.Name] = tools[c.Name] || c.OK
		}
	}
	if caller < e.CallerTurns {
		fails = append(fails, fmt.Sprintf("caller turns with text: %d, want >= %d", caller, e.CallerTurns))
	}
	if latency < e.LatencyTurns {
		fails = append(fails, fmt.Sprintf("agent turns with latency: %d, want >= %d", latency, e.LatencyTurns))
	}
	for _, name := range e.ToolsOK {
		if !tools[name] {
			fails = append(fails, fmt.Sprintf("tool %s did not succeed", name))
		}
	}
	if e.AgentHangup != nil && *e.AgentHangup != endedByAgent {
		fails = append(fails, fmt.Sprintf("ended by agent: %v, want %v", endedByAgent, *e.AgentHangup))
	}
	if e.Completion != "" && !regexp.MustCompile(e.Completion).MatchString(d.Cdr.CompletionReason) {
		fails = append(fails, fmt.Sprintf("completion reason %q does not match %q", d.Cdr.CompletionReason, e.Completion))
	}
	for _, re := range e.Events {
		r, hit := regexp.MustCompile(re), false
		for _, ev := range d.Events {
			hit = hit || r.MatchString(ev.Text+" "+ev.Sub)
		}
		if !hit {
			fails = append(fails, fmt.Sprintf("no event matches %q", re))
		}
	}
	for _, re := range e.Heard {
		if !regexp.MustCompile(re).MatchString(heard) {
			fails = append(fails, fmt.Sprintf("caller did not hear %q", re))
		}
	}
	if e.Interrupted && !interrupted {
		fails = append(fails, "no agent turn was interrupted")
	}
	return fails
}
