// Tests for the `dialogflow` verb — CES variant (Conversational Agents /
// Agent Studio, model "ces") with client-side tool calls.
//
// Chain under test:
//
//	caller speaks (our WAV)  ==RTP==>
//	feature-server dialogflow task  --dialogflow_ces_start-->
//	@jambonz/mrf  --dialogflow.start--> mediajam --BidiRunSession--> CES
//	CES returns tool_calls --> mediajam tool_calls event --> task POSTs toolHook
//	our webhook returns {outputParameters} --> task --dialogflow_ces_tool_result-->
//	@jambonz/mrf --dialogflow.toolResult--> mediajam sends SessionInput.tool_responses
//	MID-STREAM --> CES continues the turn --> audio to caller (recorded)
//
// This is the regression test for the CES tool-call stall: before the fix
// mediajam never emitted a tool_calls event for CES (it only buried the calls
// inside session_output), and had no send path for a result at all — so an
// agent-requested client-side tool was never surfaced to the application and
// the conversation hung indefinitely.
//
// Two structural differences from the CX test in dialogflow_test.go:
//
//   - CES's BidiRunSession is ONE long-lived stream owning the agent's turn
//     state, so the tool result is delivered mid-stream rather than by priming
//     a fresh turn. A stream restart here would abandon the pending turn.
//   - CES surfaces every requested tool in one event and accepts a LIST of
//     results, so all calls are answered in a single send.
//
// The tool_call payload shape also differs: CX posts
// {tool, action, input_parameters}; CES posts {id, display_name, tool, args}.
// Assertions below key off display_name / args accordingly.
//
// Skips when CES is unconfigured (cfg.HasDialogflowCES) or DEEPGRAM_API_KEY is
// missing. Only the app's coordinates are env-supplied (DIALOGFLOW_CES_APP /
// _DEPLOYMENT / _LOCATION); the prompts and canned tool output are hardcoded
// fixtures, as in the CX test. Note DIALOGFLOW_AGENT (a CX agent) is NOT a
// substitute for DIALOGFLOW_CES_APP — a CES app is a different GCP resource.
//
// Provisioning the app: it must expose a client-side (Function) tool WITH A
// NON-EMPTY DESCRIPTION. A tool imported with an empty description is never
// offered to the model at all, which presents as a silent stall — the exact
// symptom this test exists to catch — rather than an error.
package verbs

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// The app under test is Google's CES "weather tool demo": a client-side
// (Function) tool get_weather(city). These fixtures are hardcoded rather than
// configurable because the assertions below are specific to THIS app — they key
// off get_weather and args.city — so making them tunable would only pretend the
// test is app-agnostic. The app's coordinates (id / deployment / location) DO
// live in env, since those differ per environment. Mirrors the CX test, which
// hardcodes its prompts and canned tool results the same way.

// cesPrompt drives the agent into its get_weather call.
const cesPrompt = "what is the weather in Boston"

// cesPrompt2 continues the conversation after the tool result, which is what
// proves the mid-stream send did not disturb the session.
const cesPrompt2 = "what about New York"

// cesToolOutput is what our toolHook answers with; it must fit get_weather's
// declared output schema, since CES can reject a mismatched shape. The values
// are deliberately ordinary-looking but specific: cesToolEchoes derives the
// expected spoken tokens from them, so the agent has to say OUR numbers back.
// To re-run the mutation check, temporarily change these (e.g. 7 / "heavy snow"
// / 11) — the agent must then speak the new values.
const cesToolOutput = `{"temperature_f":54,"conditions":"light rain","humidity_pct":82}`

var cesSmallWords = []string{"zero", "one", "two", "three", "four", "five", "six",
	"seven", "eight", "nine", "ten", "eleven", "twelve", "thirteen", "fourteen",
	"fifteen", "sixteen", "seventeen", "eighteen", "nineteen"}

var cesTens = []string{"", "", "twenty", "thirty", "forty", "fifty", "sixty",
	"seventy", "eighty", "ninety"}

// spellInt renders 0..999 the way a TTS voice speaks it ("54" -> "fifty four"),
// so a numeric value from our tool output can be matched in an STT transcript,
// which never contains digits. Returns "" outside that range.
func spellInt(n int) string {
	switch {
	case n < 0 || n > 999:
		return ""
	case n < 20:
		return cesSmallWords[n]
	case n < 100:
		out := cesTens[n/10]
		if n%10 != 0 {
			out += " " + cesSmallWords[n%10]
		}
		return out
	default:
		out := cesSmallWords[n/100] + " hundred"
		if n%100 != 0 {
			out += " " + spellInt(n%100)
		}
		return out
	}
}

// cesToolEchoes derives, from the tool output WE answered with, the tokens that
// can only appear in the agent's speech if it actually received our result.
//
// This is what makes the test hard rather than merely green: asserting that a
// toolHook callback arrived only proves the agent ASKED for the tool. An agent
// that ignored (or never received) our result would still hallucinate a
// plausible weather reply and pass. Requiring our own values back in the
// transcript is the only assertion that distinguishes the two.
//
// String leaves are used verbatim (they survive TTS/STT intact); integers are
// spelled out because a transcript has no digits.
func cesToolEchoes(raw string) []string {
	var doc any
	if json.Unmarshal([]byte(raw), &doc) != nil {
		return nil
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		s = strings.ToLower(strings.TrimSpace(s))
		if s == "" || seen[s] || !strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyz") {
			return
		}
		seen[s] = true
		out = append(out, s)
	}
	var walk func(any)
	walk = func(v any) {
		switch t := v.(type) {
		case map[string]any:
			for _, sub := range t {
				walk(sub)
			}
		case []any:
			for _, sub := range t {
				walk(sub)
			}
		case string:
			// single letters / codes are too collision-prone to assert on
			if len(t) >= 4 {
				add(t)
			}
		case float64:
			if t == float64(int(t)) {
				add(spellInt(int(t)))
			}
		}
	}
	walk(doc)
	return out
}

// TestVerb_Dialogflow_CES — client-side tool-call round trip on the CES path.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-dialogflow-ces-verb — toolHook answers the tool call
//  4. place-call / answer-and-record
//  5. turn 1: prompt -> tool_calls -> toolHook -> mid-stream result -> reply
//  6. turn 2: continue the conversation (proves the stream survived the result)
//  7. assert-tool-callbacks — the CES round trip actually happened
//  8. hangup, event plumbing check
func TestVerb_Dialogflow_CES(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDialogflowCES() {
		s.Done()
		t.Skip("dialogflow CES test needs DIALOGFLOW_KEYFILE + DIALOGFLOW_PROJECT + DIALOGFLOW_CES_APP + DIALOGFLOW_CES_LOCATION")
	}
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("dialogflow CES test needs DEEPGRAM_API_KEY (prompt WAVs + STT verification)")
	}
	s.Done()

	ctx := WithTimeout(t, 220*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wavs")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", cesPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 1: %v", err)
	}
	prompt2WAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", cesPrompt2, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 2: %v", err)
	}
	s.Done()

	testID, sess := claimSession(t)

	s = Step(t, "script-dialogflow-ces-verb")
	// No welcomeEvent: a CES session primed with an event/text goes out of
	// audio mode, and caller audio would never reach the agent.
	args := []any{
		"credentials", cfg.DialogflowServiceKey,
		"project", cfg.DialogflowProject,
		"agent", cfg.DialogflowCESApp,
		"region", cfg.DialogflowCESLocation,
		"model", "ces",
		"lang", cfg.DialogflowLang,
		"actionHook", webhookSrv.PublicURL() + "/action/dialogflow-ces",
		"eventHook", SessionURL(sess, "dialogflow-ces-event"),
		"toolHook", SessionURL(sess, "dialogflow-ces-tool"),
		"events", []string{"session-output", "transcription", "tool-calls", "start-play", "stop-play"},
	}
	if cfg.DialogflowCESDeployment != "" {
		args = append(args, "deployment", cfg.DialogflowCESDeployment)
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dialogflow", args...),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dialogflow", "dialogflow-ces-event")

	// The toolHook answer, wrapped as {"outputParameters": <configured JSON>}.
	// It must fit the tool's DECLARED OUTPUT SCHEMA — CES can reject a
	// mismatched shape, which would look like the agent ignoring the result.
	// Override with DIALOGFLOW_CES_TOOL_OUTPUT to match your app's tool.
	if !json.Valid([]byte(cesToolOutput)) {
		s.Fatalf("DIALOGFLOW_CES_TOOL_OUTPUT is not valid JSON: %s", cesToolOutput)
	}
	cesToolResultBody, err := json.Marshal(map[string]any{
		"outputParameters": json.RawMessage(cesToolOutput),
	})
	if err != nil {
		s.Fatalf("marshal tool result: %v", err)
	}
	s.Logf("toolHook will answer with: %s", string(cesToolResultBody))
	sess.ScriptActionHookBodyFunc("dialogflow-ces-tool", func(cb webhook.Callback) []byte {
		return cesToolResultBody
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
	s.Done()

	rec1 := filepath.Join(t.TempDir(), "ces-turn1-reply.wav")
	rec2 := filepath.Join(t.TempDir(), "ces-turn2-reply.wav")

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.StartRecording(rec1); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "turn1-speak-prompt")
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "turn1-wait-for-reply")
	// CES endpointing + agent LLM + toolHook RTT + mid-stream tool result +
	// the agent's continued turn + playback
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(20 * time.Second)
	call.StopRecording()
	s.Done()

	// STRICT: the agent must speak OUR tool output back. A toolHook callback
	// alone only proves the agent asked for the tool; an agent that never
	// received our result would still invent a plausible weather reply. These
	// tokens are derived from DIALOGFLOW_CES_TOOL_OUTPUT, so this assertion
	// tracks the configured output automatically.
	s = Step(t, "turn1-assert-tool-output-spoken")
	echoes := cesToolEchoes(cesToolOutput)
	if len(echoes) == 0 {
		s.Fatalf("cannot derive any verifiable token from cesToolOutput=%s; it needs at least "+
			"one string (>=4 chars) or integer value, otherwise the test cannot prove the "+
			"agent received the tool result", cesToolOutput)
	}
	wants := echoes
	// Require TWO independent matches when we have them: a single hit could in
	// principle be a coincidence (an agent inventing a plausible temperature),
	// two of our own values co-occurring cannot. Still tolerates the agent
	// omitting one field of the result.
	minHits := 2
	if len(wants) < 2 {
		minHits = 1
	}
	s.Logf("requiring >=%d of our own tool-output values in the reply: %v", minHits, wants)
	AssertTranscriptHasMost(s, ctx, rec1, minHits, wants...)
	s.Done()

	s = Step(t, "turn2-continue-conversation")
	if err := call.StartRecording(rec2); err != nil {
		s.Fatalf("StartRecording turn 2: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(prompt2WAV); err != nil {
		s.Fatalf("SendWAV turn 2: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "turn2-wait-for-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(22 * time.Second)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	// This is the regression assertion. Before the fix no toolHook callback
	// ever arrived on the CES path: mediajam emitted no tool_calls event, so
	// the feature-server's listener never fired.
	s = Step(t, "assert-tool-callbacks")
	cbs := DrainCallbacks(sess, 5*time.Second)
	var toolCBs []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == "action/dialogflow-ces-tool" {
			toolCBs = append(toolCBs, cb)
		}
	}
	if len(toolCBs) == 0 {
		s.Fatalf("no CES toolHook callbacks captured in %d callbacks — the agent's tool call never reached the application (the stall this test guards)", len(cbs))
	}
	// CES's payload shape: {id, display_name, tool, args}. The id is what the
	// agent correlates the result on, so its presence is the load-bearing part.
	first := toolCBs[0]
	if id := first.NestedString("tool_call.id"); id == "" {
		s.Errorf("CES tool_call.id is empty — the result cannot be correlated back to the call: %s", string(first.Body))
	}
	if name := first.NestedString("tool_call.display_name"); name == "" {
		s.Errorf("CES tool_call.display_name is empty: %s", string(first.Body))
	}
	// tool is the resource path CES correlates on alongside id. It may be
	// legitimately absent for a toolset-sourced tool, but when present it must
	// be a real path, not a bare display name.
	if tool := first.NestedString("tool_call.tool"); tool == "" {
		s.Logf("WARNING: CES tool_call.tool is empty (toolset-sourced tool?): %s", string(first.Body))
	} else if !strings.Contains(tool, "/apps/") || !strings.Contains(tool, "/tools/") {
		s.Errorf("CES tool_call.tool is not a tool resource path: %q", tool)
	}

	// args must be populated: it proves the agent extracted parameters from the
	// caller's utterance and passed them through, rather than firing an empty
	// tool call. An empty args object is a real defect, not phrasing noise.
	var tcBody struct {
		ToolCall struct {
			Args map[string]any `json:"args"`
		} `json:"tool_call"`
	}
	if err := json.Unmarshal(first.Body, &tcBody); err != nil {
		s.Errorf("cannot parse tool_call body: %v", err)
	} else if len(tcBody.ToolCall.Args) == 0 {
		s.Errorf("CES tool_call.args is empty — the agent passed no parameters: %s", string(first.Body))
	} else {
		s.Logf("tool_call.args = %v", tcBody.ToolCall.Args)
	}
	s.Logf("CES tool callbacks: %d (first: display_name=%q id=%q)", len(toolCBs),
		first.NestedString("tool_call.display_name"), first.NestedString("tool_call.id"))
	s.Done()

	// A reply after the tool result proves the mid-stream send unblocked the
	// agent instead of stalling or tearing down the session.
	// The stream must have survived the mid-stream tool result: the agent is
	// still conversing on the SAME session. A restart-based implementation
	// would have abandoned the turn here.
	s = Step(t, "assert-conversation-continued")
	AssertTranscriptNonEmpty(s, ctx, rec2)
	s.Done()

	s = Step(t, "assert-event-plumbing")
	var types []string
	sawToolCalls := false
	for _, cb := range cbs {
		if cb.Hook == "action/dialogflow-ces-event" {
			ev := cb.String("event")
			types = append(types, ev)
			if ev == "tool-calls" {
				sawToolCalls = true
			}
			if id := cb.NestedString("customer_data.x_test_id"); id != "" && id != testID {
				s.Errorf("eventHook x_test_id=%q want %q", id, testID)
			}
		}
	}
	if len(types) == 0 {
		s.Errorf("no dialogflow CES eventHook callbacks captured, but the verb subscribed to %v",
			[]string{"session-output", "transcription", "tool-calls", "start-play", "stop-play"})
	}
	// The verb subscribed to 'tool-calls', so the app MUST have been notified.
	// Before the fix mediajam emitted no such event at all, so this is the
	// event-plumbing half of the regression.
	if !sawToolCalls {
		s.Errorf("eventHook never delivered a 'tool-calls' event despite being subscribed; got %v", types)
	}
	s.Logf("dialogflow CES event types: %v (tool-calls seen: %v)", types, sawToolCalls)
	s.Done()
}
