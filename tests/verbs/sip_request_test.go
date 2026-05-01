// Tests for the `sip:request` verb.
//
// Schema: schemas/verbs/sip-request — jambonz sends an in-dialog SIP
// request (INFO/NOTIFY/MESSAGE/…) to the far end.
// Required: `method`. Optional: `body`, `headers`, `actionHook`.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_SIPRequest_INFO — `sip:request INFO` should deliver an INFO
// with our custom header on the wire to our UAS.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-sip-request-info — [sip:request INFO + X-Test:hi, pause, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-wait-end — 200 OK + silence, block on StateEnded
//  5. assert-info-header — received INFO carries X-Test=hi
//
// Test     --POST /Calls-->                                  Jambonz
// Jambonz  --GET /hook-->                                    Webhook
// Webhook  --[sip:request INFO + X-Test: hi, pause, hangup]--> Jambonz
// Jambonz  --INVITE-->                                       UAS   (answer)
// Jambonz  --INFO (X-Test: hi)-->                            UAS   // assert
// UAS      --200 OK (to INFO)-->                             Jambonz
// Jambonz  --BYE-->                                          UAS
func TestVerb_SIPRequest_INFO(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-sip-request-info")
	sess.ScriptCallHook(webhook.Script{
		V("sip:request",
			"method", "INFO",
			"headers", map[string]any{"X-Test": "hi", "Content-Type": "application/x-test"},
			"body", "ping"),
		V("pause", "length", 2),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "assert-info-header")
	infos := call.ReceivedByMethod("INFO")
	if len(infos) == 0 {
		s.Fatalf("no INFO received; methods=%v", MethodsOf(call.Received()))
	}
	info := infos[0]
	if got := info.Headers["X-Test"]; got != "hi" {
		s.Errorf("INFO X-Test: got %q want %q", got, "hi")
	}
	// Body round-trip: jambonz must forward our literal "ping" payload.
	// A regression that drops the body or truncates it (e.g. forgets
	// Content-Length on the outgoing INFO) would have passed the old
	// test which only logged body-len.
	// jambonz appends a trailing CRLF/newline to forwarded SIP bodies;
	// trim before comparing so we catch real corruption (drop / mangle)
	// without flapping on a normal terminator.
	if got := strings.TrimSpace(string(info.RawRequest.Body())); got != "ping" {
		s.Errorf("INFO body: got %q want %q", got, "ping")
	}
	// Content-Type round-trip: we set application/x-test; if jambonz
	// silently rewrites Content-Type (or drops it), downstream parsers
	// would mis-handle the body. SIP header lookup is case-insensitive
	// in normal stacks but we stored both keys; check the headers map
	// for either case.
	gotCT := info.Headers["Content-Type"]
	if gotCT == "" {
		gotCT = info.Headers["content-type"]
	}
	if gotCT != "application/x-test" {
		s.Errorf("INFO Content-Type: got %q want %q", gotCT, "application/x-test")
	}
	s.Logf("sip:request INFO received: headers=%v body=%q", info.Headers, string(info.RawRequest.Body()))
	s.Done()
}
