//go:build detail

package verbs

import (
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_LLM_Nova_DTMF covers the two text inputs jambonz layers onto Nova:
// keypad entries (llmOptions.dtmf, sent as cross-modal user text since Nova has
// no DTMF) and llm:update {text}. The agent reading the digits back proves each
// reached Nova whole.
func TestVerb_LLM_Nova_DTMF(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasNova() {
		t.Log("NOVA_ACCESS_KEY_ID/NOVA_SECRET_ACCESS_KEY not set — passing without exercising nova DTMF")
		return
	}

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-and-call")
	llmVerb := V("llm",
		"vendor", "nova",
		"model", cfg.NovaModel,
		"auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-nova-event"),
		"events", []string{"textOutput", "contentStart"},
		"llmOptions", map[string]any{
			"systemPrompt": "You are a test assistant. Whenever you are given a number, reply with one short " +
				"sentence that reads the number back digit by digit.",
			"greeting":    "Say hello in three words.",
			"promptStart": novaPromptStart(),
			"dtmf":        map[string]any{"enable": true, "finishOnKey": "#"},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm", "llm-nova-event")
	// WS transport so llm:update can be sent as a session command
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	if reply := novaWaitReply(sess, 20*time.Second, ""); reply == "" {
		s.Fatalf("agent never greeted")
	}
	s.Done()

	s = Step(t, "dtmf-entry")
	elevenlabsSendDTMF(s, call, "1234#")
	if reply := novaWaitReply(sess, 20*time.Second, "1234"); !strings.Contains(digitsSpoken(reply), "1234") {
		s.Errorf("agent reply %q does not read back 1234", reply)
	}
	s.Done()

	s = Step(t, "llm-update-text")
	if err := sess.SendCommand("llm:update", map[string]any{"text": "The number is 2468."}); err != nil {
		s.Fatalf("SendCommand(llm:update): %v", err)
	}
	if reply := novaWaitReply(sess, 20*time.Second, "2468"); !strings.Contains(digitsSpoken(reply), "2468") {
		s.Errorf("agent reply %q does not read back 2468", reply)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
