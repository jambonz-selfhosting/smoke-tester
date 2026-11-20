// Repro: an inbound call that jambonz ends while still in early media (never
// answered) must still produce a terminal call_status event.
//
// sip:decline is the verb for rejecting an unanswered call (hangup is a BYE, for
// answered calls). When the verb list runs out instead, _onTasksDone rejects the
// INVITE with 603 and must also send the status.
//
// Phase-2 tests; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const earlyMediaSayText = "This call is still in early media and the app will end it."

// TestEarlyMedia_SIPDecline_FinalStatus — say(earlyMedia) then sip:decline.
func TestEarlyMedia_SIPDecline_FinalStatus(t *testing.T) {
	runEarlyMediaFinalStatus(t, "em-decline", false, webhook.Script{
		V("say", "text", earlyMediaSayText, "earlyMedia", true),
		V("sip:decline", "status", 603),
	})
}

// Steps:
//  1. script-and-provision
//  2. invite-and-await-early-media
//  3. wait-final-response — jambonz rejects the INVITE (non-2xx)
//  4. assert-terminal-status — exactly one terminal status: failed/603
func runEarlyMediaFinalStatus(t *testing.T, suffix string, useWS bool, script webhook.Script) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-and-provision")
	sess.ScriptCallHook(script)
	var appSID string
	if useWS {
		appSID = provisionWSInboundApp(t, ctx, suffix, testID)
	} else {
		appSID = provisionWebhookApp(t, ctx, suffix)
	}
	s.Done()

	s = Step(t, "invite-and-await-early-media")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	pending, err := uas.Stack.InviteEarlyMedia(ctx, dest, inviteOpts(uas, testID))
	if err != nil {
		s.Fatalf("InviteEarlyMedia: %v", err)
	}
	t.Cleanup(func() { _ = pending.Close() })
	s.Done()

	s = Step(t, "wait-final-response")
	code, err := pending.WaitFinal(ctx)
	if err != nil {
		s.Fatalf("WaitFinal: %v", err)
	}
	s.Logf("final response to INVITE: %d", code)
	if code < 300 {
		s.Fatalf("expected jambonz to reject the unanswered INVITE, got %d", code)
	}
	s.Done()

	s = Step(t, "assert-terminal-status")
	var seen, terminal []string
	for _, cb := range DrainCallbacks(sess, 8*time.Second) {
		if cb.Hook != "call_status_hook" {
			continue
		}
		st := cb.NestedString("call_status")
		seen = append(seen, fmt.Sprintf("%s/%d", st, cb.Int("sip_status")))
		switch st {
		case "completed", "failed", "no-answer", "busy":
			terminal = append(terminal, seen[len(seen)-1])
		}
	}
	s.Logf("call_status events: %v", seen)
	if len(terminal) != 1 || terminal[0] != "failed/603" {
		s.Errorf("want exactly one terminal call_status failed/603 after jambonz ended the early-media call; saw %v", seen)
	}
	s.Done()
}

// provisionWSInboundApp creates a WS app whose hook path carries testID: the
// shared WS app correlates by REST tag, which an inbound call lacks.
func provisionWSInboundApp(t *testing.T, ctx context.Context, suffix, testID string) string {
	t.Helper()
	return client.ManagedApplication(t, ctx, provision.ApplicationCreate{
		Name:                     provision.Name(suffix),
		AccountSID:               suite.AccountSID,
		CallHook:                 provision.Webhook{URL: wssURL(webhookTun.URL(), "/appws/"+testID)},
		CallStatusHook:           provision.Webhook{URL: webhookSrv.PublicURL() + "/status", Method: "POST"},
		SpeechSynthesisVendor:    "deepgram",
		SpeechSynthesisLabel:     deepgramLabel,
		SpeechSynthesisVoice:     deepgramVoice,
		SpeechRecognizerVendor:   "deepgram",
		SpeechRecognizerLabel:    deepgramLabel,
		SpeechRecognizerLanguage: "en-US",
	})
}
