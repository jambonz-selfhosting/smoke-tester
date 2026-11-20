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
//     /action/verb-hook   → callbacks/<verb>.schema.json (n/a here — base only)
//     /action/agent-turn  → callbacks/agent-turn.schema.json (eventHook payloads)
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
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// agentEchoSystemPrompt is the natural-language instruction that turns
// the LLM into an echo bot. Plain prose — the model must INFER from
// context what "echo back" means and apply it to whatever sentence the
// user actually speaks. Same wording as llmEchoSystemPrompt so the agent
// and llm verb tests assert against an identical contract.
const agentEchoSystemPrompt = "Echo back whatever you hear from the user."

// agentEchoPrompt is the user-side phrase used by the non-Echo agent
// tests (EventHook, BargeIn, Greeting, NoiseIsolation, KrispTurnDetection,
// NoResponseTimeout) that just need *some* user audio to drive the agent
// — those tests assert on event payloads, not on conversation
// correctness, so a phonetic-alphabet drill is fine.
//
// TestVerb_Agent_Echo uses agentEchoTurns instead (natural-language
// sentences) because that's where the actual echo-correctness assertion
// lives.
const agentEchoPrompt = "Please repeat exactly the following words: alpha, bravo, charlie, delta."

// agentEchoTurns is the script TestVerb_Agent_Echo drives. Sentences
// are long enough that the agent's STT+LLM has clear material to echo
// without thinking the user is mid-sentence.
var agentEchoTurns = []struct {
	prompt string
}{
	{prompt: "Hello, my name is John and I am calling from the office."},
	{prompt: "Can you please tell me what time it is in New York today?"},
}

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
	SystemPrompt      string
	Greeting          bool   // false → user speaks first; true → agent greets first
	ActionURL         string // POST target when agent ends (call kill)
	EventURL          string // POST target for user_transcript / llm_response / turn_end / user_interruption
	ToolURL           string // POST target when LLM calls a function/tool (set when Tools is non-empty)
	Tools             []map[string]any
	BargeIn           bool           // default false: easier to test deterministic round-trips
	NoResponseTimeout *int           // pointer so 0 means "explicitly disable"
	EarlyGeneration   bool           // require turnDetection=krisp with this
	TurnDetection     string         // "stt" (default), "krisp"
	NoiseIsolation    string         // "" (off), "krisp", "rnnoise"
	LLMVendor         string         // default "deepseek" when empty
	LLMModel          string         // default "deepseek-chat" when empty
	LLMApiKey         string         // default cfg.DeepseekAPIKey when empty
	STT               map[string]any // default deepgram when nil
	TTS               map[string]any // default deepgram when nil
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
	stt := opts.STT
	if stt == nil {
		stt = map[string]any{
			"vendor":   "deepgram",
			"label":    deepgramLabel,
			"language": "en-US",
		}
	}
	ttsCfg := opts.TTS
	if ttsCfg == nil {
		ttsCfg = map[string]any{
			"vendor": "deepgram",
			"label":  deepgramLabel,
			"voice":  deepgramVoice,
		}
	}
	verb := V("agent",
		"stt", stt,
		"tts", ttsCfg,
		"llm", map[string]any{
			"vendor": firstNonEmpty(opts.LLMVendor, "deepseek"),
			"model":  firstNonEmpty(opts.LLMModel, "deepseek-chat"),
			// Inline auth — feature-server skips the DB credential lookup
			// when auth is set on the verb (lib/tasks/agent/index.js:446).
			"auth": map[string]any{
				"apiKey": firstNonEmpty(opts.LLMApiKey, cfg.DeepseekAPIKey),
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
		if cb.String("type") == wantType {
			out = append(out, cb)
		}
	}
	return out
}

// agentTurnEndNamesTool reports whether any turn_end summary in cbs lists a
// tool call named name. Used both as the wait condition for the tool-calling
// turn completing and as the assertion on it, so the two cannot disagree.
func agentTurnEndNamesTool(cbs []webhook.Callback, name string) bool {
	for _, te := range findAgentEvents(cbs, "turn_end") {
		toolCalls, ok := te.JSON["tool_calls"].([]any)
		if !ok {
			continue
		}
		for _, tc := range toolCalls {
			if m, ok := tc.(map[string]any); ok && m["name"] == name {
				return true
			}
		}
	}
	return false
}

// ScriptAgent registers the canonical agent-verb call_hook script on sess
// + the empty action-hook acks for the two agent callbacks
// (`agent-complete` + `agent-turn`). It also wires the per-session
// X-Test-Id-aware URLs into agentVerbOpts so tests don't have to know
// about the eventHook/toolHook correlation footgun.
//
// The default trailing chain is `[hangup]`; pass `extra` verbs to append
// before hangup.
func ScriptAgent(sess *webhook.Session, opts agentVerbOpts, extra ...map[string]any) {
	opts.ActionURL = SessionURL(sess, "agent-complete")
	opts.EventURL = SessionURL(sess, "agent-turn")
	if len(opts.Tools) > 0 {
		opts.ToolURL = SessionURL(sess, "agent-tool")
	}
	script := webhook.Script{buildAgentVerb(opts)}
	for _, v := range extra {
		script = append(script, v)
	}
	script = append(script, V("hangup"))
	sess.ScriptCallHook(WithWarmupScript(script))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	if len(opts.Tools) > 0 {
		SessionAckEmpty(sess, "agent-tool")
	}
}

// TestVerb_Agent_Echo — multi-turn natural-language echo round-trip.
// On each turn the UA speaks a real conversational sentence and the
// agent (system-prompted "echo back whatever you hear") must speak it
// back. We record the agent's reply audio per turn, transcribe it with
// Deepgram STT (independent of the agent's own STT), and assert every
// content word from the spoken prompt appears in the recording — proves
// the full round-trip (caller TTS → agent STT → Deepseek LLM → agent
// TTS → caller recording → independent STT → assertion) on real speech,
// not on a phonetic-alphabet drill.
//
// Two turns with disjoint sentences prove the agent echoed each turn's
// content, not a fixed phrase: a static reply that "passes" turn 1
// would miss every content word in turn 2.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs (one per turn, cached on disk)
//  3. register-webhook-session
//  4. script-agent-verb
//  5. place-call
//  6. answer-and-silence
//  7. wait-for-stt
//  8. turn-N-record-and-speak (per turn): start recording, send prompt
//     WAV, wait for agent reply, stop recording
//  9. turn-N-assert-echo (per turn): STT the recording, assert every
//     content word from the prompt is present
//
// 10. hangup-and-wait-ended
func TestVerb_Agent_Echo(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	// Pre-generate one WAV per turn. EnsureWAV caches by prompt+voice on
	// disk so re-runs are free.
	wavs := make([]string, len(agentEchoTurns))
	for i, turn := range agentEchoTurns {
		s = Step(t, "ensure-prompt-wav")
		path, err := tts.EnsureWAV(ctx, "testdata/agent", turn.prompt, tts.PromptOptions{
			Model: "aura-asteria-en",
		})
		if err != nil {
			s.Fatalf("EnsureWAV turn %d: %v", i+1, err)
		}
		s.Logf("turn %d prompt wav: %s", i+1, path)
		wavs[i] = path
		s.Done()
	}

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
	})
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

	// Per-turn loop: start a fresh recording, send the prompt WAV, wait
	// for the agent's reply to land in the recording, stop, transcribe,
	// assert. One file per turn so a wrong reply on turn N can't bleed
	// into turn N+1's assertion.
	for i, turn := range agentEchoTurns {
		recPath := filepath.Join(t.TempDir(), formatAgentTurnRecPath(i+1))

		s = Step(t, formatAgentTurnStep(i+1, "record-and-speak"))
		if err := call.StartRecording(recPath); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		// Brief silence so the recording opens before our prompt audio
		// arrives — otherwise the first ~100ms of the reply can land
		// before the file is being written.
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (pre): %v", err)
		}
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		// Trail with silence so the agent's STT detects end-of-utterance
		// and the LLM is triggered to reply.
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		// Give the LLM time to reply and the agent's TTS to stream the
		// echo back into our recording.
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, formatAgentTurnStep(i+1, "assert-echo"))
		// At least 2 content words from the prompt must appear in the
		// recording transcript. STT on telephony-quality TTS-of-LLM-
		// reply audio drops occasional words; strict-Contains was too
		// brittle. 2-of-N is enough to prove the agent echoed the
		// prompt's content (a wrong/empty/hallucinated reply fails).
		words := contentWords(turn.prompt)
		AssertTranscriptHasMost(s, ctx, recPath, 2, words...)
		s.Done()
	}

	HangupAndWaitEnded(t, ctx, call)
}

// formatAgentTurnStep builds a kebab-cased step name with the turn
// number, e.g. "turn-1-record-and-speak". Mirrors the helper in
// llm_test.go but with an "agent-" prefix isn't needed — the per-test
// step namespace already keeps these unique under -parallel.
func formatAgentTurnStep(n int, suffix string) string {
	return "turn-" + strconv.Itoa(n) + "-" + suffix
}

// formatAgentTurnRecPath builds a unique recording filename per turn,
// e.g. "agent-turn-1-reply.pcm".
func formatAgentTurnRecPath(n int) string {
	return "agent-turn-" + strconv.Itoa(n) + "-reply.pcm"
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
//
// 10. assert-tool-payload — name + arguments + tool_call_id present
// 11. wait-for-llm-reply — silence while agent speaks the secret word
// 12. hangup-and-wait-ended
// 13. assert-secret-word-spoken — agent response names the secret word; reply audio is non-silent
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

	_, sess := claimSession(t)

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
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: systemPrompt,
		Tools:        []map[string]any{tool},
	})
	// agent-tool gets a JSON body (not a verb array): feature-server takes
	// our response body verbatim and feeds it to the LLM as the tool result.
	toolBody := []byte(`{"word":"` + secretWord + `"}`)
	sess.ScriptActionHookBody("agent-tool", toolBody)
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
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
	// Collect rather than discard: turn_end for this turn races the toolHook
	// POST, and WaitCallbackFor would throw it away (see the sibling test).
	toolCB, earlyCBs, err := WaitCallbackForCollecting(waitCtx, sess, "action/agent-tool")
	if err != nil {
		s.Fatalf("waiting for action/agent-tool: %v", err)
	}
	s.Logf("action/agent-tool body: %s", string(toolCB.Body))
	// Restart recording here so the reply audio gets its own file. The
	// cumulative Call.RMS/PCMBytesIn counters span the whole call and would
	// pass on any answered leg; a file bracketing just the reply is what
	// actually shows TTS reached the caller.
	call.StopRecording()
	replyPath := t.TempDir() + "/agent-tool-reply-window.pcm"
	if err := call.StartRecording(replyPath); err != nil {
		s.Fatalf("StartRecording (reply window): %v", err)
	}
	s.Done()

	s = Step(t, "assert-tool-payload")
	if got := toolCB.String("name"); got != "get_secret_word" {
		s.Errorf("tool name = %q, want %q", got, "get_secret_word")
	}
	if got := toolCB.String("tool_call_id"); got == "" {
		s.Errorf("tool_call_id missing in payload: %s", string(toolCB.Body))
	}
	// `arguments` must exist AND be empty (the tool is parameterless).
	// A regression that leaks system-prompt fragments / random tokens
	// into the args would have passed the old "non-nil" check but fails
	// here. Accept both `{}` (object) and `"{}"` (JSON-string) shapes
	// since some LLM vendors serialize differently.
	args := toolCB.NestedAny("arguments")
	if args == nil {
		s.Errorf("arguments missing in payload: %s", string(toolCB.Body))
	} else {
		switch v := args.(type) {
		case map[string]any:
			if len(v) != 0 {
				s.Errorf("arguments expected empty {}, got %v", v)
			}
		case string:
			t := strings.TrimSpace(v)
			if t != "" && t != "{}" {
				s.Errorf("arguments expected empty {}, got string %q", v)
			}
		default:
			s.Errorf("arguments has unexpected type %T: %v", args, args)
		}
	}
	s.Done()

	s = Step(t, "wait-for-turn-end")
	// Tool result fed back to LLM → second LLM round-trip → TTS streams the
	// word. Wait for the turn_end summary naming the tool rather than sleeping a
	// fixed budget: measurements on the sibling test show turn_end lands ~20-24s
	// after the tool call, so a 12s sleep truncated the recording this test then
	// reads. Callbacks are collected, not discarded.
	teCtx, tecancel := context.WithTimeout(ctx, 45*time.Second)
	defer tecancel()
	seen := WaitCallbacksUntil(teCtx, sess, func(cbs []webhook.Callback) bool {
		return agentTurnEndNamesTool(append(earlyCBs, cbs...), "get_secret_word")
	})
	if !agentTurnEndNamesTool(append(earlyCBs, seen...), "get_secret_word") {
		s.Logf("no turn_end naming get_secret_word within budget; %d callback(s) seen",
			len(earlyCBs)+len(seen))
	}
	// Short pad so TTS finishes streaming into the recording, then stop
	// recording explicitly rather than relying on teardown ordering.
	time.Sleep(2 * time.Second)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-secret-word-spoken")
	// Assert on the agent's own response text, not on STT of the reply audio.
	// Deepgram reliably mangles "kingfisher" out of Deepgram TTS over PCMU
	// ("skinfisher", "disking fisher", "skin fissure"), so a transcript check
	// here failed on a working tool round trip — the same trap the gptlive
	// test documents for "aardvark". The response field proves what matters:
	// the tool result was fed back and the agent spoke it.
	if !agentEventResponseContains(append(earlyCBs, seen...), secretWord) {
		s.Errorf("no agent event whose response contains %q; responses=%v",
			secretWord, agentEventResponses(append(earlyCBs, seen...)))
	}
	// Separately, the reply audio has to actually reach the caller. Kept
	// phonetics-free on purpose: this is a media check, not a speech check.
	// Measured over the reply window only — the whole-call counters would
	// pass on silence.
	spokenMS, err := NonSilentMS(replyPath, 500)
	if err != nil {
		s.Fatalf("NonSilentMS: %v", err)
	}
	// Runs measure ~1300ms of speech here. 800ms is clear of that margin while
	// staying far above anything stray frames could accumulate to across a
	// window this long — the point is that the reply was actually spoken, not
	// that a few samples crossed the threshold.
	if spokenMS < 800 {
		s.Errorf("reply window carries only %dms of audio; TTS did not reach the caller", spokenMS)
	} else {
		s.Logf("reply window: %dms of non-silent audio", spokenMS)
	}
	s.Done()
}

// agentEventResponses collects the response text of every agent event that
// carries one (llm_response and turn_end).
func agentEventResponses(cbs []webhook.Callback) []string {
	var out []string
	for _, kind := range []string{"llm_response", "turn_end"} {
		for _, cb := range findAgentEvents(cbs, kind) {
			if r := cb.String("response"); r != "" {
				out = append(out, r)
			}
		}
	}
	return out
}

// agentEventResponseContains reports whether any agent response mentions word,
// case-insensitively.
func agentEventResponseContains(cbs []webhook.Callback, word string) bool {
	for _, r := range agentEventResponses(cbs) {
		if strings.Contains(strings.ToLower(r), strings.ToLower(word)) {
			return true
		}
	}
	return false
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
		out = append(out, truncate(cb.String("transcript"), 80))
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
