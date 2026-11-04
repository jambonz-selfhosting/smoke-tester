package studio

import (
	"encoding/json"
	"strings"
	"testing"
)

const record = `{
  "cdr": {"completion_reason": "hangup tool", "ended": "agent"},
  "turns": [
    {"role": "agent", "text": "Hi", "latency": null, "interrupted": false, "tool_calls": []},
    {"role": "caller", "text": "where is my order", "latency": null, "tool_calls": []},
    {"role": "agent", "text": "It shipped", "latency": {"total": 900}, "interrupted": true,
     "tool_calls": [{"name": "get_order_status", "ok": true}]},
    {"role": "caller", "text": "thanks", "latency": null, "tool_calls": []},
    {"role": "agent", "text": "Bye", "latency": {"total": 700}, "tool_calls": []}
  ],
  "events": [{"text": "Transfer", "sub": "returned to the agent"}]
}`

func TestCheckPasses(t *testing.T) {
	yes := true
	e := &Expect{CallerTurns: 2, LatencyTurns: 2, ToolsOK: []string{"get_order_status"}, AgentHangup: &yes,
		Completion: "hangup", Events: []string{"(?i)transfer.*returned"}, Heard: []string{"(?i)shipped"}, Interrupted: true}
	if f := Check(e, json.RawMessage(record), true, "it shipped yesterday"); len(f) > 0 {
		t.Fatalf("want pass, got %v", f)
	}
}

func TestCheckNamesEachFailure(t *testing.T) {
	no := false
	e := &Expect{CallerTurns: 3, LatencyTurns: 3, ToolsOK: []string{"book"}, AgentHangup: &no,
		Completion: "^credential", Events: []string{"handoff"}, Heard: []string{"refund"}}
	f := strings.Join(Check(e, json.RawMessage(record), true, "it shipped"), "\n")
	for _, want := range []string{"caller turns with text: 2", "agent turns with latency: 2", "tool book",
		"ended by agent: true", `completion reason "hangup tool"`, `no event matches "handoff"`, `did not hear "refund"`} {
		if !strings.Contains(f, want) {
			t.Errorf("failures miss %q:\n%s", want, f)
		}
	}
}

func TestCheckWithoutRecord(t *testing.T) {
	if f := Check(&Expect{}, nil, false, ""); len(f) != 1 || f[0] != "no Studio call record" {
		t.Fatalf("got %v", f)
	}
}
