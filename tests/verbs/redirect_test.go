// Tests for the `redirect` verb.
//
// Schema: schemas/verbs/redirect — tells jambonz to GET a different webhook
// URL for a fresh verb array. Required: `actionHook`.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Redirect_FetchesNewHook — `redirect` pops out of the first
// call_hook and fetches a second one at actionHook.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-redirect-and-action — call_hook=[redirect], action/redirect=[say, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. watch-for-action-redirect — spawn goroutine on WaitCallbackFor("action/redirect")
//  5. answer-and-wait-end — 200 OK + silence, block on StateEnded
//  6. assert-redirect-hook-fired — redirectHits is 1 within 5s
//
// Test     --POST /Calls [app=webhookApp, tag.x_test_id]--> Jambonz
// Jambonz  --GET /hook-->                                   Webhook
// Webhook  --[redirect actionHook=<tunnel>/action/redirect]-> Jambonz
// Jambonz  --POST /action/redirect-->                       Webhook   // 2nd hook
// Webhook  --[say, hangup]-->                               Jambonz
// Jambonz  --INVITE -> BYE-->                               UAS
func TestVerb_Redirect_FetchesNewHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-redirect-and-action")
	redirectURL := webhookSrv.PublicURL() + "/action/redirect"
	// First hook returns a redirect; our server then gets a second POST at
	// /action/redirect which returns the real verb script.
	sess.ScriptCallHook(webhook.Script{
		V("redirect", "actionHook", redirectURL),
	})
	sess.ScriptActionHook("redirect", webhook.Script{
		V("say", "text", "Redirect landed."),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "watch-for-action-redirect")
	// Hit-counter on the redirect action so we can assert it fired.
	var redirectHits int32
	go func() {
		watchCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
		defer wcancel()
		if _, err := sess.WaitCallbackFor(watchCtx, "action/redirect"); err == nil {
			atomic.StoreInt32(&redirectHits, 1)
		}
	}()
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	// Record so the assert step below can prove the redirect's verb
	// chain (say "Redirect landed.") actually executed — not just that
	// jambonz hit the hook URL. A regression that fetches the hook but
	// fails to run the returned verbs would have passed the old test.
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("redirect"), WithSilence())
	s.Done()

	s = Step(t, "assert-redirect-hook-fired")
	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&redirectHits) == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	if atomic.LoadInt32(&redirectHits) == 0 {
		s.Fatalf("action/redirect callback never arrived at %s", redirectURL)
	}
	s.Logf("redirect: action/redirect fired at %s", redirectURL)
	s.Done()

	s = Step(t, "assert-redirect-verb-chain-ran")
	// The redirect's verb chain is `say "Redirect landed."` — STT the
	// recording and assert the words landed in audio. Proves the second
	// hook's verbs were actually executed, not just fetched.
	AssertTranscriptContains(s, ctx, wav, "redirect", "landed")
	s.Done()
}
