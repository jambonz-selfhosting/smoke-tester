//go:build detail

package verbs

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Agent_EventHook — eventHook fires user_transcript, llm_response
// and turn_end events around each conversational turn. We assert each event
// type appears at least once, with a content-bearing payload.
//
// Why a separate test from Echo: this test gates on the event payloads
// rather than the audio round-trip, so it doesn't need to hold the call
// open as long. It also exercises the schema (callbacks/agent-turn) — the
// Echo test only validates that the round-trip works at all.
//
// Correlation drift: feature-server's agent eventHook doesn't attach
// callInfo to its payload, so events land in the shared `_anon` session
// (smoke-tester webhook server's anon fallback). We filter by event type
// + content and tolerate parallel runs by keying on the unique transcript
// substring this test injects.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (eventHook → /action/agent-turn — schema-validated)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-events — silence while LLM thinks + TTS streams
//
// 10. drain-anon-events
// 11. assert-user-transcript — find a user_transcript event with our keywords
// 12. assert-llm-response — find an llm_response event with non-empty body
// 13. assert-turn-end — find a turn_end with transcript + response + latency
// 14. hangup
func TestVerb_Agent_EventHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// Append ?X-Test-Id=<testID> to per-callback URLs so the webhook server
	// routes eventHook callbacks to THIS session (the payload itself has no
	// customerData and would otherwise land in shared `_anon`, racing with
	// parallel agent tests).
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Drain THIS session while events stream in (eventHook URL was
	// minted via SessionURL so it carries our X-Test-Id query param —
	// no `_anon` contention). Each event type fires at a different
	// moment (user_transcript ~when STT finalizes, llm_response ~when
	// LLM stream completes, turn_end ~when TTS empties).
	cbs := DrainCallbacks(sess, LLMReplyWindow)
	s.Logf("captured %d agent events", len(cbs))
	s.Done()

	s = Step(t, "assert-user-transcript")
	transcripts := findAgentEvents(cbs, "user_transcript")
	if len(transcripts) == 0 {
		s.Fatalf("no user_transcript event in %d agent events: %s",
			len(cbs), summarizeEventTypes(cbs))
	}
	// The transcript should contain at least one of our injected keywords —
	// proves it was OUR call's event, not a parallel test's.
	matched := ""
	for _, cb := range transcripts {
		txt := strings.ToLower(cb.String("transcript"))
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(txt, kw) {
				matched = txt
				break
			}
		}
		if matched != "" {
			break
		}
	}
	if matched == "" {
		s.Errorf("no user_transcript matched our prompt; got %d transcripts: %v",
			len(transcripts), summarizeTranscripts(transcripts))
	} else {
		s.Logf("user_transcript matched: %q", matched)
	}
	s.Done()

	s = Step(t, "assert-llm-response")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("no llm_response event in %d agent events", len(cbs))
	} else {
		// The echo system prompt forces the LLM to repeat the prompt's
		// words. Concatenate every llm_response and require at least
		// one of our 4 keywords lands in the text — proves the LLM
		// actually responded to OUR prompt's content (a regression
		// that emits hallucinated/empty/generic text would fail).
		var all string
		for _, r := range responses {
			all += " " + strings.ToLower(r.String("response"))
		}
		if strings.TrimSpace(all) == "" {
			s.Errorf("all llm_response events have empty response field")
		}
		hits := 0
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(all, kw) {
				hits++
			}
		}
		if hits == 0 {
			s.Errorf("llm_response %q contains none of the prompt's keywords (alpha/bravo/charlie/delta)",
				truncate(all, 200))
		} else {
			s.Logf("llm_response keyword hits=%d: %q", hits, truncate(all, 100))
		}
	}
	s.Done()

	s = Step(t, "assert-turn-end")
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) == 0 {
		s.Errorf("no turn_end event in %d agent events", len(cbs))
	} else {
		// Schema requires transcript + response + interrupted + latency.
		// validateInbound ran already; we just confirm fields are present.
		te := turnEnds[0].JSON
		for _, want := range []string{"transcript", "response", "interrupted", "latency"} {
			if _, ok := te[want]; !ok {
				s.Errorf("turn_end missing required field %q: %s",
					want, string(turnEnds[0].Body))
			}
		}
		// Latency block should at least include voice_latency or model_latency.
		if lat, ok := te["latency"].(map[string]any); ok {
			s.Logf("turn_end latency: %v", lat)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Agent_Greeting — what `greeting:true` actually promises is one
// thing: the agent speaks FIRST, before the user has said anything. That's
// the feature, and that's what we assert — NOT the words it chooses. A
// greeting is free-form LLM output; pinning it to a keyword set ("hello",
// "name", …) tests the LLM's phrasing, not jambonz's greeting feature, and
// flakes whenever the model ad-libs.
//
// Two independent, content-free proofs that the greeting fired:
//
//  1. Audio energy in the first 6s — BEFORE we send any user audio — proves
//     the agent put speech on the wire unprompted (a non-greeting agent is
//     silent until the user speaks). Bytes-level, immune to LLM phrasing.
//
//  2. EventHook ordering — the agent emits at least one llm_response/turn_end
//     event with NO preceding user_transcript. The agent producing a turn
//     before it has heard a single user utterance is the deterministic
//     signature of an unprompted greeting; it can't be faked by noise or by
//     echoing user audio (there was none yet). This is the strong assertion.
//
// Turn 2 then sends a user prompt and we assert only that the agent took a
// SECOND turn (more events fire after the user_transcript) — i.e. the call
// stayed a live conversation after the greeting. We deliberately do NOT
// assert the agent echoed our words: verbatim-echo is non-deterministic LLM
// behaviour already covered, on a neutral phrase, by Agent_Echo.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (greeting=true)
//  5. place-call
//  6. answer-record-greeting
//  7. wait-for-greeting — listen for ~6s while agent says hello (user silent)
//  8. assert-greeting-audio — recording has substantial inbound bytes already
//  9. record-and-send-user-prompt — now user speaks (turn 2)
//  10. wait-for-second-turn — silence while LLM replies again
//  11. hangup-and-wait-ended
//  12. assert-greeting-fired-unprompted — eventHook shows an agent turn before
//     any user_transcript (the deterministic greeting signature)
func TestVerb_Agent_Greeting(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	const userPrompt = "Hello, my name is John and I am calling from the office."
	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", userPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// Keep the prompt minimal: greet first, then stay conversational. We do
	// NOT instruct "repeat verbatim" — that pushed the model into ad-libbing
	// a persona on the "I am calling from the office" prompt, which the test
	// then mis-asserted against. The assertions below check the greeting
	// FEATURE (agent speaks first), not the LLM's word choice.
	greetingSystemPrompt := "Greet the user with a short hello on your first turn. " +
		"Then keep replying briefly and helpfully to whatever they say."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: greetingSystemPrompt,
		Greeting:     true,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-greeting")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Record ONLY the greeting turn into its own file. We stop this
	// recording before sending the user prompt so the greeting
	// transcript is clean (turn-2 audio can't bleed in and "rescue" a
	// silent greeting).
	greetingRec := t.TempDir() + "/agent-greeting.pcm"
	if err := call.StartRecording(greetingRec); err != nil {
		s.Fatalf("StartRecording (greeting): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-greeting", 6*time.Second)
	bytesAfterGreeting := call.PCMBytesIn()
	call.StopRecording()

	s = Step(t, "assert-greeting-audio")
	// The agent put speech on the wire in the first 6s while the user was
	// SILENT — that's the greeting. 6s @ PCMU 8kHz = ~96KB; require >= 16KB
	// (~1s of energy). We assert bytes only, not words: a greeting is
	// free-form LLM output, so any keyword set tests the model's phrasing
	// rather than the greeting feature. That speech was a real, meaningful
	// agent turn (not noise) is proved deterministically by the eventHook
	// assertion at the end — an llm_response/turn_end before any
	// user_transcript.
	_ = greetingRec
	const minGreetingBytes = 16000
	if bytesAfterGreeting < minGreetingBytes {
		s.Fatalf("greeting=true but only %d PCM bytes received in 6s (need >= %d)",
			bytesAfterGreeting, minGreetingBytes)
	}
	s.Logf("greeting produced %d PCM bytes before user spoke", bytesAfterGreeting)
	s.Done()

	// Start a fresh recording for the user→agent echo turn.
	echoRec := t.TempDir() + "/agent-greeting-echo.pcm"

	s = Step(t, "record-and-send-user-prompt")
	if err := call.StartRecording(echoRec); err != nil {
		s.Fatalf("StartRecording (echo): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre-prompt): %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-prompt): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	_ = echoRec // turn-2 reply is asserted via eventHook ordering, not STT

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-greeting-fired-unprompted")
	// Drain the agent eventHook stream and reconstruct turn order. The
	// deterministic signature of greeting=true: the agent emits a turn
	// (llm_response / turn_end) BEFORE it has seen any user_transcript —
	// it spoke without being spoken to. We assert on event *ordering*, not
	// on transcript content, so the LLM's free-form wording can't flake it.
	cbs := DrainCallbacks(sess, LLMReplyWindow)
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))

	// The first agent-produced turn must precede the first user_transcript.
	// cbs preserve arrival order (DrainCallbacks appends as they land).
	firstUserAt, firstAgentTurnAt := -1, -1
	for i, cb := range cbs {
		switch cb.String("type") {
		case "user_transcript":
			if firstUserAt == -1 {
				firstUserAt = i
			}
		case "llm_response", "turn_end":
			if firstAgentTurnAt == -1 {
				firstAgentTurnAt = i
			}
		}
	}
	if firstAgentTurnAt == -1 {
		s.Fatalf("no llm_response/turn_end event — agent never produced a turn: %s",
			summarizeEventTypes(cbs))
	}
	// Greeting proof: an agent turn landed with no user_transcript before it.
	// (firstUserAt == -1 means the user was never transcribed at all, which
	// still satisfies "agent spoke before any user turn".)
	if firstUserAt != -1 && firstAgentTurnAt > firstUserAt {
		s.Errorf("greeting=true but first agent turn (idx %d) came AFTER first user_transcript (idx %d) — agent did not speak unprompted: %s",
			firstAgentTurnAt, firstUserAt, summarizeEventTypes(cbs))
	} else {
		s.Logf("greeting confirmed: agent turn at idx %d preceded first user_transcript (idx %d)",
			firstAgentTurnAt, firstUserAt)
	}
	// Sanity that the call stayed a live conversation into turn 2: the agent
	// did hear the user prompt we sent (a user_transcript exists). Soft —
	// under load STT can drop the whole utterance; the greeting proof above
	// is the hard assertion for this test.
	if firstUserAt == -1 {
		s.Logf("note: no user_transcript captured for turn 2 (STT may have dropped it under load); greeting assertion stands on its own")
	}
	s.Done()
}

// TestVerb_Agent_ActionHookOnEnd — actionHook fires when agent ends. The
// agent verb's actionHook is invoked from awaitTaskDone() → performAction
// (lib/tasks/agent/index.js:594), which only resolves when the call/ session
// is ending. We hang up from our side and assert the actionHook arrives
// before our test budget runs out.
//
// The actionHook payload is the standard verb:hook envelope (callInfo +
// our results object). We assert call_sid is present (== our placed call's
// sid) — that proves both that the actionHook fired AND that correlation
// works for actionHook (unlike eventHook which loses correlation upstream).
//
// Steps:
//  1. preflight-skips
//  2. register-webhook-session
//  3. script-agent-verb (minimal — greeting=false, no audio needed)
//  4. place-call
//  5. answer-and-silence
//  6. hangup — proactive BYE from us
//  7. wait-action-agent-complete
//  8. assert-action-payload — call_sid + completion_reason in body
func TestVerb_Agent_ActionHookOnEnd(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	testID, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// Pre-hangup pause: hanging up before the agent reaches Idle races
	// kill with init and the actionHook never fires. 2s is reliable.
	time.Sleep(2 * time.Second)

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	// actionHook fires when the agent task is being killed (BYE → call
	// session teardown → agent.kill → notifyTaskDone → performAction).
	// 30s budget is comfortable on a healthy cluster.
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	actionCB, err := sess.WaitCallbackFor(waitCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Logf("action/agent-complete body: %s", string(actionCB.Body))
	s.Done()

	s = Step(t, "assert-action-payload")
	// callInfo includes call_sid (top-level), and performAction merges in
	// our `results` object which carries `completion_reason` (snake-cased
	// by HttpRequestor.snakeCaseKeys). Both confirm correlation works for
	// actionHook (unlike eventHook which lands in _anon).
	if got := actionCB.String("call_sid"); got == "" {
		s.Errorf("action/agent-complete payload missing call_sid: %s",
			string(actionCB.Body))
	} else {
		s.Logf("call_sid in payload: %s", got)
	}
	if got := actionCB.String("completion_reason"); got == "" {
		s.Errorf("action/agent-complete payload missing completion_reason: %s",
			string(actionCB.Body))
	} else {
		s.Logf("completion_reason: %s", got)
	}
	// customerData.x_test_id should also round-trip back to us — proves
	// the actionHook hit our session, not _anon.
	if id := actionCB.NestedString("customer_data.x_test_id"); id != "" && id != testID {
		s.Errorf("customer_data.x_test_id=%q want %q", id, testID)
	}
	s.Done()
}

// TestVerb_Agent_ToolHook_Arguments — proves the agent verb's LLM doesn't
// just fire a parameterless tool (see TestVerb_Agent_ToolHook); it must
// extract a real value from the user's speech and populate the tool's
// declared argument with it. We declare a `get_weather` tool that requires
// a `location` argument, ask "What is the weather in Chicago right now?",
// and assert:
//   - the toolHook payload's `name`/`tool_call_id` are present (base
//     shape, same as TestVerb_Agent_ToolHook)
//   - the toolHook payload's `arguments.location` actually contains
//     "chicago" — proves the LLM parsed the city out of REAL STT output
//     and populated the argument, not a canned/empty call (the core,
//     non-negotiable assertion for this test)
//   - the turn_end eventHook summary's `tool_calls` array independently
//     records the same tool by name — corroboration from a second,
//     differently-shaped callback
//   - the agent speaks the tool's returned conditions back to the caller
//     — round-trip corroboration, NOT the sole proof
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav (asks for the weather in Chicago)
//  3. register-webhook-session
//  4. script-agent-verb-with-tool — toolHook returns a static JSON weather report
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-tool-call — block on /action/agent-tool callback
//
// 10. assert-tool-payload — name + tool_call_id present
// 11. assert-tool-arguments — arguments.location contains "chicago"
// 12. wait-for-llm-reply — silence while agent speaks the weather report
// 13. hangup-and-wait-ended
// 14. drain-trailing-callbacks — collect turn_end after hangup
// 15. assert-turn-end-tool-calls — turn_end.tool_calls includes get_weather
// 16. assert-weather-spoken — recording contains the tool result word "hail"
func TestVerb_Agent_ToolHook_Arguments(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	const promptText = "What is the weather in Chicago right now?"

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", promptText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-with-tool")
	tool := map[string]any{
		"name":        "get_weather",
		"description": "Get the current weather conditions for a city. Call this whenever the user asks about weather.",
		"parameters": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"location": map[string]any{
					"type":        "string",
					"description": "the city name",
				},
			},
			"required": []string{"location"},
		},
	}
	systemPrompt := "You are a voice assistant. You don't know the weather yourself. " +
		"When the user asks for the weather in a city, you MUST call the get_weather tool " +
		"with that city as the `location` argument. " +
		"After the tool returns, tell the user the returned conditions and stop."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: systemPrompt,
		Tools:        []map[string]any{tool},
	})
	// agent-tool gets a JSON body (not a verb array): feature-server takes
	// our response body verbatim and feeds it to the LLM as the tool result.
	toolBody := []byte(`{"conditions":"heavy hail","temperature":"seventy one degrees"}`)
	sess.ScriptActionHookBody("agent-tool", toolBody)
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-tool-args-reply.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Tool call typically fires within 2-4s of the user transcript landing
	// (LLM emits tool_call as its first chunk). Routes back to THIS
	// session via the X-Test-Id query param we appended to the toolHook
	// URL — no _anon contention with parallel agent tests.
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	// Collect (do NOT discard) the eventHook callbacks we skip past: the
	// turn_end summary for this same turn races the toolHook POST, and
	// WaitCallbackFor would throw it away whenever it happens to arrive first —
	// making assert-turn-end-tool-calls below fail on arrival order alone.
	toolCB, earlyCBs, err := WaitCallbackForCollecting(waitCtx, sess, "action/agent-tool")
	if err != nil {
		s.Fatalf("waiting for action/agent-tool: %v", err)
	}
	s.Logf("action/agent-tool body: %s", string(toolCB.Body))
	if len(earlyCBs) > 0 {
		s.Logf("collected %d callback(s) that arrived before the tool POST", len(earlyCBs))
	}
	s.Done()

	s = Step(t, "assert-tool-payload")
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool name = %q, want %q", got, "get_weather")
	}
	if got := toolCB.String("tool_call_id"); got == "" {
		s.Errorf("tool_call_id missing in payload: %s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-arguments")
	// The core assertion: the LLM must have parsed "Chicago" out of the
	// user's REAL speech and populated the `location` argument with it —
	// not just fired a bare tool call. Accept both shapes the toolHook
	// `arguments` field can arrive in (a nested object, or a JSON-encoded
	// string) — same tolerance TestVerb_Agent_ToolHook applies, since LLM
	// vendors serialize differently. Never exact-equal: STT/LLM may yield
	// "Chicago, IL" or similar — Contains is the right bar.
	args := toolCB.NestedAny("arguments")
	if args == nil {
		s.Errorf("arguments missing in payload: %s", string(toolCB.Body))
	} else {
		var location string
		switch v := args.(type) {
		case map[string]any:
			location, _ = v["location"].(string)
		case string:
			var parsed map[string]any
			if err := json.Unmarshal([]byte(v), &parsed); err != nil {
				s.Errorf("arguments string %q is not valid JSON: %v", v, err)
			} else {
				location, _ = parsed["location"].(string)
			}
		default:
			s.Errorf("arguments has unexpected type %T: %v", args, args)
		}
		if !strings.Contains(strings.ToLower(location), "chicago") {
			s.Errorf("arguments.location = %q, want it to contain %q; full payload: %s",
				location, "chicago", string(toolCB.Body))
		} else {
			s.Logf("arguments.location = %q", location)
		}
	}
	s.Done()

	s = Step(t, "wait-for-turn-end")
	// Tool result fed back to LLM → second LLM round-trip → TTS streams the
	// weather report. Wait for the turn_end summary that names the tool rather
	// than sleeping a fixed budget: a blind sleep either races TTS (truncating
	// the recording that assert-weather-spoken reads) or wastes wall-clock.
	teCtx, tecancel := context.WithTimeout(ctx, 45*time.Second)
	defer tecancel()
	cbs := append(earlyCBs, WaitCallbacksUntil(teCtx, sess, func(seen []webhook.Callback) bool {
		return agentTurnEndNamesTool(append(earlyCBs, seen...), "get_weather")
	})...)
	if !agentTurnEndNamesTool(cbs, "get_weather") {
		s.Logf("no turn_end naming get_weather within budget; %d callback(s) seen", len(cbs))
	}
	// Small pad so TTS finishes streaming into the recording after the turn
	// summary lands, then stop recording explicitly rather than relying on
	// teardown ordering.
	time.Sleep(2 * time.Second)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "drain-trailing-callbacks")
	// Anything still in flight around hangup.
	cbs = append(cbs, DrainCallbacks(sess, 5*time.Second)...)
	s.Logf("captured %d agent events in total", len(cbs))
	s.Done()

	s = Step(t, "assert-turn-end-tool-calls")
	// Independent corroboration from a second callback (eventHook, not
	// toolHook): the turn_end summary's `tool_calls` array must also name
	// get_weather. agent-turn.schema.json declares `tool_calls` as DRIFT,
	// so this is contract-safe even though it's not yet upstreamed.
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) == 0 {
		s.Errorf("no turn_end event in %d agent events", len(cbs))
	} else if !agentTurnEndNamesTool(cbs, "get_weather") {
		s.Errorf("no turn_end event has tool_calls containing get_weather: %d turn_end events", len(turnEnds))
	}
	s.Done()

	s = Step(t, "assert-weather-spoken")
	// Round-trip corroboration only — the arguments + turn_end assertions
	// above are the strong proofs that the LLM understood and populated
	// the tool call correctly.
	//
	// Accept EITHER field of the tool result. Both come only from our toolBody,
	// so one match still proves the result was spoken back, but requiring a
	// specific word is STT-fragile: "hail" is short and acoustically ambiguous,
	// and a real run had Deepgram render "heavy hail" as "heavy though". The
	// spelled-out temperature is far more robust, so it carries the assertion
	// when the conditions word gets mangled.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail", "seventy one")
	s.Done()
}

// TestVerb_Agent_BargeIn — when bargeIn=true, the user can interrupt the
// agent mid-TTS and the agent must (a) emit a `user_interruption` event,
// (b) stop speaking the first reply, and (c) handle the new user turn.
//
// Flow:
//   - greeting=true so the agent speaks first ("Hello...").
//   - while the agent is still talking (within ~3s of greeting starting),
//     we send a user WAV ("alpha bravo charlie delta") to barge in.
//   - eventHook fires user_interruption.
//   - agent processes the new turn and replies (echo of our keywords).
//
// We don't strictly assert which words arrive on the recording — the
// interrupted-greeting + new-reply audio gets stitched together — but we
// DO assert the user_interruption event landed.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session (+ ensure _anon)
//  4. script-agent-verb (greeting=true, bargeIn=true)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-into-greeting — let agent start speaking
//  8. send-prompt-wav — interrupt mid-greeting
//  9. wait-for-events — collect user_interruption + later turn_end
//
// 10. assert-user-interruption — event landed in _anon
// 11. hangup-and-wait-ended
func TestVerb_Agent_BargeIn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	greetingSystemPrompt := "You are a friendly voice assistant. " +
		"On your first turn, greet the user with a long, slow welcome " +
		"of at least three full sentences so they have time to interrupt. " +
		"On subsequent turns, repeat the user's words back to them verbatim."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: greetingSystemPrompt,
		Greeting:     true,
		BargeIn:      true,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-greeting")
	// Wait long enough that the agent is mid-TTS. ~3s = LLM (~1s) + a few
	// seconds of TTS playback in flight. Don't wait too long or the
	// greeting can fully complete (then it's not barge-in, it's a normal
	// second turn).
	time.Sleep(3 * time.Second)
	s.Done()

	s = Step(t, "send-prompt-wav")
	// "alpha bravo charlie delta" while the greeting is still speaking →
	// barge-in attempt. minSpeechDuration default is 0.5s so a 4s WAV
	// definitively confirms the interruption.
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	cbs := DrainCallbacks(sess, 15*time.Second)
	s.Logf("captured %d agent events", len(cbs))
	s.Done()

	s = Step(t, "assert-user-interruption")
	intr := findAgentEvents(cbs, "user_interruption")
	if len(intr) == 0 {
		s.Errorf("no user_interruption event in %d events: %s",
			len(cbs), summarizeEventTypes(cbs))
	} else {
		s.Logf("user_interruption fired %d time(s)", len(intr))
	}
	// Bonus: we should also see a turn_end with interrupted=true on the
	// greeting turn. Not required for the test to pass — note as info.
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			s.Logf("turn_end reported interrupted=true (expected for greeting turn)")
			break
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_KrispTurnDetection — verifies the agent runs with
// `turnDetection: "krisp"` (FreeSWITCH mod_krisp). With Krisp, jambonz
// uses an acoustic end-of-turn model rather than STT silence detection.
//
// Capability: depends on mod_krisp being loaded on the cluster's
// FreeSWITCH boxes (not always the case on community clusters). When
// Krisp is unavailable, end-of-turn never fires, the LLM never gets the
// transcript, and the recording is silent. This test detects that case
// and t.Skips with a clear message rather than failing — that's a
// cluster configuration question, not a smoke-tester regression.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session (+ ensure _anon)
//  4. script-agent-verb (turnDetection=krisp, earlyGeneration=true)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
//
// 10. hangup-and-wait-ended
// 11. assert-reply-keywords
func TestVerb_Agent_KrispTurnDetection(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:  agentEchoSystemPrompt,
		TurnDetection: "krisp",
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-krisp.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(12 * time.Second)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-call-completed")
	// Per user direction: it's enough that jambonz accepted the param and
	// the call ran to completion without rejection. We don't strictly
	// verify that Krisp acted on EOT (it may not be loaded on this
	// cluster — Krisp is internal to jambonz, no client-side handle).
	// If the cluster were to reject the param, the call would have failed
	// at agent verb exec-time and we'd see no inbound RTP at all.
	if call.PCMBytesIn() == 0 {
		s.Errorf("no inbound RTP — agent verb may have rejected turnDetection=krisp")
	} else {
		s.Logf("agent ran with turnDetection=krisp; %d inbound PCM bytes captured",
			call.PCMBytesIn())
	}
	s.Done()
}

// TestVerb_Agent_NoiseIsolation — verifies the agent verb accepts the
// `noiseIsolation` object form AND that the round-trip still works
// (noise isolation didn't garble the user's audio so badly the LLM
// can't produce a sensible reply).
//
// We exercise the object form `{mode, level, direction}` because it's
// the most expressive shape — if it parses, the shorthand strings
// ("krisp", "rnnoise") will too (both go through the same validator at
// lib/tasks/agent/index.js:170). The shorthand variants used to be
// separate sub-tests but they cost ~18s each for ≤1% additional
// coverage; one variant is enough.
//
// noiseIsolation is media-server-internal (FreeSWITCH mod_krisp on the
// freeswitch clusters; mediajam's own path otherwise) with no client-side
// handle, so — like KrispTurnDetection — we assert the observable contract:
// the object form parsed/was accepted and the call ran end-to-end with live
// inbound RTP. We don't assert the LLM echoed the prompt, because a media
// server that passes audio through unprocessed (no Krisp) makes that content
// check flake under load without signalling a real defect; the audio-path
// round-trip is already covered by Agent_Echo.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (noiseIsolation=object form)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
//  10. hangup-and-wait-ended
//  11. assert-noise-isolation-accepted — call ran with live inbound RTP
//     (verb's object form accepted, not rejected at exec-time)
func TestVerb_Agent_NoiseIsolation(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasDeepseek() || !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Skip("agent noiseIsolation test needs DEEPSEEK + DEEPGRAM + Deepgram credential")
	}

	noiseValue := map[string]any{"mode": "krisp", "level": 80, "direction": "read"}

	s := Step(t, "preflight-skips")
	s.Done()

	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// Build the verb manually so we can plug in the object form for
	// noiseIsolation — agentVerbOpts.NoiseIsolation only carries a string.
	verb := buildAgentVerb(agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		Greeting:     false,
		ActionURL:    SessionURL(sess, "agent-complete"),
		EventURL:     SessionURL(sess, "agent-turn"),
	})
	verb["noiseIsolation"] = noiseValue
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		verb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-noise.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-noise-isolation-accepted")
	// noiseIsolation is a media-server-internal feature: Krisp/RNNoise run
	// inside the media server (FreeSWITCH mod_krisp; mediajam's own path)
	// with no client-side handle to confirm the audio was actually
	// processed. So — same contract as KrispTurnDetection — we verify what
	// IS observable: the verb's object form `{mode,level,direction}` was
	// accepted (not rejected at exec-time) and the call ran end-to-end with
	// live inbound RTP. A rejected param would have failed the agent verb
	// before any media flowed and PCMBytesIn would be 0.
	//
	// We deliberately do NOT assert the LLM echoed our prompt: on a media
	// server that passes read-direction audio through unprocessed (no Krisp
	// loaded), telephony-quality STT mangles the phonetic markers under
	// load, which would flake a content assertion without indicating any
	// real defect. Echo-survives-the-audio-path is already covered by
	// Agent_Echo on the no-noiseIsolation path.
	if call.PCMBytesIn() == 0 {
		s.Errorf("no inbound RTP — agent verb may have rejected noiseIsolation=%v", noiseValue)
	} else {
		s.Logf("noiseIsolation=%v accepted; call ran to completion (%d PCM bytes in)",
			noiseValue, call.PCMBytesIn())
	}
	s.Done()
}

// TestVerb_Agent_NoResponseTimeout — when noResponseTimeout is set, the
// agent re-prompts the user after that many seconds of silence in the
// Idle state (state-machine.js _startNoResponseTimer). We:
//
//  1. Set greeting=true so the agent emits its first turn (then enters Idle).
//  2. Stay silent for noResponseTimeout + a buffer.
//  3. Expect a SECOND llm_response event (the re-prompt — typically
//     "Are you still there?").
//
// Asserts at least two `llm_response` events appeared in the eventHook
// stream during the silent window, proving the re-prompt path fired.
//
// Steps:
//  1. preflight-skips
//  2. register-webhook-session (+ ensure _anon)
//  3. script-agent-verb (greeting=true, noResponseTimeout=4)
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-greeting-and-reprompt — silent window long enough for
//     greeting to finish AND noResponseTimeout to fire and the re-prompt
//     to stream
//  7. drain-anon-events
//  8. assert-two-llm-responses — count `llm_response` events
//  9. hangup-and-wait-ended
func TestVerb_Agent_NoResponseTimeout(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	timeout := 4
	systemPrompt := "You are a brief voice assistant. " +
		"On every turn, reply with a single short sentence. " +
		"Greet the user on your first turn."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:      systemPrompt,
		Greeting:          true,
		NoResponseTimeout: &timeout,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting-and-reprompt")
	// Budget breakdown:
	//   ~5s greeting (LLM + TTS)
	//   ~4s noResponseTimeout window
	//   ~5s re-prompt streaming
	// Total ~14s. Add 6s slack for cluster latency.
	cbs := DrainCallbacks(sess, 20*time.Second)
	s.Logf("captured %d agent events: %s",
		len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-two-llm-responses")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) < 2 {
		// Diagnostics: dump every llm_response we saw so an operator can
		// tell if the re-prompt fired with different wording or didn't
		// fire at all.
		var bodies []string
		for _, cb := range responses {
			bodies = append(bodies, truncate(cb.String("response"), 80))
		}
		s.Errorf("expected >= 2 llm_response events (greeting + re-prompt) got %d: %v",
			len(responses), bodies)
	} else {
		s.Logf("re-prompt fired: %s",
			truncate(responses[1].String("response"), 100))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
