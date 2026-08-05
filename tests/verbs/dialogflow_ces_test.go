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
// missing. Note DIALOGFLOW_AGENT (a CX agent) is NOT a substitute for
// DIALOGFLOW_CES_APP — a CES app is a different GCP resource.
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

// cesReplyKeywords parses DIALOGFLOW_CES_KEYWORDS. Empty => audio is logged but
// not asserted: against a newly-built app the agent's phrasing is unknown, and a
// phrasing miss must never mask the tool-round-trip result, which is the actual
// subject of this test. minHits of 0 makes AssertTranscriptHasMost log-only.
func cesReplyKeywords() ([]string, int) {
	raw := strings.Split(cfg.DialogflowCESKeywords, ",")
	var words []string
	for _, w := range raw {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, w)
		}
	}
	if len(words) == 0 {
		// a word that will not match is still needed so the helper transcribes
		return []string{"\x00no-keywords-configured"}, 0
	}
	return words, 1
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
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", cfg.DialogflowCESPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 1: %v", err)
	}
	prompt2WAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", cfg.DialogflowCESPrompt2, tts.PromptOptions{
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
	if !json.Valid([]byte(cfg.DialogflowCESToolOutput)) {
		s.Fatalf("DIALOGFLOW_CES_TOOL_OUTPUT is not valid JSON: %s", cfg.DialogflowCESToolOutput)
	}
	cesToolResultBody, err := json.Marshal(map[string]any{
		"outputParameters": json.RawMessage(cfg.DialogflowCESToolOutput),
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

	// The strict assertion is the tool round trip below; audio is
	// LLM-phrasing-dependent, so a miss here is logged, not fatal.
	s = Step(t, "turn1-assert-reply")
	kw, minHits := cesReplyKeywords()
	AssertTranscriptHasMost(s, ctx, rec1, minHits, kw...)
	if minHits == 0 {
		s.Logf("audio not asserted (set DIALOGFLOW_CES_KEYWORDS to make it strict)")
	}
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
	if tool := first.NestedString("tool_call.tool"); tool == "" {
		s.Logf("WARNING: CES tool_call.tool is empty (toolset-sourced tool?): %s", string(first.Body))
	}
	s.Logf("CES tool callbacks: %d (first: display_name=%q id=%q)", len(toolCBs),
		first.NestedString("tool_call.display_name"), first.NestedString("tool_call.id"))
	s.Done()

	// A reply after the tool result proves the mid-stream send unblocked the
	// agent instead of stalling or tearing down the session.
	s = Step(t, "assert-conversation-continued")
	kw2, minHits2 := cesReplyKeywords()
	AssertTranscriptHasMost(s, ctx, rec2, minHits2, kw2...)
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
		s.Logf("WARNING: no dialogflow CES eventHook callbacks captured")
	} else {
		s.Logf("dialogflow CES event types: %v (tool-calls seen: %v)", types, sawToolCalls)
	}
	s.Done()
}
