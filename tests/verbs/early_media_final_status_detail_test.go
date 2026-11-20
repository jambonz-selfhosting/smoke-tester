//go:build detail

package verbs

import (
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

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

// TestRepro_MediaServerStall_WS replays the customer's stuck call: the media
// server does not answer endpoint creation for the first verb, and a redirect
// to sip:decline arrives meanwhile. Manual only: freeze the media server on
// the cluster first (kill -STOP), then run with REPRO_MEDIA_STALL=1.
//
// Steps:
//  1. script-and-provision — WS app, play tone_stream earlyMedia (customer's verb)
//  2. invite — INVITE in the background
//  3. redirect-decline — after 1s, redirect to sip:decline 486
//  4. wait-final-response — the INVITE gets 486 (480 for REPRO_REDIRECT=say) and a
//     matching failed status
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
	// the app's own decline must win; a media verb can't run, so jambonz rejects with 480
	next, wantCode := map[string]any{"verb": "sip:decline", "status": 486}, 486
	if os.Getenv("REPRO_REDIRECT") == "say" {
		next, wantCode = map[string]any{"verb": "say", "text": "hello", "earlyMedia": true}, 480
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
	} else if r.code != wantCode {
		s.Errorf("INVITE final response: got %d want %d", r.code, wantCode)
	}
	if want := fmt.Sprintf("failed/%d", wantCode); len(seen) == 0 || seen[len(seen)-1] != want {
		s.Errorf("want call_status to end with %s; saw %v", want, seen)
	}
	s.Done()
}
