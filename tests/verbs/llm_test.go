// Tests for the `llm` verb — a real-time voice conversation between the
// caller and a large language model.
//
// Schema: schemas/verbs/llm — vendor + llmOptions are required; vendor-
// specific shape is delegated. We exercise the Deepgram Voice Agent path
// (vendor=deepgram, model=voice-agent), which bundles STT, LLM (think),
// and TTS (speak) inside the verb's own Settings — there's no separate
// recognizer/synthesizer block at the verb level. Auth is supplied inline
// via auth.apiKey, bypassing /LlmCredentials provisioning (same pattern
// the agent verb test uses for Deepseek).
//
// Why Deepgram Voice Agent: it's the only LLM vendor we already have
// credentials for in the suite (DEEPGRAM_API_KEY is required at TestMain
// for STT-of-recording verification AND for SpeechCredential provisioning).
// Other LLM vendors (openai_s2s, ultravox_s2s, elevenlabs_s2s, google_s2s)
// remain skipped in ai_skips_test.go until a per-vendor key is available.
//
// Test pattern (multi-turn echo round-trip):
//   - System prompt forces the agent to repeat the user's words verbatim.
//   - Per turn: pre-generate the user prompt WAV with Deepgram TTS, start
//     a fresh recording, send the WAV, wait for the agent's TTS reply,
//     stop the recording, transcribe the recording with Deepgram STT, and
//     assert the transcript contains every keyword from the spoken
//     prompt.
//   - Two turns with disjoint keyword sets (alpha/bravo/charlie/delta vs
//     echo/foxtrot/golf/hotel) prove BOTH turns round-tripped — a
//     single-turn echo could be faked by the agent repeating a fixed
//     phrase, but it cannot fake echoing different words on each turn.
//
// Why STT-of-recording (not the agent's own ConversationText event):
// asserting against the agent's STT view of what the user said is
// circular for an echo-bot test — if the agent's STT mishears, both
// sides match and the test passes on a broken round-trip. Recording the
// audio coming back over the SIP path and transcribing it with an
// independent STT pass proves the full loop: caller TTS → cluster STT →
// LLM → cluster TTS → caller-side recording.
//
// Webhook routing:
//   - actionHook → /action/llm — fires once when the LLM session ends.
//     Payload is the standard verb:hook envelope (callInfo + results),
//     so call_sid + customerData.x_test_id round-trip and the callback
//     correlates back to this test's session. Schema:
//     schemas/callbacks/llm.schema.json (validated automatically).
//   - eventHook  → /action/llm-event with `?X-Test-Id=<id>` — fires once
//     per Voice Agent server event (Welcome, ConversationText, etc.).
//     The payload itself has no callInfo (see lib/tasks/llm/index.js
//     sendEventHook), so we MUST attach the test id via query param so
//     the webhook server's correlation layer routes it to this session
//     instead of the shared `_anon` bag.
//
// Skips cleanly when:
//   - DEEPGRAM_API_KEY is unset (configured-as-required at TestMain so
//     this should never fire under normal runs; the guard is here for
//     resilience when the key is later removed).
package verbs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// llmEchoSystemPrompt is the natural-language instruction that turns the
// LLM into an echo bot. Plain prose — the model must INFER from context
// that "echo back" means to speak back the user's exact words.
const llmEchoSystemPrompt = "Echo back whatever you hear from the user."

// llmEchoTurns is the script the UA speaks. Each turn is a natural,
// conversational sentence — not phonetic-alphabet keywords. Two distinct
// sentences prove BOTH turns round-tripped on context: a fixed-phrase
// fake cannot echo different sentences on each turn.
var llmEchoTurns = []struct {
	prompt string
}{
	{prompt: "Hello, my name is John and I am calling from the office."},
	{prompt: "Can you please tell me what time it is in New York today?"},
}

// TestVerb_LLM_Deepgram drives a two-turn voice conversation against the
// Deepgram Voice Agent. On each turn the UA speaks four distinct
// keywords and the agent (system-prompted as an echo bot) must speak
// them back. We record the agent's reply audio per turn, transcribe it
// with Deepgram STT, and assert every keyword the UA spoke shows up in
// the recording transcript — an independent verification of the full
// round-trip (caller TTS → cluster STT → LLM → cluster TTS → caller
// recording → independent STT → assertion).
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs (one per turn, cached on disk)
//  3. register-webhook-session
//  4. script-llm-verb (deepgram voice-agent + echo system prompt)
//  5. place-call
//  6. answer-and-silence
//  7. wait-for-stt — let the Voice Agent's STT arm
//  8. turn-N-record-and-speak (per turn): start recording, send prompt
//     WAV, wait for agent reply, stop recording
//  9. turn-N-assert-echo (per turn): STT the recording, assert keywords
// 10. hangup-and-wait-ended
// 11. drain-callbacks
// 12. assert-action-payload — completion_reason + call_sid present
// 13. assert-event-types — Welcome event landed (event-stream wiring)
func TestVerb_LLM_Deepgram(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("llm test needs DEEPGRAM_API_KEY (used inline as the Voice Agent api key, and to pre-gen the prompt WAVs + STT the recording)")
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	// Pre-generate one WAV per turn. EnsureWAV caches the result on disk
	// keyed by the prompt text + voice, so re-runs are free.
	wavs := make([]string, len(llmEchoTurns))
	for i, turn := range llmEchoTurns {
		s = Step(t, "ensure-prompt-wav")
		path, err := tts.EnsureWAV(ctx, "testdata/llm", turn.prompt, tts.PromptOptions{
			Model: "aura-asteria-en",
		})
		if err != nil {
			s.Fatalf("EnsureWAV turn %d: %v", i+1, err)
		}
		s.Logf("turn %d prompt wav: %s", i+1, path)
		wavs[i] = path
		s.Done()
	}

	testID, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	llmVerb := V("llm",
		"vendor", "deepgram",
		"model", "voice-agent",
		"auth", map[string]any{
			"apiKey": cfg.DeepgramAPIKey,
		},
		// /action/llm has a schema (schemas/callbacks/llm.schema.json) so the
		// completion payload gets contract-validated on arrival.
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// /action/llm-event is intentionally schema-less — Voice Agent server
		// events don't have a unified payload shape. Carry X-Test-Id as a
		// query param so per-event hooks correlate back here (sendEventHook
		// strips callInfo).
		"eventHook", SessionURL(sess, "llm-event"),
		// Subscribe to all server events so Welcome reaches us (used for
		// the event-stream wiring smoke check).
		"events", []string{"all"},
		"llmOptions", map[string]any{
			"Settings": map[string]any{
				"type": "Settings",
				"agent": map[string]any{
					// No greeting: we want the user to speak first so the
					// LLM's reply is unambiguously a response to our prompt.
					"listen": map[string]any{
						"provider": map[string]any{
							"type":  "deepgram",
							"model": "nova-2",
						},
					},
					"think": map[string]any{
						"provider": map[string]any{
							"type":  "open_ai",
							"model": "gpt-4o-mini",
						},
						"prompt": llmEchoSystemPrompt,
					},
					"speak": map[string]any{
						"provider": map[string]any{
							"type":  "deepgram",
							"model": "aura-2-thalia-en",
						},
					},
				},
			},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	// /action/llm is the test-owned actionHook; ack with empty so jambonz
	// doesn't try to chain follow-up verbs after the LLM session ends.
	SessionAckEmpty(sess, "llm", "llm-event")
	s.Done()

	s = Step(t, "place-call")
	// Budget: warmup (1s) + 2 turns * (~2s prompt + 12s reply) + drain. 90s
	// leaves comfortable headroom; the LLM verb's awaitTaskDone keeps the
	// session alive across both turns.
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

	// Per-turn loop: record the agent's reply audio, send the prompt, wait
	// for the reply, stop recording, transcribe, assert. Each turn gets
	// its own recording file so a wrong reply can't bleed into the next
	// turn's assertion.
	for i, turn := range llmEchoTurns {
		// One recording file per turn so a wrong reply on turn N can't
		// bleed into turn N+1's assertion.
		recPath := filepath.Join(t.TempDir(), formatTurnRecPath(i+1))

		s = Step(t, formatTurnStep(i+1, "record-and-speak"))
		if err := call.StartRecording(recPath); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		// Brief silence so the recording opens before our prompt audio
		// arrives — otherwise the first ~100ms of the prompt's reply can
		// land before the file is being written.
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (pre): %v", err)
		}
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		// Trail with silence so the agent's STT detects end-of-utterance
		// and the LLM is triggered to reply.
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		// Give the LLM time to reply and the agent's TTS to stream the
		// echo back into our recording.
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, formatTurnStep(i+1, "assert-echo"))
		// Sentence-level echo check: every content word from the prompt
		// the UA spoke must appear in the recording transcript.
		// AssertTranscriptContains uses Deepgram STT on the recording
		// (independent of the agent's own STT), so this proves the agent
		// spoke the prompt sentence back over the SIP path — not just
		// that its internal ConversationText said so.
		//
		// We split the prompt into words and feed each as a `wants`
		// substring rather than asserting on the whole sentence: STT
		// normalizes punctuation/casing inconsistently across releases,
		// so word-level containment is the robust granularity. Filler
		// words (a/the/is/...) are dropped because they're optional in
		// natural speech and STT occasionally elides them.
		// At least 2 content words must appear; strict-Contains is too
		// brittle for telephony-quality TTS-of-LLM-reply round-trips.
		words := contentWords(turn.prompt)
		AssertTranscriptHasMost(s, ctx, recPath, 2, words...)
		s.Done()
	}

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "drain-callbacks")
	// Drain everything in one pass — `WaitCallbackFor` would skip-and-
	// discard the eventHook traffic we need for the Welcome smoke check.
	// 5s is enough for the action/llm hook to land after BYE on a healthy
	// cluster.
	cbs := DrainCallbacks(sess, 5*time.Second)
	s.Logf("captured %d hook callbacks", len(cbs))
	s.Done()

	s = Step(t, "assert-action-payload")
	var actionCB *webhook.Callback
	for i := range cbs {
		if cbs[i].Hook == "action/llm" {
			actionCB = &cbs[i]
			break
		}
	}
	if actionCB == nil {
		s.Fatalf("no action/llm callback in %d drained callbacks", len(cbs))
	}
	s.Logf("action/llm body: %s", string(actionCB.Body))
	if got := actionCB.String("call_sid"); got == "" {
		s.Errorf("action/llm payload missing call_sid: %s", string(actionCB.Body))
	} else {
		s.Logf("call_sid in payload: %s", got)
	}
	if got := actionCB.String("completion_reason"); got == "" {
		s.Errorf("action/llm payload missing completion_reason: %s", string(actionCB.Body))
	} else {
		s.Logf("completion_reason: %s", got)
	}
	if id := actionCB.NestedString("customer_data.x_test_id"); id != "" && id != testID {
		s.Errorf("customer_data.x_test_id=%q want %q", id, testID)
	}
	s.Done()

	s = Step(t, "assert-event-types")
	// Smoke check that the eventHook stream worked end-to-end. Welcome is
	// the first event the Voice Agent emits on every successful session;
	// its absence means the per-event hook plumbing broke (X-Test-Id
	// correlation, sendEventHook, etc.) — orthogonal to the conversation
	// itself but worth a one-liner assertion.
	var events []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == "action/llm-event" {
			events = append(events, cb)
		}
	}
	if len(events) == 0 {
		s.Errorf("no eventHook callbacks captured — Voice Agent server events did not stream back")
	} else {
		var types []string
		sawWelcome := false
		for _, cb := range events {
			ty := cb.String("type")
			types = append(types, ty)
			if ty == "Welcome" {
				sawWelcome = true
			}
		}
		s.Logf("event types: %s", strings.Join(types, ","))
		if !sawWelcome {
			s.Errorf("no Welcome event among %d events: %v", len(events), types)
		}
	}
	s.Done()
}

// llmWeatherUserPrompt is what the UA speaks to trigger the tool call. The
// city name ("Chicago") is the argument value the toolHook responder later
// asserts on.
const llmWeatherUserPrompt = "What is the weather in Chicago right now?"

// llmWeatherSystemPrompt instructs the Voice Agent's think stage to call
// the declared get_weather function whenever the user asks about weather,
// then relay the function's result back to the caller.
const llmWeatherSystemPrompt = "You are a weather assistant. When the user asks about the weather in a city, call the get_weather function with that city as the location argument. After you receive the function's result, tell the user the weather conditions it returned."

// llmWeatherToolResult is the canned answer the test's toolHook responder
// returns for every get_weather call. "hail" is the distinctive word the
// final transcript assertion looks for — it can only appear in the agent's
// spoken reply if the FunctionCallResponse envelope round-tripped back
// through the Voice Agent and was spoken by the speak stage.
const llmWeatherToolResult = "The weather is heavy hail with a temperature of seventy one degrees."

// TestVerb_LLM_Deepgram_ToolHook proves the `llm` verb's Deepgram Voice
// Agent path supports app-declared LLM tool/function calling end-to-end:
// the agent's think stage calls a declared function (get_weather) with an
// argument parsed from the caller's speech, the test's toolHook responds
// with the vendor-native FunctionCallResponse envelope (echoing the live
// tool_call_id, which is only known at call time), and the agent speaks
// the tool's result back to the caller.
//
// Unlike TestVerb_LLM_Deepgram (an echo round-trip via the actionHook),
// this test exercises the verb-level toolHook and the Settings.agent.
// think.functions declaration — see feature-server
// lib/tasks/llm/llms/voice_agent_s2s.js for the vendor contract: the
// function must be declared under Settings.agent.think.functions (not a
// top-level tools array), the toolHook request carries the live
// tool_call_id + name + parsed args (field name "args", not "arguments"),
// and the response must be the vendor-native envelope, not a jambonz verb
// array.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-llm-verb — declares get_weather under think.functions and
//     wires a verb-level toolHook to a dynamic BodyFunc responder
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt — let the Voice Agent's STT arm
//  7. record-and-speak — start recording, send the weather question, pad
//     with silence so STT finalizes the utterance
//  8. wait-for-tool-call — WaitCallbackFor(action/llm-tool); assert
//     name == "get_weather" and tool_call_id is non-empty
//  9. assert-tool-args — assert args.location contains "chicago"
// 10. wait-for-reply-and-stop — let the agent speak the tool result back,
//     then stop recording
// 11. hangup-and-wait-ended
// 12. assert-tool-result-spoken — independently STT the recording and
//     assert it contains the tool result's distinctive word ("hail"),
//     proving the FunctionCallResponse envelope round-tripped
func TestVerb_LLM_Deepgram_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("llm tool-hook test needs DEEPGRAM_API_KEY (used inline as the Voice Agent api key, and to pre-gen the prompt WAV + STT the recording)")
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", llmWeatherUserPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Logf("prompt wav: %s", promptWAV)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	llmVerb := V("llm",
		"vendor", "deepgram",
		"model", "voice-agent",
		"auth", map[string]any{
			"apiKey": cfg.DeepgramAPIKey,
		},
		// /action/llm has a schema (schemas/callbacks/llm.schema.json) so the
		// completion payload gets contract-validated on arrival.
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// toolHook payloads carry no callInfo (see
		// lib/tasks/llm/llms/voice_agent_s2s.js), so X-Test-Id MUST ride the
		// query param for the webhook server's correlation layer to route
		// the request to this session instead of the shared `_anon` bag.
		"toolHook", SessionURL(sess, "llm-tool"),
		// Deliberately no eventHook here: keeping the callback stream to
		// just action/llm + action/llm-tool avoids racing
		// WaitCallbackFor("action/llm-tool") against event traffic.
		"llmOptions", map[string]any{
			"Settings": map[string]any{
				"type": "Settings",
				"agent": map[string]any{
					// No greeting: the user speaks first so the function
					// call is unambiguously triggered by our prompt.
					"listen": map[string]any{
						"provider": map[string]any{
							"type":  "deepgram",
							"model": "nova-2",
						},
					},
					"think": map[string]any{
						"provider": map[string]any{
							"type":  "open_ai",
							"model": "gpt-4o-mini",
						},
						"prompt": llmWeatherSystemPrompt,
						// Function declared under Settings.agent.think.functions
						// per voice_agent_s2s.js — NOT a top-level "tools" array
						// (that's the agent verb's shape, not this verb's).
						"functions": []map[string]any{
							{
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
							},
						},
					},
					"speak": map[string]any{
						"provider": map[string]any{
							"type":  "deepgram",
							"model": "aura-2-thalia-en",
						},
					},
				},
			},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	// /action/llm is the test-owned actionHook; ack with empty so jambonz
	// doesn't try to chain follow-up verbs after the LLM session ends.
	SessionAckEmpty(sess, "llm")
	// The toolHook response must echo the LIVE tool_call_id jambonz just
	// sent — a static body cannot work because the id is only known at
	// call time. This closure runs on the webhook server goroutine, so it
	// must stay pure: no t/s/StepCtx/assertions inside it.
	sess.ScriptActionHookBodyFunc("llm-tool", func(cb webhook.Callback) []byte {
		id := cb.String("tool_call_id")
		name := cb.String("name")
		resp := map[string]any{
			"type":    "FunctionCallResponse",
			"id":      id,
			"name":    name,
			"content": llmWeatherToolResult,
		}
		b, _ := json.Marshal(resp)
		return b
	})
	s.Done()

	s = Step(t, "place-call")
	// Budget: warmup (1s) + prompt (~2s) + tool round-trip + reply (~15s) +
	// drain. 90s mirrors TestVerb_LLM_Deepgram's headroom.
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

	recPath := filepath.Join(t.TempDir(), "llm-tool-reply.pcm")

	s = Step(t, "record-and-speak")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	// Brief silence so the recording opens before the reply audio arrives.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	// Trail with silence so the agent's STT detects end-of-utterance and
	// triggers the function call.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-tool): %v", err)
	}
	s.Logf("action/llm-tool body: %s", string(toolCB.Body))
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool call name=%q want %q; body=%s", got, "get_weather", string(toolCB.Body))
	}
	toolCallID := toolCB.String("tool_call_id")
	if toolCallID == "" {
		s.Errorf("tool call missing tool_call_id; body=%s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-args")
	// The llm verb sends parsed function arguments under "args", NOT
	// "arguments" (unlike the agent verb) — see voice_agent_s2s.js.
	location := strings.ToLower(toolCB.NestedString("args.location"))
	if !strings.Contains(location, "chicago") {
		s.Errorf("args.location=%q does not contain %q; body=%s", location, "chicago", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	// Give the agent time to speak the FunctionCallResponse content back
	// before we stop capturing.
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-tool-result-spoken")
	// "hail" only appears in the agent's spoken reply if our
	// FunctionCallResponse envelope round-tripped through the Voice Agent
	// and was relayed to the caller — proving the full tool-call loop.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}

// formatTurnStep builds a kebab-cased step name with the turn number, e.g.
// "turn-1-record-and-speak". Centralised so the bullet list at the top of
// the test stays in sync with the actual step names emitted at runtime.
func formatTurnStep(n int, suffix string) string {
	return "turn-" + strconv.Itoa(n) + "-" + suffix
}

// formatTurnRecPath builds a unique recording filename per turn, e.g.
// "llm-turn-1-reply.pcm".
func formatTurnRecPath(n int) string {
	return "llm-turn-" + strconv.Itoa(n) + "-reply.pcm"
}

// contentWords splits a natural-language prompt into the words an echo
// assertion should require in the recording transcript. Punctuation is
// stripped and a small stop-word list is removed because (a) Deepgram
// STT normalizes punctuation inconsistently across releases and (b) the
// LLM may legitimately rephrase fillers ("Hello, how are you?" →
// "hello how are you" or "hello how're you") without breaking the echo
// contract. What's left is the sentence's content words — substantive
// enough that a wrong/empty/hallucinated reply still fails.
func contentWords(prompt string) []string {
	stop := map[string]bool{
		"a": true, "an": true, "the": true,
		"is": true, "are": true, "was": true, "were": true, "be": true,
		"to": true, "of": true, "in": true, "on": true, "at": true,
		"do": true, "does": true, "did": true,
	}
	var out []string
	for _, raw := range strings.Fields(prompt) {
		w := strings.ToLower(strings.TrimFunc(raw, func(r rune) bool {
			return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '\'')
		}))
		if w == "" || stop[w] {
			continue
		}
		out = append(out, w)
	}
	return out
}
