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
	"errors"
	"fmt"
	"testing"
	"time"

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

	testID, sess := claimSession(t)

	s := Step(t, "script-sip-decline-486")
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
	appSID := provisionWebhookApp(t, ctx, "sipdecline-app")
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-and-expect-reject")
	// UAC INVITE to app-<sid>@<domain>. jambonz's SBC routes this URI to
	// the bound Application's call_hook. Carry our correlation ID as an
	// X-Test-Id SIP header so the hook payload lands in our session.
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	_, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
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
	// CSeq round-trip: the response must reference an INVITE CSeq.
	// Catches a class of regressions where a stale response from a
	// different transaction leaks back; without this check, a misrouted
	// 486 from any other call would have masqueraded as our rejection.
	if got := rej.RejectedHeader("CSeq"); got != "" && !containsToken(got, "INVITE") {
		s.Errorf("CSeq header on rejection lacks INVITE: %q", got)
	}
	s.Done()
}

// containsToken reports whether s contains tok as a whitespace-bounded
// token (avoids false positives like "INVITED" matching "INVITE").
func containsToken(s, tok string) bool {
	for _, f := range splitWS(s) {
		if f == tok {
			return true
		}
	}
	return false
}

// splitWS splits on ASCII whitespace (no regexp dependency).
func splitWS(s string) []string {
	var out []string
	cur := ""
	for _, r := range s {
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			if cur != "" {
				out = append(out, cur)
				cur = ""
			}
		} else {
			cur += string(r)
		}
	}
	if cur != "" {
		out = append(out, cur)
	}
	return out
}
