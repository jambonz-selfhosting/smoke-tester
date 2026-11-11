//go:build studio

package studiotests

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.ngrok.com/ngrok"
	"golang.ngrok.com/ngrok/config"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

// Situations (agent-studio): instructions and tools that change during a call. Rules (the cap, gating, ordering,
// persistence) are unit-tested in agent-studio; these tests check what only a real call shows: Studio's compile,
// the gateway, feature-server and the model together, through what the caller hears and the call's record.

// --- customer server: one ngrok tunnel, a namespace per test so parallel tests keep separate logs ---

type customerServer struct {
	URL string
	mu  sync.Mutex
	log map[string][]string
}

var (
	srvOnce sync.Once
	srv     *customerServer
	srvErr  error
)

func (s *customerServer) calls(ns string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log[ns])
}

func customer(t *testing.T) *customerServer {
	t.Helper()
	if cfg.NgrokAuthToken == "" {
		t.Skip("NGROK_AUTHTOKEN not set: the customer server needs a public URL")
	}
	srvOnce.Do(func() { srv, srvErr = openCustomerServer() })
	if srvErr != nil {
		recordFailure(t, "customer-server", srvErr.Error())
		t.Fatalf("customer server: %v", srvErr)
	}
	return srv
}

func openCustomerServer() (*customerServer, error) {
	type res struct {
		tun ngrok.Tunnel
		err error
	}
	ch := make(chan res, 1)
	// The session lasts as long as the test binary; only the wait for it is bounded.
	go func() {
		tun, err := ngrok.Listen(context.Background(), config.HTTPEndpoint(), ngrok.WithAuthtoken(cfg.NgrokAuthToken))
		ch <- res{tun, err}
	}()
	var r res
	select {
	case r = <-ch:
	case <-time.After(30 * time.Second):
		return nil, errors.New("ngrok tunnel not up after 30s (another session for this token still open?)")
	}
	if r.err != nil {
		return nil, r.err
	}
	s := &customerServer{URL: r.tun.URL(), log: map[string][]string{}}
	digits := regexp.MustCompile(`\D`)
	h := func(w http.ResponseWriter, req *http.Request) {
		parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
		if len(parts) != 2 {
			http.NotFound(w, req)
			return
		}
		ns, tool := parts[0], parts[1]
		q := req.URL.Query()
		var body map[string]any
		entry := tool
		switch tool {
		case "verify_identity":
			ok := digits.ReplaceAllString(q.Get("last4"), "") == "4321" && digits.ReplaceAllString(q.Get("zip"), "") == "02139"
			entry += map[bool]string{true: ":match", false: ":no-match"}[ok]
			body = map[string]any{"verified": false}
			if ok {
				body = map[string]any{"verified": true, "name": "Jordan Lee", "balance": 1240.5,
					"actions": map[string]any{"situation": "negotiation"}}
			}
		case "record_agreement":
			body = map[string]any{"recorded": true, "reference": "4471",
				"actions": map[string]any{"resolve_situation": "negotiation"}}
		default:
			http.NotFound(w, req)
			return
		}
		s.mu.Lock()
		s.log[ns] = append(s.log[ns], entry)
		s.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	}
	go func() { _ = (&http.Server{Handler: http.HandlerFunc(h)}).Serve(r.tun) }()
	return s, nil
}

// --- Studio resources, each named it-<runID>- and removed when the test ends ---

func itName(t *testing.T, what string) string {
	return "it-" + provision.RunID() + "-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + what
}

func cleanupLog(t *testing.T, what string, fn func(context.Context) error) {
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := fn(ctx); err != nil {
			t.Logf("cleanup %s: %v", what, err)
		}
	})
}

func newPrompt(t *testing.T, ctx context.Context, text string) string {
	t.Helper()
	id, err := ws.CreatePrompt(ctx, itName(t, "prompt"), text)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	cleanupLog(t, "prompt "+id, func(c context.Context) error { return ws.DeletePrompt(c, id) })
	return id
}

// newTool makes a GET tool on the customer server under the test's namespace; Studio tool names are snake_case.
func newTool(t *testing.T, ctx context.Context, s *customerServer, ns, key, desc string, params map[string]string) (name, id string) {
	t.Helper()
	name = key + "_" + provision.RunID() + "_" + ns
	id, err := ws.CreateTool(ctx, studio.HTTPGetTool(name, desc, s.URL+"/"+ns+"/"+key, params))
	if err != nil {
		t.Fatalf("tool %s: %v", name, err)
	}
	cleanupLog(t, "tool "+name, func(c context.Context) error { return ws.DeleteTool(c, id) })
	return name, id
}

func newSitAgent(t *testing.T, ctx context.Context, p studio.Pipeline) (string, []studio.Issue) {
	t.Helper()
	name := itName(t, "agent")
	id, issues, err := ws.CreatePipelineAgent(ctx, name, p)
	if id != "" {
		cleanupLog(t, "agent "+id, func(c context.Context) error { return ws.DeleteAgent(c, id, name) })
	}
	if err != nil {
		t.Fatalf("pipeline agent: %v", err)
	}
	return id, issues
}

// voice is the stt, tts and llm of an English pipeline agent in the workspace: a new agent has no credentials.
func voice(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	v, from, err := ws.PipelineVoice(ctx, os.Getenv("STUDIO_VOICE_AGENT_ID"))
	if err != nil {
		t.Fatalf("pipeline voice: %v", err)
	}
	if v == nil {
		t.Skip("no English pipeline agent with stt, tts and llm credentials to borrow them from")
	}
	t.Logf("speech and model settings from agent %q", from)
	return v
}

func hasIssue(issues []studio.Issue, code, field string) bool {
	return slices.ContainsFunc(issues, func(i studio.Issue) bool {
		return i.Code == code && i.Severity == "error" && strings.HasPrefix(i.Field, field)
	})
}

// call plays the scenario; Studio's record must meet sc.Expect.
func call(t *testing.T, ctx context.Context, sc studio.Scenario) *studio.Result {
	t.Helper()
	s := Step(t, "call")
	r, err := studio.Run(ctx, ws, sc, studio.RunOptions{OutDir: outDir, SBC: sbc, Logf: t.Logf})
	if err != nil {
		s.Fatalf("%v (artifacts in %s)", err, r.Dir)
	}
	t.Logf("call_sid=%s, artifacts in %s\n%s", r.CallSID, r.Dir, r.Transcript)
	if len(r.Failures) > 0 {
		s.Errorf("Studio's record of call %s misses:\n  %s", r.CallSID, strings.Join(r.Failures, "\n  "))
	}
	s.Done()
	return r
}

// agentAfter returns the agent's replies after the first caller turn containing `from`, up to one containing `to`.
func agentAfter(turns []studio.CallTurn, from, to string) []string {
	var out []string
	in := false
	for _, tn := range turns {
		low := strings.ToLower(tn.Text)
		switch {
		case tn.Role == "caller" && !in && strings.Contains(low, from):
			in = true
		case tn.Role == "caller" && in && to != "" && strings.Contains(low, to):
			return out
		case tn.Role == "agent" && in:
			out = append(out, tn.Text)
		}
	}
	return out
}

func anyContains(texts []string, word string) bool {
	return slices.ContainsFunc(texts, func(x string) bool { return strings.Contains(strings.ToLower(x), word) })
}

func updatesApplied(t *testing.T, ctx context.Context, callSID string) {
	s := Step(t, "updates-applied")
	evs, err := ws.CallEvents(ctx, callSID)
	if err != nil {
		s.Fatalf("call events: %v", err)
	}
	for _, e := range evs {
		if strings.HasPrefix(e.Text, "Agent update rejected") {
			s.Errorf("jambonz rejected an update: %s %s", e.Text, e.Sub)
		}
		if strings.HasPrefix(e.Text, "Agent update not acknowledged") {
			s.Errorf("jambonz never acked an update (feature-server before 11.1.13, or a lost message): %s %s", e.Text, e.Sub)
		}
	}
	s.Done()
}

func turnsOf(t *testing.T, ctx context.Context, callSID string) []studio.CallTurn {
	s := Step(t, "read-turns")
	turns, err := ws.CallTurns(ctx, callSID)
	if err != nil {
		s.Fatalf("turns: %v", err)
	}
	if !slices.ContainsFunc(turns, func(x studio.CallTurn) bool { return x.Role == "agent" && x.Text != "" }) {
		s.Fatalf("the call record has no agent text (transcripts off?), so nothing can be checked")
	}
	s.Done()
	return turns
}

const deskPrompt = `You are the customer care line for Acme Bank, on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help. Start a situation as soon as it applies and follow its instructions.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`

// marker is said at the end of every reply while hardship is active; its absence or presence is what the caller hears.
const marker = "stay strong"

var hardship = studio.Situation{Name: "hardship", Who: "model",
	When: "the caller says they can't pay because they lost their job or are ill.",
	Instructions: "Be warm. While this situation is active, end every reply with the words: " + marker + ". " +
		"It is resolved when the caller says they found a new job: then call resolve_situation with hardship.",
	OnResolve: "Stop ending replies with " + marker + "."}

// TestStudio_Situations_Config: what Studio compiles a situations agent into, and what it refuses. No call.
//
// Steps:
//   - setup-agent: a server-only negotiation that adds a tool, a model hardship that requires it, a model dispute
//   - check-verb: the model is told only about its own situations; the gated one and the server-only tool are held back
//   - check-issues: an unknown tool, a hidden built-in and an unknown requirement block publishing
//   - check-limits: 11 situations, or two with one name, are refused
//   - check-engine: a realtime voice API agent can't have situations
func TestStudio_Situations_Config(t *testing.T) {
	ctx := WithTimeout(t, time.Minute)
	t.Parallel()

	s := Step(t, "setup-agent")
	record := "record_agreement_" + provision.RunID()
	recordID, err := ws.CreateTool(ctx, studio.HTTPGetTool(record, "Record an agreement.", "https://example.com/r", nil))
	if err != nil {
		s.Fatalf("tool: %v", err)
	}
	cleanupLog(t, "tool", func(c context.Context) error { return ws.DeleteTool(c, recordID) })
	sits := []studio.Situation{
		{Name: "negotiation", Who: "code", Instructions: "Offer to settle in full.", Add: []string{record}},
		{Name: "hardship", Who: "model", When: "the caller lost their job.", Instructions: "Be warm.", Requires: "negotiation"},
		{Name: "payment_dispute", Who: "model", When: "the caller says they already paid.", Instructions: "Don't argue."},
	}
	id, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, deskPrompt), ToolIDs: []string{recordID}, Situations: sits})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return strings.HasPrefix(i.Code, "SITUATION_") }) {
		s.Errorf("a valid draft has situation issues: %+v", issues)
	}
	s.Done()

	s = Step(t, "check-verb")
	ack, err := ws.Verb(ctx, id)
	if err != nil {
		s.Fatalf("verb: %v", err)
	}
	var o struct {
		SystemPrompt string `json:"systemPrompt"`
		Tools        []struct {
			Name       string `json:"name"`
			Parameters struct {
				Properties struct {
					Name struct {
						Enum []string `json:"enum"`
					} `json:"name"`
				} `json:"properties"`
			} `json:"parameters"`
		} `json:"tools"`
	}
	for _, v := range ack {
		if v["verb"] == "agent" {
			b, _ := json.Marshal(v["llm"].(map[string]any)["llmOptions"])
			_ = json.Unmarshal(b, &o)
		}
	}
	if !strings.Contains(o.SystemPrompt, "- hardship: the caller lost their job. Only once negotiation has started.") {
		s.Errorf("the index doesn't say hardship waits for negotiation:\n%s", o.SystemPrompt)
	}
	if strings.Contains(o.SystemPrompt, "settle in full") || strings.Contains(o.SystemPrompt, "- negotiation") {
		s.Errorf("the prompt shows the server-only situation before it starts:\n%s", o.SystemPrompt)
	}
	enums := map[string][]string{}
	for _, tl := range o.Tools {
		enums[tl.Name] = tl.Parameters.Properties.Name.Enum
	}
	if !slices.Equal(enums["enter_situation"], []string{"payment_dispute"}) {
		s.Errorf("enter_situation offers %v at the start, want [payment_dispute]", enums["enter_situation"])
	}
	if _, ok := enums[record]; ok {
		s.Errorf("%s is offered before negotiation starts", record)
	}
	s.Done()

	s = Step(t, "check-issues")
	bad, err := ws.EditDraft(ctx, id, func(cfg map[string]any) {
		studio.SetSituations(cfg, []studio.Situation{
			{Name: "a", Who: "code", Instructions: "x", Add: []string{"no_such_tool"}},
			{Name: "b", Who: "code", Instructions: "x", Hide: []string{"hangup"}},
			{Name: "c", Who: "model", When: "x", Instructions: "x", Requires: "no_such_situation"},
		})
	})
	if err != nil {
		s.Fatalf("save: %v", err)
	}
	for _, w := range []struct{ code, field string }{
		{"SITUATION_TOOL_UNKNOWN", "situations[0].tools.add"},
		{"SITUATION_TOOL_UNKNOWN", "situations[1].tools.hide"},
		{"SITUATION_REQUIRES_UNKNOWN", "situations[2].requires"},
	} {
		if !hasIssue(bad, w.code, w.field) {
			s.Errorf("no %s error at %s; got %+v", w.code, w.field, bad)
		}
	}
	s.Done()

	s = Step(t, "check-limits")
	var many []studio.Situation
	for i := range 11 {
		many = append(many, studio.Situation{Name: fmt.Sprintf("case_%d", i), Who: "code", Instructions: "x"})
	}
	for _, list := range [][]studio.Situation{many, {sits[2], sits[2]}} {
		_, err := ws.EditDraft(ctx, id, func(cfg map[string]any) { studio.SetSituations(cfg, list) })
		var ae *studio.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			s.Errorf("%d situations (first %s): got %v, want 400", len(list), list[0].Name, err)
		}
	}
	s.Done()

	s = Step(t, "check-engine")
	name := itName(t, "s2s")
	sid, got, err := ws.CreateS2sAgentWithSituations(ctx, name, "openai_s2s", sits[2:])
	if sid != "" {
		cleanupLog(t, "s2s agent", func(c context.Context) error { return ws.DeleteAgent(c, sid, name) })
	}
	if err != nil {
		s.Fatalf("s2s agent: %v", err)
	}
	if !hasIssue(got, "SITUATION_ENGINE_UNSUPPORTED", "") {
		s.Errorf("no SITUATION_ENGINE_UNSUPPORTED error; got %+v", got)
	}
	s.Done()
}

// TestStudio_Situations_ServerCall: the customer's server starts and ends a situation, and a model situation that
// requires it stays shut until then and ends with it.
//
// Steps:
//   - call: the caller asks for a hardship plan before verifying, verifies, repeats it, accepts a plan, says goodbye
//   - read-turns: the call's turns as Studio recorded them
//   - check-gate: no plan before verification; hardship starts only after negotiation
//   - check-server: record_agreement reached the customer only after a matching verify_identity
//   - updates-applied: jambonz acked and applied every update
func TestStudio_Situations_ServerCall(t *testing.T) {
	ctx := WithTimeout(t, 6*time.Minute)
	t.Parallel()
	cs := customer(t)
	ns := "server"
	verify, verifyID := newTool(t, ctx, cs, ns, "verify_identity", "Verify the caller. Returns verified, and the balance when verified.",
		map[string]string{"last4": "Last four digits of the account number, digits only.", "zip": "ZIP code, digits only."})
	record, recordID := newTool(t, ctx, cs, ns, "record_agreement", "Record the plan the caller accepted. Returns its reference.",
		map[string]string{"monthly_amount": "Dollars per month, digits only."})
	prompt := fmt.Sprintf(`You are Alex, a collections agent for Acme Bank, on the phone. Keep every reply to one or two short sentences.
Never discuss the balance, a payment or a plan until the caller is verified: ask for the last four digits of their account number and their ZIP code, then call %s.
Start a situation as soon as it applies and follow its instructions.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`, verify)
	id, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, prompt), Hangup: true, Voice: voice(t, ctx),
		ToolIDs: []string{verifyID, recordID},
		Situations: []studio.Situation{
			{Name: "negotiation", Who: "code", Add: []string{record},
				Instructions: fmt.Sprintf("The caller is verified. Tell them the balance. When they accept a plan, call %s, then read its reference back digit by digit.", record)},
			{Name: "hardship", Who: "model", Requires: "negotiation",
				When:         "the caller says they can't pay because they lost their job.",
				Instructions: "Offer the hardship plan: twenty five dollars a month."},
		}})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		t.Fatalf("draft errors: %+v", issues)
	}

	r := call(t, ctx, studio.Scenario{
		Name: "situations-server", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, I lost my job last month. What payment plans can you offer me?"},
			{Say: "The last four digits are 4 3 2 1 and my ZIP code is 0 2 1 3 9."},
			{Say: "Like I said, I lost my job, so the full amount is too much for me."},
			{Say: "Okay, I'll take the twenty five dollars a month plan."},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 200, TailSec: 15,
		Expect: &studio.Expect{
			ToolsOK: []string{verify, record},
			Events: []string{
				"Started negotiation by tool " + verify,
				"Started hardship by the agent",
				"Resolved negotiation by tool " + record,
				"Resolved hardship by tool " + record + " · negotiation ended",
			},
			Heard: []string{`(?i)4[\s,.-]*4[\s,.-]*7[\s,.-]*1|four[\s,.-]+four[\s,.-]+seven[\s,.-]+one`},
		},
	})
	turns := turnsOf(t, ctx, r.CallSID)

	s := Step(t, "check-gate")
	if before := agentAfter(turns, "lost my job", "zip"); anyContains(before, "twenty five") || anyContains(before, "25") {
		s.Errorf("a plan was offered before verification: %q", before)
	}
	evs, err := ws.CallEvents(ctx, r.CallSID)
	if err != nil {
		s.Fatalf("events: %v", err)
	}
	var order []string
	for _, e := range evs {
		if e.Situation != nil && e.Situation.Change == "entered" {
			order = append(order, e.Situation.Name)
		}
	}
	if !slices.Equal(order, []string{"negotiation", "hardship"}) {
		s.Errorf("situations started in the order %v, want [negotiation hardship]", order)
	}
	s.Done()

	s = Step(t, "check-server")
	got := cs.calls(ns)
	if m, a := slices.Index(got, "verify_identity:match"), slices.Index(got, "record_agreement"); m < 0 || a < m {
		s.Errorf("record_agreement must follow a matching verify_identity: %v", got)
	}
	s.Done()
	updatesApplied(t, ctx, r.CallSID)
}

// TestStudio_Situations_ModelCall: the model starts situations up to the cap, and stops following one it resolved.
//
// Steps:
//   - call: four lines that each start a situation, then the new-job line, then a question
//   - read-turns: the call's turns as Studio recorded them
//   - check-resolved: the hardship marker is said while it is active and never after it is resolved
//   - updates-applied: jambonz acked and applied every update
func TestStudio_Situations_ModelCall(t *testing.T) {
	ctx := WithTimeout(t, 6*time.Minute)
	t.Parallel()
	id, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, deskPrompt), Hangup: true, Voice: voice(t, ctx),
		Situations: []studio.Situation{hardship,
			{Name: "payment_dispute", Who: "model", When: "the caller says they already paid.", Instructions: "Say a specialist will review it."},
			{Name: "callback_request", Who: "model", When: "the caller asks to be called back.", Instructions: "Say we will call tomorrow."},
			{Name: "legal_question", Who: "model", When: "the caller mentions a lawyer.", Instructions: "Give no legal advice."},
		}})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		t.Fatalf("draft errors: %+v", issues)
	}
	r := call(t, ctx, studio.Scenario{
		Name: "situations-model", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month, so I can't pay right now."},
			{Say: "Also, I already paid two hundred dollars last week and it isn't showing."},
			{Say: "Can you call me back tomorrow about all this?"},
			{Say: "And I'm going to talk to my lawyer about these charges."},
			{Say: "Good news, I just found a new job this morning."},
			{Say: "How do I change my address?"},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 220, TailSec: 15,
		Expect: &studio.Expect{Events: []string{
			"Refused to start legal_question by the agent · 3 situations are already active",
			"Resolved hardship by the agent",
		}},
	})
	turns := turnsOf(t, ctx, r.CallSID)

	s := Step(t, "check-resolved")
	while := agentAfter(turns, "lost my job", "new job")
	after := agentAfter(turns, "address", "goodbye")
	if !anyContains(while, marker) {
		s.Errorf("%q never said while hardship was active, so the check proves nothing: %q", marker, while)
	}
	if len(after) == 0 || anyContains(after, marker) {
		s.Errorf("after hardship was resolved the agent should reply without %q: %q", marker, after)
	}
	s.Done()
	updatesApplied(t, ctx, r.CallSID)
}

// TestStudio_Situations_DetourCall: a situation active before a secure detour is still followed after the caller
// comes back, when the agent restarts with a fresh verb.
//
// Steps:
//   - call: the job-loss line, a card payment (the detour fails and comes back), a question, goodbye
//   - read-turns: the call's turns as Studio recorded them
//   - check-after-detour: the reply after the detour still carries the hardship marker
//   - updates-applied: jambonz acked and applied every update
func TestStudio_Situations_DetourCall(t *testing.T) {
	ctx := WithTimeout(t, 6*time.Minute)
	t.Parallel()
	prompt := deskPrompt + "\nWhen the caller wants to pay by card, use the secure detour tool. If they come back, say the card line isn't available and carry on."
	id, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, prompt), Hangup: true, Voice: voice(t, ctx),
		Situations: []studio.Situation{hardship},
		Edit: func(c map[string]any) {
			studio.Sub(c, "transfer")["detour"] = map[string]any{"enabled": true,
				"destination": map[string]any{"type": "phone", "number": studio.BillingNumber},
				"say":         "Taking you to our secure card line.", "when": "the caller wants to pay by card",
				"on_failure": "return", "give_up_min": 1}
		}})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		t.Fatalf("draft errors: %+v", issues)
	}
	r := call(t, ctx, studio.Scenario{
		Name: "situations-detour", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month, so I can't pay the full amount."},
			{Say: "I'd like to pay part of it by card now, please."},
			// The detour fails anywhere from at once to about 35 s; skip its announcement, then wait for the agent to
			// speak again, which it does once back (at the latest its no-response prompt).
			{Say: "What are your branch opening hours?", PauseSec: 6, WaitSec: 60},
			{Say: "Thanks, that's all. Goodbye."},
		},
		MaxSec: 200, TailSec: 15,
		Expect: &studio.Expect{Events: []string{"Started hardship by the agent", "(?i)back (from the detour|with the agent)"}},
	})
	turns := turnsOf(t, ctx, r.CallSID)

	s := Step(t, "check-after-detour")
	if after := agentAfter(turns, "opening hours", "goodbye"); len(after) == 0 || !anyContains(after, marker) {
		s.Errorf("after the detour the agent no longer follows hardship (no %q): %q", marker, after)
	}
	s.Done()
	updatesApplied(t, ctx, r.CallSID)
}
