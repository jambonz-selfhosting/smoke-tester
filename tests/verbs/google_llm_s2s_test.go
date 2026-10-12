// Tests for the `llm` verb with LLM vendor "google" (Gemini Live, realtime
// S2S) — app-declared tool/function calling end-to-end against Gemini 3.8
// Live, once per Google platform:
//   - TestVerb_LLM_Google_ToolHook: Gemini Developer API (AI Studio), with a
//     GEMINI_API_KEY ("AIza...") sent as ?key=, against the Extended Thinking
//     variant.
//   - TestVerb_LLM_Google_Vertex_ToolHook: Vertex AI, with an OAuth token
//     minted from the GEMINI_KEYFILE service account (Vertex refuses API keys)
//     and an app-supplied connectOptions host/path, since the verb's default
//     endpoint is the Developer API.
//
// Both are OPTIONAL: with their credential unset they pass immediately without
// exercising Gemini Live — a plain `return` after a log, never t.Skip, never a
// failure, matching xai_llm_s2s_test.go.
//
// Gemini speaks its own BidiGenerateContent dialect, so the envelopes differ
// from the OpenAI-Realtime vendors (xai/gptlive/voicelive):
//   - llmOptions.setup is the BidiGenerateContentSetup body, passed through
//     verbatim by feature-server's TaskLlmGoogle_S2S.
//   - Declaring the tool: setup.tools[].functionDeclarations[], with Google's
//     uppercase JSON-schema types (OBJECT/STRING). behavior NON_BLOCKING opts
//     the call into 3.8's background/async tool execution.
//   - toolHook request: {function_calls: [{id, name, args}]} — the verb forwards
//     Gemini's native array, snake_cased on the way out, so the tool id is under
//     function_calls.0.id. The flat tool_call_id that the OpenAI-dialect vendors
//     echo is present but carries the literal string "function_call_id".
//   - toolHook response: {toolResponse: {functionResponses: [{id, name,
//     response: {output}}]}} — TaskLlmGoogle_S2S.processToolOutput rejects
//     anything without a toolResponse key.
//
// Reuses the weather prompt/system-prompt/result consts from llm_test.go.
//
// Steps:
//  1. preflight-skips — gemini key guard (plain return), then deepgram guard
//     for prompt-gen + reply STT infra
//  2. ensure-prompt-wav
//  3. script-llm-verb — google vendor/model/auth/toolHook + setup with a
//     NON_BLOCKING functionDeclaration + dynamic toolHook responder
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt — let Gemini's VAD arm
//  7. record-and-speak — start recording, send the weather question, pad with
//     silence so the model finalizes the utterance
//  8. wait-for-tool-call — assert name == "get_weather" and a non-empty id
//  9. assert-tool-args — assert args.location contains "chicago"
//  10. wait-for-reply-and-stop
//  11. hangup-and-wait-ended
//  12. assert-tool-result-spoken — independently STT the recording and assert
//     it contains "hail", proving the toolResponse envelope round-tripped
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/oauth2/google"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// geminiLiveModel is the Gemini Live s2s model. The "models/" prefix is
// Gemini's own addressing scheme and is required on the Developer API.
// -extended-thinking is the reasoning variant; it emits interactionStatus
// IN_PROGRESS while it reasons in the background, then IDLE.
const geminiLiveModel = "models/gemini-3.8-live-extended-thinking"

// TestVerb_LLM_Google_ToolHook proves the `llm` verb's google (Gemini Live)
// path supports app-declared tool/function calling end-to-end: the model calls
// a declared NON_BLOCKING function (get_weather) with an argument parsed from
// the caller's speech, the test's toolHook responds with the vendor-native
// toolResponse/functionResponses envelope (echoing the live function-call id,
// which is only known at call time), and the agent speaks the tool's result
// back to the caller.
func TestVerb_LLM_Google_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGeminiS2S() {
		t.Log("GEMINI_API_KEY not set — passing without exercising Gemini Live S2S tool calling")
		return
	}
	if !geminiLiveDeepgramReady(t) {
		return
	}

	ctx := WithTimeout(t, 180*time.Second)
	runGeminiLiveToolHook(t, ctx, geminiLiveTarget{
		model: geminiLiveModel,
		auth:  map[string]any{"apiKey": cfg.GeminiAPIKey},
		// Background reasoning. MINIMAL is not supported by this model; LOW
		// keeps the turn latency test-friendly.
		generationConfig: map[string]any{
			"thinkingConfig": map[string]any{"thinkingLevel": "LOW"},
		},
	})
}

// Vertex AI target for the Gemini Live s2s test. Multi-region locations
// (us, eu) need the aiplatform.<loc>.rep.googleapis.com host instead.
const (
	vertexLiveLocation = "us-central1"
	vertexLiveHost     = "us-central1-aiplatform.googleapis.com"
	vertexLivePath     = "/ws/google.cloud.aiplatform.v1.LlmBidiService/BidiGenerateContent"
	// Plain 3.8 Live: it rejects thinkingConfig with close 1007.
	vertexLiveModel = "gemini-3.8-live"
)

// TestVerb_LLM_Google_Vertex_ToolHook is TestVerb_LLM_Google_ToolHook over
// Vertex AI: Bearer token auth, a regional host and the full publisher model
// path. Everything after the session opens is identical.
func TestVerb_LLM_Google_Vertex_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if cfg.GeminiServiceKey == "" {
		t.Log("GEMINI_KEYFILE not set — passing without exercising Gemini Live S2S on Vertex AI")
		return
	}
	if !geminiLiveDeepgramReady(t) {
		return
	}

	ctx := WithTimeout(t, 180*time.Second)

	s := Step(t, "mint-vertex-token")
	token, project, err := vertexAccessToken(ctx, cfg.GeminiServiceKey)
	if err != nil {
		s.Fatalf("vertexAccessToken: %v", err)
	}
	s.Done()

	runGeminiLiveToolHook(t, ctx, geminiLiveTarget{
		model: fmt.Sprintf("projects/%s/locations/%s/publishers/google/models/%s",
			project, vertexLiveLocation, vertexLiveModel),
		auth: map[string]any{"apiKey": token},
		connectOptions: map[string]any{
			"host": vertexLiveHost,
			"path": vertexLivePath,
		},
	})
}

// vertexAccessToken mints a short-lived OAuth token from a service-account
// key. The llm verb takes no service account, so the app supplies the token.
func vertexAccessToken(ctx context.Context, saJSON string) (token, project string, err error) {
	creds, err := google.CredentialsFromJSON(ctx, []byte(saJSON),
		"https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return "", "", err
	}
	if creds.ProjectID == "" {
		return "", "", fmt.Errorf("service account key has no project_id")
	}
	tok, err := creds.TokenSource.Token()
	if err != nil {
		return "", "", err
	}
	return tok.AccessToken, creds.ProjectID, nil
}

// geminiLiveDeepgramReady gates on Deepgram, which is only needed for
// prompt-WAV generation + independent STT of the recorded reply.
func geminiLiveDeepgramReady(t *testing.T) bool {
	s := Step(t, "preflight-skips")
	defer s.Done()
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Log("Deepgram not available — passing without exercising Gemini Live S2S tool calling")
		return false
	}
	return true
}

// geminiWeatherTools declares get_weather in Gemini's functionDeclarations
// shape, with Google's uppercase JSON-schema types.
var geminiWeatherTools = []map[string]any{
	{
		"functionDeclarations": []map[string]any{
			{
				"name":        "get_weather",
				"description": "Get the current weather conditions for a city. Call this whenever the user asks about weather.",
				// Opt into 3.8's background/async tool execution: the model
				// keeps talking while the call is outstanding.
				"behavior": "NON_BLOCKING",
				"parameters": map[string]any{
					"type": "OBJECT",
					"properties": map[string]any{
						"location": map[string]any{
							"type":        "STRING",
							"description": "the city name",
						},
					},
					"required": []string{"location"},
				},
			},
		},
	},
}

// geminiLiveTarget is what differs between the AI Studio and Vertex runs.
type geminiLiveTarget struct {
	model            string
	auth             map[string]any
	connectOptions   map[string]any // nil = the verb's Developer API default
	generationConfig map[string]any
}

func runGeminiLiveToolHook(t *testing.T, ctx context.Context, tgt geminiLiveTarget) {
	t.Helper()
	uas := claimUAS(t, ctx)

	s := Step(t, "ensure-prompt-wav")
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
	setup := map[string]any{
		"systemInstruction": map[string]any{
			"parts": []map[string]any{{"text": llmWeatherSystemPrompt}},
		},
		"tools": geminiWeatherTools,
	}
	if tgt.generationConfig != nil {
		setup["generationConfig"] = tgt.generationConfig
	}
	verbArgs := []any{
		"vendor", "google",
		"model", tgt.model,
		"auth", tgt.auth,
		"actionHook", webhookSrv.PublicURL() + "/action/llm",
		// toolHook payloads carry no callInfo, so X-Test-Id MUST ride the
		// query param for the webhook server's correlation layer.
		"toolHook", SessionURL(sess, "llm-google-tool"),
		"llmOptions", map[string]any{
			// Passed through verbatim as BidiGenerateContentSetup. The verb
			// injects model + forces generationConfig.responseModalities.
			"setup": setup,
		},
	}
	if tgt.connectOptions != nil {
		verbArgs = append(verbArgs, "connectOptions", tgt.connectOptions)
	}
	llmVerb := V("llm", verbArgs...)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "llm")
	// The toolHook response must echo the LIVE function-call id jambonz just
	// sent. This closure runs on the webhook server goroutine, so it must stay
	// pure: no t/s/StepCtx/assertions inside it.
	sess.ScriptActionHookBodyFunc("llm-google-tool", func(cb webhook.Callback) []byte {
		id := cb.NestedString("function_calls.0.id")
		name := cb.NestedString("function_calls.0.name")
		resp := map[string]any{
			"toolResponse": map[string]any{
				"functionResponses": []map[string]any{
					{
						"id":       id,
						"name":     name,
						"response": map[string]any{"output": llmWeatherToolResult},
					},
				},
			},
		}
		b, _ := json.Marshal(resp)
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

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	recPath := filepath.Join(t.TempDir(), "llm-google-tool-reply.pcm")

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
	// Trail with silence so Gemini's VAD detects end-of-utterance.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-google-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-google-tool): %v", err)
	}
	s.Logf("action/llm-google-tool body: %s", string(toolCB.Body))
	if got := toolCB.NestedString("function_calls.0.name"); got != "get_weather" {
		s.Errorf("tool call name=%q want %q; body=%s", got, "get_weather", string(toolCB.Body))
	}
	if toolCB.NestedString("function_calls.0.id") == "" {
		s.Errorf("tool call missing function_calls.0.id; body=%s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-args")
	location := strings.ToLower(toolCB.NestedString("function_calls.0.args.location"))
	if !strings.Contains(location, "chicago") {
		s.Errorf("args.location=%q does not contain %q; body=%s", location, "chicago", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-tool-result-spoken")
	// "hail" only appears in the agent's spoken reply if our toolResponse
	// envelope round-tripped through Gemini and was relayed to the caller.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}
