// Tests for the `llm` verb with LLM vendor "nova" (Amazon Nova Sonic on
// Bedrock, realtime S2S).
//
// nova is an OPTIONAL vendor (see config.HasNova): with NOVA_ACCESS_KEY_ID /
// NOVA_SECRET_ACCESS_KEY unset the tests pass immediately without touching
// AWS — a plain `return` after a log, matching voicelive_llm_s2s_test.go.
//
// Nova differs from the websocket vendors in ways these tests pin:
//   - the media server opens a Bedrock bidirectional stream signed with the
//     inline IAM credentials, so a session that never connects fails every test
//   - Nova speaks only after input; the agent-first turn is llmOptions.greeting,
//     sent as a cross-modal user text turn
//   - events are Nova-native {event:{<name>:{...}}}, not {type}
//   - tools are toolSpecs; the toolHook gets {tool_call_id (Nova's toolUseId),
//     name, args} and any JSON reply goes back as the TOOL result
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

func novaAuth() map[string]any {
	auth := map[string]any{
		"accessKeyId":     cfg.NovaAccessKeyID,
		"secretAccessKey": cfg.NovaSecretAccessKey,
		"region":          cfg.NovaRegion,
	}
	if cfg.NovaSessionToken != "" {
		auth["sessionToken"] = cfg.NovaSessionToken
	}
	return auth
}

func novaPromptStart() map[string]any {
	return map[string]any{"audioOutputConfiguration": map[string]any{"voiceId": cfg.NovaVoice}}
}

// novaPreflight returns false (test passes without exercising nova) when the
// vendor or the Deepgram prompt/STT infra is unavailable.
func novaPreflight(t *testing.T) bool {
	if !cfg.HasNova() {
		t.Log("NOVA_ACCESS_KEY_ID/NOVA_SECRET_ACCESS_KEY not set — passing without exercising nova S2S")
		return false
	}
	s := Step(t, "preflight-skips")
	defer s.Done()
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Log("Deepgram not available — passing without exercising nova S2S")
		return false
	}
	return true
}

// TestVerb_LLM_Nova_ToolHook proves app-declared tool calling end-to-end: the
// toolUse reaches the toolHook with parsed args, and the reply round-trips
// through Nova into the spoken answer.
func TestVerb_LLM_Nova_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !novaPreflight(t) {
		return
	}

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
	// inputSchema.json is a STRING in Nova's toolSpec shape
	schema, _ := json.Marshal(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"location": map[string]any{"type": "string", "description": "the city name"},
		},
		"required": []string{"location"},
	})
	promptStart := novaPromptStart()
	promptStart["toolConfiguration"] = map[string]any{
		"tools": []map[string]any{{
			"toolSpec": map[string]any{
				"name":        "get_weather",
				"description": "Get the current weather conditions for a city. Call this whenever the user asks about weather.",
				"inputSchema": map[string]any{"json": string(schema)},
			},
		}},
	}
	llmVerb := V("llm",
		"vendor", "nova",
		"model", cfg.NovaModel,
		"auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		// toolHook payloads carry no callInfo, so X-Test-Id rides the query param
		"toolHook", SessionURL(sess, "llm-nova-tool"),
		"llmOptions", map[string]any{
			"systemPrompt": llmWeatherSystemPrompt,
			"promptStart":  promptStart,
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	// any JSON object is a valid Nova tool result
	sess.ScriptActionHookBodyFunc("llm-nova-tool", func(cb webhook.Callback) []byte {
		b, _ := json.Marshal(map[string]any{"result": llmWeatherToolResult})
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

	WaitFor(t, "wait-for-session", RecognizerArmDelay)

	recPath := filepath.Join(t.TempDir(), "llm-nova-tool-reply.pcm")

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
	// trailing silence so Nova's endpointing closes the turn
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-nova-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-nova-tool): %v", err)
	}
	s.Logf("action/llm-nova-tool body: %s", string(toolCB.Body))
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool call name=%q want %q; body=%s", got, "get_weather", string(toolCB.Body))
	}
	if toolCB.String("tool_call_id") == "" {
		s.Errorf("tool call missing tool_call_id; body=%s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-args")
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
	// "hail" is only spoken if the TOOL result content block reached Nova
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}

// novaPassphrase is a word the model would never volunteer, so hearing it
// proves the system prompt reached Nova and its audio reached the caller.
const novaPassphrase = "marmalade"

// TestVerb_LLM_Nova_Session covers the plain conversational path: the agent
// greets first (llmOptions.greeting), the caller speaks, the agent answers.
// It pins the Nova-native event stream (keys arrive snake_cased, like every
// jambonz hook payload):
//   - text_output with role USER — the caller was transcribed by Nova
//   - text_output with role ASSISTANT — the agent produced text
//   - content_end of type AUDIO — the agent finished a spoken turn
func TestVerb_LLM_Nova_Session(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !novaPreflight(t) {
		return
	}

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm",
		"Hello there, please say the magic word.", tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	llmVerb := V("llm",
		"vendor", "nova",
		"model", cfg.NovaModel,
		"auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-nova-event"),
		"events", []string{"textOutput", "contentEnd", "error"},
		"llmOptions", map[string]any{
			"systemPrompt": "You are a terse test assistant. Whenever the caller asks for the magic " +
				"word, reply with exactly one sentence containing the word " + novaPassphrase + ".",
			"greeting": "Greet me in one short sentence and ask how you can help.",
			"sessionStart": map[string]any{
				"turnDetectionConfiguration": map[string]any{"endpointingSensitivity": "HIGH"},
			},
			"promptStart": novaPromptStart(),
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

	WaitFor(t, "let-agent-greet", RecognizerArmDelay)

	recPath := filepath.Join(t.TempDir(), "llm-nova-session.pcm")

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
	want := map[string]bool{"text_output:USER": false, "text_output:ASSISTANT": false, "content_end:AUDIO": false}
	seen := map[string]int{}
	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) && !allTrue(want) {
		for _, cb := range DrainCallbacks(sess, 2*time.Second) {
			if cb.Hook != "action/llm-nova-event" {
				continue
			}
			key := novaEventKey(cb.Body)
			seen[key]++
			if key == "error" {
				s.Errorf("nova error event: %s", string(cb.Body))
			}
			if _, ok := want[key]; ok {
				want[key] = true
			}
		}
	}
	for key, got := range want {
		if !got {
			s.Errorf("eventHook never delivered %q; seen: %v", key, seen)
		}
	}
	s.Logf("event kinds seen: %v", seen)
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-passphrase-spoken")
	AssertTranscriptHasMost(s, ctx, recPath, 1, novaPassphrase)
	s.Done()
}

// novaEventKey labels a Nova event hook body as "<name>" or, for text_output /
// content_end, "<name>:<role or type>".
func novaEventKey(body []byte) string {
	var msg struct {
		Event map[string]struct {
			Role string `json:"role"`
			Type string `json:"type"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return "unparsed"
	}
	for name, v := range msg.Event {
		switch name {
		case "text_output":
			return name + ":" + v.Role
		case "content_end":
			return name + ":" + v.Type
		}
		return name
	}
	return "unknown"
}

func allTrue(m map[string]bool) bool {
	for _, v := range m {
		if !v {
			return false
		}
	}
	return true
}

// TestVerb_LLM_Nova_DTMF covers the two text inputs jambonz layers onto Nova:
// keypad entries (llmOptions.dtmf, sent as cross-modal user text since Nova has
// no DTMF) and llm:update {text}. The agent reading the digits back proves each
// reached Nova whole.
func TestVerb_LLM_Nova_DTMF(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasNova() {
		t.Log("NOVA_ACCESS_KEY_ID/NOVA_SECRET_ACCESS_KEY not set — passing without exercising nova DTMF")
		return
	}

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-and-call")
	llmVerb := V("llm",
		"vendor", "nova",
		"model", cfg.NovaModel,
		"auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-nova-event"),
		"events", []string{"textOutput", "contentStart"},
		"llmOptions", map[string]any{
			"systemPrompt": "You are a test assistant. Whenever you are given a number, reply with one short " +
				"sentence that reads the number back digit by digit.",
			"greeting":    "Say hello in three words.",
			"promptStart": novaPromptStart(),
			"dtmf":        map[string]any{"enable": true, "finishOnKey": "#"},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm", "llm-nova-event")
	// WS transport so llm:update can be sent as a session command
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	if reply := novaWaitReply(sess, 20*time.Second, ""); reply == "" {
		s.Fatalf("agent never greeted")
	}
	s.Done()

	s = Step(t, "dtmf-entry")
	elevenlabsSendDTMF(s, call, "1234#")
	if reply := novaWaitReply(sess, 20*time.Second, "1234"); !strings.Contains(digitsSpoken(reply), "1234") {
		s.Errorf("agent reply %q does not read back 1234", reply)
	}
	s.Done()

	s = Step(t, "llm-update-text")
	if err := sess.SendCommand("llm:update", map[string]any{"text": "The number is 2468."}); err != nil {
		s.Fatalf("SendCommand(llm:update): %v", err)
	}
	if reply := novaWaitReply(sess, 20*time.Second, "2468"); !strings.Contains(digitsSpoken(reply), "2468") {
		s.Errorf("agent reply %q does not read back 2468", reply)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// novaWaitReply collects FINAL assistant text from the event hook until it
// reads back want (as digits) or timeout; want "" returns the first reply.
func novaWaitReply(sess *webhook.Session, timeout time.Duration, want string) string {
	return novaWaitReplyOn(sess, "action/llm-nova-event", timeout, want)
}

func novaWaitReplyOn(sess *webhook.Session, hook string, timeout time.Duration, want string) string {
	// hook POSTs can land out of order, so text is matched to FINAL contents at the end
	final := map[string]bool{}
	texts := map[string]string{}
	var order []string
	reply := func() string {
		var b strings.Builder
		for _, id := range order {
			if final[id] {
				b.WriteString(texts[id] + " ")
			}
		}
		return b.String()
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, cb := range DrainCallbacks(sess, time.Second) {
			if cb.Hook != hook {
				continue
			}
			c, o := novaParseText(cb.Body)
			if c != nil && c.Role == "ASSISTANT" && c.Type == "TEXT" && strings.Contains(c.Additional, "FINAL") {
				final[c.ContentID] = true
			}
			if o != nil {
				if _, seen := texts[o.ContentID]; !seen {
					order = append(order, o.ContentID)
				}
				texts[o.ContentID] += o.Content
			}
		}
		if got := reply(); got != "" && (want == "" || strings.Contains(digitsSpoken(got), want)) {
			return got
		}
	}
	return reply()
}

type novaContentStart struct {
	ContentID  string `json:"content_id"`
	Role       string `json:"role"`
	Type       string `json:"type"`
	Additional string `json:"additional_model_fields"`
}

type novaTextOutput struct {
	ContentID string `json:"content_id"`
	Content   string `json:"content"`
}

// novaParseText pulls content_start / text_output out of a (snake_cased) Nova event hook body.
func novaParseText(body []byte) (*novaContentStart, *novaTextOutput) {
	var msg struct {
		Event struct {
			ContentStart *novaContentStart `json:"content_start"`
			TextOutput   *novaTextOutput   `json:"text_output"`
		} `json:"event"`
	}
	if json.Unmarshal(body, &msg) != nil {
		return nil, nil
	}
	return msg.Event.ContentStart, msg.Event.TextOutput
}
