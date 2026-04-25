// Tests for the `tag` verb.
//
// Schema: schemas/verbs/tag — attaches `data` to the call. All subsequent
// webhooks include it as `customerData`. Required: `data` (object).
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Tag_DataInCallbacks — `tag` verb attaches data that should
// appear as customerData on subsequent callbacks.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-tag-say-hangup — register [tag, say, hangup] as call_hook
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-and-wait-end — 200 OK + silence, block on StateEnded
//  5. drain-callbacks-both-sessions — drain per-test + anon (tag verb
//     replaces customerData and drops x_test_id → later hooks land in anon)
//  6. assert-customer-data-foo-bar — some callback carries customerData.foo=bar
//
// Test     --POST /Calls [app=webhookApp, tag.x_test_id]--> Jambonz
// Jambonz  --GET /hook-->                                   Webhook
// Webhook  --[tag {foo:"bar"}, say, hangup]-->              Jambonz
// Jambonz  --POST /status (customerData.foo="bar")-->       Webhook   // assert
// Jambonz  --INVITE-->                                      UAS
// Jambonz  --BYE-->                                         UAS
func TestVerb_Tag_DataInCallbacks(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-tag-say-hangup")
	// The test infra already sets customerData.x_test_id (via the POST /Calls
	// `tag` field). The `tag` verb merges its `data` into the same bucket, so
	// subsequent webhook payloads should show both keys.
	sess.ScriptCallHook(webhook.Script{
		V("tag", "data", map[string]any{"foo": "bar", "n": 7}),
		V("say", "text", "tagged call."),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "drain-callbacks-both-sessions")
	// Wait for the post-BYE burst of status hooks. 2s is enough in practice —
	// jambonz flushes "completed" within ~500ms of BYE. DrainCallbacks is a
	// bounded-wait loop that returns as soon as the channel goes idle, so the
	// budget is an upper bound, not a fixed cost.
	cbs := DrainCallbacks(sess, 2*time.Second)
	// The `tag` verb REPLACES customerData with the new object, dropping
	// our x_test_id — any subsequent hooks therefore miss correlation and
	// land in the anon session rather than this one. Read both.
	anon, _ := webhookReg.Lookup("_anon")
	if anon != nil {
		cbs = append(cbs, DrainCallbacks(anon, 500*time.Millisecond)...)
	}
	s.Done()

	s = Step(t, "assert-customer-data-foo-bar")
	var found bool
	for _, cb := range cbs {
		cd, ok := cb.JSON["customerData"].(map[string]any)
		if !ok {
			continue
		}
		if cd["foo"] == "bar" {
			found = true
			if v, _ := cd["n"].(float64); int(v) != 7 {
				s.Errorf("customerData.n: got %v want 7", cd["n"])
			}
			break
		}
	}
	if !found {
		s.Errorf("no callback carried customerData with foo=bar; saw %d callbacks", len(cbs))
		for _, cb := range cbs {
			s.Logf("  hook=%s customerData=%v", cb.Hook, cb.JSON["customerData"])
		}
	}
	s.Done()
}
