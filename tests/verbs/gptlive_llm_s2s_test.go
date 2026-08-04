// Tests for the `llm` verb with LLM vendor "gptlive" — OpenAI's GPT Live API
// (limited-access alpha).
//
// gptlive is an OPTIONAL vendor (see config.HasGptLive). When GPTLIVE_API_KEY
// is unset both tests pass immediately without exercising the GPT Live path —
// a plain `return` after a log, never t.Skip, never a failure, matching
// xai_llm_s2s_test.go / xai_agent_test.go.
//
// WHY THESE TESTS EXIST, beyond vendor coverage: GPT Live's Event API
// reference documents the event stream but NOT the connection URL, the auth
// scheme, or the session object's schema. The feature-server therefore
// *infers* wss://api.openai.com/v1/live?model=<model> with a Bearer header,
// and originally guessed where tool definitions belong. Unit tests cannot
// validate an inference about somebody else's server — only a real call can,
// and running these found two such guesses wrong (the mandatory OpenAI-Alpha
// header, and tools belonging at delegation.responses.tools rather than
// delegation.tools). TestVerb_LLM_GptLive_Session guards the URL/auth/startup
// contract,
// and reports the actionHook's completionReason on failure so a wrong guess
// reads as "connection failure" rather than a vague timeout. GPTLIVE_HOST /
// GPTLIVE_PATH let a run correct the URL without a code change.
//
// How GPT Live differs from the sibling realtime vendors (openai/xai), i.e.
// what these tests must NOT copy from xai_llm_s2s_test.go:
//   - llmOptions carries ONLY session_update. There is no response_create and
//     no response.create client event; the model starts and drives the
//     conversation itself once the session is started.
//   - `model` must NOT appear inside session_update (the feature-server
//     rejects the verb) — it travels in the connection URL.
//   - The session is not ready, and caller audio is gated, until the server
//     emits `session.started` (not session.created/session.updated).
//   - Tool calling only exists via a Responses-targeted *delegation*:
//     session_update.delegation = {type:'responses', tools:[...]}. With
//     delegation.type 'client' the model instead asks the application for
//     free-form text context and there is no function-calling protocol at
//     all — the feature-server rejects the verb if handoff/hangup/mcpServers
//     are configured without a 'responses' delegation.
//   - toolHook request: {tool_call_id, name, args} — same field names as the
//     other s2s vendors.
//   - toolHook response: {type:"delegation.function_call_output.create",
//     item:{type:"function_call_output", call_id:<echo the live
//     tool_call_id>, output:<result>}} — NOT openai/xai's
//     conversation.item.create. See feature-server
//     TaskLlmGptLive_S2S.processToolOutput, which accepts only that type.
package verbs

import (
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// gptLiveGreetPassphrase is the distinctive word the agent is told to open the
// call with. It must be a word STT returns reliably and one that cannot appear
// by chance in a generic greeting.
const gptLiveGreetPassphrase = "aardvark"

// gptLiveGreetPrompt tells the model to open the conversation. GPT Live has no
// response_create, so instructions are the only lever on the first turn.
const gptLiveGreetPrompt = "You are a voice assistant on a phone call. " +
	"Open the conversation yourself the moment the call connects: say exactly " +
	"\"Welcome to the aardvark hotline, how can I help?\" and nothing else. " +
	"Do not wait for the caller to speak first. Always speak English."

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

// gptLiveClientDelegationID returns the item id of the first client-targeted
// delegation.created in the stream, or "" if the model never raised one.
func gptLiveClientDelegationID(cbs []webhook.Callback) string {
	for _, cb := range cbs {
		if cb.Hook != "action/llm-gptlive-event" || cb.String("type") != "delegation.created" {
			continue
		}
		if cb.NestedString("item.target") == "client" {
			if id := cb.NestedString("item.id"); id != "" {
				return id
			}
		}
	}
	return ""
}

// gptLiveSawInputTranscript reports whether the event stream contains evidence
// that OpenAI recognized CALLER audio: an input transcript fragment, or a
// projected turn with role "user". Either one can only appear if the media
// server lifted its input gate and forwarded our audio.
func gptLiveSawInputTranscript(cbs []webhook.Callback) bool {
	for _, cb := range cbs {
		if cb.Hook != "action/llm-gptlive-event" {
			continue
		}
		switch cb.String("type") {
		case "input_transcript.added":
			return true
		case "turn.created", "turn.done":
			if cb.NestedString("turn.role") == "user" {
				return true
			}
		}
	}
	return false
}

// TestVerb_LLM_GptLive_Session proves the gptlive path connects, starts a
// session, and carries audio both directions.
//
// This is the test that validates the feature-server's *inferred* connection
// contract, which no unit test can reach:
//  1. the URL (wss://api.openai.com/v1/live?model=<model>) and Bearer auth
//     resolve to a real GPT Live endpoint — otherwise the verb ends with
//     completionReason "connection failure";
//  2. a session_update carrying instructions + audio.output.voice +
//     delegation{type:client} is ACCEPTED — otherwise the server sends an
//     `error` before session.started and the verb ends with "server error";
//  3. the server emits `session.started`, which is what lifts the media
//     server's input gate — if this never arrives, caller audio is silently
//     dropped for the life of the call;
//  4. output_audio.delta audio decodes and reaches the caller at the right
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
	// These writes are deliberately NON-fatal. If GPT Live rejects the session
	// (e.g. the key is not enrolled in the alpha), the feature-server ends the
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
		case errCode == "forbidden" || strings.Contains(strings.ToLower(errMsg), "access denied"):
			// The handshake succeeded (so URL/auth/OpenAI-Alpha are all right)
			// but the account is not entitled to open a voice session. This is
			// an entitlement problem, not a code problem — and it is a FAILURE
			// rather than a skip because setting GPTLIVE_API_KEY is an explicit
			// statement of intent to exercise GPT Live.
			s.Errorf("GPT Live refused the session: %s (code=%q). The websocket handshake "+
				"succeeded, so the URL, Bearer auth and OpenAI-Alpha header are correct — "+
				"this key is simply not enrolled in the GPT Live alpha. Use a key from an "+
				"account in the Early Access Program, or unset GPTLIVE_API_KEY to skip.",
				errMsg, errCode)
		case reason == "connection failure":
			s.Errorf("never reached session.started and the verb ended with %q — the GPT Live "+
				"URL/auth is wrong (tried host=%q path=%q). NOTE /v1/live also requires the "+
				"OpenAI-Alpha header (mediajam sends quicksilver=v2; override with "+
				"JAMBONES_GPTLIVE_ALPHA). events seen: %v",
				reason, cfg.GptLiveHost, cfg.GptLivePath, types)
		case errCode != "" || reason == "server error":
			s.Errorf("never reached session.started: GPT Live rejected the startup "+
				"session.update with code=%q %q (completionReason=%q) — check the session "+
				"object shape against the alpha guide. events seen: %v",
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
	// A `client` delegation is the model asking the APPLICATION for text
	// context. Answering it exercises `delegation.context.append` and — the
	// point of doing it here — the `delegation_item_id` field name, which is
	// inferred from the Event API reference and which both sample apps depend
	// on. The feature-server only whitelists the event TYPE and forwards the
	// body verbatim, so a wrong field name would be a silent no-op: OpenAI
	// replies with an `error`, which is non-fatal once the session has started,
	// and the model just proceeds without the context it asked for.
	if id := gptLiveClientDelegationID(drained); id != "" {
		s.Logf("answering client delegation %s", id)
		body := map[string]any{
			"llm_update": map[string]any{
				"type":               "delegation.context.append",
				"delegation_item_id": id,
				"content": []any{map[string]any{
					"type": "input_text",
					"text": "The caller is a long-standing customer on the Unlimited plan.",
				}},
			},
		}
		if err := client.UpdateCall(ctx, callSID, body); err != nil {
			s.Fatalf("UpdateCall(llm_update: delegation.context.append) sid=%s: %v", callSID, err)
		}
		// Any error the server raises about our shape arrives on the event hook.
		after := DrainCallbacks(sess, 8*time.Second)
		drained = append(drained, after...)
		for _, cb := range after {
			if cb.Hook == "action/llm-gptlive-event" && cb.String("type") == "error" {
				s.Errorf("GPT Live rejected our delegation.context.append: code=%q %q — the "+
					"field names (delegation_item_id / content[].input_text) are inferred from "+
					"the Event API reference and both sample apps use them",
					cb.NestedString("error.code"), cb.NestedString("error.message"))
			}
		}
	} else {
		s.Logf("no client delegation was raised on this call; delegation.context.append not exercised")
	}
	s.Done()

	// FIX: proof that CALLER audio actually reached OpenAI. The passphrase alone
	// cannot show this — GPT Live has no response_create, so the model speaks
	// first and its unprompted greeting already contains the passphrase. An
	// input transcript can only exist if the media server lifted the input gate
	// and our audio was recognized.
	if !gptLiveSawInputTranscript(drained) {
		extra := DrainCallbacks(sess, 5*time.Second)
		drained = append(drained, extra...)
	}
	if !gptLiveSawInputTranscript(drained) {
		s.Errorf("no input_transcript.added / user turn observed — the caller's audio never " +
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
	// output_audio.delta frames were base64-decoded, resampled from 24 kHz and
	// mixed back to the caller.
	AssertTranscriptHasMost(s, ctx, recPath, 1, gptLivePassphrase)
	s.Done()
}

// TestVerb_LLM_GptLive_ToolHook proves the gptlive path supports app-declared
// tool/function calling end-to-end through a Responses-targeted delegation:
// tools declared at session_update.delegation.tools reach the model, the model
// calls get_weather with an argument parsed from the caller's speech, the
// test's toolHook answers with the vendor-native
// delegation.function_call_output.create envelope (echoing the live
// tool_call_id, known only at call time), and the agent speaks the result back.
//
// This is the test that guards the tool-declaration contract the Event API
// reference does not document: delegation.responses.tools, with a mandatory
// delegation.responses.model. If either drifts, the server either rejects the
// session or silently drops the tools, and no tool call ever arrives.
//
// Reuses the weather prompt/system-prompt/result consts from llm_test.go — the
// scenario is vendor-agnostic.
//
// Steps mirror TestVerb_LLM_Xai_ToolHook, with the GPT Live envelopes:
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
		// The nested `responses` object is REQUIRED and must carry a `model`;
		// verified against the alpha, which rejects {type:"responses"} with
		// "Missing required parameter: 'delegation.responses'" and
		// {responses:{}} with "'delegation.responses.model'". Tools go INSIDE
		// it — delegation.responses.tools, not delegation.tools.
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
			// GPT Live's function-result envelope. NOT openai/xai's
			// conversation.item.create — TaskLlmGptLive_S2S.processToolOutput
			// accepts only this type, and there is no follow-on response.create.
			"type": "delegation.function_call_output.create",
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
			"Most likely causes, in order: (1) this key is not enrolled in the GPT Live alpha, "+
			"so the session was refused before any tool could be called — run "+
			"TestVerb_LLM_GptLive_Session, which diagnoses that explicitly; (2) the session "+
			"was rejected for another reason (this test has no eventHook, so check the "+
			"feature-server log for the error event); (3) the model simply chose not to call "+
			"the tool. The delegation.responses.tools placement itself is verified against "+
			"the alpha and is not a likely cause.", err)
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
	// "hail" only appears in the reply if our
	// delegation.function_call_output.create envelope round-tripped through the
	// delegation and the model relayed it to the caller — proving the full loop.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "hail")
	s.Done()
}

// TestVerb_LLM_GptLive_AgentSpeaksFirst proves the agent opens the conversation
// with NO caller speech at all.
//
// This is the only test that can prove it. The other two send a prompt WAV, so
// agent audio there may be a REPLY rather than a greeting — indistinguishable
// from the outside. Here the caller sends nothing but background silence (which
// SendSilence must keep flowing anyway, for symmetric-RTP NAT traversal), so a
// passphrase in the recording can only have come from an unprompted first turn.
//
// GPT Live has no response_create, so there is no explicit "speak now" trigger:
// the model decides, and instructions are the only lever. If this fails, the
// diagnosis below distinguishes the three causes that look identical to a
// caller — the session never started, the model never generated, or the model
// generated and something downstream discarded the audio (barge-in draining the
// playout is the live suspect, since a user turn projected from silence would
// clear the agent's own greeting).
//
// Steps:
//  1. preflight-skips  2. script-llm-verb (greeting instruction, eventHook)
//  3. place-call  4. answer-and-record (silence only, NEVER a prompt WAV)
//  5. wait-for-greeting (drain events; look for an assistant turn)
//  6. stop-and-hangup  7. assert-greeting-heard
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

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-llm-verb")
	llmVerb := gptLiveVerb(map[string]any{
		"instructions": gptLiveGreetPrompt,
		"audio":        map[string]any{"output": map[string]any{"voice": gptLiveVoice}},
		"delegation":   map[string]any{"type": "client"},
	}, "eventHook", SessionURL(sess, "llm-gptlive-event"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	SessionAckEmpty(sess, "llm-gptlive-event")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	recPath := filepath.Join(t.TempDir(), "llm-gptlive-greeting.pcm")

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Outbound RTP FIRST. jambonz uses symmetric RTP, so it cannot return audio
	// until our silence has opened the path — starting the recorder before this
	// loses the very greeting we are measuring, which is the whole assertion.
	// Silence is also what makes the vendor generate, but the caller says
	// NOTHING, so anything recorded is the agent's own opening turn.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	// A FIXED capture window, deliberately not gated on an event. Stopping as
	// soon as output_audio.playback_started appeared made this test flaky (it
	// passed alone and failed under parallel load, capturing rms=2.9): that
	// event can be a queued or very short first burst, so the recorder was
	// stopped before the substantive greeting arrived. The model decides when to
	// open — there is no response_create to sequence against — so just record
	// long enough for it, and use the event stream only to diagnose a failure.
	var drained []webhook.Callback
	sawSessionStarted, agentPlayed, firstTurnRole := false, false, ""
	deadline := time.Now().Add(GptLiveGreetingWindow)
	for time.Now().Before(deadline) {
		batch := DrainCallbacks(sess, 2*time.Second)
		drained = append(drained, batch...)
		for _, cb := range batch {
			if cb.Hook != "action/llm-gptlive-event" {
				continue
			}
			switch cb.String("type") {
			case "session.started":
				sawSessionStarted = true
			case "output_audio.playback_started":
				agentPlayed = true
			case "turn.done":
				if firstTurnRole == "" {
					firstTurnRole = cb.NestedString("turn.role")
				}
			}
		}
	}
	if firstTurnRole != "" && firstTurnRole != "assistant" {
		s.Errorf("the first projected turn was role=%q, not assistant — the agent did not open "+
			"the conversation", firstTurnRole)
	}
	s.Done()

	s = Step(t, "stop-and-hangup")
	call.StopRecording()
	HangupAndWaitEnded(t, ctx, call)
	s.Done()

	s = Step(t, "assert-greeting-heard")
	if !agentPlayed {
		// Separate the three causes that are identical from the caller's seat.
		types := make([]string, 0, len(drained))
		errMsg := ""
		interrupted := false
		for _, cb := range drained {
			if cb.Hook != "action/llm-gptlive-event" {
				continue
			}
			ty := cb.String("type")
			if ty != "" {
				types = append(types, ty)
			}
			if ty == "error" {
				errMsg = cb.NestedString("error.code") + ": " + cb.NestedString("error.message")
			}
			if ty == "output_audio.playback_stopped" &&
				strings.Contains(string(cb.Body), "interrupted") {
				interrupted = true
			}
		}
		switch {
		case !sawSessionStarted:
			s.Errorf("no session.started, so the media server's input gate never lifted and no "+
				"caller audio (not even silence) reached the vendor — with no audio the model "+
				"never generates. error=%q events=%v", errMsg, types)
		case interrupted:
			s.Errorf("the agent DID generate a greeting and it was then discarded: an "+
				"output_audio.playback_stopped/interrupted arrived, i.e. barge-in drained the "+
				"playout. A user turn projected from the caller's silence would do exactly "+
				"this. events=%v", types)
		default:
			s.Errorf("session started and caller silence was flowing, but the vendor never sent "+
				"any agent audio within 25s — the model chose not to open the conversation. "+
				"instructions are the only lever (no response_create in this API). "+
				"error=%q events=%v", errMsg, types)
		}
	}
	// Measure the capture before transcribing it. An empty transcript has two
	// very different causes — nothing was recorded (RTP/ordering problem on our
	// side) versus audio was recorded but is silence (the agent's samples never
	// made it through resampling/playout) — and STT alone cannot tell them apart.
	if fi, err := os.Stat(recPath); err != nil {
		s.Errorf("no recording produced: %v", err)
	} else {
		pcm, rerr := os.ReadFile(recPath)
		if rerr != nil {
			s.Errorf("cannot read recording: %v", rerr)
		} else {
			var sum float64
			var peak int
			n := len(pcm) / 2
			for i := 0; i < n; i++ {
				v := int(int16(uint16(pcm[i*2]) | uint16(pcm[i*2+1])<<8))
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
				sum += float64(v) * float64(v)
			}
			rms := 0.0
			if n > 0 {
				rms = math.Sqrt(sum / float64(n))
			}
			s.Logf("recording: %d bytes (%d samples), rms=%.1f peak=%d", fi.Size(), n, rms, peak)
			if n > 0 && peak < 200 {
				s.Errorf("the recording is effectively silent (peak=%d) even though the media "+
					"server reported playing agent audio — the greeting's samples did not reach "+
					"the caller's RTP stream", peak)
			}
		}
	}
	// The passphrase can only be in the recording if the agent spoke unprompted
	// AND its 24kHz audio survived resampling and playout to the caller.
	AssertTranscriptHasMost(s, ctx, recPath, 1, gptLiveGreetPassphrase)
	s.Done()
}
