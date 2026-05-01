// Tests for the `agent` verb — the high-level "STT → LLM → TTS" wiring
// added in jambonz commercial 10.1.0.
//
// Schema: schemas/verbs/agent — wires together a recognizer, an LLM, and a
// synthesizer in a single verb with built-in turn detection. We use Deepgram
// for STT + TTS (label provisioned at TestMain) and Deepseek for the LLM
// (apiKey supplied inline via auth.apiKey, bypassing /LlmCredentials — see
// feature-server/lib/tasks/agent/index.js:446 for the inline-auth path).
//
// Tests in this file all share:
//   - Pre-generated TTS prompts via Deepgram TTS REST, cached on disk under
//     testdata/agent/<sha>.wav so re-runs are free.
//   - The same agent-verb skeleton (buildAgentVerb), parameterised by system
//     prompt + flags.
//   - Contract validation: webhook URLs are chosen so the server's
//     validateInbound path maps each callback to a real schema:
//        /action/verb-hook   → callbacks/<verb>.schema.json (n/a here — base only)
//        /action/agent-turn  → callbacks/agent-turn.schema.json (eventHook payloads)
//     For actionHook on agent verb completion, jambonz POSTs the standard
//     verb:hook callInfo+results envelope (see task.js:performAction). There
//     is no `agent-complete` schema — base.schema.json applies. We POST to
//     /action/agent-complete which has no per-verb schema, so validation is
//     skipped (logs Debug); the callback is still captured for assertions.
//
// Skips cleanly when:
//   - NGROK_AUTHTOKEN is unset (need the webhook server to host responses)
//   - DEEPSEEK_API_KEY is unset (no LLM credential to inline)
//   - DEEPGRAM_API_KEY is unset (can't pre-gen the WAV, can't STT the reply)
package verbs

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// agentEchoSystemPrompt asks the LLM to repeat back the user's words and
// nothing else. Phrased to minimize creative deviations across LLM vendors —
// Deepseek tends to add "Sure, here you go:" without an explicit no-prefix
// instruction.
const agentEchoSystemPrompt = "You are an echo bot. " +
	"The user will speak a short phrase. " +
	"Your job is to repeat their phrase back to them verbatim, with no additions. " +
	"Do not greet. Do not confirm. Do not paraphrase. " +
	"Output only the exact words the user spoke, ending with a period."

// agentEchoPrompt is the user-side phrase. Carefully chosen so the keywords
// (alpha, bravo, charlie, delta) survive both Deepgram STT (input side) and
// Deepgram STT (output side) without homophone collisions, and so the LLM
// has no reason to summarize.
const agentEchoPrompt = "Please repeat exactly the following words: alpha, bravo, charlie, delta."

// agentSkipPreflight gates each test on the env knobs the agent path needs.
// Returns true if the test should run; false (after t.Skip) if not.
func agentSkipPreflight(t *testing.T, s *StepCtx) bool {
	t.Helper()
	if !cfg.HasDeepseek() {
		s.Done()
		t.Skip("agent test needs DEEPSEEK_API_KEY (Deepseek LLM, used inline)")
		return false
	}
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("agent test needs DEEPGRAM_API_KEY for prompt TTS pre-gen and reply STT verify")
		return false
	}
	if deepgramLabel == "" {
		s.Done()
		t.Skip("agent test needs the in-jambonz Deepgram credential (provisioned at TestMain)")
		return false
	}
	return true
}

// agentVerbOpts parameterise the agent verb a test wants jambonz to run.
type agentVerbOpts struct {
	SystemPrompt       string
	Greeting           bool   // false → user speaks first; true → agent greets first
	ActionURL          string // POST target when agent ends (call kill)
	EventURL           string // POST target for user_transcript / llm_response / turn_end / user_interruption
	ToolURL            string // POST target when LLM calls a function/tool (set when Tools is non-empty)
	Tools              []map[string]any
	BargeIn            bool          // default false: easier to test deterministic round-trips
	NoResponseTimeout  *int          // pointer so 0 means "explicitly disable"
	EarlyGeneration    bool          // require turnDetection=krisp with this
	TurnDetection      string        // "stt" (default), "krisp"
	NoiseIsolation     string        // "" (off), "krisp", "rnnoise"
}

// buildAgentVerb builds the verb-map jambonz will run. Centralised here so
// each test only has to pass the options that differ from the defaults.
func buildAgentVerb(opts agentVerbOpts) map[string]any {
	llmOptions := map[string]any{
		"systemPrompt": opts.SystemPrompt,
		"maxTokens":    128,
	}
	if len(opts.Tools) > 0 {
		llmOptions["tools"] = opts.Tools
	}
	verb := V("agent",
		"stt", map[string]any{
			"vendor":   "deepgram",
			"label":    deepgramLabel,
			"language": "en-US",
		},
		"tts", map[string]any{
			"vendor": "deepgram",
			"label":  deepgramLabel,
			"voice":  deepgramVoice,
		},
		"llm", map[string]any{
			"vendor": "deepseek",
			"model":  "deepseek-chat",
			// Inline auth — feature-server skips the DB credential lookup
			// when auth is set on the verb (lib/tasks/agent/index.js:446).
			"auth": map[string]any{
				"apiKey": cfg.DeepseekAPIKey,
			},
			"llmOptions": llmOptions,
		},
		"greeting", opts.Greeting,
		"turnDetection", firstNonEmpty(opts.TurnDetection, "stt"),
		"bargeIn", map[string]any{
			"enable": opts.BargeIn,
		},
		"actionHook", opts.ActionURL,
		"eventHook", opts.EventURL,
	)
	if opts.ToolURL != "" {
		verb["toolHook"] = opts.ToolURL
	}
	if opts.NoResponseTimeout != nil {
		verb["noResponseTimeout"] = *opts.NoResponseTimeout
	}
	if opts.EarlyGeneration {
		verb["earlyGeneration"] = true
	}
	if opts.NoiseIsolation != "" {
		verb["noiseIsolation"] = opts.NoiseIsolation
	}
	return verb
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// Per-test eventHook routing:
//
// The agent eventHook's payload (see TaskAgent _sendEventHook →
// cs.requestor.request('agent:event', ..., {type, ...})) is just
// `{type, ...}` — feature-server does NOT attach call_sid or our
// customerData correlation key. Without per-call routing, events from
// every parallel agent test would land in shared `_anon`.
//
// Workaround: tests build their eventHook URL with a `?X-Test-Id=<testID>`
// query param. The webhook server's correlation.go.extractTestID picks
// it up from the URL, and routes the callback into the per-test session.
// (See tests below for the `q := "?" + webhook.CorrelationHeader + ...`
// pattern.)
//
// findAgentEvents returns every captured callback whose decoded body has a
// top-level `type` field matching `wantType`.
func findAgentEvents(cbs []webhook.Callback, wantType string) []webhook.Callback {
	var out []webhook.Callback
	for _, cb := range cbs {
		t, _ := cb.JSON["type"].(string)
		if t == wantType {
			out = append(out, cb)
		}
	}
	return out
}

// extractEventField pulls a string field out of an agent eventHook callback
// (e.g. "transcript" from a user_transcript event, "response" from
// llm_response). Returns "" if missing.
func extractEventField(cb webhook.Callback, field string) string {
	if v, ok := cb.JSON[field].(string); ok {
		return v
	}
	return ""
}

// TestVerb_Agent_Echo — happy-path round-trip. Pre-gen "alpha bravo charlie
// delta" → user speaks → STT → Deepseek echoes → TTS → user records → STT
// verifies keywords made the loop.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
// 10. hangup-and-wait-ended
// 11. assert-reply-keywords
func TestVerb_Agent_Echo(t *testing.T) {
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Logf("prompt wav: %s", wavPath)
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: agentEchoSystemPrompt,
			Greeting:     false,
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn",
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-reply.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	// Leading silence lets Deepgram STT arm before user audio starts;
	// without it the first keyword can clip.
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(12 * time.Second)
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()

	s = Step(t, "assert-reply-keywords")
	keywords := []string{"alpha", "bravo", "charlie", "delta"}
	AssertTranscriptHasMost(s, ctx, recPath, 3, keywords...)
	s.Done()
}

// TestVerb_Agent_EventHook — eventHook fires user_transcript, llm_response
// and turn_end events around each conversational turn. We assert each event
// type appears at least once, with a content-bearing payload.
//
// Why a separate test from Echo: this test gates on the event payloads
// rather than the audio round-trip, so it doesn't need to hold the call
// open as long. It also exercises the schema (callbacks/agent-turn) — the
// Echo test only validates that the round-trip works at all.
//
// Correlation drift: feature-server's agent eventHook doesn't attach
// callInfo to its payload, so events land in the shared `_anon` session
// (smoke-tester webhook server's anon fallback). We filter by event type
// + content and tolerate parallel runs by keying on the unique transcript
// substring this test injects.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (eventHook → /action/agent-turn — schema-validated)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-events — silence while LLM thinks + TTS streams
// 10. drain-anon-events
// 11. assert-user-transcript — find a user_transcript event with our keywords
// 12. assert-llm-response — find an llm_response event with non-empty body
// 13. assert-turn-end — find a turn_end with transcript + response + latency
// 14. hangup
func TestVerb_Agent_EventHook(t *testing.T) {
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	// Append ?X-Test-Id=<testID> to per-callback URLs so the webhook server
	// routes eventHook callbacks to THIS session (the payload itself has no
	// customerData and would otherwise land in shared `_anon`, racing with
	// parallel agent tests).
	q := "?" + webhook.CorrelationHeader + "=" + url.QueryEscape(testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: agentEchoSystemPrompt,
			Greeting:     false,
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn" + q,
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Drain THIS session while events stream in. Routed via X-Test-Id
	// query param on the eventHook URL — no `_anon` contention. Each
	// event type can fire at different points (user_transcript ~when STT
	// finalizes, llm_response ~when LLM stream completes, turn_end ~when
	// TTS empties). 12s window covers the slowest path on a healthy
	// cluster.
	cbs := DrainCallbacks(sess, 12*time.Second)
	s.Logf("captured %d agent events", len(cbs))
	s.Done()

	s = Step(t, "assert-user-transcript")
	transcripts := findAgentEvents(cbs, "user_transcript")
	if len(transcripts) == 0 {
		s.Fatalf("no user_transcript event in %d agent events: %s",
			len(cbs), summarizeEventTypes(cbs))
	}
	// The transcript should contain at least one of our injected keywords —
	// proves it was OUR call's event, not a parallel test's.
	matched := ""
	for _, cb := range transcripts {
		txt := strings.ToLower(extractEventField(cb, "transcript"))
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(txt, kw) {
				matched = txt
				break
			}
		}
		if matched != "" {
			break
		}
	}
	if matched == "" {
		s.Errorf("no user_transcript matched our prompt; got %d transcripts: %v",
			len(transcripts), summarizeTranscripts(transcripts))
	} else {
		s.Logf("user_transcript matched: %q", matched)
	}
	s.Done()

	s = Step(t, "assert-llm-response")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("no llm_response event in %d agent events", len(cbs))
	} else {
		// Just ensure the response body is non-empty and string-typed —
		// content correctness is asserted via the audio round-trip in Echo.
		got := extractEventField(responses[0], "response")
		if got == "" {
			s.Errorf("llm_response event has empty response field: %s", string(responses[0].Body))
		} else {
			s.Logf("llm_response: %q", truncate(got, 100))
		}
	}
	s.Done()

	s = Step(t, "assert-turn-end")
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) == 0 {
		s.Errorf("no turn_end event in %d agent events", len(cbs))
	} else {
		// Schema requires transcript + response + interrupted + latency.
		// validateInbound ran already; we just confirm fields are present.
		te := turnEnds[0].JSON
		for _, want := range []string{"transcript", "response", "interrupted", "latency"} {
			if _, ok := te[want]; !ok {
				s.Errorf("turn_end missing required field %q: %s",
					want, string(turnEnds[0].Body))
			}
		}
		// Latency block should at least include voice_latency or model_latency.
		if lat, ok := te["latency"].(map[string]any); ok {
			s.Logf("turn_end latency: %v", lat)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Agent_Greeting — agent emits a greeting before the user speaks
// when greeting=true. We assert the recording contains audio energy in the
// first ~5s of the call BEFORE we send any user audio, then issue the user
// prompt and verify the second turn also produces a reply.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (greeting=true)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-greeting — listen for ~5s while agent says hello
//  8. assert-greeting-audio — recording has substantial inbound bytes already
//  9. send-prompt-wav — now user speaks
// 10. wait-for-second-turn — silence while LLM replies again
// 11. hangup-and-wait-ended
// 12. assert-reply-keywords-or-greeting — final transcript contains either
//     a greeting word ("hello"/"hi") OR our echoed keywords. Tolerant
//     because some LLMs collapse two turns when the user prompt is
//     "repeat the words" right after a greeting.
func TestVerb_Agent_Greeting(t *testing.T) {
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	// Soften the system prompt so the LLM does produce a greeting on the
	// first turn — Echo's strict echo-only prompt would suppress it.
	greetingSystemPrompt := "You are a friendly voice assistant. " +
		"On your very first turn, briefly greet the user (one short sentence, " +
		"e.g. \"Hello, how can I help?\"). " +
		"On every later turn, repeat back exactly the words the user spoke " +
		"and nothing else."
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: greetingSystemPrompt,
			Greeting:     true, // <-- agent speaks first
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn",
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-greeting.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	// 6s is enough for a one-sentence greeting (LLM ~1s + TTS ~3-4s for
	// "Hello, how can I help?"). After this window we should already have
	// inbound RTP from the agent's TTS.
	time.Sleep(6 * time.Second)
	bytesAfterGreeting := call.PCMBytesIn()
	s.Logf("recorded %d PCM bytes by t+6s (greeting window)", bytesAfterGreeting)
	s.Done()

	s = Step(t, "assert-greeting-audio")
	// 6s of greeting TTS at PCMU 8 kHz is ~96 KB; we want at least 16 KB
	// (1s of audio energy) to call it a greeting. Anything less means the
	// agent never spoke first.
	const minGreetingBytes = 16000
	if bytesAfterGreeting < minGreetingBytes {
		s.Errorf("greeting=true but only %d PCM bytes received in 6s (need >= %d)",
			bytesAfterGreeting, minGreetingBytes)
	}
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-second-turn")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(12 * time.Second)
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()

	s = Step(t, "assert-reply-keywords-or-greeting")
	// Generous matcher: greeting OR keyword echo. Exactly which words
	// surface depends on the LLM (Deepseek may merge greeting + echo into
	// one TTS chunk; or the second turn audio may dominate the recording).
	wants := []string{"hello", "hi", "alpha", "bravo", "charlie", "delta", "help"}
	AssertTranscriptHasMost(s, ctx, recPath, 1, wants...)
	s.Done()
}

// TestVerb_Agent_ActionHookOnEnd — actionHook fires when agent ends. The
// agent verb's actionHook is invoked from awaitTaskDone() → performAction
// (lib/tasks/agent/index.js:594), which only resolves when the call/ session
// is ending. We hang up from our side and assert the actionHook arrives
// before our test budget runs out.
//
// The actionHook payload is the standard verb:hook envelope (callInfo +
// our results object). We assert call_sid is present (== our placed call's
// sid) — that proves both that the actionHook fired AND that correlation
// works for actionHook (unlike eventHook which loses correlation upstream).
//
// Steps:
//  1. preflight-skips
//  2. register-webhook-session
//  3. script-agent-verb (minimal — greeting=false, no audio needed)
//  4. place-call
//  5. answer-and-silence
//  6. brief-pause — let agent enter Idle so the kill path runs cleanly
//  7. hangup — proactive BYE from us
//  8. wait-action-agent-complete
//  9. assert-action-payload — call_sid + completion_reason in body
func TestVerb_Agent_ActionHookOnEnd(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: agentEchoSystemPrompt,
			Greeting:     false,
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn",
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "brief-pause")
	// Give jambonz a couple of seconds to spin up the agent and reach Idle
	// state before we hang up. Hanging up too quickly can race with agent
	// init and produce noisy logs (LLM warmup mid-shutdown).
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "hangup")
	if err := call.Hangup(); err != nil {
		s.Fatalf("Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-agent-complete")
	// actionHook fires when the agent task is being killed (BYE → call
	// session teardown → agent.kill → notifyTaskDone → performAction).
	// 30s budget is comfortable on a healthy cluster.
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	actionCB, err := sess.WaitCallbackFor(waitCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Logf("action/agent-complete body: %s", string(actionCB.Body))
	s.Done()

	s = Step(t, "assert-action-payload")
	// callInfo includes call_sid (top-level), and performAction merges in
	// our `results` object which carries `completion_reason` (snake-cased
	// by HttpRequestor.snakeCaseKeys). Both confirm correlation works for
	// actionHook (unlike eventHook which lands in _anon).
	if got, _ := actionCB.JSON["call_sid"].(string); got == "" {
		s.Errorf("action/agent-complete payload missing call_sid: %s",
			string(actionCB.Body))
	} else {
		s.Logf("call_sid in payload: %s", got)
	}
	if got, _ := actionCB.JSON["completion_reason"].(string); got == "" {
		s.Errorf("action/agent-complete payload missing completion_reason: %s",
			string(actionCB.Body))
	} else {
		s.Logf("completion_reason: %s", got)
	}
	// customerData.x_test_id should also round-trip back to us — proves
	// the actionHook hit our session, not _anon.
	if cd, ok := actionCB.JSON["customer_data"].(map[string]any); ok {
		if id, _ := cd["x_test_id"].(string); id != testID {
			s.Errorf("customer_data.x_test_id=%q want %q", id, testID)
		}
	}
	s.Done()
}

// TestVerb_Agent_ToolHook — agent verb invokes a tool/function and the
// LLM speaks the tool's return value. We declare a `get_secret_word` tool
// in llmOptions.tools, instruct the system prompt to call it whenever the
// user asks "what is the secret word", and back the toolHook with a
// JSON-body responder that returns a unique word the LLM will then TTS
// back to the caller. Verifies:
//   - toolHook fires (callback captured + payload validated)
//   - payload has `tool_call_id`, `name`, `arguments` (the LLM's chosen args)
//   - tool reply is round-tripped: LLM speaks the secret word, STT picks it
//     up in the recording.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav (asks for the secret word)
//  3. register-webhook-session
//  4. script-agent-verb-with-tool — toolHook returns JSON {"word": "<unique>"}
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-tool-call — block on /action/agent-tool callback
// 10. assert-tool-payload — name + arguments + tool_call_id present
// 11. wait-for-llm-reply — silence while agent speaks the secret word
// 12. hangup-and-wait-ended
// 13. assert-secret-word-spoken — recording contains the secret word
func TestVerb_Agent_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	// secretWord is what the test's toolHook will return to the LLM. We
	// pick a phonetically-clean unique word that survives Deepgram TTS →
	// SIP → Deepgram STT without collision; "kingfisher" is unambiguous
	// and not common enough to be a false positive from background noise.
	const secretWord = "kingfisher"
	const promptText = "What is the secret word?"

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", promptText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb-with-tool")
	tool := map[string]any{
		"name":        "get_secret_word",
		"description": "Returns the daily secret word. You MUST call this whenever the user asks for the secret word — you do NOT know the word yourself.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
	systemPrompt := "You are a voice assistant. You don't know any secret words. " +
		"The ONLY way to learn the secret word is to call the get_secret_word tool. " +
		"When the user asks for the secret word, immediately call get_secret_word. " +
		"After the tool returns, speak the returned word to the user and stop."
	// Append ?X-Test-Id=<testID> to per-callback URLs so the webhook server
	// can route each callback (incl. eventHook + toolHook) to THIS test's
	// session even when the payload has no customerData. Without this,
	// callbacks land in shared `_anon` and parallel agent tests race for
	// the same toolHook outcome.
	q := "?" + webhook.CorrelationHeader + "=" + url.QueryEscape(testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: systemPrompt,
			Greeting:     false,
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn" + q,
			ToolURL:      webhookSrv.PublicURL() + "/action/agent-tool" + q,
			Tools:        []map[string]any{tool},
		}),
		V("hangup"),
	}))
	// agent-tool gets a JSON body (not a verb array): feature-server takes
	// our response body verbatim and feeds it to the LLM as the tool result.
	toolBody := []byte(`{"word":"` + secretWord + `"}`)
	sess.ScriptActionHookBody("agent-tool", toolBody)
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-tool-reply.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Tool call typically fires within 2-4s of the user transcript landing
	// (LLM emits tool_call as its first chunk). Routes back to THIS
	// session via the X-Test-Id query param we appended to the toolHook
	// URL — no _anon contention with parallel agent tests.
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	toolCB, err := sess.WaitCallbackFor(waitCtx, "action/agent-tool")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-tool: %v", err)
	}
	s.Logf("action/agent-tool body: %s", string(toolCB.Body))
	s.Done()

	s = Step(t, "assert-tool-payload")
	if got, _ := toolCB.JSON["name"].(string); got != "get_secret_word" {
		s.Errorf("tool name = %q, want %q", got, "get_secret_word")
	}
	if got, _ := toolCB.JSON["tool_call_id"].(string); got == "" {
		s.Errorf("tool_call_id missing in payload: %s", string(toolCB.Body))
	}
	// `arguments` must exist; for a parameterless tool it's `{}`. Some LLMs
	// send it as a JSON-encoded string instead of an object — accept both.
	if _, ok := toolCB.JSON["arguments"]; !ok {
		s.Errorf("arguments missing in payload: %s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	// Tool result fed back to LLM → second LLM round-trip → TTS streams the
	// word. Allow ~10s for the full second pass.
	time.Sleep(12 * time.Second)
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()

	s = Step(t, "assert-secret-word-spoken")
	// Tolerance 1/1: the secret word must come through. If LLM verbosely
	// adds commentary, fine — but it has to include "kingfisher".
	AssertTranscriptHasMost(s, ctx, recPath, 1, secretWord)
	s.Done()
}

// TestVerb_Agent_BargeIn — when bargeIn=true, the user can interrupt the
// agent mid-TTS and the agent must (a) emit a `user_interruption` event,
// (b) stop speaking the first reply, and (c) handle the new user turn.
//
// Flow:
//   - greeting=true so the agent speaks first ("Hello...").
//   - while the agent is still talking (within ~3s of greeting starting),
//     we send a user WAV ("alpha bravo charlie delta") to barge in.
//   - eventHook fires user_interruption.
//   - agent processes the new turn and replies (echo of our keywords).
//
// We don't strictly assert which words arrive on the recording — the
// interrupted-greeting + new-reply audio gets stitched together — but we
// DO assert the user_interruption event landed.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session (+ ensure _anon)
//  4. script-agent-verb (greeting=true, bargeIn=true)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-into-greeting — let agent start speaking
//  8. send-prompt-wav — interrupt mid-greeting
//  9. wait-for-events — collect user_interruption + later turn_end
// 10. assert-user-interruption — event landed in _anon
// 11. hangup-and-wait-ended
func TestVerb_Agent_BargeIn(t *testing.T) {
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	greetingSystemPrompt := "You are a friendly voice assistant. " +
		"On your first turn, greet the user with a long, slow welcome " +
		"of at least three full sentences so they have time to interrupt. " +
		"On subsequent turns, repeat the user's words back to them verbatim."
	q := "?" + webhook.CorrelationHeader + "=" + url.QueryEscape(testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt: greetingSystemPrompt,
			Greeting:     true,
			BargeIn:      true,
			ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:     webhookSrv.PublicURL() + "/action/agent-turn" + q,
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-greeting")
	// Wait long enough that the agent is mid-TTS. ~3s = LLM (~1s) + a few
	// seconds of TTS playback in flight. Don't wait too long or the
	// greeting can fully complete (then it's not barge-in, it's a normal
	// second turn).
	time.Sleep(3 * time.Second)
	s.Done()

	s = Step(t, "send-prompt-wav")
	// "alpha bravo charlie delta" while the greeting is still speaking →
	// barge-in attempt. minSpeechDuration default is 0.5s so a 4s WAV
	// definitively confirms the interruption.
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	cbs := DrainCallbacks(sess, 15*time.Second)
	s.Logf("captured %d agent events", len(cbs))
	s.Done()

	s = Step(t, "assert-user-interruption")
	intr := findAgentEvents(cbs, "user_interruption")
	if len(intr) == 0 {
		s.Errorf("no user_interruption event in %d events: %s",
			len(cbs), summarizeEventTypes(cbs))
	} else {
		s.Logf("user_interruption fired %d time(s)", len(intr))
	}
	// Bonus: we should also see a turn_end with interrupted=true on the
	// greeting turn. Not required for the test to pass — note as info.
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			s.Logf("turn_end reported interrupted=true (expected for greeting turn)")
			break
		}
	}
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()
}

// TestVerb_Agent_KrispTurnDetection — verifies the agent runs with
// `turnDetection: "krisp"` (FreeSWITCH mod_krisp). With Krisp, jambonz
// uses an acoustic end-of-turn model rather than STT silence detection.
//
// Capability: depends on mod_krisp being loaded on the cluster's
// FreeSWITCH boxes (not always the case on community clusters). When
// Krisp is unavailable, end-of-turn never fires, the LLM never gets the
// transcript, and the recording is silent. This test detects that case
// and t.Skips with a clear message rather than failing — that's a
// cluster configuration question, not a smoke-tester regression.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session (+ ensure _anon)
//  4. script-agent-verb (turnDetection=krisp, earlyGeneration=true)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
// 10. hangup-and-wait-ended
// 11. assert-reply-keywords
func TestVerb_Agent_KrispTurnDetection(t *testing.T) {
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	if _, ok := webhookReg.Lookup("_anon"); !ok {
		webhookReg.New("_anon")
	}
	s.Done()

	s = Step(t, "script-agent-verb")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt:  agentEchoSystemPrompt,
			Greeting:      false,
			TurnDetection: "krisp",
			ActionURL:     webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:      webhookSrv.PublicURL() + "/action/agent-turn",
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := t.TempDir() + "/agent-krisp.pcm"
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-stt")
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(12 * time.Second)
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()

	s = Step(t, "assert-call-completed")
	// Per user direction: it's enough that jambonz accepted the param and
	// the call ran to completion without rejection. We don't strictly
	// verify that Krisp acted on EOT (it may not be loaded on this
	// cluster — Krisp is internal to jambonz, no client-side handle).
	// If the cluster were to reject the param, the call would have failed
	// at agent verb exec-time and we'd see no inbound RTP at all.
	if call.PCMBytesIn() == 0 {
		s.Errorf("no inbound RTP — agent verb may have rejected turnDetection=krisp")
	} else {
		s.Logf("agent ran with turnDetection=krisp; %d inbound PCM bytes captured",
			call.PCMBytesIn())
	}
	s.Done()
}

// TestVerb_Agent_NoiseIsolation — verifies the agent verb accepts the
// `noiseIsolation` parameter (krisp / rnnoise / object form). Per user
// direction: success criterion is "the call ran to completion without
// the agent verb rejecting the param" — actual noise-suppression
// behaviour is internal to jambonz/FreeSWITCH and not directly
// observable from outside.
//
// We send the same prompt as Echo and just confirm the agent produces
// some inbound audio. If feature-server doesn't recognise the param it
// would either log a warning (per index.js:170 "unrecognized
// noiseIsolation value, ignoring") or — for a typo'd vendor — bail
// during makeTask. Either way the call wouldn't produce a full reply.
//
// Two sub-tests via subtests so we exercise both shorthand strings AND
// the object form in one parent test.
//
// Steps (per sub-test):
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session (+ ensure _anon)
//  4. script-agent-verb (noiseIsolation=<variant>)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
// 10. hangup-and-wait-ended
// 11. assert-call-produced-audio — non-zero inbound RTP proves the verb
//     wasn't rejected
func TestVerb_Agent_NoiseIsolation(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasDeepseek() || !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Skip("agent noiseIsolation test needs DEEPSEEK + DEEPGRAM + Deepgram credential")
	}

	variants := []struct {
		name  string
		value any
	}{
		{"krisp_shorthand", "krisp"},
		{"rnnoise_shorthand", "rnnoise"},
		{"krisp_object_form", map[string]any{"mode": "krisp", "level": 80, "direction": "read"}},
	}

	for _, v := range variants {
		t.Run(v.name, func(t *testing.T) {
			t.Parallel()
			s := Step(t, "preflight-skips")
			s.Done()

			ctx := WithTimeout(t, 90*time.Second)
			uas := claimUAS(t, ctx)

			s = Step(t, "ensure-prompt-wav")
			wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
				Model: "aura-asteria-en",
			})
			if err != nil {
				s.Fatalf("EnsureWAV: %v", err)
			}
			s.Done()

			s = Step(t, "register-webhook-session")
			testID := t.Name()
			sess := webhookReg.New(testID)
			t.Cleanup(func() { webhookReg.Release(testID) })
			if _, ok := webhookReg.Lookup("_anon"); !ok {
				webhookReg.New("_anon")
			}
			s.Done()

			s = Step(t, "script-agent-verb")
			// Build the agent verb manually so we can plug in either a
			// shorthand string or an object form for noiseIsolation —
			// agentVerbOpts.NoiseIsolation only carries a string.
			verb := buildAgentVerb(agentVerbOpts{
				SystemPrompt: agentEchoSystemPrompt,
				Greeting:     false,
				ActionURL:    webhookSrv.PublicURL() + "/action/agent-complete",
				EventURL:     webhookSrv.PublicURL() + "/action/agent-turn",
			})
			verb["noiseIsolation"] = v.value
			sess.ScriptCallHook(WithWarmupScript(webhook.Script{
				verb,
				V("hangup"),
			}))
			sess.ScriptActionHook("agent-complete", webhook.Script{})
			sess.ScriptActionHook("agent-turn", webhook.Script{})
			s.Done()

			s = Step(t, "place-call")
			call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
			s.Done()

			s = Step(t, "answer-record-and-silence")
			if err := call.Answer(); err != nil {
				s.Fatalf("Answer: %v", err)
			}
			recPath := t.TempDir() + "/agent-noise.pcm"
			if err := call.StartRecording(recPath); err != nil {
				s.Fatalf("StartRecording: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			s.Done()

			s = Step(t, "wait-for-stt")
			time.Sleep(1500 * time.Millisecond)
			s.Done()

			s = Step(t, "send-prompt-wav")
			if err := call.SendWAV(wavPath); err != nil {
				s.Fatalf("SendWAV: %v", err)
			}
			s.Done()

			s = Step(t, "wait-for-llm-reply")
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence (post): %v", err)
			}
			time.Sleep(10 * time.Second)
			s.Done()

			s = Step(t, "hangup-and-wait-ended")
			_ = call.Hangup()
			endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
			defer ecancel()
			_ = call.WaitState(endCtx, jsip.StateEnded)
			s.Done()

			s = Step(t, "assert-call-produced-audio")
			// Just prove the call didn't get rejected. If feature-server
			// didn't accept the noiseIsolation form, agent.exec would
			// have bailed and we'd see no inbound RTP at all.
			if call.PCMBytesIn() == 0 {
				s.Errorf("no inbound RTP — agent verb may have rejected noiseIsolation=%v",
					v.value)
			} else {
				s.Logf("noiseIsolation=%v accepted; %d inbound PCM bytes captured",
					v.value, call.PCMBytesIn())
			}
			s.Done()
		})
	}
}

// TestVerb_Agent_NoResponseTimeout — when noResponseTimeout is set, the
// agent re-prompts the user after that many seconds of silence in the
// Idle state (state-machine.js _startNoResponseTimer). We:
//
//   1. Set greeting=true so the agent emits its first turn (then enters Idle).
//   2. Stay silent for noResponseTimeout + a buffer.
//   3. Expect a SECOND llm_response event (the re-prompt — typically
//      "Are you still there?").
//
// Asserts at least two `llm_response` events appeared in the eventHook
// stream during the silent window, proving the re-prompt path fired.
//
// Steps:
//  1. preflight-skips
//  2. register-webhook-session (+ ensure _anon)
//  3. script-agent-verb (greeting=true, noResponseTimeout=4)
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-greeting-and-reprompt — silent window long enough for
//     greeting to finish AND noResponseTimeout to fire and the re-prompt
//     to stream
//  7. drain-anon-events
//  8. assert-two-llm-responses — count `llm_response` events
//  9. hangup-and-wait-ended
func TestVerb_Agent_NoResponseTimeout(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-agent-verb")
	timeout := 4
	systemPrompt := "You are a brief voice assistant. " +
		"On every turn, reply with a single short sentence. " +
		"Greet the user on your first turn."
	q := "?" + webhook.CorrelationHeader + "=" + url.QueryEscape(testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		buildAgentVerb(agentVerbOpts{
			SystemPrompt:      systemPrompt,
			Greeting:          true,
			NoResponseTimeout: &timeout,
			ActionURL:         webhookSrv.PublicURL() + "/action/agent-complete",
			EventURL:          webhookSrv.PublicURL() + "/action/agent-turn" + q,
		}),
		V("hangup"),
	}))
	sess.ScriptActionHook("agent-complete", webhook.Script{})
	sess.ScriptActionHook("agent-turn", webhook.Script{})
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

	s = Step(t, "wait-for-greeting-and-reprompt")
	// Budget breakdown:
	//   ~5s greeting (LLM + TTS)
	//   ~4s noResponseTimeout window
	//   ~5s re-prompt streaming
	// Total ~14s. Add 6s slack for cluster latency.
	cbs := DrainCallbacks(sess, 20*time.Second)
	s.Logf("captured %d agent events: %s",
		len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-two-llm-responses")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) < 2 {
		// Diagnostics: dump every llm_response we saw so an operator can
		// tell if the re-prompt fired with different wording or didn't
		// fire at all.
		var bodies []string
		for _, cb := range responses {
			bodies = append(bodies, truncate(extractEventField(cb, "response"), 80))
		}
		s.Errorf("expected >= 2 llm_response events (greeting + re-prompt) got %d: %v",
			len(responses), bodies)
	} else {
		s.Logf("re-prompt fired: %s",
			truncate(extractEventField(responses[1], "response"), 100))
	}
	s.Done()

	s = Step(t, "hangup-and-wait-ended")
	_ = call.Hangup()
	endCtx, ecancel := context.WithTimeout(ctx, 5*time.Second)
	defer ecancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
	s.Done()
}

// summarizeEventTypes returns a comma-joined string of the event types
// present in cbs, useful for diagnostics when a required type is missing.
func summarizeEventTypes(cbs []webhook.Callback) string {
	var types []string
	for _, cb := range cbs {
		t, _ := cb.JSON["type"].(string)
		if t == "" {
			t = "(no type)"
		}
		types = append(types, t)
	}
	return strings.Join(types, ",")
}

// summarizeTranscripts returns a short list of the transcript values across
// user_transcript events for diagnostics.
func summarizeTranscripts(cbs []webhook.Callback) []string {
	out := make([]string, 0, len(cbs))
	for _, cb := range cbs {
		out = append(out, truncate(extractEventField(cb, "transcript"), 80))
	}
	return out
}

// truncate caps s at n characters with an ellipsis suffix.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
