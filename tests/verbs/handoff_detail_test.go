//go:build detail

package verbs

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Agent_OpenAI_Handoff_WarmCallerID — regression: `callerId` on the
// agent verb's handoff block must be presented to the transfer destination.
// This mirrors the exact production failure (eu.jambonz.io, 2026-07-12): an
// agent-verb WARM handoff with callerId configured dialed the human leg with
// an EMPTY From user (the transfer task drops data.callerId on the warm
// path), sbc-outbound presented "anonymous", and Twilio rejected the leg with
// 403 / error 32204 "Invalid Caller ID" — the handoff never reached a human.
// Blind/dial handoff (covered by TestVerb_Agent_OpenAI_Handoff) reads
// data.callerId directly and is not affected; the bug is warm-specific.
//
// A registered-user target accepts the call regardless of caller ID, letting
// us assert the From header on the UAS wire without a PSTN carrier in the
// loop. brief:"none" keeps the warm choreography minimal: park caller, dial
// human, bridge on answer.
//
// Asserts:
//
//	(a) the INVITE received by the human target carries the configured
//	    callerId in its From header (and does not present anonymous),
//	(b) the warm handoff completes end-to-end (agent actionHook reports
//	    completion_reason=="transferred").
//
// The handoff block also carries onHoldHook and custom SIP headers, pinning
// the handoff→transfer field forwarding (buildHandoffTransferData once
// silently dropped fields it didn't explicitly list — callerId, onHoldHook,
// headers were all lost that way at different times).
//
// Steps:
//  1. preflight-skips              — needs OPENAI_API_KEY + DEEPGRAM_API_KEY + deepgram label
//  2. script-agent-handoff-warm-callerid — agent verb with handoff{mode:warm, callerId, onHoldHook, headers} + hook scripts
//  3. spawn-target-goroutine       — async: answer, silence, brief hold, hang up (answerAndIdleTarget)
//  4. place-caller                 — POST /Calls, answer caller leg, hold until ended
//  5. wait-target-done             — wait for target goroutine to finish
//  6. assert-target-answered       — target received INVITE, sent 100/180/200
//  7. assert-target-caller-id      — target INVITE From contains the configured callerId
//  8. assert-target-custom-header  — target INVITE carries the handoff's custom SIP header
//  9. wait-onhold-callback         — /action/onhold was invoked with event_type=="transfer.on-hold"
//
// 10. wait-action-agent-callback   — block on /action/agent-complete
// 11. assert-completion-transferred — completion_reason=="transferred"
func TestVerb_Agent_OpenAI_Handoff_WarmCallerID(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasOpenAI() || !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Skip("agent OpenAI warm-handoff test needs OPENAI_API_KEY + DEEPGRAM_API_KEY (and a provisioned deepgram label)")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	// Distinctive E.164 number no other fixture uses: if it shows up in the
	// target's From header it can only have come from the handoff's callerId.
	const handoffCallerID = "+15005550043"

	s = Step(t, "script-agent-handoff-warm-callerid")
	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	// Hold verbs served while the caller is parked. The target answers
	// immediately and brief is "none", so the say may be cut short by the
	// bridge — this test only asserts the hook was INVOKED (field forwarding);
	// the audible-hold-audio contract is covered by
	// TestVerb_Transfer_WarmParked_OnHoldHook.
	sess.ScriptActionHook("onhold", webhook.Script{
		V("say", "text", "Please hold while we connect you."),
	})
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
			"mode":          "warm",
			"callerPresent": false,
			"brief":         "none",
			"callerId":      handoffCallerID,
			"onHoldHook":    SessionURL(sess, "onhold"),
			"headers":       map[string]any{"X-Handoff-Test": "smoke-1"},
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
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). Passing targetCtx into answerAndIdleTarget makes its
	// internal <-ctx.Done()/ctx.Err() lifetime guards bind to this child.
	targetCtx, targetCancel := context.WithCancel(ctx)
	go answerAndIdleTarget(t, targetCtx, targetUAS, targetDone, &targetCall)()
	// Always join the goroutine, even if a later Step fatals (t.Fatalf →
	// runtime.Goexit skips the explicit join below). Registered after spawn so
	// it runs (LIFO) before WithTimeout's ctx-cancel cleanup, while t is still
	// valid: cancel unblocks the goroutine, then we wait for it to exit —
	// preventing a "Log in goroutine after test completed" panic on a
	// place-call fatal (e.g. "480 no available feature servers").
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})
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
		s.Fatal("target call was never handed to the handler (warm handoff did not dial the human)")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "assert-target-caller-id")
	// The core regression assertion: the handoff's callerId must survive
	// agent verb → transfer task (warm path) → sbc-outbound → target INVITE.
	// With the bug, the From user is empty and sbc-outbound substitutes
	// "anonymous" (which a PSTN carrier would reject outright).
	from := targetCall.From()
	s.Logf("target INVITE From: %s", from)
	if !strings.Contains(from, handoffCallerID) {
		s.Errorf("target INVITE From = %q, want it to contain configured handoff callerId %q", from, handoffCallerID)
	}
	if strings.Contains(strings.ToLower(from), "anonymous") {
		s.Errorf("target INVITE From = %q presents anonymous — handoff callerId was dropped", from)
	}
	s.Done()

	s = Step(t, "assert-target-custom-header")
	// Pins headers forwarding: handoff → buildHandoffTransferData → transfer
	// task → placeHumanLeg opts.headers → INVITE (sbc-outbound proxies all
	// non-internal request headers to the B-leg).
	if got := targetCall.Header("X-Handoff-Test"); got != "smoke-1" {
		s.Errorf("target INVITE X-Handoff-Test: got %q want %q — handoff headers were dropped", got, "smoke-1")
	}
	s.Done()

	s = Step(t, "wait-onhold-callback")
	// Pins onHoldHook forwarding: the hook fired while the caller was parked;
	// its callback is already queued by the time the call ends.
	waitCtxH, wcancelH := context.WithTimeout(ctx, 15*time.Second)
	defer wcancelH()
	hcb, err := sess.WaitCallbackFor(waitCtxH, "action/onhold")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/onhold: %v — handoff onHoldHook was dropped", err)
	}
	s.Logf("action/onhold body: %s", string(hcb.Body))
	if got := hcb.String("event_type"); got != "transfer.on-hold" {
		s.Errorf("onHoldHook event_type: got %q want %q", got, "transfer.on-hold")
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
