// Tests for the `llm` verb with LLM vendor "voicelive" (Azure Voice Live,
// realtime S2S) — app-declared tool/function calling end-to-end.
//
// voicelive is an OPTIONAL vendor (see config.HasVoiceLive). It needs BOTH
// VOICELIVE_API_KEY and VOICELIVE_HOST, because the endpoint is
// resource-specific (wss://<resource>.services.ai.azure.com/voice-live/
// realtime) and there is no default host to fall back on. When either is
// unset the test passes immediately without exercising Voice Live — a plain
// `return` after a log, never t.Skip, never a failure, matching
// xai_llm_s2s_test.go.
//
// Voice Live reuses the Azure OpenAI Realtime EVENT vocabulary, so the
// tool-calling contract is the same as the openai/xai vendors: the toolHook
// receives {tool_call_id, name, args} and answers with
// {type:"conversation.item.create", item:{type:"function_call_output",
// call_id:<echo>, output:<result>}}.
//
// What is vendor-specific — and what this test exists to pin — is the SESSION
// shape. Voice Live keeps the flat layout that openai_s2s.js rewrites away:
// voice, turn_detection and modalities sit at the top level of the session
// body, not nested under audio.input / audio.output. On top of that it takes
// Azure-only settings. So this test configures:
//   - voice as an OBJECT ({name, type:"azure-standard"}) — an Azure TTS voice,
//     not an OpenAI voice id string
//   - turn_detection type azure_semantic_vad — an Azure-only VAD that does not
//     exist on the OpenAI Realtime API
//
// If the feature-server ever converts this payload to the OpenAI GA nested
// shape, Azure rejects the unknown fields and the session never configures,
// so the tool call never arrives and this test fails.
//
// Reuses the weather prompt/system-prompt/result consts from llm_test.go —
// the scenario is vendor-agnostic.
//
// Steps:
//  1. preflight-skips — voicelive guard (plain return), then deepgram guard
//     (plain return) for prompt-gen + reply STT infra
//  2. ensure-prompt-wav
//  3. script-llm-verb — voicelive vendor/model/auth/connectOptions/toolHook,
//     flat Azure session_update with tools, dynamic toolHook responder
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt — let Azure's STT/VAD arm
//  7. record-and-speak — start recording, send the weather question, pad with
//     silence so the semantic VAD finalizes the utterance
//  8. wait-for-tool-call — assert name == "get_weather" and a non-empty
//     tool_call_id
//  9. assert-tool-args — assert args.location contains "chicago"
//  10. wait-for-reply-and-stop
//  11. hangup-and-wait-ended
//  12. assert-tool-result-spoken — independently STT the recording and assert
//     it contains the tool result's distinctive word ("hail")
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

// TestVerb_LLM_VoiceLive_ToolHook proves the `llm` verb's voicelive path
// configures a Voice Live session with Azure-native settings and supports
// app-declared tool/function calling end-to-end.
func TestVerb_LLM_VoiceLive_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasVoiceLive() {
		t.Log("VOICELIVE_API_KEY/VOICELIVE_HOST not set — passing without exercising voicelive S2S")
		return
	}

	// Deepgram is only needed for prompt-WAV generation + independent STT of
	// the recorded reply — not for the Voice Live session itself.
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising voicelive S2S")
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
		"vendor", "voicelive",
		"model", cfg.VoiceLiveModel,
		"auth", map[string]any{
			"apiKey": cfg.VoiceLiveAPIKey,
		},
		// The Voice Live endpoint is resource-specific; there is no default.
		"connectOptions", map[string]any{
			"host": cfg.VoiceLiveHost,
		},
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// toolHook payloads carry no callInfo, so X-Test-Id MUST ride the
		// query param for the webhook server's correlation layer.
		"toolHook", SessionURL(sess, "llm-voicelive-tool"),
		"llmOptions", map[string]any{
			// Sent verbatim as {type:'session.update', session:<this>} in
			// Voice Live's FLAT shape — see the file comment.
			"session_update": map[string]any{
				"modalities":   []string{"text", "audio"},
				"instructions": llmWeatherSystemPrompt,
				// An Azure TTS voice, declared as an object.
				"voice": map[string]any{
					"name": cfg.VoiceLiveVoice,
					"type": "azure-standard",
				},
				// Azure-only VAD — rejected by the OpenAI Realtime API, so
				// this reaching the service unaltered is itself the assertion.
				"turn_detection": map[string]any{
					"type":                "azure_semantic_vad",
					"silence_duration_ms": 500,
					"remove_filler_words": true,
				},
				"input_audio_noise_reduction": map[string]any{
					"type": "azure_deep_noise_suppression",
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
			},
			"response_create": map[string]any{},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "llm")
	// The toolHook response must echo the LIVE tool_call_id jambonz just
	// sent. This closure runs on the webhook server goroutine, so it must
	// stay pure: no t/s/StepCtx/assertions inside it.
	sess.ScriptActionHookBodyFunc("llm-voicelive-tool", func(cb webhook.Callback) []byte {
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

	recPath := filepath.Join(t.TempDir(), "llm-voicelive-tool-reply.pcm")

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
	// Trail with silence so the semantic VAD detects end-of-utterance.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-voicelive-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-voicelive-tool): %v", err)
	}
	s.Logf("action/llm-voicelive-tool body: %s", string(toolCB.Body))
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool call name=%q want %q; body=%s", got, "get_weather", string(toolCB.Body))
	}
	if toolCB.String("tool_call_id") == "" {
		s.Errorf("tool call missing tool_call_id; body=%s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-args")
	// The llm verb sends parsed function arguments under "args", not
	// "arguments".
	location := strings.ToLower(toolCB.NestedString("args.location"))
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
	// "hail" only appears in the agent's spoken reply if the
	// function_call_output envelope round-tripped through Voice Live and was
	// relayed to the caller — proving the full tool-call loop, and with it
	// that the flat Azure session actually configured.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}

// voiceLivePassphrase is a word the model would never volunteer, so hearing it
// in the recording proves the agent-first response.create reached the service
// and its audio reached the caller.
const voiceLivePassphrase = "marmalade"

// TestVerb_LLM_VoiceLive_Session covers the plain conversational path, with no
// tools involved: the agent greets first, the caller speaks, the agent answers,
// and the verb completes normally. It also pins the event stream, which the
// tool test never looks at.
//
// The three events asserted are the ones that prove the session really ran
// rather than merely connecting:
//   - session.updated — Azure accepted our flat session config; until it
//     arrives the media server gates caller audio, so nothing else can happen
//   - conversation.item.input_audio_transcription.completed — the caller was
//     actually transcribed (in cascaded mode this is Azure speech to text)
//   - response.done — the agent produced a complete turn
func TestVerb_LLM_VoiceLive_Session(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasVoiceLive() {
		t.Log("VOICELIVE_API_KEY/VOICELIVE_HOST not set — passing without exercising voicelive S2S")
		return
	}

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising voicelive S2S")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm",
		"Hello there, please say the magic word.", tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	llmVerb := V("llm",
		"vendor", "voicelive",
		"model", cfg.VoiceLiveModel,
		"auth", map[string]any{"apiKey": cfg.VoiceLiveAPIKey},
		"connectOptions", map[string]any{"host": cfg.VoiceLiveHost},
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// carries the raw Voice Live server events; X-Test-Id must ride the
		// query param because event payloads have no callInfo
		"eventHook", SessionURL(sess, "llm-voicelive-event"),
		"llmOptions", map[string]any{
			"session_update": map[string]any{
				"modalities": []string{"text", "audio"},
				"instructions": "You are a terse test assistant. Whenever the caller asks for the magic " +
					"word, reply with exactly one sentence containing the word " + voiceLivePassphrase + ".",
				"voice": map[string]any{
					"name": cfg.VoiceLiveVoice,
					"type": "azure-standard",
				},
				"turn_detection": map[string]any{
					"type":                "azure_semantic_vad",
					"silence_duration_ms": 500,
				},
				// Must be explicit. Azure speech to text is automatic only for
				// NON-multimodal models; a native-audio model such as
				// gpt-realtime-2.1 emits no caller transcripts unless asked.
				"input_audio_transcription": map[string]any{"model": "azure-speech"},
			},
			"response_create": map[string]any{
				"instructions": "Greet the caller in one short sentence and ask how you can help.",
			},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
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

	recPath := filepath.Join(t.TempDir(), "llm-voicelive-session.pcm")

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

	s = Step(t, "collect-events")
	// DrainCallbacks rather than WaitCallbackFor: the latter discards
	// non-matching hooks, and we need to see several different event types.
	want := map[string]bool{
		"session.updated": false,
		"conversation.item.input_audio_transcription.completed": false,
		"response.done": false,
	}
	seen := map[string]int{}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		allFound := true
		for _, got := range want {
			if !got {
				allFound = false
			}
		}
		if allFound {
			break
		}
		for _, cb := range DrainCallbacks(sess, 2*time.Second) {
			if cb.Hook != "action/llm-voicelive-event" {
				continue
			}
			typ := cb.String("type")
			seen[typ]++
			if _, ok := want[typ]; ok {
				want[typ] = true
			}
		}
	}
	for typ, got := range want {
		if !got {
			s.Errorf("eventHook never delivered %q; types seen: %v", typ, seen)
		}
	}
	s.Logf("event types seen: %v", seen)
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	// collect-events returns as soon as the three events land, which is well
	// before the agent has finished speaking — give the reply time to arrive.
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-passphrase-spoken")
	// Proves the agent's audio actually reached the caller, which no event
	// assertion can show.
	AssertTranscriptHasMost(s, ctx, recPath, 1, voiceLivePassphrase)
	s.Done()
}

// TestVerb_LLM_VoiceLive_Hangup covers the built-in hangup tool on voicelive.
//
// Worth its own test because the hangup tool is declared with no parameters, so
// the model calls it with an empty `arguments` string — a path that is easy to
// drop on the floor while still passing the get_weather test, which always
// carries an argument.
func TestVerb_LLM_VoiceLive_Hangup(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasVoiceLive() {
		t.Log("VOICELIVE_API_KEY/VOICELIVE_HOST not set — passing without exercising voicelive hangup")
		return
	}

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising voicelive hangup")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance,
		tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV caller utterance: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-hangup")
	verb := V("llm",
		"vendor", "voicelive",
		"model", cfg.VoiceLiveModel,
		"auth", map[string]any{"apiKey": cfg.VoiceLiveAPIKey},
		"connectOptions", map[string]any{"host": cfg.VoiceLiveHost},
		"actionHook", SessionURL(sess, "llm"),
		"llmOptions", map[string]any{
			"session_update": map[string]any{
				"modalities":   []string{"text", "audio"},
				"instructions": hangupRealtimePrompt,
				"voice": map[string]any{
					"name": cfg.VoiceLiveVoice,
					"type": "azure-standard",
				},
				"turn_detection": map[string]any{
					"type":                "azure_semantic_vad",
					"silence_duration_ms": 500,
				},
			},
			"response_create": map[string]any{
				"instructions": "Greet the caller in one short sentence and ask how you can help.",
			},
		},
		"hangup", map[string]any{"reason": hangupAppReason},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "drive-conversation")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	driveHangupConversation(s, ctx, call, callerWAV)
	s.Done()

	s = Step(t, "assert-server-hangup")
	// "" → the model supplies its own free-form reason, which by design wins
	// over hangup.reason; assert only that SOME reason propagated.
	assertServerHangup(s, call, "")
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "llm")
	s.Done()
}
