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
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
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

// TestEarlyMedia_TasksDone_FinalStatus — say(earlyMedia) and nothing else.
func TestEarlyMedia_TasksDone_FinalStatus(t *testing.T) {
	runEarlyMediaFinalStatus(t, "em-tasksdone", false, webhook.Script{
		V("say", "text", earlyMediaSayText, "earlyMedia", true),
	})
}

// TestEarlyMedia_SIPDecline_WS_FinalStatus — same as the sip:decline case over a
// WebSocket app, where call_status rides the app socket (the customer's setup).
func TestEarlyMedia_SIPDecline_WS_FinalStatus(t *testing.T) {
	runEarlyMediaFinalStatus(t, "em-decline-ws", true, webhook.Script{
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

// TestRepro_MediaServerStall_WS replays the customer's stuck call: the media
// server does not answer endpoint creation for the first verb, and a redirect
// to sip:decline arrives meanwhile. Manual only: freeze the media server on
// the cluster first (kill -STOP), then run with REPRO_MEDIA_STALL=1.
//
// Steps:
//  1. script-and-provision — WS app, play tone_stream earlyMedia (customer's verb)
//  2. invite — INVITE in the background
//  3. redirect-decline — after 1s, redirect to sip:decline 603
//  4. wait-final-response — log how long until the INVITE gets a final response
func TestRepro_MediaServerStall_WS(t *testing.T) {
	if os.Getenv("REPRO_MEDIA_STALL") == "" {
		t.Skip("manual repro: freeze the media server, then set REPRO_MEDIA_STALL=1")
	}
	requireWebhook(t)

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-and-provision")
	sess.ScriptCallHook(webhook.Script{
		V("play", "url", "tone_stream://%(2000,4000,440,480);loops=3",
			"loop", 10, "timeoutSecs", 180, "earlyMedia", true),
	})
	appSID := provisionWSInboundApp(t, ctx, "stall-ws", testID)
	s.Done()

	s = Step(t, "invite")
	start := time.Now()
	type result struct {
		code int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		call, err := inviteApp(ctx, uas, appSID, testID)
		var rej *jsip.InviteRejected
		switch {
		case err == nil:
			_ = call.Hangup()
			done <- result{code: 200}
		case errors.As(err, &rej):
			done <- result{code: rej.StatusCode}
		default:
			done <- result{err: err}
		}
	}()
	s.Done()

	s = Step(t, "redirect-decline")
	time.Sleep(time.Second)
	// REPRO_REDIRECT=say stands in for the customer's llm: a verb that needs media again
	next := map[string]any{"verb": "sip:decline", "status": 603}
	if os.Getenv("REPRO_REDIRECT") == "say" {
		next = map[string]any{"verb": "say", "text": "hello", "earlyMedia": true}
	}
	if err := sess.SendCommand("redirect", []any{next}); err != nil {
		s.Errorf("SendCommand redirect: %v", err)
	}
	s.Done()

	s = Step(t, "wait-final-response")
	r := <-done
	s.Logf("INVITE final=%d err=%v after %v", r.code, r.err, time.Since(start).Round(time.Second))
	var seen []string
	for _, cb := range DrainCallbacks(sess, 3*time.Second) {
		if cb.Hook == "call_status_hook" {
			seen = append(seen, fmt.Sprintf("%s/%d", cb.NestedString("call_status"), cb.Int("sip_status")))
		}
	}
	s.Logf("call_status events: %v", seen)
	if r.err != nil {
		s.Errorf("no final response to the INVITE: %v", r.err)
	}
	s.Done()
}
