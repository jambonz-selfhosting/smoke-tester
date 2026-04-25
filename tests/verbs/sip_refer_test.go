// Tests for the `sip:refer` verb.
//
// Schema: schemas/verbs/sip-refer — jambonz sends a REFER to the far end
// (our UAS) with a Refer-To URI. Required: `referTo`.
//
// We assert the REFER arrives in call.Received() and carries the expected
// Refer-To. We don't complete the transfer (would need a third-party UAS);
// a 202 Accepted is enough to prove jambonz emitted the REFER correctly.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_SIPRefer_EmitsRefer — `sip:refer` should emit a REFER with
// our target URI in the Refer-To header.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-sip-refer — [sip:refer referTo=sip:transfer@..., pause, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-wait-end — 200 OK + silence, block on StateEnded
//  5. assert-refer-header — received REFER carries Refer-To with target URI
//
// Test     --POST /Calls-->                            Jambonz
// Jambonz  --GET /hook-->                              Webhook
// Webhook  --[sip:refer referTo=sip:transfer@..., hangup]--> Jambonz
// Jambonz  --INVITE-->                                 UAS   (answer)
// Jambonz  --REFER (Refer-To: sip:transfer@...)-->     UAS   // assert
// UAS      --202/200 OK-->                             Jambonz
// Jambonz  --BYE-->                                    UAS
func TestVerb_SIPRefer_EmitsRefer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-sip-refer")
	const transferTarget = "sip:transfer-target@example.invalid"
	sess.ScriptCallHook(webhook.Script{
		V("sip:refer", "referTo", transferTarget),
		V("pause", "length", 3),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "assert-refer-header")
	refers := call.ReceivedByMethod("REFER")
	if len(refers) == 0 {
		s.Fatalf("no REFER received; methods=%v", MethodsOf(call.Received()))
	}
	referTo := refers[0].Headers["Refer-To"]
	if !strings.Contains(referTo, transferTarget) {
		s.Errorf("Refer-To: got %q want substring %q", referTo, transferTarget)
	}
	s.Logf("sip:refer REFER received: Refer-To=%q", referTo)
	s.Done()
}
