// Tests for the Layer-1 `handoff` capability on conversational verbs (spec §6):
// a declarative `handoff` block auto-injects a `transfer_to_human` tool into the
// LLM toolset, and when the model calls it jambonz runs the packaged transfer
// choreography internally. These tests use the OpenAI vendor and require an
// OpenAI API key (OPENAI_API_KEY, passed inline as the verb's llm auth); they
// skip cleanly when it is absent.
//
// Both tests use handoff{blindMethod:"dial"} so jambonz BRIDGES the caller to an
// answering target UAS leg — a deterministic "bridged" outcome — and the
// conversational verb's OWN actionHook reports completion_reason=="transferred"
// (Layer-1 suppresses the inner transfer actionHook; the outcome surfaces on the
// agent/llm verb). brief:"none" keeps the injected tool argument-free so the
// model can call it cleanly on its first turn.
package verbs

import (
	"context"
	"fmt"
	"slices"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// handoffForcePrompt makes the model call the injected transfer_to_human tool
// on its very first turn, with no speech. Used by the cascaded agent verb
// (chat-completions), which honors a forced first-turn tool call reliably.
const handoffForcePrompt = "You are a call-routing assistant. On your very first turn, " +
	"before saying anything else, immediately call the transfer_to_human function to hand the " +
	"caller to a human agent. Do not greet the caller. Do not speak. Call the tool first."

// handoffRealtimePrompt is for the realtime (s2s) llm verb. Per the canonical
// jambonz openai-s2s example, the realtime model is driven conversationally:
// it greets, the caller speaks, and the model calls the tool IN RESPONSE to the
// caller — its natural, reliable mode. (Forcing a cold first-turn tool call with
// no audio makes gpt-realtime emit the args as text, not a function_call.)
const handoffRealtimePrompt = "You are a call-routing assistant for a support line. " +
	"When the caller asks to speak to a human, a person, or an agent — or says they want a " +
	"transfer — immediately call the transfer_to_human function. Keep spoken replies very short."

// handoffCallerUtterance is the caller speech that should trigger the realtime
// model to call transfer_to_human. Synthesized to a WAV via Deepgram TTS.
const handoffCallerUtterance = "I need to speak to a human agent please, can you transfer me to a person."

// targetHoldAfterAnswer is how long the target/human leg stays up after
// answering before it hangs up. The bridged dial holds the agent verb alive
// until the bridge tears down, so the agent's actionHook (completion_reason=
// "transferred") only fires once this leg ends. A short hold proves the bridge
// formed without serializing the whole test behind the 90s call timeLimit.
const targetHoldAfterAnswer = 4 * time.Second

// answerAndIdleTarget answers the bridged target INVITE (100/180/200), sends
// silence to prove media flows, briefly holds, then hangs up to release the
// bridge so the agent/llm verb completes and fires its actionHook. Publishes
// the Call via *out for the main goroutine (Call holds a mutex — pointer, no copy).
func answerAndIdleTarget(t *testing.T, ctx context.Context, targetUAS *UAS, done chan<- struct{}, out **jsip.Call) func() {
	return func() {
		defer close(done)
		select {
		case c := <-targetUAS.Inbound:
			*out = c
			t.Logf("[target:trying] start")
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "target:trying", "Trying: %v", err)
				return
			}
			t.Logf("[target:ringing] start")
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "target:ringing", "Ringing: %v", err)
				return
			}
			t.Logf("[target:answer] start")
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "target:answer", "Answer: %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "target:silence", "SendSilence: %v", err)
				return
			}
			// Hold briefly to prove the bridge, then hang up to release it so
			// the agent/llm verb completes and fires its actionHook.
			select {
			case <-time.After(targetHoldAfterAnswer):
				t.Logf("[target] hanging up to release bridge")
				if err := c.Hangup(); err != nil {
					t.Logf("[target] hangup: %v", err)
				}
			case <-c.Done():
				t.Logf("[target] leg ended")
			case <-ctx.Done():
				t.Logf("[target] ctx done")
			}
		case <-ctx.Done():
			// Only a genuine timeout is a failure. context.Canceled means the
			// main goroutine already returned (e.g. an earlier step failed and
			// the test is tearing down) — logging via t.Errorf then would panic
			// ("Log after test completed"), so exit quietly.
			if ctx.Err() == context.DeadlineExceeded {
				GoroutineFailf(t, "target", "never received INVITE: %v", ctx.Err())
			}
		}
	}
}

// TestVerb_LLM_OpenAI_Handoff — Layer-1 handoff on the `llm` verb, OpenAI vendor
// (s2s realtime / gpt-realtime). Driven CONVERSATIONALLY, matching the canonical
// jambonz openai-s2s example: response_create greets, tool_choice is "auto", and
// the model calls transfer_to_human IN RESPONSE to the caller asking for a human.
//
// Why conversational, not a forced first-turn call: gpt-realtime, when forced
// (tool_choice:{type:function,...}) to call a tool cold on turn 1 with no audio
// and a "don't speak" prompt, returns the tool ARGUMENTS as a text message
// (response.output_text) instead of a function_call output item — so jambonz
// never sees a tool call. Given real caller audio it emits a proper function_call
// reliably. This mirrors the example, which only calls get_weather after the
// caller asks; it never forces a tool call.
func TestVerb_LLM_OpenAI_Handoff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	// Needs OpenAI (the realtime LLM) + Deepgram (to synthesize the caller's
	// spoken request — the realtime model needs real audio to call the tool).
	if !cfg.HasOpenAI() || !cfg.HasDeepgram() {
		s.Done()
		t.Skip("llm OpenAI handoff test needs OPENAI_API_KEY (llm.auth) + DEEPGRAM_API_KEY (caller TTS)")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/handoff", handoffCallerUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV caller utterance: %v", err)
	}
	s.Logf("caller utterance wav: %s", callerWAV)
	s.Done()

	s = Step(t, "script-llm-openai-handoff")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// Matches the openai-s2s example shape: response_create greets (speaks),
	// session_update carries instructions + tools; the handoff tool is injected
	// by the feature-server. tool_choice defaults to "auto" — we do NOT force it,
	// so the realtime model emits a real function_call when the caller asks.
	llmVerb := V("llm",
		"vendor", "openai",
		"auth", map[string]any{"apiKey": cfg.OpenAIAPIKey},
		"actionHook", SessionURL(sess, "llm"),
		"llmOptions", map[string]any{
			"response_create": map[string]any{
				"instructions": "Greet the caller in one short sentence and ask how you can help.",
			},
			"session_update": map[string]any{
				"type":         "realtime",
				"instructions": handoffRealtimePrompt,
			},
		},
		"handoff", map[string]any{
			"mode":        "blind",
			"blindMethod": "dial",
			"brief":       "none",
			"target": []any{map[string]any{
				"type": "user",
				"name": target,
			}},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		llmVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	go answerAndIdleTarget(t, ctx, targetUAS, targetDone, &targetCall)()
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Let the realtime session connect and the model deliver its greeting, then
	// speak the request. Trailing silence lets server-VAD detect end-of-utterance
	// so the model takes its turn and calls transfer_to_human.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	WaitFor(t, "let-model-greet", RecognizerArmDelay)
	if err := call.SendWAV(callerWAV); err != nil {
		s.Fatalf("SendWAV caller utterance: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-answered")
	// The human leg answering (100/180/200) proves jambonz ran the handoff
	// transfer and bridged — the whole point of Layer-1.
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler (handoff did not dial the human)")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "wait-action-llm-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/llm")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/llm: %v", err)
	}
	s.Logf("action/llm body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-completion-transferred")
	if got := cb.String("completion_reason"); got != "transferred" {
		s.Errorf("completion_reason: got %q want %q", got, "transferred")
	}
	s.Done()
}

// TestVerb_Agent_OpenAI_Handoff — Layer-1 handoff on the cascaded `agent` verb
// (Deepgram STT/TTS, OpenAI LLM via inline auth). greeting:true drives the model
// to act on turn 1; the force prompt makes it call transfer_to_human; jambonz
// dials + bridges the target, and the agent actionHook reports
// completion_reason=="transferred".
func TestVerb_Agent_OpenAI_Handoff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasOpenAI() || !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Skip("agent OpenAI handoff test needs OPENAI_API_KEY + DEEPGRAM_API_KEY (and a provisioned deepgram label)")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-agent-openai-handoff")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	agentVerb := V("agent",
		"stt", map[string]any{"vendor": "deepgram", "label": deepgramLabel, "language": "en-US"},
		"tts", map[string]any{"vendor": "deepgram", "label": deepgramLabel, "voice": deepgramVoice},
		"llm", map[string]any{
			"vendor": "openai",
			"model":  "gpt-4o",
			"auth":   map[string]any{"apiKey": cfg.OpenAIAPIKey},
			"llmOptions": map[string]any{
				"systemPrompt": handoffForcePrompt,
				"maxTokens":    128,
			},
		},
		// greeting:true → the model takes the first turn (no caller audio needed)
		// and, per the prompt, calls the handoff tool immediately.
		"greeting", true,
		"turnDetection", "stt",
		"bargeIn", map[string]any{"enable": false},
		"actionHook", SessionURL(sess, "agent-complete"),
		"eventHook", SessionURL(sess, "agent-turn"),
		"handoff", map[string]any{
			"mode":        "blind",
			"blindMethod": "dial",
			"brief":       "none",
			"target": []any{map[string]any{
				"type": "user",
				"name": target,
			}},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		agentVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "spawn-target-goroutine")
	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	go answerAndIdleTarget(t, ctx, targetUAS, targetDone, &targetCall)()
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	_ = AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "wait-target-done")
	<-targetDone
	s.Done()

	s = Step(t, "assert-target-answered")
	if targetCall == nil {
		s.Fatal("target call was never handed to the handler (handoff did not dial the human)")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "wait-action-agent-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/agent-complete: %v", err)
	}
	s.Logf("action/agent-complete body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-completion-transferred")
	if got := cb.String("completion_reason"); got != "transferred" {
		s.Errorf("completion_reason: got %q want %q", got, "transferred")
	}
	s.Done()
}
