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
