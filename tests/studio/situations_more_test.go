//go:build studio

package studiotests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

// The rest of the situations coverage: limits and engines, a server-only situation that hides a tool, makes room at
// the cap and is resolved and replaced in one result, MCP tools, the timeline tags, situations kept across a
// transfer, a detour and a gw reconnect, a published version on a DID, and the two gaps the review found
// (docs/plan/MID-CALL-CONTROL-REVIEW.md in agent-studio): the history summary and prerequisites.

// fourModelSituations: three fill the cap, the fourth is refused until one is resolved.
var fourModelSituations = []studio.Situation{
	{Name: "hardship", Who: "model", When: "the caller says they can't pay because they lost their job or are ill.",
		Instructions: "Be warm and unhurried. Tell the caller we can pause payments for a month. This situation is resolved when the caller says they have found a new job: then call resolve_situation with hardship."},
	{Name: "payment_dispute", Who: "model", When: "the caller says they already paid or disputes an amount.",
		Instructions: "Do not argue. Say a specialist will review the payment within two business days."},
	{Name: "callback_request", Who: "model", When: "the caller asks to be called back later.",
		Instructions: "Say we will call them back tomorrow morning."},
	{Name: "legal_question", Who: "model", When: "the caller mentions a lawyer, court or their legal rights.",
		Instructions: "Do not give legal advice. Say they are welcome to have their lawyer contact us in writing."},
}

func cleanupCtx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Second)
}

// httpTool creates a run-unique GET tool on the customer server and returns its name and id.
func httpTool(t *testing.T, ctx context.Context, srv *customerServer, key, desc string, params map[string]string) (string, string) {
	t.Helper()
	name := key + "_" + provision.RunID()
	id, err := ws.CreateTool(ctx, studio.HTTPGetTool(name, desc, srv.URL+"/"+key, params))
	if err != nil {
		t.Fatalf("tool %s: %v", name, err)
	}
	t.Cleanup(func() { c, cancel := cleanupCtx(); defer cancel(); _ = ws.DeleteTool(c, id) })
	return name, id
}

func prompt(t *testing.T, ctx context.Context, text string) string {
	t.Helper()
	id, err := ws.CreatePrompt(ctx, "smoke "+provision.RunID()+": "+t.Name(), text)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	t.Cleanup(func() { c, cancel := cleanupCtx(); defer cancel(); _ = ws.DeletePrompt(c, id) })
	return id
}

func mustClean(t *testing.T, issues []studio.Issue) {
	t.Helper()
	var bad []studio.Issue
	for _, i := range issues {
		if i.Severity == "error" {
			bad = append(bad, i)
		}
	}
	if len(bad) > 0 {
		t.Fatalf("draft has errors: %+v", bad)
	}
}

func playWith(t *testing.T, ctx context.Context, sc studio.Scenario, before func(int)) *studio.Result {
	t.Helper()
	r, err := studio.Run(ctx, ws, sc, studio.RunOptions{OutDir: outDir, SBC: sbc, Logf: t.Logf, BeforeTurn: before,
		FromHost: os.Getenv("STUDIO_DID_FROM_HOST")})
	if err != nil {
		t.Fatalf("%s: %v (artifacts in %s)", sc.Name, err, r.Dir)
	}
	t.Logf("call_sid=%s ended_by_agent=%v, artifacts in %s\n%s", r.CallSID, r.EndedByAgent, r.Dir, r.Transcript)
	if len(r.Failures) > 0 {
		t.Errorf("%s: Studio's record of call %s misses:\n  %s", sc.Name, r.CallSID, strings.Join(r.Failures, "\n  "))
	}
	return r
}

// situationEvents returns the call's situation events as "change name" in timeline order, and checks each one is
// tagged for the Calls page.
func situationEvents(t *testing.T, ctx context.Context, callSID string) []string {
	t.Helper()
	evs, err := ws.CallEvents(ctx, callSID)
	if err != nil {
		t.Fatalf("call events: %v", err)
	}
	var out []string
	for _, e := range evs {
		if strings.HasPrefix(e.Text, "Agent update rejected") {
			t.Errorf("jambonz rejected an agent update: %s %s", e.Text, e.Sub)
		}
		for _, verb := range []string{"Started", "Resolved", "Dropped", "Refused to start", "Refused to resolve"} {
			if !strings.HasPrefix(e.Text, verb+" ") {
				continue
			}
			if e.Situation == nil {
				t.Errorf("situation event %q has no situation tag", e.Text)
				break
			}
			out = append(out, e.Situation.Change+" "+e.Situation.Name)
			break
		}
	}
	t.Logf("situation timeline: %v", out)
	return out
}

func count(list []string, v string) int {
	n := 0
	for _, x := range list {
		if x == v {
			n++
		}
	}
	return n
}

// TestStudio_Situations_CompileLimits: more than 10 situations is refused, and the engines that can't change
// instructions mid-call (hosted, harness) flag situations.
//
// Steps:
//   - eleven: a draft with 11 situations is refused
//   - engines: hosted and harness agents with a situation get SITUATION_ENGINE_UNSUPPORTED
func TestStudio_Situations_CompileLimits(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	id, _ := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, deskPrompt)})

	t.Run("eleven", func(t *testing.T) {
		var many []studio.Situation
		for i := range 11 {
			many = append(many, studio.Situation{Name: fmt.Sprintf("case_%d", i), Who: "code", Instructions: "x"})
		}
		_, err := ws.EditDraft(ctx, id, func(cfg map[string]any) { studio.SetSituations(cfg, many) })
		var ae *studio.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("11 situations: got %v, want 400", err)
		}
		_, err = ws.EditDraft(ctx, id, func(cfg map[string]any) { studio.SetSituations(cfg, many[:10]) })
		if err != nil {
			t.Errorf("10 situations refused: %v", err)
		}
	})

	t.Run("engines", func(t *testing.T) {
		for _, body := range []map[string]any{
			{"engine": "hosted", "hosted_platform": "elevenlabs"},
			{"engine": "harness"},
		} {
			name := fmt.Sprintf("smoke %s %s situations", provision.RunID(), body["engine"])
			body["name"] = name
			var created struct {
				Agent *struct {
					ID string `json:"id"`
				} `json:"agent"`
			}
			if err := ws.Do(ctx, http.MethodPost, "/agents", body, &created); err != nil || created.Agent == nil {
				t.Logf("%s agent not created (%v): engine not enabled on this Studio, not checked", body["engine"], err)
				continue
			}
			aid := created.Agent.ID
			t.Cleanup(func() { c, cancel := cleanupCtx(); defer cancel(); _ = ws.DeleteAgent(c, aid, name) })
			issues, err := ws.EditDraft(ctx, aid, func(cfg map[string]any) {
				studio.SetSituations(cfg, fourModelSituations[:1])
			})
			if err != nil {
				t.Errorf("%s draft: %v", body["engine"], err)
				continue
			}
			if !hasIssue(issues, "SITUATION_ENGINE_UNSUPPORTED", "error", "") {
				t.Errorf("%s agent: no SITUATION_ENGINE_UNSUPPORTED; got %+v", body["engine"], errorsOf(issues, "SITUATION_"))
			}
		}
	})
}

const complaintsPrompt = `You are the customer care line for Acme Bank, speaking on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help. Follow your situations' instructions when they apply.
When the caller asks for their balance, call check_balance with their account number, if you have that tool.
When the caller wants to file a formal complaint, call file_complaint with a short reason.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`

// TestStudio_Situations_ServerAdvanced: a server-only situation that hides a base tool, starts at the cap by dropping
// the oldest situation the model started, is not started twice, and is resolved and replaced in one tool result.
//
// Steps:
//   - setup: customer server, tools check_balance (base), file_complaint (starts escalation), add_note (only in
//     escalation; its result names escalation again) and close_complaint (resolves escalation, starts followup)
//   - call: balance, three model situations, a complaint, a note, the balance again, close the complaint, goodbye
//   - record: hardship dropped for escalation, escalation started once, resolved and followup started by
//     close_complaint, every situation event tagged
//   - server: check_balance was not called while escalation hid it
func TestStudio_Situations_ServerAdvanced(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	srv := startCustomerServer(t)
	bal, balID := httpTool(t, ctx, srv, "check_balance", "Look up the caller's balance by account number.",
		map[string]string{"account": "The account number, digits only."})
	fileC, fileID := httpTool(t, ctx, srv, "file_complaint", "File a formal complaint. Returns the complaint reference.",
		map[string]string{"reason": "A short reason for the complaint."})
	note, noteID := httpTool(t, ctx, srv, "add_note", "Add a detail the caller gives to their open complaint.",
		map[string]string{"note": "The detail, in a few words."})
	closeC, closeID := httpTool(t, ctx, srv, "close_complaint", "Close the caller's complaint once they say it is sorted.",
		map[string]string{"outcome": "A short outcome."})
	sits := append(slices.Clone(fourModelSituations[:3]),
		studio.Situation{Name: "escalation", Who: "code", Add: []string{note, closeC}, Hide: []string{bal},
			Instructions: "A formal complaint is open. Balance lookups are paused while it is open: if asked, say the balance is unavailable until the complaint is closed. Add any detail the caller gives with add_note. When the caller says the complaint is sorted, call close_complaint."},
		studio.Situation{Name: "followup", Who: "code",
			Instructions: "The complaint is closed. Offer the caller a short satisfaction survey by text message."})
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, complaintsPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), ToolIDs: []string{balID, fileID, noteID, closeID}, Situations: sits})
	mustClean(t, issues)

	yes := true
	r := playWith(t, ctx, studio.Scenario{
		Name: "situations-server-advanced", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, what's my balance? My account number is 5 5 5 1."},
			{Say: "I lost my job last month, so I can't pay right now."},
			{Say: "Also, I already paid two hundred dollars last week and it isn't showing."},
			{Say: "Can you call me back tomorrow about all this?"},
			{Say: "I want to file a formal complaint about the missing payment."},
			{Say: "Please add a note to the complaint that I paid on March third."},
			{Say: "And what's my balance right now?"},
			{Say: "Actually the payment just showed up, so the complaint is sorted. Please close it."},
			{Say: "Okay, thank you. That's all, goodbye."},
		},
		MaxSec: 360, TailSec: 20,
		Expect: &studio.Expect{
			CallerTurns: 7, AgentHangup: &yes, ToolsOK: []string{bal, fileC, closeC},
			Events: []string{
				"Dropped hardship to make room for a situation started by tool " + fileC,
				"Started escalation by tool " + fileC,
				"Resolved escalation by tool " + closeC,
				"Started followup by tool " + closeC,
			},
		},
	}, nil)

	got := situationEvents(t, ctx, r.CallSID)
	if n := count(got, "entered escalation"); n != 1 {
		t.Errorf("escalation started %d times, want 1 (add_note names it again while it is active)", n)
	}
	calls := srv.calls()
	t.Logf("customer server calls: %v", calls)
	opened, closed := slices.Index(calls, "file_complaint"), slices.Index(calls, "close_complaint")
	if opened >= 0 {
		end := len(calls)
		if closed > opened {
			end = closed
		}
		if slices.Contains(calls[opened:end], "check_balance") {
			t.Errorf("check_balance reached the server while escalation hid it: %v", calls)
		}
	}
	if !slices.Contains(calls, "add_note") {
		t.Logf("note: the model did not call add_note, so a repeated start was not exercised")
	}
}

// mcpServer is a minimal streamable-HTTP MCP server with one tool whose result starts a server-only situation.
func (s *customerServer) mcp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if json.Unmarshal(body, &req) != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if len(req.ID) == 0 { // a notification
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var result any
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(req.Params, &p)
		result = map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": "smoke-situations", "version": "1.0.0"}}
	case "tools/list":
		result = map[string]any{"tools": []any{map[string]any{
			"name": "open_ticket", "description": "Open a support ticket for an outage. Returns the ticket number.",
			"inputSchema": map[string]any{"type": "object", "required": []string{"issue"},
				"properties": map[string]any{"issue": map[string]any{"type": "string", "description": "The problem in a few words."}}},
		}}}
	case "tools/call":
		s.note("mcp:open_ticket")
		text, _ := json.Marshal(map[string]any{"ticket": "T88", "actions": map[string]any{"situation": "outage"}})
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": string(text)}}}
	default:
		result = map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// TestStudio_Situations_MCP: an MCP tool's result starts a server-only situation.
//
// Steps:
//   - setup: an MCP server on the customer server, an agent using its open_ticket tool, a server-only outage
//     situation
//   - call: report an outage, say goodbye
//   - record: outage started by the MCP tool, and the ticket reached the caller
func TestStudio_Situations_MCP(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	srv := startCustomerServer(t)
	name := "smoke_" + provision.RunID()
	mcpID, err := ws.CreateMcpServer(ctx, name, srv.URL+"/mcp")
	if err != nil {
		t.Fatalf("mcp server: %v", err)
	}
	t.Cleanup(func() { c, cancel := cleanupCtx(); defer cancel(); _ = ws.DeleteMcpServer(c, mcpID) })
	tool := name + "__open_ticket"
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{Hangup: true, Voice: borrowedVoice(t, ctx),
		PromptID: prompt(t, ctx, deskPrompt+"\nWhen the caller reports an outage, open a ticket with "+tool+"."),
		Situations: []studio.Situation{{Name: "outage", Who: "code",
			Instructions: "An outage ticket is open. Give the caller the ticket number from the tool result and say engineers are on it."}},
		Edit: func(cfg map[string]any) {
			studio.Sub(cfg, "tools")["mcp"] = []map[string]any{{"server_id": mcpID,
				"tools": []map[string]any{{"name": "open_ticket", "enabled": true}}}}
		}})
	mustClean(t, issues)
	yes := true
	r := playWith(t, ctx, studio.Scenario{
		Name: "situations-mcp", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, my internet has been down since this morning. Can you open a ticket?"},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 150, TailSec: 20,
		Expect: &studio.Expect{CallerTurns: 2, AgentHangup: &yes, ToolsOK: []string{tool},
			Events: []string{"Started outage by tool " + tool}, Heard: []string{`(?i)T[\s-]*8[\s-]*8|eighty[\s-]eight`}},
	}, nil)
	situationEvents(t, ctx, r.CallSID)
	if !slices.Contains(srv.calls(), "mcp:open_ticket") {
		t.Errorf("open_ticket never reached the MCP server")
	}
}

const awayPrompt = deskPrompt + `
When the caller asks about billing, tell them you are putting them through and use the transfer tool. If it does not go through, say billing is not available right now and carry on.
When the caller wants to pay by card, use the secure detour tool. If they come back, say the card line is not available right now and carry on.`

// keptAcross plays two model situations, then something that takes the call away from the agent (before turn 2),
// then two more: with the first two kept, the fourth is refused at the cap of 3.
func keptAcross(t *testing.T, name, away string, edit func(cfg map[string]any), before func(int)) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, awayPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), Situations: fourModelSituations, Edit: edit})
	mustClean(t, issues)
	turns := []studio.Turn{
		{Say: "I lost my job last month, so I can't pay right now."},
		{Say: "Also, I already paid two hundred dollars last week and it isn't showing."},
	}
	if away != "" {
		turns = append(turns, studio.Turn{Say: away})
	}
	turns = append(turns,
		studio.Turn{Say: "Can you call me back tomorrow about all this?"},
		studio.Turn{Say: "And I'm going to talk to my lawyer about these charges."},
		studio.Turn{Say: "Okay, thank you. That's all, goodbye."})
	r := playWith(t, ctx, studio.Scenario{
		Name: name, AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: turns, MaxSec: 300, TailSec: 20,
		Expect: &studio.Expect{CallerTurns: 4,
			Events: []string{"Started hardship by the agent", "Started payment_dispute by the agent",
				"Started callback_request by the agent"}},
	}, before)
	got := situationEvents(t, ctx, r.CallSID)
	// Kept means the gateway still holds the first two after the call came back: either the fourth is refused at
	// the cap, or one of them is resolved later (the gateway records a resolve only for an active situation).
	after := got
	if i := slices.Index(got, "entered callback_request"); i >= 0 {
		after = got[i:]
	}
	if !slices.Contains(after, "refused legal_question") && !slices.Contains(after, "resolved hardship") &&
		!slices.Contains(after, "resolved payment_dispute") {
		t.Errorf("after %s the gateway shows no sign of hardship or payment_dispute (timeline %v)", name, got)
	}
}

// TestStudio_Situations_TransferReturn: active situations are kept when a transfer fails and the caller comes back.
//
// Steps:
//   - setup: an agent with a billing transfer to a number that never answers
//   - call: two situations, ask for billing (the transfer comes back), two more situations
//   - record: the fourth is refused at the cap, or one of the first two is resolved later, so they were kept
func TestStudio_Situations_TransferReturn(t *testing.T) {
	keptAcross(t, "situations-transfer-return", "Can you put me through to someone in billing?", func(cfg map[string]any) {
		tr := studio.Sub(cfg, "transfer")
		tr["enabled"] = true
		tr["destination"] = map[string]any{"type": "phone", "number": studio.BillingNumber}
		tr["description"] = "Billing questions go to the billing team."
		tr["mode"] = "blind-dial"
		tr["timeout_s"] = 10
	}, nil)
}

// TestStudio_Situations_DetourReturn: active situations are kept when a secure detour fails and the caller comes back.
//
// Steps:
//   - setup: an agent with a secure detour to a number that never answers, returning on failure
//   - call: two situations, ask to pay by card (the detour comes back), two more situations
//   - record: the fourth is refused at the cap, or one of the first two is resolved later, so they were kept
func TestStudio_Situations_DetourReturn(t *testing.T) {
	keptAcross(t, "situations-detour-return", "I'd like to pay by card now, please.", func(cfg map[string]any) {
		tr := studio.Sub(cfg, "transfer")
		tr["detour"] = map[string]any{"enabled": true,
			"destination": map[string]any{"type": "phone", "number": studio.BillingNumber},
			"say":         "Taking you to our secure card line.", "when": "the caller wants to pay by card",
			"on_failure": "return", "give_up_min": 1}
	}, nil)
}

// TestStudio_Situations_Reconnect: active situations survive the gateway restarting mid-call. Runs only with
// STUDIO_GW_RESTART_CMD, a shell command that restarts Studio's gw (e.g. ssh ... 'pm2 restart studio-gw').
//
// Steps:
//   - call: two situations, the gw restarts, two more situations
//   - record: the fourth is refused at the cap, so the first two were restored
func TestStudio_Situations_Reconnect(t *testing.T) {
	cmd := os.Getenv("STUDIO_GW_RESTART_CMD")
	if cmd == "" {
		t.Skip("STUDIO_GW_RESTART_CMD not set")
	}
	keptAcross(t, "situations-reconnect", "", nil, func(i int) {
		if i != 2 {
			return
		}
		out, err := exec.Command("sh", "-c", cmd).CombinedOutput()
		t.Logf("gw restart (%v): %s", err, strings.TrimSpace(string(out)))
		time.Sleep(5 * time.Second)
	})
}

// TestStudio_Situations_Published: a published version answers a DID call with its situations. Runs only with
// STUDIO_DID (the number), STUDIO_DID_NUMBER_SID (its jambonz sid) and STUDIO_DID_FROM_HOST (a whitelisted carrier IP).
//
// Steps:
//   - setup: publish the agent and route the number to it
//   - call: the hardship line on the DID
//   - record: hardship started by the agent
func TestStudio_Situations_Published(t *testing.T) {
	did, numSID := os.Getenv("STUDIO_DID"), os.Getenv("STUDIO_DID_NUMBER_SID")
	if did == "" || numSID == "" || os.Getenv("STUDIO_DID_FROM_HOST") == "" {
		t.Skip("STUDIO_DID, STUDIO_DID_NUMBER_SID and STUDIO_DID_FROM_HOST not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, deskPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), Situations: fourModelSituations})
	mustClean(t, issues)
	if err := ws.Publish(ctx, id); err != nil {
		t.Fatalf("publish: %v", err)
	}
	if err := ws.AssignNumber(ctx, id, numSID); err != nil {
		t.Fatalf("route %s: %v", did, err)
	}
	t.Cleanup(func() { c, cancel := cleanupCtx(); defer cancel(); _ = ws.UnassignNumber(c, id, numSID) })
	yes := true
	r := playWith(t, ctx, studio.Scenario{
		Name: "situations-published", AgentID: id, Direction: "inbound-did", DID: did, Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month, so I can't pay right now."},
			{Say: "Okay, thank you. That's all, goodbye."},
		},
		MaxSec: 150, TailSec: 20,
		Expect: &studio.Expect{CallerTurns: 1, AgentHangup: &yes, Events: []string{"Started hardship by the agent"}},
	}, nil)
	situationEvents(t, ctx, r.CallSID)
}

// TestStudio_Situations_Gap_SummaryKept (review finding A): after feature-server summarizes the history, a situation
// change must not erase the summary. Runs only with STUDIO_FS_SUMMARIZE_TURNS set to the JAMBONES_AGENT_SUMMARIZE_TURNS
// feature-server runs with (8 or more).
//
// Steps:
//   - call: the caller gives a name and account number, small talk until feature-server summarizes, a situation
//     starts, the caller asks for their name and account number
//   - heard: the agent still knows both
func TestStudio_Situations_Gap_SummaryKept(t *testing.T) {
	n := os.Getenv("STUDIO_FS_SUMMARIZE_TURNS")
	if n == "" {
		t.Skip("STUDIO_FS_SUMMARIZE_TURNS not set: feature-server does not summarize")
	}
	var every int
	fmt.Sscanf(n, "%d", &every)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, deskPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), Situations: fourModelSituations})
	mustClean(t, issues)
	small := []string{"What are your opening hours?", "Do you have a branch downtown?", "Is there a fee for paper statements?",
		"Can I change my address by phone?", "How long does a transfer take?", "Do you offer savings accounts?",
		"What's the interest on savings?", "Can I order new checks?", "Is the mobile app free?", "Do you work on Saturdays?"}
	turns := []studio.Turn{{Say: "Hi, my name is Priyanka and my account number is 7 7 1 2."}}
	for i := 0; len(turns) < every+1 && i < len(small); i++ {
		turns = append(turns, studio.Turn{Say: small[i]})
	}
	turns = append(turns,
		studio.Turn{Say: "Can you call me back tomorrow about my account?"},
		studio.Turn{Say: "Before I go, can you remind me what name and account number I gave you?"},
		studio.Turn{Say: "Thanks, goodbye."})
	playWith(t, ctx, studio.Scenario{
		Name: "situations-gap-summary", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: turns, MaxSec: 540, TailSec: 20,
		Expect: &studio.Expect{Events: []string{"Started callback_request by the agent"},
			Heard: []string{`(?i)priyanka`, `(?i)7[\s,.-]*7[\s,.-]*1[\s,.-]*2|seven[\s,.-]+seven[\s,.-]+one[\s,.-]+two`}},
	}, nil)
}

// TestStudio_Situations_RequiresGate (review finding F): a model situation that requires a server-only one can't
// start before it. hardship requires negotiation, which only verify_identity starts.
//
// Steps:
//   - call: before verifying, the caller says they lost their job and asks for a plan, then verifies, then says
//     the job line again
//   - record: hardship is not started before negotiation, and no plan is offered before verification
func TestStudio_Situations_RequiresGate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	srv := startCustomerServer(t)
	verify, verifyID := httpTool(t, ctx, srv, "verify_identity", "Verify the caller. Returns verified true or false, and their balance when verified.",
		map[string]string{"last4": "The last four digits of the account number, digits only.", "zip": "The ZIP code, digits only."})
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, collectionsPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), ToolIDs: []string{verifyID},
		Situations: []studio.Situation{
			{Name: "negotiation", Who: "code", Instructions: "The caller is verified. Tell them their balance."},
			{Name: "hardship", Who: "model", When: "the caller says they can't pay because they lost their job or are ill.",
				Instructions: "Offer the smallest plan: twenty five dollars a month for twelve months.", Requires: "negotiation"},
		}})
	mustClean(t, issues)
	yes := true
	r := playWith(t, ctx, studio.Scenario{
		Name: "situations-requires", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month. What payment plans can you give me?"},
			{Say: "Okay. The last four digits are 4 3 2 1 and my ZIP code is 0 2 1 3 9."},
			{Say: "Like I said, I lost my job, so I can't pay the full amount."},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 200, TailSec: 20,
		Expect: &studio.Expect{AgentHangup: &yes, ToolsOK: []string{verify},
			Events: []string{"Started negotiation by tool " + verify}},
	}, nil)
	got := situationEvents(t, ctx, r.CallSID)
	h, n := slices.Index(got, "entered hardship"), slices.Index(got, "entered negotiation")
	if h >= 0 && (n < 0 || h < n) {
		t.Errorf("hardship started before negotiation (timeline %v)", got)
	}
	turns, err := ws.CallTurns(ctx, r.CallSID)
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	for _, tn := range turns {
		if tn.Role == "caller" && strings.Contains(strings.ToLower(tn.Text), "zip") {
			break
		}
		if tn.Role == "agent" && (strings.Contains(strings.ToLower(tn.Text), "twenty five") || strings.Contains(tn.Text, "25")) {
			t.Errorf("a plan was offered before verification: %q", tn.Text)
		}
	}
}

// TestStudio_Situations_ResolveStops: once a situation is resolved, the agent stops doing what it asked for.
// hardship asks the agent to end every reply with "stay strong"; its "When it's resolved" says to stop.
//
// Steps:
//   - call: the job-loss line, a question, the new-job line (the agent resolves hardship), two more questions
//   - record: hardship started and resolved by the agent; "stay strong" is said while it is active and not after
func TestStudio_Situations_ResolveStops(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: prompt(t, ctx, deskPrompt), Hangup: true,
		Voice: borrowedVoice(t, ctx), Situations: []studio.Situation{
			{Name: "hardship", Who: "model", When: "the caller says they can't pay because they lost their job or are ill.",
				Instructions: "Be warm. While this situation is active, end every reply with the words: stay strong. " +
					"It is resolved when the caller says they found a new job: then call resolve_situation with hardship.",
				OnResolve: "Stop ending replies with stay strong."},
		}})
	mustClean(t, issues)
	yes := true
	r := playWith(t, ctx, studio.Scenario{
		Name: "situations-resolve-stops", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month, so I can't pay my bill right now."},
			{Say: "What are your branch opening hours?"},
			{Say: "Good news, I just found a new job this morning."},
			{Say: "How do I change my address?"},
			{Say: "Can I order new checks?"},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 240, TailSec: 20,
		Expect: &studio.Expect{AgentHangup: &yes,
			Events: []string{"Started hardship by the agent", "Resolved hardship by the agent"}},
	}, nil)
	situationEvents(t, ctx, r.CallSID)
	turns, err := ws.CallTurns(ctx, r.CallSID)
	if err != nil {
		t.Fatalf("turns: %v", err)
	}
	phase, before, after := "start", 0, []string{}
	for _, tn := range turns {
		low := strings.ToLower(tn.Text)
		switch {
		case tn.Role == "caller" && strings.Contains(low, "lost my job"):
			phase = "active"
		case tn.Role == "caller" && strings.Contains(low, "address"):
			phase = "resolved"
		case tn.Role == "caller" && strings.Contains(low, "goodbye"):
			phase = "end"
		case tn.Role == "agent" && phase == "active" && strings.Contains(low, "stay strong"):
			before++
		case tn.Role == "agent" && phase == "resolved":
			after = append(after, tn.Text)
		}
	}
	if before == 0 {
		t.Errorf("the agent never said \"stay strong\" while hardship was active, so the check proves nothing")
	}
	if len(after) == 0 {
		t.Errorf("no agent reply after the address question")
	}
	for _, a := range after {
		if strings.Contains(strings.ToLower(a), "stay strong") {
			t.Errorf("the agent still follows hardship after it was resolved: %q", a)
		}
	}
	t.Logf("\"stay strong\" while active: %d replies; replies after resolve: %q", before, after)
}
