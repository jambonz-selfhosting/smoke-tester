// Tests for the built-in `hangup` capability on conversational verbs
// (feature-server commit 95cfcc5 "support builtin hangup"). A declarative
// `hangup` block ({reason?}) auto-injects a `hangup` tool into the LLM's
// toolset; when the model calls it, the runtime ends the call from the SERVER
// side — sending a BYE with the reason placed in an X-Reason SIP header — and
// the conversational verb's own actionHook reports completion_reason=="hangup".
//
// This is the sibling of the built-in `handoff` (handoff_test.go). Where handoff
// bridges the caller to a human and reports "transferred", hangup terminates the
// call and reports "hangup".
//
// ---- why both inbound AND outbound origination -----------------------------
// The reason-in-X-Reason path lives in the call-session's _jambonzHangup(reason),
// and jambonz has a DIFFERENT call-session subclass per origination direction:
//
//   - OUTBOUND (POST /Calls, application_sid) → RestCallSession
//   - INBOUND  (UAC dials sip:app-<sid>@realm) → InboundCallSession
//
// The reason must ride the BYE regardless of which subclass is driving the call.
// A regression that implements _jambonzHangup(reason) in only one subclass drops
// the X-Reason header on the other — invisible to a single-direction test. So we
// cover BOTH directions for BOTH conversational verbs (agent, llm). The outbound
// pair mirrors handoff_test.go's origination; the inbound pair mirrors
// answer_test.go's (provision an Application, UAC-INVITE its sip:app-<sid> URI).
//
// The observable contract this test pins (identical across all four cases):
//
//  1. jambonz initiates the BYE — the leg ends "remote-bye" WITHOUT the test
//     ever calling Hangup(). The LLM tool tore the call down, not the caller.
//  2. the BYE carries the X-Reason header derived from the hangup config / tool
//     arg (idiom from TestVerb_Hangup_WithHeaders:
//     call.ReceivedByMethod("BYE")[0].Headers["X-Reason"]).
//  3. the verb actionHook fires with completion_reason=="hangup".
//
// All four require OpenAI (OPENAI_API_KEY, inline verb auth) + Deepgram
// (STT/TTS + caller-utterance synthesis) and skip cleanly when absent.
package verbs

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// hangupAppReason is the app-supplied default reason placed on the hangup config
// block. When the model calls the injected hangup tool WITHOUT its own reason
// arg, the runtime falls back to this value and serializes it into X-Reason.
// A distinctive, header-safe token so an unrelated BYE reason can't false-pass.
const hangupAppReason = "smoke-test-caller-done"

// hangupForcePrompt drives the cascaded agent verb (chat-completions) to call
// the injected hangup tool on its very first turn, no caller speech required.
// The model must NOT supply its own reason arg so the runtime falls back to
// hangup.reason (asserted on the BYE's X-Reason).
const hangupForcePrompt = "You are a call-flow assistant. On your very first turn, " +
	"before saying anything else, immediately call the hangup function to end the call. " +
	"Do not greet the caller. Do not speak. Do not pass any arguments to the function. " +
	"Call the hangup tool first."

// hangupRealtimePrompt is for the realtime (s2s) llm verb, driven
// conversationally (see handoff_test.go for why gpt-realtime is not forced cold
// on turn 1). When the caller says they are done, the model calls hangup.
const hangupRealtimePrompt = "You are a support-line assistant. Keep spoken replies very " +
	"short. When the caller says they are finished, done, or want to end the call, " +
	"immediately call the hangup function to end the call."

// hangupCallerUtterance is the caller speech that should trigger the realtime
// model to call the hangup tool. Synthesized to a WAV via Deepgram TTS.
const hangupCallerUtterance = "Okay, that's everything I needed, I'm all done now. Please end the call, goodbye."

// --- shared verb builders ---------------------------------------------------

// buildHangupAgentVerb builds the cascaded agent verb with a built-in hangup
// block. actionEventURLs are wired by the caller (they differ per test since
// the actionHook key differs), keeping the verb identical across in/outbound.
func buildHangupAgentVerb(actionURL, eventURL string) map[string]any {
	return V("agent",
		"stt", map[string]any{"vendor": "deepgram", "label": deepgramLabel, "language": "en-US"},
		"tts", map[string]any{"vendor": "deepgram", "label": deepgramLabel, "voice": deepgramVoice},
		"llm", map[string]any{
			"vendor": "openai",
			"model":  "gpt-4o",
			"auth":   map[string]any{"apiKey": cfg.OpenAIAPIKey},
			"llmOptions": map[string]any{
				"systemPrompt": hangupForcePrompt,
				"maxTokens":    128,
			},
		},
		// greeting:true → the model takes the first turn (no caller audio needed)
		// and, per the prompt, calls the hangup tool immediately.
		"greeting", true,
		"turnDetection", "stt",
		"bargeIn", map[string]any{"enable": false},
		"actionHook", actionURL,
		"eventHook", eventURL,
		// The built-in hangup block: presence injects the tool; reason is the
		// app default used when the model supplies none.
		"hangup", map[string]any{"reason": hangupAppReason},
	)
}

// buildHangupLlmVerb builds the realtime OpenAI s2s llm verb with a built-in
// hangup block.
func buildHangupLlmVerb(actionURL string) map[string]any {
	return V("llm",
		"vendor", "openai",
		"auth", map[string]any{"apiKey": cfg.OpenAIAPIKey},
		"actionHook", actionURL,
		"llmOptions", map[string]any{
			"response_create": map[string]any{
				"instructions": "Greet the caller in one short sentence and ask how you can help.",
			},
			"session_update": map[string]any{
				"type":         "realtime",
				"instructions": hangupRealtimePrompt,
			},
		},
		"hangup", map[string]any{"reason": hangupAppReason},
	)
}

// --- shared assertions ------------------------------------------------------

// assertServerHangup pins the shared contract for a built-in hangup: the call
// ended server-side (remote-bye) and the BYE carried an X-Reason header.
//
// wantReason:
//   - non-empty → assert X-Reason CONTAINS it (case-insensitive). Use when the
//     reason is deterministic: the cascaded agent tests force the model to pass
//     NO tool arg, so the runtime falls back to the app default hangup.reason.
//   - "" → assert only that X-Reason is present and non-empty. Use for the
//     realtime s2s (llm) path: gpt-realtime supplies its OWN free-form reason
//     arg, which by design WINS over the app default (e.g. "User indicated they
//     are finished"). That text is model-generated and non-deterministic —
//     asserting its content would test the LLM's phrasing, not the feature. The
//     feature's contract is that SOME reason propagated onto the BYE.
func assertServerHangup(s *StepCtx, call *jsip.Call, wantReason string) {
	// remote-bye proves jambonz sent the BYE. The test never calls Hangup(), so
	// any non-remote-bye reason means the call didn't end the way the feature
	// promises (e.g. it timed out on timeLimit, or the caller-side tore down).
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("expected server-initiated hangup (end reason 'remote-bye'), got %q — "+
			"the LLM hangup tool did not tear the call down", reason)
	}
	byes := call.ReceivedByMethod("BYE")
	if len(byes) == 0 {
		s.Fatalf("no BYE captured; got methods=%v", MethodsOf(call.Received()))
	}
	got := byes[0].Headers["X-Reason"]
	if got == "" {
		s.Errorf("BYE missing X-Reason header (hangup reason not propagated); BYE headers=%v",
			byes[0].Headers)
		return
	}
	if wantReason != "" && !strings.Contains(strings.ToLower(got), strings.ToLower(wantReason)) {
		s.Errorf("BYE X-Reason = %q, want it to contain %q", got, wantReason)
	} else {
		s.Logf("BYE X-Reason = %q", got)
	}
}

// waitServerBye blocks until jambonz tears the call down. We never call Hangup()
// ourselves — the hangup tool must be what ends the call.
func waitServerBye(s *StepCtx, ctx context.Context, call *jsip.Call) {
	endCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := call.WaitState(endCtx, jsip.StateEnded); err != nil {
		s.Fatalf("call did not end (hangup tool never tore it down): %v", err)
	}
}

// assertCompletionHangup waits for the verb's actionHook and asserts
// completion_reason=="hangup".
func assertCompletionHangup(s *StepCtx, ctx context.Context, sess *webhook.Session, actionKey string) {
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/"+actionKey)
	if err != nil {
		s.Fatalf("WaitCallbackFor action/%s: %v", actionKey, err)
	}
	s.Logf("action/%s body: %s", actionKey, string(cb.Body))
	if got := cb.String("completion_reason"); got != "hangup" {
		s.Errorf("completion_reason: got %q want %q", got, "hangup")
	}
}

// inviteInbound provisions a webhook Application bound to sess's call_hook and
// UAC-INVITEs its dialable sip:app-<sid>@realm URI, threading testID as the
// X-Test-Id correlation header. Returns the outbound *jsip.Call (jambonz is the
// callee → the call runs under InboundCallSession on the server). Mirrors
// answer_test.go's inbound origination.
func inviteInbound(s *StepCtx, ctx context.Context, uas *UAS, testID, appSuffix string) *jsip.Call {
	appSID := provisionWebhookApp(s.t, ctx, appSuffix)
	s.Logf("provisioned Application sid=%s", appSID)
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers:   jsip.H{webhook.CorrelationHeader: testID},
	})
	if err != nil {
		s.Fatalf("Invite %s: %v", dest, err)
	}
	if got := call.AnsweredStatus(); got != 200 {
		s.Fatalf("inbound INVITE answered status: got %d want 200", got)
	}
	return call
}

// hangupPreflightAgent gates the cascaded-agent hangup tests.
func hangupPreflightAgent(t *testing.T, s *StepCtx) bool {
	if !cfg.HasOpenAI() || !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Skip("agent OpenAI hangup test needs OPENAI_API_KEY + DEEPGRAM_API_KEY (and a provisioned deepgram label)")
		return false
	}
	return true
}

// hangupPreflightLlm gates the realtime-llm hangup tests.
func hangupPreflightLlm(t *testing.T, s *StepCtx) bool {
	if !cfg.HasOpenAI() || !cfg.HasDeepgram() {
		s.Done()
		t.Skip("llm OpenAI hangup test needs OPENAI_API_KEY (llm.auth) + DEEPGRAM_API_KEY (caller TTS)")
		return false
	}
	return true
}

// =============================== AGENT (outbound) ===========================

// TestVerb_Agent_OpenAI_Hangup_Outbound — built-in hangup on the cascaded
// `agent` verb, OUTBOUND origination (POST /Calls → RestCallSession). greeting
// + force prompt makes the model call hangup with no arg → runtime falls back
// to hangup.reason → X-Reason on the BYE; agent actionHook reports "hangup".
//
// Test    --POST /Calls [agent{hangup:{reason}}]--> Jambonz
// Jambonz --INVITE-->                                UAS
// UAS     --200 OK-->                                Jambonz   (Answer)
//
//	// model calls hangup() on turn 1
//
// Jambonz --BYE (X-Reason: <reason>)-->              UAS       // end=remote-bye
// Jambonz --POST /action/agent-complete-->           Webhook   // completion_reason=hangup
//
// Steps:
//  1. preflight-skips
//  2. script-agent-hangup
//  3. place-caller (POST /Calls)
//  4. answer-and-wait-ended — answer, silence, block on server BYE
//  5. assert-server-hangup — remote-bye + X-Reason
//  6. assert-completion-hangup — actionHook completion_reason==hangup
func TestVerb_Agent_OpenAI_Hangup_Outbound(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !hangupPreflightAgent(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-agent-hangup")
	verb := buildHangupAgentVerb(SessionURL(sess, "agent-complete"), SessionURL(sess, "agent-turn"))
	// No trailing verb: if the tool fails to fire the call lingers to timeLimit
	// (end reason != remote-bye) and the assertion fails clearly — a trailing
	// hangup verb would mask a broken tool by ending the call anyway.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "place-caller")
	// timeLimit backstop: if the tool never fires, the call ends on the limit
	// (end reason != remote-bye) so the assertion fails instead of hanging.
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-wait-ended")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
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

// =============================== AGENT (inbound) ============================

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

// =============================== LLM (outbound) =============================

// TestVerb_LLM_OpenAI_Hangup_Outbound — built-in hangup on the realtime `llm`
// verb (OpenAI s2s / gpt-realtime), OUTBOUND origination (POST /Calls →
// RestCallSession). Driven conversationally: model greets, caller says "I'm
// done", model calls hangup. The s2s model may supply its own reason (which
// wins over the app default), so X-Reason is a substring check against the
// app default; the hard proof is remote-bye + completion_reason==hangup.
//
// Steps:
//  1. preflight-skips
//  2. ensure-caller-wav
//  3. script-llm-hangup
//  4. place-caller (POST /Calls)
//  5. drive-conversation — answer, greet-window, speak "I'm done", wait BYE
//  6. assert-server-hangup
//  7. assert-completion-hangup
func TestVerb_LLM_OpenAI_Hangup_Outbound(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !hangupPreflightLlm(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

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

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "drive-conversation")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
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

// =============================== LLM (inbound) =============================

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

// driveHangupConversation lets the realtime session connect + greet, speaks the
// "I'm done" utterance, then blocks on the server-initiated BYE. Shared by the
// inbound + outbound llm tests. The call must already be answered.
func driveHangupConversation(s *StepCtx, ctx context.Context, call *jsip.Call, callerWAV string) {
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	WaitFor(s.t, "let-model-greet", RecognizerArmDelay)
	if err := call.SendWAV(callerWAV); err != nil {
		s.Fatalf("SendWAV caller utterance: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	waitServerBye(s, ctx, call)
}
