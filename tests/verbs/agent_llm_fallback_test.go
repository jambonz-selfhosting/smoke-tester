// Tests for the agent verb's llm.fallback chain: when the configured LLM
// fails before it says anything, the agent switches to the next LLM in
// llm.fallback and the caller still gets an answer. Each switch is reported
// in the actionHook payload as llm_fallback: [{from, to, reason}].
//
// Real vendors cover a bad key (BadKey), a first-token timeout (Timeout)
// and a missing credential (NoCredentials). The rest point the primary at
// the scriptable fake OpenAI server on the webhook tunnel, for failures a
// real vendor won't produce on demand: an error mid-call after a tool turn,
// a persistent 429, and a stream cut after the caller heard part of a reply.
// The App* tests add the application's fallback LLM (portal setting), which
// joins the end of the chain behind the verb's own fallbacks.
package verbs

import (
	"context"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const llmFallbackTurnWait = 30 * time.Second

func deepseekFallback() []map[string]any {
	return []map[string]any{
		{"vendor": "deepseek", "model": "deepseek-chat", "auth": map[string]any{"apiKey": cfg.DeepseekAPIKey}},
	}
}

// TestVerb_Agent_LLMFallback_BadKey — primary openai with an invalid key,
// fallback deepseek.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_BadKey(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, fallbackCase{
		build: func(*webhook.Session) agentVerbOpts {
			return agentVerbOpts{
				SystemPrompt: agentEchoSystemPrompt,
				LLMVendor:    "openai",
				LLMModel:     "gpt-4o-mini",
				LLMApiKey:    "sk-it-invalid-smoke-key",
				LLMFallback:  deepseekFallback(),
			}
		},
		hops:   [][2]string{{"openai", "deepseek"}},
		reason: regexp.MustCompile(`(?i)401|api key|incorrect|auth`),
	})
}

// TestVerb_Agent_LLMFallback_Timeout — primary deepseek with a working key
// but fallbackTimeoutMs=1, fallback openai: a silent vendor is abandoned
// instead of leaving the caller hanging.
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
	runLLMFallbackCall(t, fallbackCase{
		build: func(*webhook.Session) agentVerbOpts {
			return agentVerbOpts{
				SystemPrompt:      agentEchoSystemPrompt,
				FallbackTimeoutMs: &timeout,
				LLMFallback: []map[string]any{
					{"vendor": "openai", "model": "gpt-4o-mini", "auth": map[string]any{"apiKey": cfg.OpenAIAPIKey}},
				},
			}
		},
		hops:   [][2]string{{"deepseek", "openai"}},
		reason: regexp.MustCompile(`no response within 1ms`),
	})
}

// TestVerb_Agent_LLMFallback_NoCredentials — primary anthropic with no
// inline auth and a label no credential carries (the cluster may hold an
// unlabelled anthropic credential at SP level), so it is skipped at start.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_NoCredentials(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, fallbackCase{
		build: func(*webhook.Session) agentVerbOpts {
			return agentVerbOpts{
				SystemPrompt: agentEchoSystemPrompt,
				LLMVendor:    "anthropic",
				LLMModel:     "claude-sonnet-4-5",
				LLMNoAuth:    true,
				LLMLabel:     "it-no-such-credential",
				LLMFallback:  deepseekFallback(),
			}
		},
		hops:   [][2]string{{"anthropic", "deepseek"}},
		reason: regexp.MustCompile(`No LLM credentials found for anthropic, label:it-no-such-credential`),
	})
}

// TestVerb_Agent_LLMFallback_RateLimit — the fake answers 429 with
// Retry-After: 20, twice. The agent retries once at once (no SDK backoff
// waiting out Retry-After), then falls back: exactly two fake requests and
// deepseek answers within seconds.
//
// Steps: see runLLMFallbackCall, plus
//
//	assert-quick-retry — two fake requests, reply soon after the first
func TestVerb_Agent_LLMFallback_RateLimit(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	limited := webhook.LLMReply{Status: 429, RetryAfter: "20", ErrorCode: "rate_limit_exceeded"}
	var sess *webhook.Session
	cbs := runLLMFallbackCall(t, fallbackCase{
		build: func(s *webhook.Session) agentVerbOpts {
			sess = s
			s.ScriptLLM(limited, limited)
			return fakeOpenAIPrimary(s, agentVerbOpts{SystemPrompt: agentEchoSystemPrompt})
		},
		hops:   [][2]string{{"openai", "deepseek"}},
		reason: regexp.MustCompile(`429`),
	})
	if cbs == nil {
		return
	}

	s := Step(t, "assert-quick-retry")
	reqs := sess.LLMRequests()
	if len(reqs) != 2 {
		s.Errorf("fake LLM got %d requests, want 2 (one quick retry, no SDK retries)", len(reqs))
	}
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 || len(reqs) == 0 {
		s.Fatalf("no llm_response after the 429s (requests=%d)", len(reqs))
	}
	if lag := responses[0].Received.Sub(reqs[0].Received); lag > 10*time.Second {
		s.Errorf("reply %s after the first 429: Retry-After was waited out", lag)
	} else {
		s.Logf("deepseek replied %s after the first 429", lag.Round(time.Millisecond))
	}
	s.Done()
}

// fallbackCase is one runLLMFallbackCall scenario.
type fallbackCase struct {
	build  func(*webhook.Session) agentVerbOpts
	app    *appFallback   // nil: the suite's shared application
	hops   [][2]string    // the llm_fallback switches expected, {from, to} vendors in order
	reason *regexp.Regexp // matches the last switch's reason
}

// appFallback configures the per-test application's fallback LLM: a stored
// deepseek credential, with the portal flag on or off.
type appFallback struct {
	enabled bool
}

// runLLMFallbackCall places one agent call with the opts c.build returns,
// speaks the echo prompt, and asserts the reply came through and the
// actionHook reports exactly the switches in c.hops. Returns every callback
// seen, or nil if the test passed without running.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-fallback
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. send-prompt
//  8. assert-llm-response — the fallback LLM echoed our keywords
//  9. hangup
//
// 10. wait-action-agent-complete
// 11. assert-llm-fallback — llm_fallback in the actionHook payload
func runLLMFallbackCall(t *testing.T, c fallbackCase) []webhook.Callback {
	t.Helper()
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return nil
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)
	wavs := ensurePromptWAVs(ctx, t, agentEchoPrompt)
	_, sess := claimSession(t)
	appSID := provisionAppFallback(ctx, t, c.app)

	s = Step(t, "script-agent-verb-fallback")
	ScriptAgent(sess, c.build(sess))
	s.Done()

	call := startAgentCall(ctx, t, uas, sess, appSID)
	all := speakAndWait(ctx, t, call, sess, "send-prompt", wavs[0], nil, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "turn_end")) > 0
	})

	s = Step(t, "assert-llm-response")
	reply := llmResponseText(all)
	if !containsAny(reply, "alpha", "bravo", "charlie", "delta") {
		s.Errorf("fallback LLM did not echo our prompt; llm_response text: %q", reply)
	} else {
		s.Logf("llm_response: %q", reply)
	}
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	actionCB := awaitAgentComplete(ctx, t, sess, all)
	assertFallbacks(t, actionCB, c.hops, c.reason)
	return all
}

// TestVerb_Agent_LLMFallback_MidCallToolHistory — the primary (fake openai)
// runs a tool-call turn, then fails with 402 insufficient_quota on the next
// turn. Deepseek takes over and must answer from the tool result in the
// carried-over history: the secret word exists nowhere else, and the
// toolHook must not be called a second time.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav (x2)
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
	wavs := ensurePromptWAVs(ctx, t, "Please get the secret word.", "What is the secret word?")
	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-fake-primary")
	sess.ScriptLLM(
		webhook.LLMReply{ToolCalls: []webhook.LLMToolCall{{ID: "call_smoke1", Name: "get_secret_word", Args: "{}"}}},
		webhook.LLMReply{Text: "I have fetched the secret word."},
		webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"},
	)
	ScriptAgent(sess, fakeOpenAIPrimary(sess, agentVerbOpts{
		SystemPrompt: "You are a voice assistant. When the user asks you to get the secret word, call get_secret_word. " +
			"When the user asks what the secret word is, answer with the word from the earlier tool result in this " +
			"conversation. Do not call the tool again.",
		Tools: []map[string]any{{
			"name":        "get_secret_word",
			"description": "Fetches the secret word.",
			"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
		}},
	}))
	// after ScriptAgent, which acks agent-tool with an empty body
	sess.ScriptActionHookBody("agent-tool", []byte(`{"word":"`+secretWord+`"}`))
	s.Done()

	call := startAgentCall(ctx, t, uas, sess, "")
	all := speakAndWait(ctx, t, call, sess, "turn-1-ask-for-tool", wavs[0], nil, func(c []webhook.Callback) bool {
		return agentTurnEndNamesTool(c, "get_secret_word")
	})
	all = speakAndWait(ctx, t, call, sess, "turn-2-ask-for-word", wavs[1], all, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "turn_end")) >= 2
	})

	s = Step(t, "assert-word-from-history")
	reply := llmResponseText(all)
	if !strings.Contains(reply, secretWord) {
		s.Errorf("deepseek did not answer %q from the carried-over tool result; llm_response: %q", secretWord, reply)
	}
	if n := countHook(all, "action/agent-tool"); n != 1 {
		s.Errorf("toolHook called %d times, want 1 (a second call means the history did not carry the result)", n)
	}
	s.Logf("llm_response: %q", reply)
	s.Done()

	s = Step(t, "assert-fake-requests")
	reqs := sess.LLMRequests()
	if len(reqs) != 3 {
		s.Errorf("fake LLM got %d requests, want 3 (tool call, continuation, 402)", len(reqs))
	} else if !hasMessageRole(reqs[1], "tool") {
		s.Errorf("continuation request carried no tool result: %v", reqs[1].Body["messages"])
	}
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	actionCB := awaitAgentComplete(ctx, t, sess, all)
	assertFallbacks(t, actionCB, [][2]string{{"openai", "deepseek"}}, regexp.MustCompile(`402|quota`))
}

// TestVerb_Agent_LLMFallback_MidStreamNoFallback — the fake streams part of
// a reply and cuts the connection. The caller already heard that text, so
// the agent must not replay the turn anywhere.
//
// Steps: see runNoFallbackCall.
func TestVerb_Agent_LLMFallback_MidStreamNoFallback(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runNoFallbackCall(t, webhook.LLMReply{Text: "Alpha, bravo,", DropAfterText: true}, nil,
		func(s *webhook.Session) agentVerbOpts {
			return fakeOpenAIPrimary(s, agentVerbOpts{SystemPrompt: agentEchoSystemPrompt})
		})
}

// TestVerb_Agent_LLMFallback_App — the verb names only a primary (fake
// openai, 402); the application's fallback LLM (stored deepseek credential)
// takes over.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_App(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, fallbackCase{
		build: func(s *webhook.Session) agentVerbOpts {
			s.ScriptLLM(webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"})
			return fakePrimaryOnly(s, nil)
		},
		app:    &appFallback{enabled: true},
		hops:   [][2]string{{"openai", "deepseek"}},
		reason: regexp.MustCompile(`402`),
	})
}

// TestVerb_Agent_LLMFallback_VerbThenApp — the verb's own fallback (openai
// with a bad key) fails too; the application's fallback is the last link.
//
// Steps: see runLLMFallbackCall.
func TestVerb_Agent_LLMFallback_VerbThenApp(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runLLMFallbackCall(t, fallbackCase{
		build: func(s *webhook.Session) agentVerbOpts {
			s.ScriptLLM(webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"})
			return fakePrimaryOnly(s, []map[string]any{
				{"vendor": "openai", "model": "gpt-4o-mini", "auth": map[string]any{"apiKey": "sk-it-invalid-smoke-key"}},
			})
		},
		app:    &appFallback{enabled: true},
		hops:   [][2]string{{"openai", "openai"}, {"openai", "deepseek"}},
		reason: regexp.MustCompile(`(?i)401|api key|incorrect`),
	})
}

// TestVerb_Agent_LLMFallback_AppOptOut — `fallback: []` on the verb turns
// the application's fallback off.
//
// Steps: see runNoFallbackCall.
func TestVerb_Agent_LLMFallback_AppOptOut(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runNoFallbackCall(t, webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"}, &appFallback{enabled: true},
		func(s *webhook.Session) agentVerbOpts { return fakePrimaryOnly(s, []map[string]any{}) })
}

// TestVerb_Agent_LLMFallback_AppFlagOff — the application names a fallback
// LLM but its "use a fallback LLM" flag is off: no fallback.
//
// Steps: see runNoFallbackCall.
func TestVerb_Agent_LLMFallback_AppFlagOff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	runNoFallbackCall(t, webhook.LLMReply{Status: 402, ErrorCode: "insufficient_quota"}, &appFallback{enabled: false},
		func(s *webhook.Session) agentVerbOpts { return fakePrimaryOnly(s, nil) })
}

// runNoFallbackCall scripts the fake primary with fail, speaks the echo
// prompt and asserts the agent ends itself without switching LLMs: one fake
// request, no llm_fallback, completion_reason llm_failure.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-fake-primary
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. send-prompt — until the agent ends itself
//  8. wait-action-agent-complete
//  9. assert-no-fallback
func runNoFallbackCall(t *testing.T, fail webhook.LLMReply, app *appFallback, build func(*webhook.Session) agentVerbOpts) {
	t.Helper()
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	wavs := ensurePromptWAVs(ctx, t, agentEchoPrompt)
	_, sess := claimSession(t)
	appSID := provisionAppFallback(ctx, t, app)

	s = Step(t, "script-agent-verb-fake-primary")
	sess.ScriptLLM(fail)
	ScriptAgent(sess, build(sess))
	s.Done()

	call := startAgentCall(ctx, t, uas, sess, appSID)
	all := speakAndWait(ctx, t, call, sess, "send-prompt", wavs[0], nil, func(c []webhook.Callback) bool {
		return findHook(c, "action/agent-complete") != nil
	})
	actionCB := awaitAgentComplete(ctx, t, sess, all)

	s = Step(t, "assert-no-fallback")
	if fb := llmFallbackEntries(actionCB); len(fb) != 0 {
		s.Errorf("switched LLMs, want none: %v", fb)
	}
	if got := actionCB.String("completion_reason"); got != "llm_failure" {
		s.Errorf("completion_reason=%q, want llm_failure", got)
	}
	if n := len(sess.LLMRequests()); n != 1 {
		s.Errorf("fake LLM got %d requests, want 1", n)
	}
	s.Done()
	_ = call.Hangup()
}

// --- helpers ---

func fakeLLMBaseURL(sess *webhook.Session) string {
	return webhookSrv.PublicURL() + "/llm/" + url.PathEscape(sess.ID()) + "/v1"
}

// fakeOpenAIPrimary points the agent's main LLM at sess's fake LLM, with
// deepseek as the fallback.
func fakeOpenAIPrimary(sess *webhook.Session, opts agentVerbOpts) agentVerbOpts {
	opts.LLMVendor = "openai"
	opts.LLMModel = "gpt-fake"
	opts.LLMAuth = map[string]any{"apiKey": "sk-fake", "baseURL": fakeLLMBaseURL(sess)}
	opts.LLMFallback = deepseekFallback()
	return opts
}

// fakePrimaryOnly is fakeOpenAIPrimary with the verb's own fallback list
// set to fallback (nil: none; empty: opts out of the application's).
func fakePrimaryOnly(sess *webhook.Session, fallback []map[string]any) agentVerbOpts {
	opts := fakeOpenAIPrimary(sess, agentVerbOpts{SystemPrompt: agentEchoSystemPrompt})
	opts.LLMFallback = fallback
	return opts
}

// provisionAppFallback creates an application whose fallback LLM is deepseek
// on a stored credential, returning its SID ("" when app is nil).
func provisionAppFallback(ctx context.Context, t *testing.T, app *appFallback) string {
	t.Helper()
	if app == nil {
		return ""
	}
	s := Step(t, "provision-app-fallback")
	defer s.Done()
	suffix := "llmfb-" + strings.ToLower(strings.TrimPrefix(t.Name(), "TestVerb_Agent_LLMFallback_"))
	label := provision.Name(suffix)
	client.ManagedAccountLlmCredential(t, ctx, suite.AccountSID, provision.LlmCredentialCreate{
		Vendor: "deepseek", Label: label, APIKey: cfg.DeepseekAPIKey,
	})
	return provisionWebhookApp(t, ctx, suffix, func(a *provision.ApplicationCreate) {
		if app.enabled {
			a.UseForFallbackLlm = 1
		}
		a.FallbackLlmVendor, a.FallbackLlmModel, a.FallbackLlmLabel = "deepseek", "deepseek-chat", label
	})
}

func ensurePromptWAVs(ctx context.Context, t *testing.T, prompts ...string) []string {
	t.Helper()
	s := Step(t, "ensure-prompt-wav")
	wavs := make([]string, len(prompts))
	for i, p := range prompts {
		w, err := tts.EnsureWAV(ctx, "testdata/agent", p, tts.PromptOptions{Model: "aura-asteria-en"})
		if err != nil {
			s.Fatalf("EnsureWAV %q: %v", p, err)
		}
		wavs[i] = w
	}
	s.Done()
	return wavs
}

// startAgentCall places the call (on appSID, or the suite's application when
// empty), answers with silence and waits for STT to arm.
func startAgentCall(ctx context.Context, t *testing.T, uas *UAS, sess *webhook.Session, appSID string) *jsip.Call {
	t.Helper()
	s := Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120), func(c *provision.CallCreate) {
		if appSID != "" {
			c.ApplicationSID = appSID
		}
	})
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
	return call
}

// speakAndWait sends wav, then collects callbacks until done holds for
// everything seen so far (seen included). Returns seen plus the new ones.
func speakAndWait(ctx context.Context, t *testing.T, call *jsip.Call, sess *webhook.Session, step, wav string,
	seen []webhook.Callback, done func([]webhook.Callback) bool) []webhook.Callback {
	t.Helper()
	s := Step(t, step)
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, llmFallbackTurnWait)
	got := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return done(append(append([]webhook.Callback(nil), seen...), c...))
	})
	cancel()
	all := append(seen, got...)
	s.Logf("events so far: %s", summarizeEventTypes(all))
	s.Done()
	return all
}

// awaitAgentComplete returns the agent's actionHook callback, from seen if a
// wait already drained it.
func awaitAgentComplete(ctx context.Context, t *testing.T, sess *webhook.Session, seen []webhook.Callback) *webhook.Callback {
	t.Helper()
	s := Step(t, "wait-action-agent-complete")
	defer s.Done()
	if cb := findHook(seen, "action/agent-complete"); cb != nil {
		return cb
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Logf("action/agent-complete body: %s", string(cb.Body))
	return &cb
}

// assertFallbacks checks the actionHook's llm_fallback lists exactly hops,
// with the last reason matching reasonRe.
func assertFallbacks(t *testing.T, actionCB *webhook.Callback, hops [][2]string, reasonRe *regexp.Regexp) {
	t.Helper()
	s := Step(t, "assert-llm-fallback")
	defer s.Done()
	fb := llmFallbackEntries(actionCB)
	if len(fb) != len(hops) {
		s.Fatalf("want %d llm_fallback entries, got %d: %s", len(hops), len(fb), string(actionCB.Body))
	}
	var reason string
	for i, hop := range hops {
		from, _ := fb[i]["from"].(map[string]any)
		to, _ := fb[i]["to"].(map[string]any)
		reason, _ = fb[i]["reason"].(string)
		if from["vendor"] != hop[0] || to["vendor"] != hop[1] {
			s.Errorf("llm_fallback[%d] %v -> %v, want %s -> %s", i, from["vendor"], to["vendor"], hop[0], hop[1])
		}
		s.Logf("llm_fallback[%d]: %s -> %s (%s)", i, from["vendor"], to["vendor"], reason)
	}
	if !reasonRe.MatchString(reason) {
		s.Errorf("last llm_fallback reason %q does not match %s", reason, reasonRe)
	}
	if got := actionCB.String("completion_reason"); got == "llm_failure" {
		s.Errorf("completion_reason=llm_failure: the fallback should have kept the agent alive")
	}
}

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

func llmResponseText(cbs []webhook.Callback) string {
	var parts []string
	for _, r := range findAgentEvents(cbs, "llm_response") {
		parts = append(parts, strings.ToLower(r.String("response")))
	}
	return strings.Join(parts, " ")
}

func containsAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
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

func hasMessageRole(req webhook.LLMRequest, role string) bool {
	msgs, _ := req.Body["messages"].([]any)
	for _, m := range msgs {
		if mm, _ := m.(map[string]any); mm["role"] == role {
			return true
		}
	}
	return false
}
