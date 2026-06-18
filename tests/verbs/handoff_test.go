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
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// handoffForcePrompt makes the model call the injected transfer_to_human tool
// on its very first turn, with no speech. Determinism strategy for a live LLM:
// be explicit and tool-first. brief:"none" means the tool takes no required
// args, so the model only has to emit the call.
const handoffForcePrompt = "You are a call-routing assistant. On your very first turn, " +
	"before saying anything else, immediately call the transfer_to_human function to hand the " +
	"caller to a human agent. Do not greet the caller. Do not speak. Call the tool first."

// answerAndIdleTarget is the target/human UAS goroutine for the handoff dial
// bridge: answer the inbound INVITE, send silence, then wait for the leg to end
// (jambonz BYEs it when the parent call ends) or ctx to expire. Its only job is
// to ANSWER (100/180/200) so the dial bridges and the handoff resolves bridged.
// It publishes the received Call via *out so the main goroutine can assert the
// SIP wire after the leg ends (Call must not be copied — it holds a mutex).
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
			select {
			case <-c.Done():
				t.Logf("[target] leg ended")
			case <-ctx.Done():
				t.Logf("[target] ctx done")
			}
		case <-ctx.Done():
			GoroutineFailf(t, "target", "never received INVITE: %v", ctx.Err())
		}
	}
}

// TestVerb_LLM_OpenAI_Handoff — Layer-1 handoff on the `llm` verb, OpenAI vendor.
// The model is prompted to call transfer_to_human on its first turn; jambonz
// dials + bridges the target UAS, the handoff resolves "bridged", and the llm
// verb's actionHook reports completion_reason=="transferred".
//
// Steps:
//  1. preflight-skips             — skip unless OPENAI_API_KEY set
//  2. script-llm-openai-handoff   — [llm openai + handoff(dial,target), hangup] + empty action ack
//  3. spawn-target-goroutine      — async: target answers (100/180/200) so the bridge forms
//  4. place-caller                — POST /Calls, answer caller, send silence
//  5. wait-target-done            — target leg ended
//  6. assert-target-answered      — target received INVITE, sent 100/180/200 (bridge formed)
//  7. wait-action-llm-callback    — block on /action/llm
//  8. assert-completion-transferred — completion_reason=="transferred"
func TestVerb_LLM_OpenAI_Handoff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasOpenAI() {
		s.Done()
		t.Skip("llm OpenAI handoff test needs OPENAI_API_KEY (passed inline as llm.auth.apiKey)")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-llm-openai-handoff")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// response_create is required by openai_s2s; session_update must exist for
	// the handoff tool to be injected into the realtime session's tool list.
	llmVerb := V("llm",
		"vendor", "openai",
		"auth", map[string]any{"apiKey": cfg.OpenAIAPIKey},
		"actionHook", SessionURL(sess, "llm"),
		"llmOptions", map[string]any{
			// session_update is applied BEFORE the initial response.create (verb
			// orders them session→response), so tool_choice here forces the model
			// to emit the transfer_to_human function call on its first turn rather
			// than an empty text response (observed live with tool_choice:auto).
			"response_create": map[string]any{
				"instructions": handoffForcePrompt,
			},
			"session_update": map[string]any{
				"type":         "realtime",
				"instructions": handoffForcePrompt,
				"tool_choice": map[string]any{
					"type": "function",
					"name": "transfer_to_human",
				},
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
	// No caller audio needed: the forced prompt makes the model call the tool on
	// its first turn. Answer + send silence; jambonz ends the leg on bridge.
	_ = AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
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
//
// Steps:
//  1. preflight-skips                — skip unless OPENAI_API_KEY + DEEPGRAM (+ label) present
//  2. script-agent-openai-handoff    — [agent(deepgram stt/tts, openai llm) + handoff(dial,target), hangup] + acks
//  3. spawn-target-goroutine         — async: target answers so the bridge forms
//  4. place-caller                   — POST /Calls, answer caller, send silence
//  5. wait-target-done               — target leg ended
//  6. assert-target-answered         — target received INVITE, 100/180/200
//  7. wait-action-agent-callback     — block on /action/agent-complete
//  8. assert-completion-transferred  — completion_reason=="transferred"
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
