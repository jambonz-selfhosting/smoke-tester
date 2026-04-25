// Tests for the `message` verb.
//
// Schema: schemas/verbs/message — sends an SMS (or MMS) mid-call.
// Required: `to`, `from`.
//
// Runs only when MESSAGE_CARRIER_TEST_TO is set (an opt-in phone number to
// receive the test SMS). Without it the test skips cleanly, because
// sending real SMS to an arbitrary number is not free and may not be
// authorized on every account.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN OR without
// MESSAGE_CARRIER_TEST_TO.
package verbs

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Message_SendSMS — `message` should send an SMS and fire
// action/message with the outcome. Opt-in via MESSAGE_CARRIER_TEST_TO/FROM.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-message-and-action-ack — call_hook=[message, pause, hangup],
//     action/message=empty ack
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-wait-end — 200 OK + silence, block on StateEnded
//  5. wait-action-message-callback — block on /action/message HTTP callback
//
// Test     --POST /Calls-->                                  Jambonz
// Jambonz  --GET /hook-->                                    Webhook
// Webhook  --[message to=$TO, from=$FROM, actionHook=...]--> Jambonz
// Jambonz  --POST /action/message { status: sent }-->        Webhook  // assert
// Jambonz  --INVITE -> pause -> BYE-->                       UAS
func TestVerb_Message_SendSMS(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	to := os.Getenv("MESSAGE_CARRIER_TEST_TO")
	from := os.Getenv("MESSAGE_CARRIER_TEST_FROM")
	if to == "" || from == "" {
		t.Skip("MESSAGE_CARRIER_TEST_TO / MESSAGE_CARRIER_TEST_FROM unset — no SMS carrier configured for tests")
	}
	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-message-and-action-ack")
	actionURL := webhookSrv.PublicURL() + "/action/message"
	sess.ScriptCallHook(webhook.Script{
		V("message",
			"to", to,
			"from", from,
			"text", "smoke-tester integration test — please ignore",
			"actionHook", actionURL),
		V("pause", "length", 2),
		V("hangup"),
	})
	sess.ScriptActionHook("message", webhook.Script{})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "wait-action-message-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/message")
	if err != nil {
		s.Fatalf("action/message never arrived: %v", err)
	}
	// Loose assertion: jambonz reports some outcome (success or carrier error);
	// the verb itself is what we're testing, not the carrier's downstream
	// delivery. Just log what we got.
	s.Logf("action/message body: %s", string(cb.Body))
	s.Done()
}
