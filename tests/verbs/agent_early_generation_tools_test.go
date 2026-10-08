// Early generation for agents with tools (jambonz/feature-server#252): Flux
// EagerEndOfTurn starts the LLM before the turn is confirmed, and the reply may
// choose tools. A tool must never run on a guess, and never run twice.
//
// Mirrors the trial runbook: the agent verb, prompt, tools and WebSocket tool
// output of jambonz-test-agent /flux-tools (trial/flux-tools-early-generation),
// scenarios A-E, each run twice at eotThreshold 0.7 and once at 0.8.
package verbs

import (
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const earlyGenToolsPrompt = "You are Maya, calling from Acme Solar about the free solar quote the person requested online. " +
	"You are on a phone call: speak naturally, one or two short sentences at a time, no formatting. " +
	"Greet them, confirm you are speaking with the person who asked for a quote, and ask if they " +
	"have a minute to be connected with a specialist who can give them their quote. " +
	"If they agree: in ONE response say a short line like \"Great, one moment please\" and call BOTH " +
	"record_lead and transfer_to_specialist. " +
	"If they want a call later: ask when, then call schedule_callback and say goodbye. " +
	"If they are not interested: call mark_not_interested, thank them, and say goodbye."

var earlyGenTools = []map[string]any{
	{
		"name":        "record_lead",
		"description": "Record that the caller agreed to talk to a specialist.",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "transfer_to_specialist",
		"description": "Transfer the caller to a solar specialist.",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	},
	{
		"name":        "schedule_callback",
		"description": "Schedule a call back at the time the caller asked for.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"date": map[string]any{"type": "string", "description": "YYYY-MM-DD"},
				"time": map[string]any{"type": "string", "description": "HH:MM, 24-hour"},
			},
			"required": []string{"date", "time"},
		},
	},
	{
		"name":        "mark_not_interested",
		"description": "Record that the caller is not interested.",
		"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
	},
}

var earlyGenToolResults = map[string]any{
	"record_lead":            map[string]any{"ok": true},
	"transfer_to_specialist": map[string]any{"ok": true, "note": "simulated: no real transfer; tell the caller they are being connected"},
	"schedule_callback":      map[string]any{"ok": true},
	"mark_not_interested":    map[string]any{"ok": true},
}

// earlyGenAgentVerb is handleToolTrialSession's agent verb. Only the
// credential labels and the inline LLM are the harness's own.
func earlyGenAgentVerb(eotThreshold float64) map[string]any {
	// deepseek skipped the tool on ~1 call in 8, which reads as a dropped tool.
	vendor, model, key := "deepseek", "deepseek-chat", cfg.DeepseekAPIKey
	if cfg.HasOpenAI() {
		vendor, model, key = "openai", "gpt-4.1-mini", cfg.OpenAIAPIKey
	}
	return V("agent",
		"stt", map[string]any{
			"vendor": "deepgramflux",
			"label":  deepgramFluxLabel,
			"deepgramOptions": map[string]any{
				"eagerEotThreshold": earlyGenEagerEotThreshold,
				"eotThreshold":      eotThreshold,
			},
		},
		"tts", map[string]any{"vendor": "deepgram", "label": deepgramLabel, "voice": "aura-2-asteria-en"},
		"llm", map[string]any{
			"vendor": vendor,
			"model":  model,
			"auth":   map[string]any{"apiKey": key},
			"llmOptions": map[string]any{
				"messages": []map[string]any{{"role": "system", "content": earlyGenToolsPrompt}},
				"tools":    earlyGenTools,
			},
		},
		"turnDetection", "stt",
		"earlyGeneration", true,
		"bargeIn", map[string]any{"enable": true, "strategy": "vad"},
		"toolHook", "/tool-call",
		"eventHook", "/agent-event",
		"actionHook", "/agent-complete",
	)
}

// earlyGenUtterance is one caller turn; a non-zero Pause splits it into two
// halves with that much silence between them.
type earlyGenUtterance struct {
	First string
	Pause time.Duration
	Then  string
}

type earlyGenScenario struct {
	Turns    []earlyGenUtterance
	FollowUp string   // answers the agent's follow-up question, only if Expect has not all run yet
	Expect   []string // each must run exactly once; every other tool must not run
}

var (
	earlyGenAgreeTwoTurns = earlyGenScenario{
		Turns: []earlyGenUtterance{
			{First: "Yes, that's me."},
			{First: "Sure, go ahead."},
		},
		FollowUp: "Yes, please connect me.",
		Expect:   []string{"record_lead", "transfer_to_specialist"},
	}
	earlyGenAgreeAfterPause = earlyGenScenario{
		Turns: []earlyGenUtterance{
			{First: "Yes.", Pause: time.Second, Then: "I'd like to talk to someone."},
		},
		FollowUp: "Yes, please connect me.",
		Expect:   []string{"record_lead", "transfer_to_specialist"},
	}
	// The eager guess on "Yes" is a transfer; the confirmed turn is not.
	earlyGenYesThenCallback = earlyGenScenario{
		Turns: []earlyGenUtterance{
			{First: "Yes.", Pause: 500 * time.Millisecond, Then: "Actually no, call me tomorrow at three."},
		},
		FollowUp: earlyGenDateAnswer,
		Expect:   []string{"schedule_callback"},
	}
	earlyGenCallback = earlyGenScenario{
		Turns: []earlyGenUtterance{
			{First: "Not now, call me tomorrow at ten."},
		},
		FollowUp: earlyGenDateAnswer,
		Expect:   []string{"schedule_callback"},
	}
	earlyGenNotInterested = earlyGenScenario{
		Turns: []earlyGenUtterance{
			{First: "No thanks, not interested."},
		},
		FollowUp: "No, I'm not interested.",
		Expect:   []string{"mark_not_interested"},
	}
)

// earlyGenDateAnswer answers "what date?": schedule_callback requires one and
// the prompt gives the model no calendar.
var earlyGenDateAnswer = "Tomorrow is " + time.Now().AddDate(0, 0, 1).Format("Monday, January 2") + "."

const earlyGenEagerEotThreshold = 0.5

// One test per runbook call (A–E, twice at EOT 0.7, once at 0.8) so each can
// be run or repeated on its own.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs — pause turns are two TTS halves joined by silence
//  3. script-agent-verb — the /flux-tools verb, tool output over the app WS
//  4. place-ws-call
//  5. answer-record-and-silence
//  6. wait-for-greeting
//  7. turn-N-speak + turn-N-wait-for-reply (per caller turn)
//  8. wait-for-expected-tools — answers one follow-up question if needed
//  9. settle — a late duplicate would land here
//
// 10. hangup-and-wait-ended
// 11. assert-no-tool-ran-twice
// 12. assert-no-tool-before-confirmed-turn
// 13. assert-expected-tools
// 14. assert-early-generation-ran

// A: "Yes, that's me." … "Sure, go ahead." → record_lead, transfer_to_specialist.
func TestVerb_Agent_EarlyGenTools_A_AgreeTwoTurns_Eot07_Run1(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeTwoTurns, 0.7)
}

func TestVerb_Agent_EarlyGenTools_A_AgreeTwoTurns_Eot07_Run2(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeTwoTurns, 0.7)
}

func TestVerb_Agent_EarlyGenTools_A_AgreeTwoTurns_Eot08(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeTwoTurns, 0.8)
}

// B: "Yes… (1 s) …I'd like to talk to someone." → record_lead, transfer_to_specialist.
func TestVerb_Agent_EarlyGenTools_B_AgreeAfterPause_Eot07_Run1(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeAfterPause, 0.7)
}

func TestVerb_Agent_EarlyGenTools_B_AgreeAfterPause_Eot07_Run2(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeAfterPause, 0.7)
}

func TestVerb_Agent_EarlyGenTools_B_AgreeAfterPause_Eot08(t *testing.T) {
	runEarlyGenTools(t, earlyGenAgreeAfterPause, 0.8)
}

// C: "Yes… (½ s) …actually no, call me tomorrow at three." → schedule_callback only, no transfer.
func TestVerb_Agent_EarlyGenTools_C_YesThenCallbackNoTransfer_Eot07_Run1(t *testing.T) {
	runEarlyGenTools(t, earlyGenYesThenCallback, 0.7)
}

func TestVerb_Agent_EarlyGenTools_C_YesThenCallbackNoTransfer_Eot07_Run2(t *testing.T) {
	runEarlyGenTools(t, earlyGenYesThenCallback, 0.7)
}

func TestVerb_Agent_EarlyGenTools_C_YesThenCallbackNoTransfer_Eot08(t *testing.T) {
	runEarlyGenTools(t, earlyGenYesThenCallback, 0.8)
}

// D: "Not now, call me tomorrow at ten." → schedule_callback.
func TestVerb_Agent_EarlyGenTools_D_Callback_Eot07_Run1(t *testing.T) {
	runEarlyGenTools(t, earlyGenCallback, 0.7)
}

func TestVerb_Agent_EarlyGenTools_D_Callback_Eot07_Run2(t *testing.T) {
	runEarlyGenTools(t, earlyGenCallback, 0.7)
}

func TestVerb_Agent_EarlyGenTools_D_Callback_Eot08(t *testing.T) {
	runEarlyGenTools(t, earlyGenCallback, 0.8)
}

// E: "No thanks, not interested." → mark_not_interested.
func TestVerb_Agent_EarlyGenTools_E_NotInterested_Eot07_Run1(t *testing.T) {
	runEarlyGenTools(t, earlyGenNotInterested, 0.7)
}

func TestVerb_Agent_EarlyGenTools_E_NotInterested_Eot07_Run2(t *testing.T) {
	runEarlyGenTools(t, earlyGenNotInterested, 0.7)
}

func TestVerb_Agent_EarlyGenTools_E_NotInterested_Eot08(t *testing.T) {
	runEarlyGenTools(t, earlyGenNotInterested, 0.8)
}

func runEarlyGenTools(t *testing.T, sc earlyGenScenario, eotThreshold float64) {
	t.Helper()
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	if deepgramFluxLabel == "" {
		s.Done()
		t.Skip("needs the in-jambonz Deepgram Flux credential (provisioned at TestMain)")
	}
	s.Done()

	ctx := WithTimeout(t, 240*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-wavs")
	wavs := make([]string, len(sc.Turns))
	for i, u := range sc.Turns {
		first, err := tts.EnsureWAV(ctx, "testdata/agent", u.First, tts.PromptOptions{Model: "aura-asteria-en"})
		if err != nil {
			s.Fatalf("EnsureWAV(%q): %v", u.First, err)
		}
		wavs[i] = first
		if u.Then == "" {
			continue
		}
		then, err := tts.EnsureWAV(ctx, "testdata/agent", u.Then, tts.PromptOptions{Model: "aura-asteria-en"})
		if err != nil {
			s.Fatalf("EnsureWAV(%q): %v", u.Then, err)
		}
		wavs[i] = joinWAVsWithPause(t, first, then, u.Pause)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{earlyGenAgentVerb(eotThreshold), V("hangup")}))
	sess.ScriptToolOutput(func(cb webhook.Callback) any {
		if out, ok := earlyGenToolResults[cb.String("name")]; ok {
			return out
		}
		return map[string]any{"ok": false, "error": "unknown tool"}
	})
	s.Done()

	// Drain continuously: the session queue holds only 32 callbacks.
	col := collectCallbacks(ctx, sess)

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(180))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "early-gen-tools.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	if !waitForAgentReply(s, col, recPath, 0, 30*time.Second) {
		s.Fatalf("agent never greeted within 30s; events=%s", summarizeEventTypes(col.snapshot()))
	}
	s.Done()

	for i, wav := range wavs {
		s = Step(t, fmt.Sprintf("turn-%d-speak", i+1))
		replies := len(findAgentEvents(col.snapshot(), "llm_response"))
		if err := call.SendWAV(wav); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		s.Done()

		s = Step(t, fmt.Sprintf("turn-%d-wait-for-reply", i+1))
		if !waitForAgentReply(s, col, recPath, replies, 20*time.Second) {
			s.Logf("no llm_response within 20s")
		}
		s.Done()
	}

	s = Step(t, "wait-for-expected-tools")
	if !waitForTools(col, sc.Expect, 15*time.Second) && sc.FollowUp != "" {
		follow, err := tts.EnsureWAV(ctx, "testdata/agent", sc.FollowUp, tts.PromptOptions{Model: "aura-asteria-en"})
		if err != nil {
			s.Fatalf("EnsureWAV(%q): %v", sc.FollowUp, err)
		}
		s.Logf("expected tools not run yet; answering the agent: %q", sc.FollowUp)
		replies := len(findAgentEvents(col.snapshot(), "llm_response"))
		if err := call.SendWAV(follow); err != nil {
			s.Fatalf("SendWAV(follow-up): %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (follow-up): %v", err)
		}
		waitForAgentReply(s, col, recPath, replies, 20*time.Second)
		waitForTools(col, sc.Expect, 20*time.Second)
	}
	s.Done()

	s = Step(t, "settle")
	time.Sleep(10 * time.Second)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
	time.Sleep(2 * time.Second) // agent-complete races the BYE
	cbs := col.snapshot()
	counts := toolCounts(cbs)
	s = Step(t, "log-call")
	s.Logf("eotThreshold=%.1f tools executed=%v events=%s", eotThreshold, counts, summarizeEventTypes(cbs))
	for _, cb := range cbs {
		if cb.Hook == "agent_tool_call" {
			s.Logf("tool call: name=%s id=%s args=%v", cb.String("name"), cb.String("tool_call_id"), cb.NestedAny("arguments"))
		}
	}
	for _, te := range findAgentEvents(cbs, "turn_end") {
		s.Logf("turn_end: transcript=%q response=%q preflight=%v tool_calls=%v",
			te.String("transcript"), truncate(te.String("response"), 120),
			te.NestedAny("latency.preflight"), te.JSON["tool_calls"])
	}
	s.Done()

	s = Step(t, "assert-no-tool-ran-twice")
	ids := toolCallIDs(cbs)
	for name, n := range counts {
		if n <= 1 {
			continue
		}
		cause := "distinct tool_call_ids: the LLM asked for it more than once"
		if len(ids[name]) < n {
			cause = "a tool_call_id was dispatched more than once: feature-server ran one call twice"
		}
		s.Errorf("%s ran %d times — a tool must never run twice (%s); ids=%v executed=%v",
			name, n, cause, ids[name], counts)
	}
	s.Done()

	s = Step(t, "assert-no-tool-before-confirmed-turn")
	for _, v := range toolsBeforeConfirmedTurn(cbs) {
		s.Errorf("%s ran before its turn was confirmed (no user_transcript since the last turn_end) "+
			"— a tool ran on a guess", v)
	}
	s.Done()

	s = Step(t, "assert-expected-tools")
	want := map[string]bool{}
	for _, name := range sc.Expect {
		want[name] = true
		if counts[name] == 0 {
			s.Errorf("%s never ran; expected %v once each, executed=%v", name, sc.Expect, counts)
		}
	}
	for name, n := range counts {
		if !want[name] && n > 0 {
			s.Errorf("%s ran but the scenario never asks for it; expected only %v, executed=%v "+
				"(see turn_end above: a confirmed turn that asked for it means STT ended the turn early)",
				name, sc.Expect, counts)
		}
	}
	s.Done()

	s = Step(t, "assert-early-generation-ran")
	// Without this the scenario passes on a build that skips early generation
	// for agents with tools, which is the behaviour #252 replaces.
	results := preflightResults(cbs)
	s.Logf("preflight results per turn: %v", results)
	if len(results) == 0 {
		s.Errorf("no turn ran early generation (no turn_end.latency.preflight) — this build does " +
			"not speculate for agents with tools, so nothing here was exercised")
	}
	s.Done()
}

// callbackCollector drains a session's callbacks into a slice for the life of ctx.
type callbackCollector struct {
	mu  sync.Mutex
	cbs []webhook.Callback
}

func collectCallbacks(ctx context.Context, sess *webhook.Session) *callbackCollector {
	c := &callbackCollector{}
	go func() {
		for {
			cb, err := sess.WaitCallback(ctx)
			if err != nil {
				return
			}
			c.mu.Lock()
			c.cbs = append(c.cbs, cb)
			c.mu.Unlock()
		}
	}()
	return c
}

func (c *callbackCollector) snapshot() []webhook.Callback {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]webhook.Callback(nil), c.cbs...)
}

// toolCounts is the test agent's `executed`: one /tool-call event is one execution.
func toolCounts(cbs []webhook.Callback) map[string]int {
	out := map[string]int{}
	for _, cb := range cbs {
		if cb.Hook == "agent_tool_call" {
			out[cb.String("name")]++
		}
	}
	return out
}

// toolsBeforeConfirmedTurn names tool calls not preceded by a user_transcript
// since the last turn_end. WS frames arrive in send order, and feature-server
// sends user_transcript only once the turn is confirmed.
func toolsBeforeConfirmedTurn(cbs []webhook.Callback) []string {
	var out []string
	confirmed := false
	for _, cb := range cbs {
		switch {
		case cb.Hook == "agent_tool_call" && !confirmed:
			out = append(out, cb.String("name"))
		case cb.Hook == "agent_event" && cb.String("type") == "user_transcript":
			confirmed = true
		case cb.Hook == "agent_event" && cb.String("type") == "turn_end":
			confirmed = false
		}
	}
	return out
}

// toolCallIDs returns the distinct tool_call_ids seen per tool name.
func toolCallIDs(cbs []webhook.Callback) map[string][]string {
	seen := map[string]bool{}
	out := map[string][]string{}
	for _, cb := range cbs {
		id := cb.String("tool_call_id")
		if cb.Hook != "agent_tool_call" || seen[id] {
			continue
		}
		seen[id] = true
		out[cb.String("name")] = append(out[cb.String("name")], id)
	}
	return out
}

func waitForTools(col *callbackCollector, names []string, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for !allToolsRan(toolCounts(col.snapshot()), names) {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(250 * time.Millisecond)
	}
	return true
}

func allToolsRan(counts map[string]int, names []string) bool {
	for _, n := range names {
		if counts[n] == 0 {
			return false
		}
	}
	return true
}

// preflightResults returns turn_end.latency.preflight.result for each turn that speculated.
func preflightResults(cbs []webhook.Callback) []string {
	var out []string
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if r := te.NestedString("latency.preflight.result"); r != "" {
			out = append(out, r)
		}
	}
	sort.Strings(out)
	return out
}

// waitForAgentReply waits for an llm_response beyond the first `seen`, then
// for the agent to stop speaking, so the caller never talks over it.
func waitForAgentReply(s *StepCtx, col *callbackCollector, pcmPath string, seen int, budget time.Duration) bool {
	deadline := time.Now().Add(budget)
	for len(findAgentEvents(col.snapshot(), "llm_response")) <= seen {
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(100 * time.Millisecond)
	}
	// llm_response fires when generation ends, before TTS audio starts.
	if waitForSpeech(s, pcmPath, 8*time.Second) {
		waitForQuiet(s, pcmPath, 1500*time.Millisecond, 30*time.Second)
	}
	return true
}

// waitForQuiet returns once the last `quiet` of the recording carries no speech.
func waitForQuiet(s *StepCtx, pcmPath string, quiet, budget time.Duration) bool {
	n := int(quiet.Seconds()*8000) * 2
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pcmPath)
		if err == nil && len(raw) >= n {
			var peak int16
			tail := raw[len(raw)-n:]
			for i := 0; i+1 < len(tail); i += 2 {
				v := int16(binary.LittleEndian.Uint16(tail[i:]))
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			if peak <= 1500 {
				return true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Logf("agent still speaking after %s", budget)
	return false
}

// joinWAVsWithPause writes a, `pause` of silence, then b as one telephony WAV.
// Assumes the 44-byte 8 kHz 16-bit mono header internal/tts writes.
func joinWAVsWithPause(t *testing.T, a, b string, pause time.Duration) string {
	t.Helper()
	const headerLen = 44
	ra, err := os.ReadFile(a)
	if err != nil {
		helperFatalf(t, "join-wavs", "ReadFile(%s): %v", a, err)
	}
	rb, err := os.ReadFile(b)
	if err != nil {
		helperFatalf(t, "join-wavs", "ReadFile(%s): %v", b, err)
	}
	if len(ra) <= headerLen || len(rb) <= headerLen {
		helperFatalf(t, "join-wavs", "%s or %s is not a WAV", a, b)
	}
	gap := make([]byte, int(pause.Seconds()*8000)*2)
	data := append(append(append([]byte{}, ra[headerLen:]...), gap...), rb[headerLen:]...)
	out := append(append([]byte{}, ra[:headerLen]...), data...)
	binary.LittleEndian.PutUint32(out[4:], uint32(36+len(data)))
	binary.LittleEndian.PutUint32(out[40:], uint32(len(data)))
	name := strings.NewReplacer("/", "_", " ", "_").Replace(t.Name()) + ".wav"
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, out, 0o644); err != nil {
		helperFatalf(t, "join-wavs", "WriteFile(%s): %v", path, err)
	}
	return path
}
