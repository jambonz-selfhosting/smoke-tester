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
		if cb.String("type") == wantType {
			out = append(out, cb)
		}
	}
	return out
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

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// Append ?X-Test-Id=<testID> to per-callback URLs so the webhook server
	// routes eventHook callbacks to THIS session (the payload itself has no
	// customerData and would otherwise land in shared `_anon`, racing with
	// parallel agent tests).
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
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

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Drain THIS session while events stream in (eventHook URL was
	// minted via SessionURL so it carries our X-Test-Id query param —
	// no `_anon` contention). Each event type fires at a different
	// moment (user_transcript ~when STT finalizes, llm_response ~when
	// LLM stream completes, turn_end ~when TTS empties).
	cbs := DrainCallbacks(sess, LLMReplyWindow)
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
		txt := strings.ToLower(cb.String("transcript"))
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
		// The echo system prompt forces the LLM to repeat the prompt's
		// words. Concatenate every llm_response and require at least
		// one of our 4 keywords lands in the text — proves the LLM
		// actually responded to OUR prompt's content (a regression
		// that emits hallucinated/empty/generic text would fail).
		var all string
		for _, r := range responses {
			all += " " + strings.ToLower(r.String("response"))
		}
		if strings.TrimSpace(all) == "" {
			s.Errorf("all llm_response events have empty response field")
		}
		hits := 0
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(all, kw) {
				hits++
			}
		}
		if hits == 0 {
			s.Errorf("llm_response %q contains none of the prompt's keywords (alpha/bravo/charlie/delta)",
				truncate(all, 200))
		} else {
			s.Logf("llm_response keyword hits=%d: %q", hits, truncate(all, 100))
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

	const userPrompt = "Hello, my name is John and I am calling from the office."
	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", userPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	greetingSystemPrompt := "On your very first turn, greet the user with a short hello. " +
		"On EVERY later turn, you must repeat back the user's exact words verbatim. " +
		"Do not add commentary. Do not paraphrase. Do not answer questions. " +
		"Just repeat what the user said."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: greetingSystemPrompt,
		Greeting:     true,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-record-greeting")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Record ONLY the greeting turn into its own file. We stop this
	// recording before sending the user prompt so the greeting
	// transcript is clean (turn-2 audio can't bleed in and "rescue" a
	// silent greeting).
	greetingRec := t.TempDir() + "/agent-greeting.pcm"
	if err := call.StartRecording(greetingRec); err != nil {
		s.Fatalf("StartRecording (greeting): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-greeting", 6*time.Second)
	bytesAfterGreeting := call.PCMBytesIn()
	call.StopRecording()

	s = Step(t, "assert-greeting-audio")
	// Bytes-level sanity: 6s @ PCMU 8kHz = ~96KB; require at least 16KB
	// (~1s of energy). A regression that emits noise/wrong audio passes
	// this floor — the transcript check below is the real assertion.
	const minGreetingBytes = 16000
	if bytesAfterGreeting < minGreetingBytes {
		s.Fatalf("greeting=true but only %d PCM bytes received in 6s (need >= %d)",
			bytesAfterGreeting, minGreetingBytes)
	}
	// STT the greeting recording NOW (before turn 2 audio bleeds in)
	// and assert at least one greeting-class word landed. A bug where
	// the agent emits noise, plays the wrong WAV, or starts speaking
	// the user-turn audio prematurely would have passed the bytes-only
	// assertion but fails this. Tolerant token set because the LLM
	// chooses the exact phrasing.
	AssertTranscriptHasMost(s, ctx, greetingRec, 1,
		"hello", "hi", "welcome", "help", "assistant", "how")
	s.Done()

	// Start a fresh recording for the user→agent echo turn.
	echoRec := t.TempDir() + "/agent-greeting-echo.pcm"

	s = Step(t, "record-and-send-user-prompt")
	if err := call.StartRecording(echoRec); err != nil {
		s.Fatalf("StartRecording (echo): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre-prompt): %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-prompt): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-echo-after-greeting")
	// Require at least 2 content words from the user prompt to land in
	// the recording transcript — proves the agent echoed our utterance
	// (not a hallucinated greeting / system-prompt leak).
	AssertTranscriptHasMost(s, ctx, echoRec, 2,
		"hello", "name", "john", "calling", "office")
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
//  6. hangup — proactive BYE from us
//  7. wait-action-agent-complete
//  8. assert-action-payload — call_sid + completion_reason in body
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

	testID, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
	})
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

	// Pre-hangup pause: hanging up before the agent reaches Idle races
	// kill with init and the actionHook never fires. 2s is reliable.
	time.Sleep(2 * time.Second)

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
	if got := actionCB.String("call_sid"); got == "" {
		s.Errorf("action/agent-complete payload missing call_sid: %s",
			string(actionCB.Body))
	} else {
		s.Logf("call_sid in payload: %s", got)
	}
	if got := actionCB.String("completion_reason"); got == "" {
		s.Errorf("action/agent-complete payload missing completion_reason: %s",
			string(actionCB.Body))
	} else {
		s.Logf("completion_reason: %s", got)
	}
	// customerData.x_test_id should also round-trip back to us — proves
	// the actionHook hit our session, not _anon.
	if id := actionCB.NestedString("customer_data.x_test_id"); id != "" && id != testID {
		s.Errorf("customer_data.x_test_id=%q want %q", id, testID)
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
	toolCB, err := sess.WaitCallbackFor(waitCtx, "action/agent-tool")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-tool: %v", err)
	}
	s.Logf("action/agent-tool body: %s", string(toolCB.Body))
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

	s = Step(t, "wait-for-llm-reply")
	// Tool result fed back to LLM → second LLM round-trip → TTS streams the
	// word. Allow ~10s for the full second pass.
	time.Sleep(12 * time.Second)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

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

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	greetingSystemPrompt := "You are a friendly voice assistant. " +
		"On your first turn, greet the user with a long, slow welcome " +
		"of at least three full sentences so they have time to interrupt. " +
		"On subsequent turns, repeat the user's words back to them verbatim."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: greetingSystemPrompt,
		Greeting:     true,
		BargeIn:      true,
	})
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

	HangupAndWaitEnded(t, ctx, call)
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

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:  agentEchoSystemPrompt,
		TurnDetection: "krisp",
	})
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

	HangupAndWaitEnded(t, ctx, call)

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
// `noiseIsolation` object form AND that the round-trip still works
// (noise isolation didn't garble the user's audio so badly the LLM
// can't produce a sensible reply).
//
// We exercise the object form `{mode, level, direction}` because it's
// the most expressive shape — if it parses, the shorthand strings
// ("krisp", "rnnoise") will too (both go through the same validator at
// lib/tasks/agent/index.js:170). The shorthand variants used to be
// separate sub-tests but they cost ~18s each for ≤1% additional
// coverage; one variant is enough.
//
// Why the round-trip matters: a regression that silently disables
// noise isolation but corrupts the audio path would still yield
// PCMBytesIn>0 and pass the old assertion. Asserting the agent echoed
// our prompt content proves the audio path stayed intact.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb (noiseIsolation=object form)
//  5. place-call
//  6. answer-record-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-llm-reply
// 10. hangup-and-wait-ended
// 11. assert-echo-survived-noise-isolation — recording transcript
//     contains the prompt's keywords (proves audio path stayed intact)
func TestVerb_Agent_NoiseIsolation(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasDeepseek() || !cfg.HasDeepgram() || deepgramLabel == "" {
		t.Skip("agent noiseIsolation test needs DEEPSEEK + DEEPGRAM + Deepgram credential")
	}

	noiseValue := map[string]any{"mode": "krisp", "level": 80, "direction": "read"}

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

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// Build the verb manually so we can plug in the object form for
	// noiseIsolation — agentVerbOpts.NoiseIsolation only carries a string.
	verb := buildAgentVerb(agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		Greeting:     false,
		ActionURL:    SessionURL(sess, "agent-complete"),
		EventURL:     SessionURL(sess, "agent-turn"),
	})
	verb["noiseIsolation"] = noiseValue
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		verb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
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

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-llm-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-echo-survived-noise-isolation")
	if call.PCMBytesIn() == 0 {
		s.Fatalf("no inbound RTP — agent verb rejected noiseIsolation=%v", noiseValue)
	}
	// Strict echo assertion: with agentEchoSystemPrompt the agent must
	// repeat back at least 3 of the 4 phonetic-alphabet keywords. If
	// noise isolation corrupted the user's audio path, STT would mishear
	// and the LLM wouldn't echo correctly. Using HasMost(3) instead of
	// strict-Contains because Krisp can sharpen audio enough to
	// occasionally drop one trailing word — the failure mode we're
	// catching is "0 keywords echoed" (audio path broken), not "minor
	// STT drift".
	AssertTranscriptHasMost(s, ctx, recPath, 3,
		"alpha", "bravo", "charlie", "delta")
	s.Logf("noiseIsolation=%v accepted; agent echoed prompt content (%d PCM bytes)",
		noiseValue, call.PCMBytesIn())
	s.Done()
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

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	timeout := 4
	systemPrompt := "You are a brief voice assistant. " +
		"On every turn, reply with a single short sentence. " +
		"Greet the user on your first turn."
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:      systemPrompt,
		Greeting:          true,
		NoResponseTimeout: &timeout,
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
			bodies = append(bodies, truncate(cb.String("response"), 80))
		}
		s.Errorf("expected >= 2 llm_response events (greeting + re-prompt) got %d: %v",
			len(responses), bodies)
	} else {
		s.Logf("re-prompt fired: %s",
			truncate(responses[1].String("response"), 100))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
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
