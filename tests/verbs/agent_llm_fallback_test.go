// Tests for the agent verb's llm.fallback chain: when the configured LLM
// fails before it says anything, the agent switches to the next LLM in
// llm.fallback and the caller still gets an answer. The switch is reported
// in the actionHook payload as llm_fallback: [{from, to, reason}].
//
// Each test breaks the primary LLM a different way:
//   - BadKey: an invalid inline API key (the vendor answers 401)
//   - Timeout: a real key, but fallbackTimeoutMs=1 so the primary never
//     answers in time
//   - NoCredentials: no inline auth and no stored credential with its label,
//     so the primary is skipped before the call starts
//
// The fallback is always Deepseek (the default agent LLM), except in Timeout
// where Deepseek is the primary and OpenAI the fallback. Missing keys pass
// without exercising the test (log + return), like xai_agent_test.go.
package verbs

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const llmFallbackTurnWait = 30 * time.Second

// TestVerb_Agent_LLMFallback_BadKey — primary openai with an invalid key,
// fallback deepseek. The agent must answer on deepseek and report the switch.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_BadKey(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		LLMVendor:    "openai",
		LLMModel:     "gpt-4o-mini",
		LLMApiKey:    "sk-it-invalid-smoke-key",
		LLMFallback: []map[string]any{
			{"vendor": "deepseek", "model": "deepseek-chat", "auth": map[string]any{"apiKey": cfg.DeepseekAPIKey}},
		},
	}, "openai", "deepseek", regexp.MustCompile(`(?i)401|api key|incorrect|auth`))
}

// TestVerb_Agent_LLMFallback_Timeout — primary deepseek with a working key
// but fallbackTimeoutMs=1, fallback openai. Proves the first-token timeout
// path: a silent vendor is abandoned instead of leaving the caller hanging.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_Timeout(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasOpenAI() {
		t.Log("OPENAI_API_KEY not set — passing without exercising the fallback timeout")
		return
	}
	timeout := 1
	runLLMFallbackCall(t, agentVerbOpts{
		SystemPrompt:      agentEchoSystemPrompt,
		FallbackTimeoutMs: &timeout,
		LLMFallback: []map[string]any{
			{"vendor": "openai", "model": "gpt-4o-mini", "auth": map[string]any{"apiKey": cfg.OpenAIAPIKey}},
		},
	}, "deepseek", "openai", regexp.MustCompile(`no response within 1ms`))
}

// TestVerb_Agent_LLMFallback_NoCredentials — primary anthropic with no
// inline auth and a label no credential carries (the cluster may hold an
// unlabelled anthropic credential at SP level), so the primary is skipped at
// start and the call runs on deepseek.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_NoCredentials(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		LLMVendor:    "anthropic",
		LLMModel:     "claude-sonnet-4-5",
		LLMNoAuth:    true,
		LLMLabel:     "it-no-such-credential",
		LLMFallback: []map[string]any{
			{"vendor": "deepseek", "model": "deepseek-chat", "auth": map[string]any{"apiKey": cfg.DeepseekAPIKey}},
		},
	}, "anthropic", "deepseek", regexp.MustCompile(`No LLM credentials found for anthropic, label:it-no-such-credential`))
}

// runLLMFallbackCall places one agent call with opts, speaks the echo prompt,
// and asserts the reply came through and the actionHook reports exactly one
// switch from wantFrom to wantTo with a reason matching reasonRe.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-fallback
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-turn-end
//  9. assert-llm-response — the fallback LLM echoed our keywords
//
// 10. hangup
// 11. wait-action-agent-complete
// 12. assert-llm-fallback — llm_fallback in the actionHook payload
func runLLMFallbackCall(t *testing.T, opts agentVerbOpts, wantFrom, wantTo string, reasonRe *regexp.Regexp) {
	t.Helper()
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
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

	s = Step(t, "script-agent-verb-fallback")
	ScriptAgent(sess, opts)
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
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-turn-end")
	waitCtx, cancel := context.WithTimeout(ctx, llmFallbackTurnWait)
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "turn_end")) > 0
	})
	cancel()
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-llm-response")
	var all string
	for _, r := range findAgentEvents(cbs, "llm_response") {
		all += " " + strings.ToLower(r.String("response"))
	}
	hit := false
	for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
		if strings.Contains(all, kw) {
			hit = true
			break
		}
	}
	if !hit {
		s.Errorf("fallback LLM did not echo our prompt; llm_response text: %q", all)
	} else {
		s.Logf("llm_response: %q", strings.TrimSpace(all))
	}
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	actCtx, acancel := context.WithTimeout(ctx, 30*time.Second)
	defer acancel()
	actionCB, err := sess.WaitCallbackFor(actCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Logf("action/agent-complete body: %s", string(actionCB.Body))
	s.Done()

	s = Step(t, "assert-llm-fallback")
	switches, _ := actionCB.JSON["llm_fallback"].([]any)
	if len(switches) != 1 {
		s.Fatalf("want exactly 1 llm_fallback entry, got %d: %s", len(switches), string(actionCB.Body))
	}
	sw, _ := switches[0].(map[string]any)
	from, _ := sw["from"].(map[string]any)
	to, _ := sw["to"].(map[string]any)
	reason, _ := sw["reason"].(string)
	if from["vendor"] != wantFrom || to["vendor"] != wantTo {
		s.Errorf("llm_fallback %v -> %v, want %s -> %s", from["vendor"], to["vendor"], wantFrom, wantTo)
	}
	if !reasonRe.MatchString(reason) {
		s.Errorf("llm_fallback reason %q does not match %s", reason, reasonRe)
	}
	if got := actionCB.String("completion_reason"); got == "llm_failure" {
		s.Errorf("completion_reason=llm_failure: the fallback should have kept the agent alive")
	}
	s.Logf("llm_fallback: %s -> %s (%s)", from["vendor"], to["vendor"], reason)
	s.Done()
}

// fakeLLMBaseURL is the OpenAI-compatible base URL of sess's fake LLM
// (internal/webhook/fakellm.go), reachable by jambonz through the tunnel.
func fakeLLMBaseURL(sess *webhook.Session) string {
	return webhookSrv.PublicURL() + "/llm/" + url.PathEscape(sess.ID()) + "/v1"
}

// fakeOpenAIPrimary points the agent's main LLM at sess's fake LLM, with
// deepseek as the fallback.
func fakeOpenAIPrimary(sess *webhook.Session, opts agentVerbOpts) agentVerbOpts {
	opts.LLMVendor = "openai"
	opts.LLMModel = "gpt-fake"
	opts.LLMNoAuth = true
	opts.LLMFallback = []map[string]any{
		{"vendor": "deepseek", "model": "deepseek-chat", "auth": map[string]any{"apiKey": cfg.DeepseekAPIKey}},
	}
	return opts
}

// scriptFakeAgent is ScriptAgent with the main LLM's inline auth carrying the
// fake's baseURL (buildAgentVerb drops auth when LLMNoAuth is set).
func scriptFakeAgent(sess *webhook.Session, opts agentVerbOpts) {
	opts.ActionURL = SessionURL(sess, "agent-complete")
	opts.EventURL = SessionURL(sess, "agent-turn")
	if len(opts.Tools) > 0 {
		opts.ToolURL = SessionURL(sess, "agent-tool")
	}
	verb := buildAgentVerb(opts)
	llm := verb["llm"].(map[string]any)
	llm["auth"] = map[string]any{"apiKey": "sk-fake", "baseURL": fakeLLMBaseURL(sess)}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
}

func countHook(cbs []webhook.Callback, hook string) int {
	n := 0
	for _, cb := range cbs {
		if cb.Hook == hook {
			n++
		}
	}
	return n
}

func findHook(cbs []webhook.Callback, hook string) *webhook.Callback {
	for i := range cbs {
		if cbs[i].Hook == hook {
			return &cbs[i]
		}
	}
	return nil
}

// llmFallbackEntries returns the actionHook's llm_fallback list.
func llmFallbackEntries(cb *webhook.Callback) []map[string]any {
	raw, _ := cb.JSON["llm_fallback"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestVerb_Agent_LLMFallback_MidCallToolHistory — the primary (fake openai)
// runs a tool-call turn, then fails with 402 insufficient_quota on the next
// turn. Deepseek takes over and must answer from the tool result in the
// carried-over history: the secret word exists nowhere else, and the
// toolHook must not be called a second time.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-agent-verb-fake-primary (fake: tool call, text, 402)
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. turn-1-ask-for-tool — wait for the tool turn to end
//  8. turn-2-ask-for-word — wait for deepseek's answer
//  9. assert-word-from-history
//
// 10. assert-fake-requests — 3 requests, the second carrying the tool result
// 11. hangup
// 12. wait-action-agent-complete
// 13. assert-llm-fallback
func TestVerb_Agent_LLMFallback_MidCallToolHistory(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)
	const secretWord = "marmalade"
	prompts := []string{"Please get the secret word.", "What is the secret word?"}

	s = Step(t, "ensure-prompt-wavs")
	wavs := make([]string, len(prompts))
	for i, p := range prompts {
		w, err := tts.EnsureWAV(ctx, "testdata/agent", p, tts.PromptOptions{Model: "aura-asteria-en"})
		if err != nil {
			s.Fatalf("EnsureWAV %q: %v", p, err)
		}
		wavs[i] = w
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-fake-primary")
	sess.ScriptLLM(
		webhook.LLMReply{ToolCalls: []webhook.LLMToolCall{{ID: "call_smoke1", Name: "get_secret_word", Args: "{}"}}},
		webhook.LLMReply{Text: "I have fetched the secret word."},
		webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"},
	)
	sess.ScriptActionHookBody("agent-tool", []byte(`{"word":"`+secretWord+`"}`))
	scriptFakeAgent(sess, fakeOpenAIPrimary(sess, agentVerbOpts{
		SystemPrompt: "You are a voice assistant. When the user asks you to get the secret word, call get_secret_word. " +
			"When the user asks what the secret word is, answer with the word from the earlier tool result in this " +
			"conversation. Do not call the tool again.",
		Tools: []map[string]any{{
			"name":        "get_secret_word",
			"description": "Fetches the secret word.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
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

	var all []webhook.Callback
	speak := func(name, wav string, done func([]webhook.Callback) bool) {
		s := Step(t, name)
		if err := call.SendWAV(wav); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
		waitCtx, cancel := context.WithTimeout(ctx, llmFallbackTurnWait)
		got := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return done(append(all, c...)) })
		cancel()
		all = append(all, got...)
		s.Logf("events so far: %s", summarizeEventTypes(all))
		s.Done()
	}
	speak("turn-1-ask-for-tool", wavs[0], func(c []webhook.Callback) bool {
		return agentTurnEndNamesTool(c, "get_secret_word")
	})
	speak("turn-2-ask-for-word", wavs[1], func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "turn_end")) >= 2
	})

	s = Step(t, "assert-word-from-history")
	var reply string
	for _, r := range findAgentEvents(all, "llm_response") {
		reply += " " + strings.ToLower(r.String("response"))
	}
	if !strings.Contains(reply, secretWord) {
		s.Errorf("deepseek did not answer %q from the carried-over tool result; llm_response: %q", secretWord, reply)
	}
	if n := countHook(all, "action/agent-tool"); n != 1 {
		s.Errorf("toolHook called %d times, want 1 (a second call means the history did not carry the result)", n)
	}
	s.Logf("llm_response: %q", strings.TrimSpace(reply))
	s.Done()

	s = Step(t, "assert-fake-requests")
	reqs := sess.LLMRequests()
	if len(reqs) != 3 {
		s.Errorf("fake LLM got %d requests, want 3 (tool call, continuation, 402)", len(reqs))
	} else {
		msgs, _ := reqs[1].Body["messages"].([]any)
		hasTool := false
		for _, m := range msgs {
			if mm, _ := m.(map[string]any); mm["role"] == "tool" {
				hasTool = true
			}
		}
		if !hasTool {
			s.Errorf("continuation request carried no tool result: %v", msgs)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	actionCB := findHook(all, "action/agent-complete")
	if actionCB == nil {
		actCtx, acancel := context.WithTimeout(ctx, 30*time.Second)
		defer acancel()
		cb, err := sess.WaitCallbackFor(actCtx, "action/agent-complete")
		if err != nil {
			s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
		}
		actionCB = &cb
	}
	s.Done()

	s = Step(t, "assert-llm-fallback")
	fb := llmFallbackEntries(actionCB)
	if len(fb) != 1 {
		s.Fatalf("want 1 llm_fallback entry, got %d: %s", len(fb), string(actionCB.Body))
	}
	reason, _ := fb[0]["reason"].(string)
	if to, _ := fb[0]["to"].(map[string]any); to["vendor"] != "deepseek" || !regexp.MustCompile(`402|quota`).MatchString(reason) {
		s.Errorf("llm_fallback %v, want openai -> deepseek on 402", fb[0])
	}
	s.Logf("llm_fallback reason: %s", reason)
	s.Done()
}

// TestVerb_Agent_LLMFallback_NoRetryOn429 — the fake answers 429 with
// Retry-After: 20. With a fallback configured the SDK must not retry (it
// would wait out Retry-After, silencing the caller): the fake sees exactly
// one request and deepseek answers within seconds.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-fake-primary (fake: 429 Retry-After 20)
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-turn-end
//  9. assert-no-retry — one fake request, reply soon after it
//
// 10. hangup
// 11. wait-action-agent-complete
// 12. assert-llm-fallback
func TestVerb_Agent_LLMFallback_NoRetryOn429(t *testing.T) {
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
	wav, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-fake-primary")
	sess.ScriptLLM(webhook.LLMReply{Status: 429, RetryAfter: "20", ErrorCode: "rate_limit_exceeded"})
	scriptFakeAgent(sess, fakeOpenAIPrimary(sess, agentVerbOpts{SystemPrompt: agentEchoSystemPrompt}))
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
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-turn-end")
	waitCtx, cancel := context.WithTimeout(ctx, llmFallbackTurnWait)
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "llm_response")) > 0
	})
	cancel()
	s.Done()

	s = Step(t, "assert-no-retry")
	reqs := sess.LLMRequests()
	responses := findAgentEvents(cbs, "llm_response")
	if len(reqs) != 1 {
		s.Errorf("fake LLM got %d requests, want 1: the SDK retried the 429", len(reqs))
	}
	if len(responses) == 0 || len(reqs) == 0 {
		s.Fatalf("no llm_response after the 429 (requests=%d)", len(reqs))
	}
	if lag := responses[0].Received.Sub(reqs[0].Received); lag > 10*time.Second {
		s.Errorf("reply %s after the 429: Retry-After was waited out", lag)
	} else {
		s.Logf("deepseek replied %s after the 429", lag.Round(time.Millisecond))
	}
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	actCtx, acancel := context.WithTimeout(ctx, 30*time.Second)
	defer acancel()
	actionCB, err := sess.WaitCallbackFor(actCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Done()

	s = Step(t, "assert-llm-fallback")
	fb := llmFallbackEntries(&actionCB)
	if len(fb) != 1 {
		s.Fatalf("want 1 llm_fallback entry, got %d: %s", len(fb), string(actionCB.Body))
	}
	if reason, _ := fb[0]["reason"].(string); !strings.Contains(reason, "429") {
		s.Errorf("llm_fallback reason %q, want the 429", reason)
	}
	s.Done()
}

// TestVerb_Agent_LLMFallback_MidStreamNoFallback — the fake streams part of
// a reply and cuts the connection. The caller already heard that text, so
// the agent must NOT replay the turn on deepseek: no llm_fallback, and the
// verb ends with completion_reason llm_failure as before.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-fake-primary (fake: text then drop, twice)
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-action-agent-complete — the agent ends itself
//  9. assert-no-fallback
func TestVerb_Agent_LLMFallback_MidStreamNoFallback(t *testing.T) {
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
	wav, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-fake-primary")
	cut := webhook.LLMReply{Text: "Alpha, bravo,", DropAfterText: true}
	sess.ScriptLLM(cut, cut)
	scriptFakeAgent(sess, fakeOpenAIPrimary(sess, agentVerbOpts{SystemPrompt: agentEchoSystemPrompt}))
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
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	actCtx, acancel := context.WithTimeout(ctx, 40*time.Second)
	defer acancel()
	actionCB, err := sess.WaitCallbackFor(actCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v (fake requests: %d)", err, len(sess.LLMRequests()))
	}
	s.Logf("action/agent-complete body: %s", string(actionCB.Body))
	s.Done()

	s = Step(t, "assert-no-fallback")
	if fb := llmFallbackEntries(&actionCB); len(fb) != 0 {
		s.Errorf("fell back after the caller heard part of the reply: %v", fb)
	}
	if got := actionCB.String("completion_reason"); got != "llm_failure" {
		s.Errorf("completion_reason=%q, want llm_failure", got)
	}
	s.Logf("fake requests: %d", len(sess.LLMRequests()))
	s.Done()
	_ = call.Hangup()
}
