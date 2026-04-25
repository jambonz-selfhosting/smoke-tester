// Tests for the `sip:decline` verb.
//
// Schema: schemas/verbs/sip-decline — required `status`. Jambonz responds
// to the INVITE with a 4xx/5xx/6xx status, optional reason phrase, and
// optional custom headers. The verb is only meaningful on the
// jambonz-is-callee leg — which means the test needs to originate the
// call *into* jambonz over SIP, not via `POST /Calls`.
//
// Flow:
//
//  1. Provision a webhook Application whose `call_hook` returns
//     [{sip:decline status=<code>, reason=<text>, headers=...}]. Jambonz
//     auto-creates a dialable SIP URI: app-<application-sid>@<domain>.
//  2. UAC INVITEs that URI with our X-Test-Id header so the hook lands
//     in the per-test session.
//  3. Jambonz fetches the hook, runs sip:decline, sends us the
//     negative final response. diago returns ErrDialogResponse; our
//     Stack.Invite converts it into a typed InviteRejected.
//  4. Assert the status, reason and custom headers we asked for came
//     back verbatim.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_SIPDecline_Basic — jambonz rejects an inbound INVITE via
// `sip:decline`. Requires UAC origination (we place the INVITE into
// jambonz, not the other way round).
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-sip-decline-486 — call_hook returns [sip:decline 486 Busy Here + X-Custom-A]
//  3. provision-application — CreateApplication pointing at our webhook tunnel
//  4. invite-and-expect-reject — UAC INVITE sip:app-<sid>@<domain>; err expected
//  5. assert-rejection-details — status=486, reason="Busy Here", X-Custom-A header
//
// Test     --CreateApplication call_hook=<tunnel>/hook-->        api-server
// Test     --INVITE sip:app-<sid>@sip.jambonz.me (X-Test-Id)-->  SBC
// SBC      --hook fetch-->                                       Webhook server
// Webhook  --[{sip:decline, status=486, reason="Busy Here", ...}]--> Jambonz
// Jambonz  --486 Busy Here (X-Custom-A:foo)-->                   UAC
//                                                                 // assert status,
//                                                                 // reason, header
func TestVerb_SIPDecline_Basic(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-sip-decline-486")
	// Script jambonz will execute when someone INVITEs the provisioned
	// Application: respond with 486 Busy Here + a custom header.
	sess.ScriptCallHook(webhook.Script{
		V("sip:decline",
			"status", 486,
			"reason", "Busy Here",
			"headers", jsip.H{
				"X-Custom-A": "decline-verb-works",
			}),
	})
	s.Done()

	s = Step(t, "provision-application")
	// Provision a dedicated Application for this test. Both call_hook and
	// call_status_hook point at the same webhook server; the hook URL has
	// no correlation — we rely on the SIP X-Test-Id header the UAC sets
	// on its INVITE.
	appCtx, appCancel := context.WithTimeout(ctx, 15*time.Second)
	defer appCancel()
	appSID, err := client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:       provision.Name("sipdecline-app"),
		AccountSID: cfg.AccountSID,
		CallHook: provision.Webhook{
			URL:    webhookSrv.PublicURL() + "/hook",
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    webhookSrv.PublicURL() + "/status",
			Method: "POST",
		},
		// Speech creds are harmless here; sip:decline doesn't use them but
		// jambonz validates these fields exist on the Application.
		SpeechSynthesisVendor:    "deepgram",
		SpeechSynthesisLabel:     deepgramLabel,
		SpeechSynthesisVoice:     deepgramVoice,
		SpeechRecognizerVendor:   "deepgram",
		SpeechRecognizerLabel:    deepgramLabel,
		SpeechRecognizerLanguage: "en-US",
	})
	if err != nil {
		s.Fatalf("create Application: %v", err)
	}
	t.Cleanup(func() {
		dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer dcancel()
		_ = client.DeleteApplication(dctx, appSID)
	})
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-and-expect-reject")
	// UAC INVITE to app-<sid>@<domain>. jambonz's SBC routes this URI to
	// the bound Application's call_hook. Carry our correlation ID as an
	// X-Test-Id SIP header so the hook payload lands in our session.
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, cfg.SIPDomain)
	_, err = uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers: jsip.H{
			webhook.CorrelationHeader: testID,
		},
	})
	if err == nil {
		s.Fatal("Invite succeeded, expected sip:decline to reject")
	}
	s.Done()

	s = Step(t, "assert-rejection-details")
	// Expect InviteRejected with the status/reason/header we asked for.
	var rej *jsip.InviteRejected
	if !errors.As(err, &rej) {
		s.Fatalf("Invite returned non-rejection error: %v", err)
	}
	s.Logf("rejected: %d %q", rej.StatusCode, rej.Reason)
	if rej.StatusCode != 486 {
		s.Errorf("status: got %d want 486", rej.StatusCode)
	}
	if rej.Reason != "Busy Here" {
		s.Errorf("reason: got %q want %q", rej.Reason, "Busy Here")
	}
	if got := rej.RejectedHeader("X-Custom-A"); got != "decline-verb-works" {
		s.Errorf("X-Custom-A: got %q want %q", got, "decline-verb-works")
	}
	s.Done()
}
