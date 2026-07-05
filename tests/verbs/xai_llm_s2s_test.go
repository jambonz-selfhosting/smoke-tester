// Tests for the `llm` verb with LLM vendor "xai" (xAI Voice Agent, realtime
// S2S) — app-declared tool/function calling end-to-end.
//
// xai is an OPTIONAL vendor (see config.HasXai in internal/config/config.go).
// When XAI_API_KEY is unset the test passes immediately without exercising
// the xai realtime path — a plain `return` after a log, never t.Skip, never
// a failure, matching xai_agent_test.go / xai_stt_test.go.
//
// xAI's Voice Agent speaks the OpenAI-Realtime GA dialect (see
// handoff_test.go / builtin_hangup_test.go for the sibling openai-vendor
// llmOptions shape: llmOptions.session_update is the session body the
// feature-server wraps as {type:'session.update', session:<this>}, and
// llmOptions.response_create triggers the initial response.create). The
// tool-calling contract, however, mirrors the Deepgram Voice Agent test
// (TestVerb_LLM_Deepgram_ToolHook in llm_test.go) in STRUCTURE only — the
// vendor-native envelopes differ:
//   - Declaring the tool: xAI wants a flat OpenAI-style function tool under
//     session_update.tools (NOT Settings.agent.think.functions, which is
//     Deepgram-specific).
//   - toolHook request: {tool_call_id, name, args} — same field names as
//     Deepgram (args, not arguments; tool_call_id, not call_id).
//   - toolHook response: the OpenAI-Realtime envelope
//     {type:"conversation.item.create", item:{type:"function_call_output",
//     call_id:<echo the live tool_call_id>, output:<result>}} — NOT
//     Deepgram's {type:"FunctionCallResponse", id, name, content}. See
//     feature-server TaskLlmXai_S2S.processToolOutput, which requires
//     type === 'conversation.item.create' with a function_call_output item.
//
// Reuses the weather prompt/system-prompt/result consts from llm_test.go
// (llmWeatherUserPrompt / llmWeatherSystemPrompt / llmWeatherToolResult) —
// the scenario ("ask about the weather in Chicago, agent calls get_weather,
// speaks back the canned result") is vendor-agnostic.
//
// Steps:
//  1. preflight-skips — xai key guard (plain return), then deepgram guard
//     (plain return) for prompt-gen + reply STT infra
//  2. ensure-prompt-wav
//  3. script-llm-verb — xai vendor/model/auth/toolHook + session_update
//     tools declaration + dynamic toolHook responder
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt — let the Voice Agent's STT/VAD arm
//  7. record-and-speak — start recording, send the weather question, pad
//     with silence so server_vad finalizes the utterance
//  8. wait-for-tool-call — WaitCallbackFor(action/llm-xai-tool); assert
//     name == "get_weather" and tool_call_id is non-empty
//  9. assert-tool-args — assert args.location contains "chicago"
//  10. wait-for-reply-and-stop — let the agent speak the tool result back,
//     then stop recording
//  11. hangup-and-wait-ended
//  12. assert-tool-result-spoken — independently STT the recording and
//     assert it contains the tool result's distinctive word ("hail"),
//     proving the conversation.item.create/function_call_output envelope
//     round-tripped
package verbs

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// xaiVoiceAgentModel is the xAI realtime Voice Agent model, distinct from
// xaiLlmModel (verbsmain_test.go) which targets xAI's chat-completions
// endpoint for the cascaded `agent` verb test. The realtime S2S `llm` verb
// exercised here needs xAI's voice-specific model.
const xaiVoiceAgentModel = "grok-voice-latest"

// TestVerb_LLM_Xai_ToolHook proves the `llm` verb's xai (xAI Voice Agent,
// realtime S2S) path supports app-declared LLM tool/function calling
// end-to-end: the model calls a declared function (get_weather) with an
// argument parsed from the caller's speech, the test's toolHook responds
// with the vendor-native conversation.item.create/function_call_output
// envelope (echoing the live tool_call_id, which is only known at call
// time), and the agent speaks the tool's result back to the caller.
func TestVerb_LLM_Xai_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasXai() {
		t.Log("XAI_API_KEY not set — passing without exercising xai S2S tool calling")
		return
	}

	// Deepgram is only needed for prompt-WAV generation + independent STT of
	// the recorded reply — not for the xai realtime session itself. Missing
	// it is a pass-and-return, matching xai_agent_test.go's preflight (never
	// t.Skip / fail).
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising xai S2S tool calling")
		return
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
		"vendor", "xai",
		"model", xaiVoiceAgentModel,
		"auth", map[string]any{
			"apiKey": cfg.XaiAPIKey,
		},
		// /action/llm has a schema (schemas/callbacks/llm.schema.json) so the
		// completion payload gets contract-validated on arrival.
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// toolHook payloads carry no callInfo, so X-Test-Id MUST ride the
		// query param for the webhook server's correlation layer to route
		// the request to this session instead of the shared `_anon` bag.
		"toolHook", SessionURL(sess, "llm-xai-tool"),
		"llmOptions", map[string]any{
			// The feature-server wraps this as {type:'session.update',
			// session:<this>}. turn_detection lives at the TOP LEVEL of the
			// session body per xAI's placement (not nested under "audio").
			"session_update": map[string]any{
				"type":         "realtime",
				"instructions": llmWeatherSystemPrompt,
				"turn_detection": map[string]any{
					"type": "server_vad",
				},
				"voice": xaiVoice,
				// Flat OpenAI-style function tool declaration — NOT
				// Deepgram's Settings.agent.think.functions shape, and not
				// nested under session.audio.
				"tools": []map[string]any{
					{
						"type":        "function",
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
			"response_create": map[string]any{},
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
	// sent — a static body cannot work because the id is only known at call
	// time. This closure runs on the webhook server goroutine, so it must
	// stay pure: no t/s/StepCtx/assertions inside it.
	sess.ScriptActionHookBodyFunc("llm-xai-tool", func(cb webhook.Callback) []byte {
		id := cb.String("tool_call_id")
		resp := map[string]any{
			"type": "conversation.item.create",
			"item": map[string]any{
				"type":    "function_call_output",
				"call_id": id,
				"output":  llmWeatherToolResult,
			},
		}
		b, _ := json.Marshal(resp)
		return b
	})
	s.Done()

	s = Step(t, "place-call")
	// Budget: warmup (1s) + prompt (~2s) + tool round-trip + reply (~15s) +
	// drain. 90s mirrors TestVerb_LLM_Deepgram_ToolHook's headroom.
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

	recPath := filepath.Join(t.TempDir(), "llm-xai-tool-reply.pcm")

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
	// Trail with silence so server_vad detects end-of-utterance and
	// triggers the function call.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-xai-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-xai-tool): %v", err)
	}
	s.Logf("action/llm-xai-tool body: %s", string(toolCB.Body))
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
	// "arguments" — same field name as the Deepgram voice-agent path.
	location := strings.ToLower(toolCB.NestedString("args.location"))
	if !strings.Contains(location, "chicago") {
		s.Errorf("args.location=%q does not contain %q; body=%s", location, "chicago", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	// Give the agent time to speak the function_call_output content back
	// before we stop capturing.
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-tool-result-spoken")
	// "hail" only appears in the agent's spoken reply if our
	// conversation.item.create/function_call_output envelope round-tripped
	// through the Voice Agent and was relayed to the caller — proving the
	// full tool-call loop.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}
