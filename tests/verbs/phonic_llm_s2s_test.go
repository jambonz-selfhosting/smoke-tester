// Tests for the `llm` verb with vendor "phonic" (Phonic speech-to-speech).
//
// phonic is an OPTIONAL vendor (config.HasPhonic): with PHONIC_API_KEY unset
// each test passes without exercising it. Deepgram synthesizes the caller's
// speech and transcribes the recorded replies.
//
// Phonic's config message is llmOptions.config; jambonz sets the audio formats
// and injects its own tools (hangup, handoff, MCP) in Phonic's strict inline
// shape. Inline tool calls reach the toolHook as {tool_call_id, name, args}.
//
// The caller speaks only once the agent has greeted (phonicGreetWait). Phonic's
// VAD ignores, or mis-hears, an utterance whose start it never got, and the
// warmup pause plus session setup would cut the start of an earlier utterance.
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	phonicEventHook = "action/llm-phonic-event"
	// a word the caller can only hear if the welcome message played
	phonicWelcome = "Welcome to the Juniper Cafe pineapple line, how can I help?"
	// warmup pause + session setup + a short greeting
	phonicGreetWait = 5 * time.Second
)

func phonicPreflight(t *testing.T, s *StepCtx) bool {
	if !cfg.HasPhonic() {
		s.Done()
		t.Log("PHONIC_API_KEY not set — passing without exercising phonic S2S")
		return false
	}
	if !cfg.HasDeepgram() {
		s.Done()
		t.Log("Deepgram not available — passing without exercising phonic S2S")
		return false
	}
	return true
}

// phonicVerb is the llm verb with an inline key and the given config.
func phonicVerb(actionURL string, config map[string]any, extra ...any) map[string]any {
	if _, ok := config["voice_id"]; !ok {
		config["voice_id"] = cfg.PhonicVoiceID
	}
	args := []any{
		"vendor", "phonic",
		"auth", map[string]any{"apiKey": cfg.PhonicAPIKey},
		"actionHook", actionURL,
		"llmOptions", map[string]any{"config": config},
	}
	return V("llm", append(args, extra...)...)
}

func phonicEvents(cbs []webhook.Callback, typ string) []webhook.Callback {
	var out []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == phonicEventHook && cb.String("type") == typ {
			out = append(out, cb)
		}
	}
	return out
}

// TestVerb_LLM_Phonic_Greeting: the session starts, the welcome message is
// heard by the caller, and the eventHook carries Phonic's events plus the
// normalized turn events. Hanging up ends the verb.
func TestVerb_LLM_Phonic_Greeting(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   "You are a brief, friendly phone assistant.",
		"welcome_message": phonicWelcome,
	}, "eventHook", SessionURL(sess, "llm-phonic-event"), "events", []string{"all"})
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	SessionAckEmpty(sess, "llm-phonic-event")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "phonic-greeting.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-events")
	events := DrainCallbacks(sess, 10*time.Second)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-events")
	for _, typ := range []string{"ready_to_start_conversation", "conversation_created", "assistant_started_speaking",
		"output_audio.playback_started", "llm_response"} {
		if len(phonicEvents(events, typ)) == 0 {
			s.Errorf("no %s event on the eventHook", typ)
		}
	}
	if cc := phonicEvents(events, "conversation_created"); len(cc) > 0 && !strings.HasPrefix(cc[0].String("conversation_id"), "conv_") {
		s.Errorf("conversation_created without a conversation id: %s", cc[0].Body)
	}
	for _, cb := range phonicEvents(events, "audio_chunk") {
		if cb.String("audio") != "" {
			s.Errorf("audio_chunk forwarded with its audio")
			break
		}
	}
	s.Done()

	s = Step(t, "assert-welcome-heard")
	AssertTranscriptHasMost(s, ctx, recPath, 1, "juniper", "pineapple")
	s.Done()
}

// TestVerb_LLM_Phonic_ToolHook: an inline tool declared in config is called
// with an argument from the caller's speech; the toolHook's bare result is
// wrapped as tool_call_output and the agent speaks it back.
func TestVerb_LLM_Phonic_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", llmWeatherUserPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   llmWeatherSystemPrompt,
		"welcome_message": "Hi.",
		"tools": []any{map[string]any{
			"type":                        "custom_websocket",
			"execution_mode":              "sync",
			"tool_call_output_timeout_ms": 15000,
			"tool_schema": map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        "get_weather",
					"description": "Get the current weather conditions for a city. Call this whenever the user asks about weather.",
					"parameters": map[string]any{
						"type": "object",
						"properties": map[string]any{
							"location": map[string]any{"type": "string", "description": "the city name"},
						},
						"required":             []string{"location"},
						"additionalProperties": false,
					},
					"strict": true,
				},
			},
		}},
	}, "toolHook", SessionURL(sess, "llm-phonic-tool"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	// a bare result: jambonz wraps it as Phonic's tool_call_output
	sess.ScriptActionHookBodyFunc("llm-phonic-tool", func(cb webhook.Callback) []byte {
		b, _ := json.Marshal(map[string]any{"weather": llmWeatherToolResult})
		return b
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "let-model-greet", phonicGreetWait)

	recPath := filepath.Join(t.TempDir(), "phonic-tool-reply.pcm")
	s = Step(t, "record-and-speak")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-phonic-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-phonic-tool): %v", err)
	}
	s.Logf("tool call: %s", toolCB.Body)
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool name=%q want get_weather", got)
	}
	if toolCB.String("tool_call_id") == "" {
		s.Errorf("tool call without tool_call_id")
	}
	if loc := strings.ToLower(toolCB.NestedString("args.location")); !strings.Contains(loc, "chicago") {
		s.Errorf("args.location=%q does not contain chicago", loc)
	}
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-tool-result-spoken")
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}

// TestVerb_LLM_Phonic_Hangup: the injected hangup tool ends the call with a
// server BYE carrying X-Reason, and the actionHook reports "hangup".
func TestVerb_LLM_Phonic_Hangup(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-verb")
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   hangupRealtimePrompt,
		"welcome_message": "Hi, how can I help?",
	}, "hangup", map[string]any{"reason": hangupAppReason})
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "drive-conversation")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	phonicDriveToEnd(s, ctx, call, callerWAV)
	s.Done()

	s = Step(t, "assert-server-hangup")
	assertServerHangup(s, call, "")
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "llm")
	s.Done()
}

// TestVerb_LLM_Phonic_Handoff: the injected transfer_to_human tool dials the
// target and bridges; the llm actionHook reports "transferred".
func TestVerb_LLM_Phonic_Handoff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/handoff", handoffCallerUtterance, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-verb")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   handoffRealtimePrompt,
		"welcome_message": "Hi, how can I help?",
	}, "handoff", map[string]any{
		"mode": "blind", "blindMethod": "dial", "brief": "none",
		"target": []any{map[string]any{"type": "user", "name": target}},
	})
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	targetCtx, targetCancel := context.WithCancel(ctx)
	go answerAndIdleTarget(t, targetCtx, targetUAS, targetDone, &targetCall)()
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	WaitFor(t, "let-model-greet", phonicGreetWait)
	if err := call.SendWAV(callerWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	if targetCall == nil {
		s.Fatal("target was never dialed (handoff did not run)")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "assert-completion-transferred")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/llm")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/llm: %v", err)
	}
	if got := cb.String("completion_reason"); got != "transferred" {
		s.Errorf("completion_reason: got %q want transferred; body=%s", got, cb.Body)
	}
	s.Done()
}

// TestVerb_LLM_Phonic_EndConversation: Phonic's own natural_conversation_ending
// tool ends the conversation; jambonz ends the verb after the goodbye plays
// (Phonic itself keeps the socket open) and the next verb runs.
func TestVerb_LLM_Phonic_EndConversation(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-verb")
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   "You are a brief phone assistant. When the caller says goodbye, say goodbye and end the conversation.",
		"welcome_message": "Hi, how can I help?",
		"tools":           []any{"natural_conversation_ending"},
	})
	// the hangup after the llm verb is what ends the call
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "drive-conversation")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	phonicDriveToEnd(s, ctx, call, callerWAV)
	s.Done()

	s = Step(t, "assert-completion")
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("end reason %q, want remote-bye from the hangup verb", reason)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/llm")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/llm: %v", err)
	}
	if got := cb.String("completion_reason"); got != "assistant ended conversation" {
		s.Errorf("completion_reason: got %q want %q; body=%s", got, "assistant ended conversation", cb.Body)
	}
	s.Done()
}

// TestVerb_LLM_Phonic_AccountCredential: with no auth, the verb uses the
// account's phonic_s2s credential chosen by label.
func TestVerb_LLM_Phonic_AccountCredential(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !phonicPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "create-phonic_s2s-credential")
	label := "it-phonic-s2s-" + provision.RunID()
	sid := client.ManagedAccountLlmCredential(t, ctx, suite.AccountSID, map[string]any{
		"vendor": "phonic_s2s", "label": label, "api_key": cfg.PhonicAPIKey,
	})
	s.Logf("llm credential sid=%s label=%s", sid, label)
	s.Done()

	s = Step(t, "script-llm-without-auth")
	verb := phonicVerb(SessionURL(sess, "llm"), map[string]any{
		"system_prompt":   "You are a brief, friendly phone assistant.",
		"welcome_message": phonicWelcome,
	})
	delete(verb, "auth")
	verb["label"] = label
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "phonic-account-cred.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(10 * time.Second)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-welcome-heard")
	AssertTranscriptHasMost(s, ctx, recPath, 1, "juniper", "pineapple")
	s.Done()
}

// phonicDriveToEnd speaks once the agent has greeted, then waits for jambonz to
// end the call.
func phonicDriveToEnd(s *StepCtx, ctx context.Context, call *jsip.Call, callerWAV string) {
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	WaitFor(s.t, "let-model-greet", phonicGreetWait)
	if err := call.SendWAV(callerWAV); err != nil {
		s.Fatalf("SendWAV caller utterance: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	waitServerBye(s, ctx, call)
}
