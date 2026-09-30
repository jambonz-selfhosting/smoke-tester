// Tests for the `llm` verb with LLM vendor "gptlive" — OpenAI's GPT-Live API
// (GA, wss://api.openai.com/v1/live/sessions).
//
// gptlive is an OPTIONAL vendor (see config.HasGptLive). When GPTLIVE_API_KEY
// is unset the tests pass immediately without exercising the GPT-Live path —
// a plain `return` after a log, never t.Skip, never a failure, matching
// xai_llm_s2s_test.go / xai_agent_test.go.
//
// How GPT-Live differs from the sibling realtime vendors (openai/xai), i.e.
// what these tests must NOT copy from xai_llm_s2s_test.go:
//   - llmOptions carries ONLY session_update, which the feature-server sends
//     as the session of the startup `session.start` (with `model` filled in).
//     There is no response_create; the model drives the conversation itself.
//   - The session is not ready, and caller audio is gated, until the server
//     emits `session.started`.
//   - Tool calling only exists via a Responses-targeted *delegation*
//     (delegation.responses.tools). Its events arrive wrapped in a
//     `response.event` envelope.
//   - toolHook request: {tool_call_id, name, args} — same as other s2s vendors.
//   - toolHook response: {type:"response.item.create",
//     item:{type:"function_call_output", call_id:<echo the live tool_call_id>,
//     output:<result>}}; the feature-server sends the follow-on response.create.
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// gptLiveVoice is a GPT Live output voice (the Event API reference's own
// example uses "marin").
const gptLiveVoice = "marin"

// gptLivePassphrase is the distinctive phrase the model is told to say. It is
// asserted against an independent STT of the recording, so it must be a word
// STT reliably returns and one that cannot plausibly appear by chance in a
// generic greeting. "pineapple" satisfies both.
const gptLivePassphrase = "pineapple"

// gptLiveEchoPrompt instructs the model to answer with the passphrase. GPT
// Live has no per-response instruction override (no response_create), so this
// has to be session-level.
const gptLiveEchoPrompt = "You are a test fixture on a phone call. Whatever the caller says, reply with exactly this sentence and nothing else: The magic word is pineapple. Always answer in English."

// GptLiveGreetingWindow is how long the agent-speaks-first test captures for.
// Generous on purpose: the model chooses when to open (no response_create), and
// a greeting has been observed anywhere from ~2s to ~10s after session.started
// depending on cluster load.
const GptLiveGreetingWindow = 20 * time.Second

// gptLiveGreetPassphrase / gptLiveGreetPassphrase2 are the distinctive words the
// agent is told to open with. Two of them, and both ordinary English: the first
// attempt at this used "aardvark", the agent said it correctly, and Deepgram
// transcribed it "ardvoc" — failing the assertion on a working greeting. Asserting
// that EITHER lands keeps the test about the agent rather than about STT, while
// neither word can appear by chance in a generic greeting.
const (
	gptLiveGreetPassphrase  = "pineapple"
	gptLiveGreetPassphrase2 = "hotline"
)

// gptLiveGreetPrompt is the session persona. Deliberately says NOTHING about
// greeting: a greeting placed in instructions does not open the call (measured
// 0/5), so putting it here would test a mechanism that does not work.
const gptLiveGreetPrompt = "You are a voice assistant on a phone call. " +
	"Keep replies short. Always speak English."

// gptLiveGreetRequest is the session.commentary.append text that opens the call:
// the intended wording plus when to speak.
const gptLiveGreetRequest = "Immediately greet the caller using the exact text below. " +
	"Do not wait for the caller to speak first. After the greeting, pause and listen.\n\n" +
	"Welcome to the pineapple hotline, how can I help?"

// gptLiveConnectOptions maps the optional GPTLIVE_HOST / GPTLIVE_PATH env
// overrides onto the verb's connectOptions, or returns nil so the
// feature-server uses its own default URL. Kept in one place because both
// tests need identical connection behavior — if the inferred URL is wrong,
// one env var should fix both.
func gptLiveConnectOptions() map[string]any {
	opts := map[string]any{}
	if cfg.GptLiveHost != "" {
		opts["host"] = cfg.GptLiveHost
	}
	if cfg.GptLivePath != "" {
		opts["path"] = cfg.GptLivePath
	}
	if len(opts) == 0 {
		return nil
	}
	return opts
}

// gptLiveVerb builds the llm verb with the gptlive vendor, the shared
// connection settings, and the caller-supplied session_update body. extra
// receives any additional top-level verb properties (toolHook, etc.).
func gptLiveVerb(sessionUpdate map[string]any, extra ...any) map[string]any {
	args := []any{
		"vendor", "gptlive",
		"model", cfg.GptLiveModel,
		"auth", map[string]any{
			"apiKey": cfg.GptLiveAPIKey,
		},
		// /action/llm has a schema (schemas/callbacks/llm.schema.json) so the
		// completion payload gets contract-validated on arrival.
		"actionHook", webhookSrv.PublicURL() + "/action/llm",
		// llmOptions carries ONLY session_update — there is no
		// response_create in this API.
		"llmOptions", map[string]any{
			"session_update": sessionUpdate,
		},
	}
	if co := gptLiveConnectOptions(); co != nil {
		args = append(args, "connectOptions", co)
	}
	args = append(args, extra...)
	return V("llm", args...)
}

// gptLiveCompletionReason pulls the completionReason out of a drained
// /action/llm callback, or "" if none arrived. Used only to turn a failure
// into a diagnosis: "connection failure" means the inferred URL/auth is
// wrong, "server error" means the session_update was rejected.
func gptLiveCompletionReason(cbs []webhook.Callback) string {
	for _, cb := range cbs {
		if cb.Hook == "action/llm" {
			if r := cb.String("completionReason"); r != "" {
				return r
			}
		}
	}
	return ""
}

// gptLiveClientDelegationID returns the id of the first client-targeted
// session.delegation.created in the stream, or "" if the model never raised one.
func gptLiveClientDelegationID(cbs []webhook.Callback) string {
	for _, cb := range cbs {
		if cb.Hook != "action/llm-gptlive-event" || cb.String("type") != "session.delegation.created" {
			continue
		}
		if cb.NestedString("delegation.target") == "client" {
			if id := cb.NestedString("delegation.id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// gptLiveSawInputTranscript reports whether OpenAI recognized CALLER audio: an
// input transcript fragment can only appear if the media server lifted its
// input gate and forwarded our audio.
func gptLiveSawInputTranscript(cbs []webhook.Callback) bool {
	for _, cb := range cbs {
		if cb.Hook == "action/llm-gptlive-event" && cb.String("type") == "session.input_transcript.delta" {
			return true
		}
	}
	return false
}

// TestVerb_LLM_GptLive_Session proves the gptlive path connects, starts a
// session, and carries audio both directions.
//
// It validates the live connection contract, which no unit test can reach:
//  1. the URL (wss://api.openai.com/v1/live/sessions) and Bearer auth resolve
//     to a real GPT-Live endpoint — otherwise the verb ends with
//     completionReason "connection failure";
//  2. a session.start carrying model + instructions + audio.output.voice +
//     delegation{type:client} is ACCEPTED — otherwise the server sends an
//     `error` before session.started and the verb ends with "server error";
//  3. the server emits `session.started`, which is what lifts the media
//     server's input gate — if this never arrives, caller audio is silently
//     dropped for the life of the call;
//  4. session.output_audio.delta audio decodes and reaches the caller at the right
//     rate — asserted by independently transcribing the recording and finding
//     the passphrase the model was instructed to say.
//
// Steps:
//  1. preflight-skips — gptlive key guard (plain return), then deepgram guard
//     (plain return) for prompt-gen + reply STT infra
//  2. ensure-prompt-wav
//  3. script-llm-verb — gptlive vendor/auth + client-delegation session_update
//     + eventHook so session.started is observable
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt — let the session start and its VAD arm
//  7. record-and-speak — start recording, say anything, pad with silence
//  8. wait-for-session-started — scan eventHook traffic for session.started
//  9. wait-for-reply-and-stop
//  10. hangup-and-wait-ended
//  11. assert-passphrase-spoken — independent STT of the recording
func TestVerb_LLM_GptLive_Session(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S")
		return
	}

	// Deepgram is only needed for prompt-WAV generation + independent STT of
	// the recorded reply — not for the GPT Live session itself.
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", "Hello there, please say the magic word.", tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Logf("prompt wav: %s", promptWAV)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	s.Logf("model=%s host=%q path=%q (empty = feature-server default)",
		cfg.GptLiveModel, cfg.GptLiveHost, cfg.GptLivePath)
	llmVerb := gptLiveVerb(map[string]any{
		"instructions": gptLiveEchoPrompt,
		"audio": map[string]any{
			"output": map[string]any{
				"voice": gptLiveVoice,
			},
		},
		// A client delegation is the simplest configuration: the model may ask
		// us for text context, which we simply never answer. No tools are
		// declared, so the feature-server's tools-need-a-responses-delegation
		// guard does not apply.
		"delegation": map[string]any{
			"type": "client",
		},
	},
		// eventHook carries the raw GPT Live server events; X-Test-Id must ride
		// the query param because event payloads carry no callInfo for the
		// webhook server to correlate on.
		"eventHook", SessionURL(sess, "llm-gptlive-event"),
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "llm")
	SessionAckEmpty(sess, "llm-gptlive-event")
	s.Done()

	s = Step(t, "place-call")
	callSID, call := placeWebhookCallToWithSID(ctx, t, uas, sess, withTimeLimit(90))
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

	recPath := filepath.Join(t.TempDir(), "llm-gptlive-reply.pcm")

	s = Step(t, "record-and-speak")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	// These writes are deliberately NON-fatal. If GPT-Live rejects the session,
	// the feature-server ends the
	// llm verb and the scripted hangup runs, closing the RTP socket underneath
	// us — SendWAV then fails with "use of closed network connection", which
	// masks the actual cause. The real diagnosis is on the event/action hooks,
	// so log and press on to wait-for-session-started.
	if err := call.SendSilence(); err != nil {
		s.Logf("SendSilence (pre) failed, call may already be torn down: %v", err)
	}
	if err := call.SendWAV(promptWAV); err != nil {
		s.Logf("SendWAV failed, call may already be torn down: %v", err)
	}
	// Trail with silence so the server finalizes the caller's utterance.
	if err := call.SendSilence(); err != nil {
		s.Logf("SendSilence (post) failed, call may already be torn down: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-session-started")
	// session.started is the gate event: until it arrives the media server
	// drops every caller frame, so its absence explains an otherwise silent
	// failure. Scan the event stream for it — WaitCallbackFor discards
	// non-matching callbacks, so loop on the event hook and inspect each type.
	// DrainCallbacks (not WaitCallbackFor) because the diagnosis below needs the
	// /action/llm callback that carries completionReason. WaitCallbackFor
	// DISCARDS non-matching hooks, so filtering in-process is the only way to
	// see both the event stream and the completion payload.
	var drained []webhook.Callback
	started := false
	deadline := time.Now().Add(30 * time.Second)
	for !started && time.Now().Before(deadline) {
		batch := DrainCallbacks(sess, 2*time.Second)
		if len(batch) == 0 {
			continue
		}
		drained = append(drained, batch...)
		for _, cb := range batch {
			if cb.Hook == "action/llm-gptlive-event" && cb.String("type") == "session.started" {
				started = true
				s.Logf("session.started: %s", string(cb.Body))
			}
		}
	}
	if !started {
		// Turn the failure into a diagnosis instead of a bare timeout. The
		// server's own `error` event is the most informative thing available,
		// so lead with it; completionReason is the fallback.
		reason := gptLiveCompletionReason(drained)
		types := make([]string, 0, len(drained))
		errCode, errMsg := "", ""
		for _, cb := range drained {
			if cb.Hook != "action/llm-gptlive-event" {
				continue
			}
			if ty := cb.String("type"); ty != "" {
				types = append(types, ty)
			}
			if cb.String("type") == "error" {
				if c := cb.NestedString("error.code"); c != "" {
					errCode = c
				}
				if m := cb.NestedString("error.message"); m != "" {
					errMsg = m
				}
			}
		}
		switch {
		case errCode == "forbidden" || strings.Contains(errCode, "access_denied") ||
			strings.Contains(strings.ToLower(errMsg), "access"):
			// Entitlement, not code. A FAILURE rather than a skip because setting
			// GPTLIVE_API_KEY is an explicit statement of intent to exercise GPT-Live.
			s.Errorf("GPT-Live refused the session: %s (code=%q). The handshake succeeded, so "+
				"URL and auth are right; this account is not entitled to the requested session "+
				"(a leftover OpenAI-Alpha header routes to the retired alpha backend).",
				errMsg, errCode)
		case reason == "connection failure":
			s.Errorf("never reached session.started and the verb ended with %q — the GPT-Live "+
				"URL/auth is wrong (tried host=%q path=%q). events seen: %v",
				reason, cfg.GptLiveHost, cfg.GptLivePath, types)
		case errCode != "" || reason == "server error":
			s.Errorf("never reached session.started: GPT-Live rejected the startup "+
				"session.start with code=%q %q (completionReason=%q). events seen: %v",
				errCode, errMsg, reason, types)
		default:
			s.Errorf("never observed session.started (completionReason=%q); events seen: %v",
				reason, types)
		}
		// No session means no audio, so the assertions below would only add
		// redundant failures after a pointless wait. Stop with the one
		// diagnosis that matters.
		s.Done()
		call.StopRecording()
		HangupAndWaitEnded(t, ctx, call)
		return
	}

	s = Step(t, "answer-client-delegation")
	// A `client` delegation asks the APPLICATION for context. Answer it with a
	// quiet session.thinking.append keyed by delegation_id: a wrong shape is only
	// a non-fatal `error` on a started session, so assert no error comes back.
	if id := gptLiveClientDelegationID(drained); id != "" {
		s.Logf("answering client delegation %s", id)
		body := map[string]any{
			"llm_update": map[string]any{
				"type":          "session.thinking.append",
				"delegation_id": id,
				"content":       "The caller is a long-standing customer on the Unlimited plan.",
			},
		}
		if err := client.UpdateCall(ctx, callSID, body); err != nil {
			s.Fatalf("UpdateCall(llm_update: session.thinking.append) sid=%s: %v", callSID, err)
		}
		after := DrainCallbacks(sess, 8*time.Second)
		drained = append(drained, after...)
		for _, cb := range after {
			if cb.Hook == "action/llm-gptlive-event" && cb.String("type") == "error" {
				s.Errorf("GPT-Live rejected our session.thinking.append: code=%q %q",
					cb.NestedString("error.code"), cb.NestedString("error.message"))
			}
		}
	} else {
		s.Logf("no client delegation was raised on this call; session.thinking.append not exercised")
	}
	s.Done()

	// Proof that CALLER audio reached OpenAI: the passphrase alone cannot show
	// it, since an unprompted greeting may already contain it.
	if !gptLiveSawInputTranscript(drained) {
		extra := DrainCallbacks(sess, 5*time.Second)
		drained = append(drained, extra...)
	}
	if !gptLiveSawInputTranscript(drained) {
		s.Errorf("no session.input_transcript.delta observed — the caller's audio never " +
			"reached OpenAI even though the session started (check the media server's input " +
			"gate on session.started, and whether SendWAV failed above)")
	}
	s.Done()

	s = Step(t, "wait-for-reply-and-stop")
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-passphrase-spoken")
	// The passphrase is only in the recording if the whole audio path worked:
	// caller audio reached OpenAI through the input gate, and the model's
	// session.output_audio.delta frames were base64-decoded, resampled from 24 kHz and
	// mixed back to the caller.
	AssertTranscriptHasMost(s, ctx, recPath, 1, gptLivePassphrase)
	s.Done()
}

// TestVerb_LLM_GptLive_ToolHook proves the gptlive path supports app-declared
// tool/function calling end-to-end through a Responses-targeted delegation:
// tools declared at delegation.responses.tools reach the model, the model
// calls get_weather with an argument parsed from the caller's speech, the
// test's toolHook answers with a response.item.create envelope (echoing the
// live tool_call_id), the feature-server continues the delegation with
// response.create, and the agent speaks the result back.
//
// Reuses the weather prompt/system-prompt/result consts from llm_test.go — the
// scenario is vendor-agnostic.
//
// Steps mirror TestVerb_LLM_Xai_ToolHook, with the GPT-Live envelopes:
//  1. preflight-skips  2. ensure-prompt-wav  3. script-llm-verb (responses
//     delegation + tools + dynamic toolHook responder)  4. place-call
//  5. answer-and-silence  6. wait-for-stt  7. record-and-speak
//  8. wait-for-tool-call  9. assert-tool-args  10. wait-for-reply-and-stop
//  11. hangup-and-wait-ended  12. assert-tool-result-spoken
func TestVerb_LLM_GptLive_ToolHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S tool calling")
		return
	}

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S tool calling")
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
	llmVerb := gptLiveVerb(map[string]any{
		"instructions": llmWeatherSystemPrompt,
		"audio": map[string]any{
			"output": map[string]any{
				"voice": gptLiveVoice,
			},
		},
		// Tool calling REQUIRES a Responses-targeted delegation; with
		// type:'client' the feature-server would reject a verb carrying
		// handoff/hangup/mcpServers, and a model with a client delegation has
		// no function-calling protocol at all.
		// The nested `responses` object is required and must carry a `model`;
		// tools go inside it — delegation.responses.tools.
		"delegation": map[string]any{
			"type": "responses",
			"responses": map[string]any{
				"model": cfg.GptLiveDelegationModel,
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
		},
	},
		// toolHook payloads carry no callInfo, so X-Test-Id MUST ride the query
		// param for the webhook server to route to this session rather than the
		// shared `_anon` bag. No eventHook here: it would add traffic that
		// WaitCallbackFor("action/llm-gptlive-tool") silently discards.
		"toolHook", SessionURL(sess, "llm-gptlive-tool"),
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "llm")
	// The toolHook response must echo the LIVE tool_call_id jambonz just sent —
	// a static body cannot work. This closure runs on the webhook server
	// goroutine, so it stays pure: no t/s/StepCtx/assertions inside.
	sess.ScriptActionHookBodyFunc("llm-gptlive-tool", func(cb webhook.Callback) []byte {
		id := cb.String("tool_call_id")
		resp := map[string]any{
			// GPT-Live's function-result envelope; the feature-server sends the
			// follow-on response.create itself.
			"type": "response.item.create",
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

	recPath := filepath.Join(t.TempDir(), "llm-gptlive-tool-reply.pcm")

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

	s = Step(t, "wait-for-tool-call")
	toolCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	toolCB, err := sess.WaitCallbackFor(toolCtx, "action/llm-gptlive-tool")
	cancel()
	if err != nil {
		s.Fatalf("WaitCallbackFor(action/llm-gptlive-tool): %v — no tool call arrived. "+
			"Most likely causes, in order: (1) the session was refused before any tool "+
			"could be called — run TestVerb_LLM_GptLive_Session, which diagnoses that "+
			"explicitly (this test has no eventHook; check the feature-server log for the "+
			"error event); (2) the model simply chose not to call the tool.", err)
	}
	s.Logf("action/llm-gptlive-tool body: %s", string(toolCB.Body))
	if got := toolCB.String("name"); got != "get_weather" {
		s.Errorf("tool call name=%q want %q; body=%s", got, "get_weather", string(toolCB.Body))
	}
	if toolCB.String("tool_call_id") == "" {
		s.Errorf("tool call missing tool_call_id; body=%s", string(toolCB.Body))
	}
	s.Done()

	s = Step(t, "assert-tool-args")
	// Parsed function arguments arrive under "args", not "arguments".
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
	// Either phrase only appears in the reply if our response.item.create
	// round-tripped through the delegation and the model relayed it to the
	// caller — proving the full loop, response.create included.
	// Both are accepted because telephony STT renders "heavy hail" as "heavy
	// hill" often enough to fail a run where the loop worked.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail", "seventy one")
	s.Done()
}

// TestVerb_LLM_GptLive_AgentSpeaksFirst checks the agent CAN open the
// conversation with no caller speech at all.
//
// Only this test can establish it: the other two send a prompt WAV, so agent
// audio there may be a REPLY rather than a greeting. Here the caller sends
// nothing but the background silence symmetric-RTP requires, so audible audio
// can only be an unprompted first turn.
//
// HOW the agent is made to open the call: there is no response.create, and a
// greeting in `instructions` does not open the call, so once session.started
// arrives the test sends session.commentary.append (content for the model to
// speak) as an llm:update COMMAND. That is reachable only over the application
// WebSocket (REST updateCall rejects llm_update), hence placeWSCallTo.
//
// WHY IT STILL RETRIES. Appended content guides the model; it may stay silent,
// paraphrase, or be interrupted, streaming digital silence instead. So this
// makes up to gptLiveGreetAttempts calls and passes if any one greets.
//
// CRITICAL: output_audio.playback_started is NOT proof the agent spoke — the
// media server emits it for silent deltas too. Only the recording's amplitude
// is proof, which is why every attempt measures peak/rms.
const gptLiveGreetAttempts = 3

func TestVerb_LLM_GptLive_AgentSpeaksFirst(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S")
		return
	}
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 300*time.Second)

	// one attempt: place a call, send only silence, measure what the caller heard
	attempt := func(n int) (peak int, recPath string, sawSessionStarted bool, types []string) {
		uas := claimUAS(t, ctx)
		_, sess := claimSession(t)

		st := Step(t, fmt.Sprintf("attempt-%d-script-and-call", n))
		llmVerb := gptLiveVerb(map[string]any{
			"instructions": gptLiveGreetPrompt,
			"audio":        map[string]any{"output": map[string]any{"voice": gptLiveVoice}},
			"delegation":   map[string]any{"type": "client"},
		}, "eventHook", SessionURL(sess, "llm-gptlive-event"))
		sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
		SessionAckEmpty(sess, "llm")
		SessionAckEmpty(sess, "llm-gptlive-event")

		// WS transport: session.commentary.append is only reachable as an llm:update
		// COMMAND over the application WebSocket — the REST updateCall API does
		// not accept llm_update — so a webhook-transport call cannot ask the
		// agent to open the conversation at all.
		call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(90))
		if err := call.Answer(); err != nil {
			st.Fatalf("Answer: %v", err)
		}
		// outbound RTP FIRST: jambonz uses symmetric RTP and cannot return audio
		// until our silence opens the path, so recording before this loses the
		// greeting outright.
		if err := call.SendSilence(); err != nil {
			st.Fatalf("SendSilence: %v", err)
		}
		recPath = filepath.Join(t.TempDir(), fmt.Sprintf("greeting-%d.pcm", n))
		if err := call.StartRecording(recPath); err != nil {
			st.Fatalf("StartRecording: %v", err)
		}
		st.Done()

		st = Step(t, fmt.Sprintf("attempt-%d-listen", n))
		// A FIXED window, deliberately not gated on a playback event: that event
		// fires for silent deltas, so gating on it stopped the recorder before the
		// real greeting and made this flaky in both directions.
		var drained []webhook.Callback
		requested := false
		deadline := time.Now().Add(GptLiveGreetingWindow)
		for time.Now().Before(deadline) {
			batch := DrainCallbacks(sess, 2*time.Second)
			drained = append(drained, batch...)
			for _, cb := range batch {
				if cb.Hook != "action/llm-gptlive-event" {
					continue
				}
				ty := cb.String("type")
				if ty != "" {
					types = append(types, ty)
				}
				if ty != "session.started" {
					continue
				}
				sawSessionStarted = true
				if requested {
					continue
				}
				requested = true
				// Ask the agent to open the call, the moment the session is up.
				if err := sess.SendCommand("llm:update", map[string]any{
					"type":          "session.commentary.append",
					"delegation_id": nil,
					"content":       gptLiveGreetRequest,
				}); err != nil {
					st.Fatalf("SendCommand(llm:update session.commentary.append): %v", err)
				}
			}
		}
		if !requested && sawSessionStarted {
			st.Errorf("session.started arrived but the greeting request was never sent")
		}
		call.StopRecording()
		HangupAndWaitEnded(t, ctx, call)

		peak = gptLivePeak(t, recPath)
		st.Logf("attempt %d: peak=%d sessionStarted=%v", n, peak, sawSessionStarted)
		st.Done()
		return peak, recPath, sawSessionStarted, types
	}

	var bestPeak int
	var bestPath string
	var lastStarted bool
	var lastTypes []string
	for n := 1; n <= gptLiveGreetAttempts; n++ {
		peak, path, started, types := attempt(n)
		lastStarted, lastTypes = started, types
		if peak > bestPeak {
			bestPeak, bestPath = peak, path
		}
		if peak >= gptLiveAudibledB {
			break
		}
	}

	s = Step(t, "assert-greeting-heard")
	if bestPeak < gptLiveAudibledB {
		if !lastStarted {
			s.Errorf("no session.started in %d attempts, so the media server's input gate never "+
				"lifted and no caller audio reached the vendor — with no audio the model never "+
				"generates. events=%v", gptLiveGreetAttempts, lastTypes)
		} else {
			s.Errorf("the agent never opened the conversation in %d attempts (best peak=%d, "+
				"i.e. silence) even though a session.commentary.append greeting was sent on "+
				"each. Appended content GUIDES the model and it may stay silent, but %d "+
				"consecutive declines means either the request is not reaching the vendor "+
				"(check for a session.commentary.appended ack) or the vendor's behavior "+
				"changed. events=%v",
				gptLiveGreetAttempts, bestPeak, gptLiveGreetAttempts, lastTypes)
		}
		s.Done()
		return
	}
	// The passphrase can only be present if the agent spoke unprompted AND its
	// 24kHz audio survived resampling and playout to the caller.
	AssertTranscriptHasMost(s, ctx, bestPath, 1, gptLiveGreetPassphrase, gptLiveGreetPassphrase2)
	s.Done()
}

// gptLiveAudibledB is the peak amplitude above which a recording contains real
// speech rather than the comfort-noise floor (measured: silence peaks ~56,
// speech ~10000-20000).
const gptLiveAudibledB = 200

// gptLivePeak returns the loudest absolute sample in a raw PCM16 recording.
// Amplitude is the ONLY reliable evidence the agent spoke: the vendor streams
// silent audio frames when it declines, so frame counts and playback events
// cannot distinguish speech from silence.
func gptLivePeak(t *testing.T, path string) int {
	pcm, err := os.ReadFile(path)
	if err != nil {
		t.Logf("cannot read recording %s: %v", path, err)
		return -1
	}
	peak := 0
	for i := 0; i+1 < len(pcm); i += 2 {
		v := int(int16(uint16(pcm[i]) | uint16(pcm[i+1])<<8))
		if v < 0 {
			v = -v
		}
		if v > peak {
			peak = v
		}
	}
	return peak
}

// gptLiveMonologue is spoken via session.commentary.append so the agent is
// reliably mid-speech when the caller interrupts. The marker word appears
// only in the last sentence, so hearing it means the playout was not cut.
const (
	gptLiveMonologueMarker = "marmalade"
	gptLiveMonologue       = "Read the following story aloud, word for word, at a relaxed pace. " +
		"Do not stop until you reach the end unless the caller speaks.\n\n" +
		"Welcome to the pineapple story hour. Long ago, on a quiet island far from any city, " +
		"there lived an old lighthouse keeper named Tomas. Every evening he climbed one hundred " +
		"and twelve stone steps to light the great lamp, and every morning he walked down again " +
		"to feed his three grey cats. The fishermen of the island trusted his light more than " +
		"the stars, because in forty years it had never once failed. One stormy winter night the " +
		"wind grew so fierce that the windows rattled and the old stairs groaned beneath his " +
		"boots, but Tomas kept climbing, one careful step at a time, singing an old song his " +
		"mother had taught him. When he finally reached the top, the lamp flickered, steadied, " +
		"and burned brighter than ever before. And that is the end of the story of the " +
		"marmalade lighthouse."
	gptLiveInterruptPrompt = "Stop please. What is two plus two?"
)

// gptLiveCutMaxMS is the longest the agent may keep talking after the caller
// starts interrupting: vendor transcript latency plus playout drain. Without a
// drain the audio generated ahead of realtime keeps playing far longer.
const gptLiveCutMaxMS = 4000

// TestVerb_LLM_GptLive_BargeIn proves the caller can interrupt the agent.
// GPT-Live has no interruption event, so mediajam infers barge-in from the
// transcript timeline and drains its playout; the proof is the recording:
// the agent goes quiet soon after the caller starts talking, the story's last
// sentence is never heard, and the interrupting question gets answered.
//
// Steps:
//  1. preflight-skips — gptlive key guard, then deepgram guard (plain return)
//  2. ensure-prompt-wav — the interrupting question
//  3. script-and-call — gptlive verb + eventHook, WS transport (llm:update)
//  4. answer-and-record
//  5. start-monologue — on session.started, session.commentary.append
//  6. wait-for-agent-speech — first session.output_transcript.delta, then 2s
//  7. interrupt — send the question WAV while the agent is talking
//  8. wait-for-answer
//  9. hangup-and-wait-ended
//  10. assert-interruption-heard — input transcript mentions "two"
//  11. assert-playout-cut — mediajam reports an interrupted playout, and the
//      recording goes quiet within gptLiveCutMaxMS of the interruption
//  12. assert-answer-not-story — "four" heard, marker word not heard
func TestVerb_LLM_GptLive_BargeIn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S barge-in")
		return
	}
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S barge-in")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", gptLiveInterruptPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-and-call")
	llmVerb := gptLiveVerb(map[string]any{
		"instructions": "You are a voice assistant on a phone call. Always speak English. " +
			"If the caller interrupts you, stop and answer their question in one short sentence.",
		"audio":      map[string]any{"output": map[string]any{"voice": gptLiveVoice}},
		"delegation": map[string]any{"type": "client"},
	}, "eventHook", SessionURL(sess, "llm-gptlive-event"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	SessionAckEmpty(sess, "llm-gptlive-event")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "gptlive-bargein.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	recStart := time.Now()
	s.Done()

	var drained []webhook.Callback
	eventsOf := func(batch []webhook.Callback, ty string) []webhook.Callback {
		var out []webhook.Callback
		for _, cb := range batch {
			if cb.Hook == "action/llm-gptlive-event" && cb.String("type") == ty {
				out = append(out, cb)
			}
		}
		return out
	}

	s = Step(t, "start-monologue")
	started := false
	for deadline := time.Now().Add(20 * time.Second); !started && time.Now().Before(deadline); {
		batch := DrainCallbacks(sess, time.Second)
		drained = append(drained, batch...)
		started = len(eventsOf(batch, "session.started")) > 0
	}
	if !started {
		s.Fatalf("no session.started within 20s; completionReason=%q", gptLiveCompletionReason(drained))
	}
	if err := sess.SendCommand("llm:update", map[string]any{
		"type":          "session.commentary.append",
		"delegation_id": nil,
		"content":       gptLiveMonologue,
	}); err != nil {
		s.Fatalf("SendCommand(llm:update session.commentary.append): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-agent-speech")
	speaking := false
	for deadline := time.Now().Add(20 * time.Second); !speaking && time.Now().Before(deadline); {
		batch := DrainCallbacks(sess, 500*time.Millisecond)
		drained = append(drained, batch...)
		speaking = len(eventsOf(batch, "session.output_transcript.delta")) > 0
	}
	if !speaking {
		s.Fatalf("the agent never started the monologue (no session.output_transcript.delta in 20s)")
	}
	// transcripts trail the audio; this puts the caller well inside the story
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "interrupt")
	interruptMS := int(time.Since(recStart).Milliseconds())
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Logf("interrupted at %dms into the recording", interruptMS)
	s.Done()

	s = Step(t, "wait-for-answer")
	drained = append(drained, DrainCallbacks(sess, 12*time.Second)...)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-interruption-heard")
	var heard strings.Builder
	for _, cb := range eventsOf(drained, "session.input_transcript.delta") {
		heard.WriteString(cb.String("delta"))
	}
	s.Logf("caller transcript seen by GPT-Live: %q", heard.String())
	if !strings.Contains(strings.ToLower(heard.String()), "two") {
		s.Errorf("GPT-Live never transcribed the interruption (%q); without it mediajam has no "+
			"barge-in signal", heard.String())
	}
	s.Done()

	s = Step(t, "assert-playout-cut")
	// mediajam emits this only when it drains the playout on a barge-in, which
	// separates its drain from the vendor simply pausing
	drainedByMediajam := false
	for _, cb := range eventsOf(drained, "output_audio.playback_stopped") {
		if cb.String("completion_reason") == "interrupted" {
			drainedByMediajam = true
		}
	}
	if !drainedByMediajam {
		s.Errorf("no output_audio.playback_stopped{completion_reason:interrupted} — mediajam " +
			"never took a barge-in from the transcript timeline")
	}
	gapAt, err := firstSilenceAfterMS(recPath, interruptMS, 800, gptLiveAudibledB)
	if err != nil {
		s.Fatalf("firstSilenceAfterMS: %v", err)
	}
	s.Logf("agent audio went quiet %dms after the interruption began", gapAt-interruptMS)
	if gapAt < 0 || gapAt-interruptMS > gptLiveCutMaxMS {
		s.Errorf("the agent kept talking for more than %dms after the caller interrupted "+
			"(quiet gap at %dms, interruption at %dms) — mediajam did not drain the playout",
			gptLiveCutMaxMS, gapAt, interruptMS)
	}
	s.Done()

	s = Step(t, "assert-answer-not-story")
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe: %v", err)
	}
	s.Logf("transcript: %q", transcript)
	if !strings.Contains(transcript, "pineapple") {
		s.Errorf("the story's opening was never heard, so nothing was interrupted: %q", transcript)
	}
	if strings.Contains(transcript, gptLiveMonologueMarker) {
		s.Errorf("the story's last sentence (%q) was heard — the interruption did not cut it off: %q",
			gptLiveMonologueMarker, transcript)
	}
	if !strings.Contains(transcript, "four") && !strings.Contains(transcript, " 4") {
		s.Errorf("the interrupting question was not answered (no \"four\"): %q", transcript)
	}
	s.Done()
}

// firstSilenceAfterMS returns the start (ms into a PCM16 8kHz recording) of
// the first run of at least minGapMS whose 10ms frames all peak below thresh,
// searching from fromMS; -1 if there is none.
func firstSilenceAfterMS(pcmPath string, fromMS, minGapMS int, thresh int) (int, error) {
	data, err := os.ReadFile(pcmPath)
	if err != nil {
		return -1, err
	}
	const frameBytes = 80 * 2
	runStart, run := -1, 0
	for off := (fromMS / 10) * frameBytes; off+frameBytes <= len(data); off += frameBytes {
		peak := 0
		for i := 0; i < frameBytes; i += 2 {
			v := int(int16(uint16(data[off+i]) | uint16(data[off+i+1])<<8))
			if v < 0 {
				v = -v
			}
			if v > peak {
				peak = v
			}
		}
		if peak >= thresh {
			run = 0
			continue
		}
		if run == 0 {
			runStart = off / frameBytes * 10
		}
		run += 10
		if run >= minGapMS {
			return runStart, nil
		}
	}
	return -1, nil
}
