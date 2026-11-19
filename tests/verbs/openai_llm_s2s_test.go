// llm verb, vendor openai: the gpt-realtime-2.1 models still call the tool and speak its output.
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

func openAIRealtimeToolVerb(model, toolHook string) map[string]any {
	verb := V("llm",
		"vendor", "openai",
		"auth", map[string]any{"apiKey": cfg.OpenAIAPIKey},
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"toolHook", toolHook,
		"llmOptions", map[string]any{
			"session_update": map[string]any{
				"type":         "realtime",
				"instructions": llmWeatherSystemPrompt + " Always answer in English.",
				"audio": map[string]any{
					"input": map[string]any{
						"turn_detection": map[string]any{"type": "server_vad"},
					},
					"output": map[string]any{"voice": "marin"},
				},
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
				"tool_choice": "auto",
			},
			"response_create": map[string]any{},
		},
	)
	if model != "" {
		verb["model"] = model
	}
	return verb
}

func TestVerb_LLM_OpenAI_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasOpenAI() {
		t.Log("OPENAI_API_KEY not set — passing without exercising openai S2S tool calling")
		return
	}
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Log("Deepgram not available — passing without exercising openai S2S tool calling")
		return
	}

	for _, tc := range []struct{ name, model string }{
		{"default", ""},
		{"mini", "gpt-realtime-2.1-mini"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithTimeout(t, 180*time.Second)
			uas := claimUAS(t, ctx)

			s := Step(t, "ensure-prompt-wav")
			promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", llmWeatherUserPrompt, tts.PromptOptions{
				Model: "aura-asteria-en",
			})
			if err != nil {
				s.Fatalf("EnsureWAV: %v", err)
			}
			s.Done()

			_, sess := claimSession(t)

			s = Step(t, "script-llm-verb")
			// toolHook payloads carry no callInfo, so X-Test-Id rides the query param.
			sess.ScriptCallHook(WithWarmupScript(webhook.Script{
				openAIRealtimeToolVerb(tc.model, SessionURL(sess, "llm-openai-tool")),
				V("hangup"),
			}))
			SessionAckEmpty(sess, "llm")
			// Must echo the live tool_call_id; output must be a string or OpenAI
			// answers with an invalid_type error and the session ends.
			sess.ScriptActionHookBodyFunc("llm-openai-tool", func(cb webhook.Callback) []byte {
				b, _ := json.Marshal(map[string]any{
					"type": "conversation.item.create",
					"item": map[string]any{
						"type":    "function_call_output",
						"call_id": cb.String("tool_call_id"),
						"output":  llmWeatherToolResult,
					},
				})
				return b
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

			// Let the session connect and the model finish its greeting first.
			WaitFor(t, "let-model-greet", RecognizerArmDelay)

			recPath := filepath.Join(t.TempDir(), "llm-openai-tool-reply.pcm")

			s = Step(t, "record-and-speak")
			if err := call.StartRecording(recPath); err != nil {
				s.Fatalf("StartRecording: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence (pre): %v", err)
			}
			if err := call.SendWAV(promptWAV); err != nil {
				s.Fatalf("SendWAV: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence (post): %v", err)
			}
			s.Done()

			s = Step(t, "wait-for-tool-call")
			toolCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
			toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-openai-tool")
			cancel()
			if err != nil {
				s.Fatalf("WaitCallbackFor(action/llm-openai-tool): %v — the model never called the tool", err)
			}
			s.Logf("action/llm-openai-tool body: %s", string(toolCB.Body))
			if got := toolCB.String("name"); got != "get_weather" {
				s.Errorf("tool call name=%q want %q", got, "get_weather")
			}
			if toolCB.String("tool_call_id") == "" {
				s.Errorf("tool call missing tool_call_id; body=%s", string(toolCB.Body))
			}
			s.Done()

			s = Step(t, "assert-tool-args")
			location := strings.ToLower(toolCB.NestedString("args.location"))
			if !strings.Contains(location, "chicago") {
				s.Errorf("args.location=%q does not contain %q", location, "chicago")
			}
			s.Done()

			s = Step(t, "wait-for-reply-and-stop")
			time.Sleep(LLMReplyWindow)
			call.StopRecording()
			s.Done()

			HangupAndWaitEnded(t, ctx, call)

			s = Step(t, "assert-tool-result-spoken")
			// "hail" is only in the tool output: hearing it proves the output reached
			// the model and the model relayed it, rather than claiming a result.
			AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
			s.Done()
		})
	}
}
