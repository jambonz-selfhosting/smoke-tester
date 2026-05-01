// Tests for the `answer` verb.
//
// Schema: schemas/verbs/answer — no required fields. Most verbs answer the
// call implicitly; `answer` forces an explicit 200 OK before any downstream
// verb runs. Only meaningful on the leg where jambonz is the *callee* —
// so the test must originate the INVITE *into* jambonz, not via POST /Calls.
//
// Flow (mirror of sip:decline_test.go):
//
//   1. Provision a webhook Application whose call_hook returns
//      [answer, pause, hangup]. Jambonz auto-creates a dialable URI
//      `sip:app-<application_sid>@<domain>`.
//   2. UAC INVITEs that URI with our X-Test-Id header.
//   3. Jambonz fetches the hook, runs `answer` → returns 200 OK explicitly,
//      then `pause` for 1s, then `hangup` (BYE to us).
//   4. Assert the 200 came back with our chosen status, and the call ran
//      to completion.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Answer_Basic — UAC INVITEs an Application that runs
// [answer, pause, hangup]. Asserts the explicit 200 OK from `answer`
// and a remote-bye end after the pause.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-answer-pause-hangup — call_hook returns [answer, pause 1s, hangup]
//  3. provision-application — CreateApplication pointing at our webhook tunnel
//  4. invite-and-expect-200 — UAC INVITE sip:app-<sid>@<domain>; success expected
//  5. assert-answered-200 — call.AnsweredStatus() == 200
//  6. wait-for-bye — block on StateEnded (jambonz hangs up after pause)
//  7. assert-end-and-sip-methods — end reason remote-bye, BYE in Received()
//
// Test     --CreateApplication call_hook=<tunnel>/hook-->        api-server
// Test     --INVITE sip:app-<sid>@sip.jambonz.me (X-Test-Id)-->  SBC
// SBC      --hook fetch-->                                       Webhook
// Webhook  --[answer, pause 1s, hangup]-->                       Jambonz
// Jambonz  --200 OK-->                                           UAC   // assert
// Jambonz  --BYE (after 1s pause + hangup)-->                    UAC   // assert
func TestVerb_Answer_Basic(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause-hangup")
	sess.ScriptCallHook(webhook.Script{
		V("answer"),
		V("pause", "length", 1),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "provision-application")
	appSID := provisionWebhookApp(t, ctx, "answer-app")
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-and-expect-200")
	// UAC INVITE to app-<sid>@<domain>. jambonz's SBC routes this URI to
	// the bound Application's call_hook. Carry the correlation ID as an
	// X-Test-Id SIP header so the hook payload lands in our session.
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers: jsip.H{
			webhook.CorrelationHeader: testID,
		},
	})
	if err != nil {
		s.Fatalf("Invite: %v", err)
	}
	s.Done()

	s = Step(t, "assert-answered-200")
	if got := call.AnsweredStatus(); got != 200 {
		s.Errorf("answered status: got %d want 200", got)
	}
	// Check the wire: the only final status jambonz sent must be 200.
	// A regression that emitted an error before answering (e.g. 5xx
	// then a recovery 200) would still pass AnsweredStatus() == 200 but
	// would fail this. Allow provisional 100/180/183 — they're normal.
	for _, code := range StatusesOf(call.Sent()) {
		if code >= 200 && code != 200 {
			s.Errorf("jambonz sent unexpected non-200 final status: %d", code)
		}
	}
	s.Done()

	s = Step(t, "wait-for-bye")
	if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
		s.Fatalf("wait end: %v", err)
	}
	s.Done()

	s = Step(t, "assert-end-and-sip-methods")
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("end reason: got %q want %q", reason, "remote-bye")
	}
	RequireRecvMethods(s, call, "BYE")
	s.Done()
}
