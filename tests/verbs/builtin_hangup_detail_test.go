//go:build detail

package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Agent_OpenAI_Hangup_Inbound — same feature, INBOUND origination
// (UAC dials sip:app-<sid>@realm → InboundCallSession). Proves the X-Reason
// path works when jambonz is the callee, not just when it placed the call.
//
// Test    --CreateApplication call_hook=<tunnel>-->  api-server
// Test    --INVITE sip:app-<sid>@realm (X-Test-Id)-> Jambonz
// Jambonz --200 OK-->                                UAC   // greeting turn
//
//	// model calls hangup() on turn 1
//
// Jambonz --BYE (X-Reason: <reason>)-->              UAC   // end=remote-bye
// Jambonz --POST /action/agent-complete-->           Webhook // completion_reason=hangup
//
// Steps:
//  1. preflight-skips
//  2. script-agent-hangup (call_hook = [answer, pause, agent{hangup}])
//  3. invite-inbound — provision app + UAC INVITE its URI
//  4. send-silence-wait-ended — NAT latch, block on server BYE
//  5. assert-server-hangup — remote-bye + X-Reason
//  6. assert-completion-hangup
func TestVerb_Agent_OpenAI_Hangup_Inbound(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !hangupPreflightAgent(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s = Step(t, "script-agent-hangup")
	verb := buildHangupAgentVerb(SessionURL(sess, "agent-complete"), SessionURL(sess, "agent-turn"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "invite-inbound")
	call := inviteInbound(s, ctx, uas, testID, "agent-hangup-app")
	s.Done()

	s = Step(t, "send-silence-wait-ended")
	// We are the UAC; jambonz already answered (200 OK). Push silence to open
	// the NAT pinhole so the agent's media flows, then wait for the server BYE.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	waitServerBye(s, ctx, call)
	s.Done()

	s = Step(t, "assert-server-hangup")
	assertServerHangup(s, call, hangupAppReason)
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "agent-complete")
	s.Done()
}

// TestVerb_LLM_OpenAI_Hangup_Inbound — same realtime-llm feature, INBOUND
// origination (UAC dials sip:app-<sid>@realm → InboundCallSession).
//
// Steps:
//  1. preflight-skips
//  2. ensure-caller-wav
//  3. script-llm-hangup (call_hook = [answer, pause, llm{hangup}])
//  4. invite-inbound
//  5. drive-conversation — greet-window, speak "I'm done", wait BYE
//  6. assert-server-hangup
//  7. assert-completion-hangup
func TestVerb_LLM_OpenAI_Hangup_Inbound(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !hangupPreflightLlm(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV caller utterance: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-hangup")
	verb := buildHangupLlmVerb(SessionURL(sess, "llm"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "invite-inbound")
	call := inviteInbound(s, ctx, uas, testID, "llm-hangup-app")
	s.Done()

	s = Step(t, "drive-conversation")
	driveHangupConversation(s, ctx, call, callerWAV)
	s.Done()

	s = Step(t, "assert-server-hangup")
	// "" → require a non-empty X-Reason, not the app default: gpt-realtime
	// supplies its own free-form reason, which by design wins over hangup.reason.
	assertServerHangup(s, call, "")
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "llm")
	s.Done()
}
