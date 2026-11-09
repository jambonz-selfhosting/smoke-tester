//go:build studio

package studiotests

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
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

// Situations (agent-studio feat/situations, PR #38): instructions and tools that change during a call. These tests
// cover the compile (what the agent verb starts with and what the builder refuses), a situation only the
// customer's server starts and resolves, and situations the model starts and resolves within the cap of 3.

const collectionsPrompt = `You are Alex, a collections agent for Acme Bank, speaking on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help.
Never discuss the balance, a payment or a plan until the caller is verified: ask for the last four digits of their account number and their ZIP code, then call verify_identity with both.
If verification fails, say so and ask them to try again.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`

const negotiationInstructions = `The caller is verified. Tell them their balance from the verification result and offer two options: pay it in full, or the minimum of fifty dollars a month.
When the caller agrees to an option, call the record agreement tool with the amount per month (the full balance if they pay in full).
After it returns, read the agreement reference back to the caller digit by digit before saying anything else.`

const deskPrompt = `You are the customer care line for Acme Bank, speaking on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help. Listen to what the caller says and follow your situations' instructions when they apply.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`

// customerServer is the customer's own server: verify_identity starts the server-only situation on a match, and
// record_agreement resolves it. It records each request in order.
type customerServer struct {
	URL string
	mu  sync.Mutex
	log []string
}

func (s *customerServer) note(e string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.log = append(s.log, e)
}

func (s *customerServer) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.log)
}

var (
	sharedSrv     *customerServer
	sharedSrvErr  error
	sharedSrvOnce sync.Once
)

// startCustomerServer returns the package's one customer server, its call log emptied. One ngrok session serves every
// test: the account limits sessions, and a second one waits for the first to go. Tests using it don't run in parallel.
func startCustomerServer(t *testing.T) *customerServer {
	t.Helper()
	if cfg.NgrokAuthToken == "" {
		t.Skip("NGROK_AUTHTOKEN not set: the customer server needs a public URL")
	}
	sharedSrvOnce.Do(func() { sharedSrv, sharedSrvErr = openCustomerServer() })
	if sharedSrvErr != nil {
		t.Fatalf("customer server: %v", sharedSrvErr)
	}
	sharedSrv.mu.Lock()
	sharedSrv.log = nil
	sharedSrv.mu.Unlock()
	t.Logf("customer server at %s", sharedSrv.URL)
	return sharedSrv
}

func openCustomerServer() (*customerServer, error) {
	type opened struct {
		tun ngrok.Tunnel
		err error
	}
	ch := make(chan opened, 1)
	// The session lives as long as the test binary, so its context is never cancelled; the wait is bounded here.
	go func() {
		tun, err := ngrok.Listen(context.Background(), config.HTTPEndpoint(), ngrok.WithAuthtoken(cfg.NgrokAuthToken))
		ch <- opened{tun, err}
	}()
	var o opened
	select {
	case o = <-ch:
	case <-time.After(30 * time.Second):
		return nil, errors.New("ngrok tunnel not up after 30s (another ngrok session for this token still open?)")
	}
	if o.err != nil {
		return nil, o.err
	}
	tun := o.tun
	s := &customerServer{URL: tun.URL()}
	reply := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/verify_identity", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		digits := func(v string) string {
			return strings.Map(func(r rune) rune {
				if r >= '0' && r <= '9' {
					return r
				}
				return -1
			}, v)
		}
		ok := digits(q.Get("last4")) == "4321" && digits(q.Get("zip")) == "02139"
		s.note("verify_identity:" + map[bool]string{true: "match", false: "no-match"}[ok])
		if !ok {
			reply(w, map[string]any{"verified": false})
			return
		}
		reply(w, map[string]any{"verified": true, "name": "Jordan Lee", "balance": 1240.5, "minimum_monthly": 50,
			"actions": map[string]any{"situation": "negotiation"}})
	})
	mux.HandleFunc("/record_agreement", func(w http.ResponseWriter, r *http.Request) {
		s.note("record_agreement")
		reply(w, map[string]any{"recorded": true, "reference": "4471",
			"actions": map[string]any{"resolve_situation": "negotiation"}})
	})
	fixed := map[string]map[string]any{
		"check_balance":   {"balance": 1240.5, "currency": "USD"},
		"file_complaint":  {"reference": "C512", "actions": map[string]any{"situation": "escalation"}},
		"add_note":        {"added": true, "actions": map[string]any{"situation": "escalation"}},
		"close_complaint": {"closed": true, "actions": map[string]any{"resolve_situation": "escalation", "situation": "followup"}},
	}
	for path, body := range fixed {
		mux.HandleFunc("/"+path, func(w http.ResponseWriter, r *http.Request) {
			s.note(path)
			reply(w, body)
		})
	}
	mux.HandleFunc("/mcp", s.mcp)
	go func() { _ = (&http.Server{Handler: mux}).Serve(tun) }()
	return s, nil
}

type agentParts struct {
	promptID string
	toolIDs  map[string]string // tool name prefix -> id
	names    map[string]string // tool name prefix -> run-unique name
}

// setupParts creates the prompt and HTTP tools an agent uses, each removed when the test ends.
func setupParts(t *testing.T, ctx context.Context, prompt string, srv *customerServer) agentParts {
	t.Helper()
	p := agentParts{toolIDs: map[string]string{}, names: map[string]string{}}
	id, err := ws.CreatePrompt(ctx, "smoke "+provision.RunID()+": "+t.Name(), prompt)
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	t.Cleanup(func() { _ = ws.DeletePrompt(context.Background(), id) })
	p.promptID = id
	base := "https://example.com"
	if srv != nil {
		base = srv.URL
	}
	tools := []struct {
		key, desc string
		params    map[string]string
	}{
		{"verify_identity", "Verify the caller. Returns verified true or false, and their balance when verified.",
			map[string]string{"last4": "The last four digits of the account number, digits only.", "zip": "The ZIP code, digits only."}},
		{"record_agreement", "Record the payment agreement the caller accepted. Returns the agreement reference.",
			map[string]string{"monthly_amount": "The amount per month in dollars, digits only."}},
	}
	for _, tl := range tools {
		name := tl.key + "_" + provision.RunID()
		tid, err := ws.CreateTool(ctx, studio.HTTPGetTool(name, tl.desc, base+"/"+tl.key, tl.params))
		if err != nil {
			t.Fatalf("tool %s: %v", name, err)
		}
		t.Cleanup(func() { _ = ws.DeleteTool(context.Background(), tid) })
		p.toolIDs[tl.key], p.names[tl.key] = tid, name
	}
	return p
}

// borrowedVoice is the stt, tts and llm of a working pipeline agent in the workspace; the test skips without one.
func borrowedVoice(t *testing.T, ctx context.Context) map[string]any {
	t.Helper()
	v, from, err := ws.PipelineVoice(ctx, os.Getenv("STUDIO_VOICE_AGENT_ID"))
	if err != nil {
		t.Fatalf("pipeline voice: %v", err)
	}
	if v == nil {
		t.Skip("no pipeline agent with stt, tts and llm credentials in the workspace to borrow them from")
	}
	t.Logf("speech and model settings from agent %q", from)
	return v
}

func newPipelineAgent(t *testing.T, ctx context.Context, p studio.Pipeline) (string, []studio.Issue) {
	t.Helper()
	name := "smoke " + provision.RunID() + " " + t.Name()
	id, issues, err := ws.CreatePipelineAgent(ctx, name, p)
	if id != "" {
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := ws.DeleteAgent(cctx, id, name); err != nil {
				t.Logf("delete agent %s: %v", id, err)
			}
		})
	}
	if err != nil {
		t.Fatalf("pipeline agent: %v", err)
	}
	return id, issues
}

func errorsOf(issues []studio.Issue, prefix string) []studio.Issue {
	var out []studio.Issue
	for _, i := range issues {
		if strings.HasPrefix(i.Code, prefix) {
			out = append(out, i)
		}
	}
	return out
}

func hasIssue(issues []studio.Issue, code, severity, pathPrefix string) bool {
	for _, i := range issues {
		if i.Code == code && i.Severity == severity && strings.HasPrefix(i.Field, pathPrefix) {
			return true
		}
	}
	return false
}

// TestStudio_Situations_Compile: what a pipeline agent with situations starts its calls with, and what the
// builder flags.
//
// Steps:
//   - setup: a prompt, two HTTP tools, an agent with a model situation and a server-only one
//   - clean-draft: no SITUATION_* issue
//   - verb-prompt: the system prompt lists the model situation in its index, never the server-only one
//   - verb-tools: enter_situation / resolve_situation offer only the model situation; a tool only the
//     server-only situation adds is withheld until it starts
//   - invalid-drafts: an unknown tool, a hidden built-in, no "when" line and an empty situation are flagged
//   - duplicate-names: the draft is refused
//   - s2s-unsupported: a realtime voice API agent with situations gets SITUATION_ENGINE_UNSUPPORTED
func TestStudio_Situations_Compile(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	parts := setupParts(t, ctx, collectionsPrompt, nil)
	verify, record := parts.names["verify_identity"], parts.names["record_agreement"]
	good := []studio.Situation{
		{Name: "hardship", Who: "model", When: "the caller can't pay because of job loss or illness.",
			Instructions: "Be warm and unhurried."},
		{Name: "negotiation", Who: "code", Instructions: negotiationInstructions, Add: []string{record}},
	}
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: parts.promptID,
		ToolIDs: []string{parts.toolIDs["verify_identity"], parts.toolIDs["record_agreement"]}, Situations: good})

	t.Run("clean-draft", func(t *testing.T) {
		if bad := errorsOf(issues, "SITUATION_"); len(bad) > 0 {
			t.Errorf("a valid draft has situation issues: %+v", bad)
		}
	})

	ack, err := ws.Verb(ctx, id)
	if err != nil {
		t.Fatalf("verb: %v", err)
	}
	var opts struct {
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
			_ = json.Unmarshal(b, &opts)
		}
	}

	t.Run("verb-prompt", func(t *testing.T) {
		if !strings.Contains(opts.SystemPrompt, "## Situations") || !strings.Contains(opts.SystemPrompt, "- hardship:") {
			t.Errorf("system prompt has no situation index listing hardship:\n%s", opts.SystemPrompt)
		}
		if strings.Contains(opts.SystemPrompt, "negotiation") || strings.Contains(opts.SystemPrompt, "fifty dollars") {
			t.Errorf("system prompt mentions the server-only situation before it starts:\n%s", opts.SystemPrompt)
		}
	})

	t.Run("verb-tools", func(t *testing.T) {
		got := map[string][]string{}
		for _, tl := range opts.Tools {
			got[tl.Name] = tl.Parameters.Properties.Name.Enum
		}
		for _, n := range []string{"enter_situation", "resolve_situation"} {
			enum, ok := got[n]
			if !ok {
				t.Errorf("%s not offered", n)
			} else if !slices.Equal(enum, []string{"hardship"}) {
				t.Errorf("%s offers %v, want only [hardship]", n, enum)
			}
		}
		if _, ok := got[verify]; !ok {
			t.Errorf("base tool %s not offered", verify)
		}
		if _, ok := got[record]; ok {
			t.Errorf("%s is offered before negotiation starts", record)
		}
	})

	t.Run("invalid-drafts", func(t *testing.T) {
		bad, err := ws.EditDraft(ctx, id, func(cfg map[string]any) {
			studio.SetSituations(cfg, []studio.Situation{
				{Name: "unknown_tool", Who: "code", Instructions: "x", Add: []string{"no_such_tool"}},
				{Name: "hides_hangup", Who: "code", Instructions: "x", Hide: []string{"hangup"}},
				{Name: "no_when", Who: "model", Instructions: "x"},
				{Name: "empty", Who: "code"},
				{Name: "bad_requires", Who: "model", When: "x", Instructions: "x", Requires: "no_such_situation"},
			})
		})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		for _, w := range []struct{ code, sev, path string }{
			{"SITUATION_TOOL_UNKNOWN", "error", "situations[0].tools.add"},
			{"SITUATION_TOOL_UNKNOWN", "error", "situations[1].tools.hide"},
			{"SITUATION_WHEN_MISSING", "warning", "situations[2]"},
			{"SITUATION_INSTRUCTIONS_EMPTY", "warning", "situations[3]"},
			{"SITUATION_REQUIRES_UNKNOWN", "error", "situations[4].requires"},
		} {
			if !hasIssue(bad, w.code, w.sev, w.path) {
				t.Errorf("no %s %s at %s; got %+v", w.sev, w.code, w.path, errorsOf(bad, "SITUATION_"))
			}
		}
	})

	t.Run("requires", func(t *testing.T) {
		gated := []studio.Situation{good[1],
			{Name: "hardship", Who: "model", When: "the caller can't pay because of job loss or illness.",
				Instructions: "Be warm and unhurried.", Requires: "negotiation"},
			{Name: "payment_dispute", Who: "model", When: "the caller says they already paid.", Instructions: "Don't argue."}}
		if _, err := ws.EditDraft(ctx, id, func(cfg map[string]any) { studio.SetSituations(cfg, gated) }); err != nil {
			t.Fatalf("save: %v", err)
		}
		ack, err := ws.Verb(ctx, id)
		if err != nil {
			t.Fatalf("verb: %v", err)
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
		if !strings.Contains(o.SystemPrompt, "- hardship: the caller can't pay because of job loss or illness. Only once negotiation has started.") {
			t.Errorf("index does not say hardship waits for negotiation:\n%s", o.SystemPrompt)
		}
		for _, tl := range o.Tools {
			if tl.Name == "enter_situation" && !slices.Equal(tl.Parameters.Properties.Name.Enum, []string{"payment_dispute"}) {
				t.Errorf("enter_situation offers %v at the start, want only [payment_dispute]", tl.Parameters.Properties.Name.Enum)
			}
		}
	})

	t.Run("duplicate-names", func(t *testing.T) {
		_, err := ws.EditDraft(ctx, id, func(cfg map[string]any) {
			studio.SetSituations(cfg, []studio.Situation{good[0], good[0]})
		})
		var ae *studio.APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest {
			t.Errorf("two situations with one name: got %v, want 400", err)
		}
	})

	t.Run("s2s-unsupported", func(t *testing.T) {
		name := "smoke " + provision.RunID() + " s2s situations"
		sid, got, err := ws.CreateS2sAgentWithSituations(ctx, name, "openai_s2s", good[:1])
		if sid != "" {
			t.Cleanup(func() { _ = ws.DeleteAgent(context.Background(), sid, name) })
		}
		if err != nil {
			t.Fatalf("s2s agent: %v", err)
		}
		if !hasIssue(got, "SITUATION_ENGINE_UNSUPPORTED", "error", "") {
			t.Errorf("no SITUATION_ENGINE_UNSUPPORTED error; got %+v", errorsOf(got, "SITUATION_"))
		}
	})
}

func playScenario(t *testing.T, ctx context.Context, sc studio.Scenario) *studio.Result {
	t.Helper()
	r, err := studio.Run(ctx, ws, sc, studio.RunOptions{OutDir: outDir, SBC: sbc, Logf: t.Logf})
	if err != nil {
		t.Fatalf("%s: %v (artifacts in %s)", sc.Name, err, r.Dir)
	}
	t.Logf("call_sid=%s ended_by_agent=%v, artifacts in %s\n%s", r.CallSID, r.EndedByAgent, r.Dir, r.Transcript)
	if len(r.Failures) > 0 {
		t.Errorf("%s: Studio's record of call %s misses:\n  %s", sc.Name, r.CallSID, strings.Join(r.Failures, "\n  "))
	}
	return r
}

// TestStudio_Situations_Server: a situation only the customer's server starts and resolves, on a live call.
//
// Steps:
//   - setup: the customer server behind ngrok, its two tools, an agent whose negotiation situation is server-only
//     and adds record_agreement
//   - call: ask for the balance before verifying, fail verification, pass it, accept a plan, say goodbye
//   - record: Studio shows negotiation started by verify_identity and resolved by record_agreement, both tools ran,
//     and the caller heard the agreement reference read back (the deferred resolve)
//   - server: record_agreement was only called after a matching verify_identity
func TestStudio_Situations_Server(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Minute)
	defer cancel()
	srv := startCustomerServer(t)
	parts := setupParts(t, ctx, collectionsPrompt, srv)
	verify, record := parts.names["verify_identity"], parts.names["record_agreement"]
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: parts.promptID, Hangup: true,
		Voice:   borrowedVoice(t, ctx),
		ToolIDs: []string{parts.toolIDs["verify_identity"], parts.toolIDs["record_agreement"]},
		Situations: []studio.Situation{
			{Name: "negotiation", Who: "code", Instructions: negotiationInstructions, Add: []string{record}},
		}})
	if bad := errorsOf(issues, "SITUATION_"); len(bad) > 0 {
		t.Fatalf("draft issues: %+v", bad)
	}

	yes := true
	sc := studio.Scenario{
		Name: "situations-server", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, I'd like to know my balance and set up a payment plan."},
			{Say: "The last four digits are 9 9 9 9 and my ZIP code is 9 0 2 1 0."},
			{Say: "Sorry, my mistake. The last four digits are 4 3 2 1 and my ZIP code is 0 2 1 3 9."},
			{Say: "I'll take the fifty dollars a month option, please."},
			{Say: "Great, thank you. That's all, goodbye."},
		},
		MaxSec: 240, TailSec: 20,
		Expect: &studio.Expect{
			CallerTurns: 4,
			ToolsOK:     []string{verify, record},
			AgentHangup: &yes,
			Events: []string{
				"Started negotiation by tool " + verify,
				"Resolved negotiation by tool " + record,
			},
			Heard: []string{`(?i)4[\s,.-]*4[\s,.-]*7[\s,.-]*1|four[\s,.-]+four[\s,.-]+seven[\s,.-]+one`},
		},
	}
	playScenario(t, ctx, sc)

	got := srv.calls()
	t.Logf("customer server calls: %v", got)
	match := slices.Index(got, "verify_identity:match")
	agree := slices.Index(got, "record_agreement")
	switch {
	case match < 0:
		t.Errorf("no matching verify_identity reached the server")
	case agree < 0:
		t.Errorf("record_agreement never reached the server")
	case agree < match:
		t.Errorf("record_agreement ran before the caller was verified: %v", got)
	}
	if !slices.Contains(got, "verify_identity:no-match") {
		t.Logf("note: the wrong ZIP was not sent to verify_identity (the model may have refused it itself)")
	}
}

// TestStudio_Situations_Model: situations the model starts and resolves, the cap of 3, and room made by a resolve.
//
// Steps:
//   - setup: an agent with four model situations
//   - call: hardship, dispute and callback lines (3 active), a lawyer line (refused at the cap), a new-job line
//     (hardship resolved), the lawyer line again (now started), goodbye
//   - record: three starts, the refusal, the resolve and the later start are on Studio's timeline
func TestStudio_Situations_Model(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 7*time.Minute)
	defer cancel()
	parts := setupParts(t, ctx, deskPrompt, nil)
	id, issues := newPipelineAgent(t, ctx, studio.Pipeline{PromptID: parts.promptID, Hangup: true,
		Voice: borrowedVoice(t, ctx),
		Situations: []studio.Situation{
			{Name: "hardship", Who: "model", When: "the caller says they can't pay because they lost their job or are ill.",
				Instructions: "Be warm and unhurried. Tell the caller we can pause payments for a month. This situation is resolved when the caller says they have found a new job: then call resolve_situation with hardship."},
			{Name: "payment_dispute", Who: "model", When: "the caller says they already paid or disputes an amount.",
				Instructions: "Do not argue. Say a specialist will review the payment within two business days."},
			{Name: "callback_request", Who: "model", When: "the caller asks to be called back later.",
				Instructions: "Say we will call them back tomorrow morning."},
			{Name: "legal_question", Who: "model", When: "the caller mentions a lawyer, court or their legal rights.",
				Instructions: "Do not give legal advice. Say they are welcome to have their lawyer contact us in writing."},
		}})
	if bad := errorsOf(issues, "SITUATION_"); len(bad) > 0 {
		t.Fatalf("draft issues: %+v", bad)
	}

	yes := true
	sc := studio.Scenario{
		Name: "situations-model", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "I lost my job last month, so I can't pay my bill right now."},
			{Say: "Also, I already paid two hundred dollars last week and it isn't showing."},
			{Say: "Can you call me back tomorrow about all this?"},
			{Say: "And I'm going to talk to my lawyer about these charges."},
			{Say: "Actually, good news, I just found a new job this morning."},
			{Say: "So about my lawyer, I'm going to ask my lawyer about these charges."},
			{Say: "Okay, thank you. That's all, goodbye."},
		},
		MaxSec: 300, TailSec: 20,
		Expect: &studio.Expect{
			CallerTurns: 6,
			AgentHangup: &yes,
			Events: []string{
				"Started hardship by the agent",
				"Started payment_dispute by the agent",
				"Started callback_request by the agent",
				"Refused to start legal_question by the agent · 3 situations are already active",
				"Resolved hardship by the agent",
				"Started legal_question by the agent",
			},
		},
	}
	playScenario(t, ctx, sc)
}
