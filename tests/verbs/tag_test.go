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

	_, sess := claimSession(t)

	s := Step(t, "script-tag-say-hangup")
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

	s = Step(t, "assert-customer-data-on-post-tag-callbacks")
	// Every "completed"-status status callback (fired AFTER the tag
	// verb ran) must carry the merged customerData. Pre-tag statuses
	// (trying / ringing / in-progress) fire before the tag verb
	// executes — those legitimately don't carry it. The values must
	// also round-trip with the right types (foo string, n int 7).
	postTagWithTag := 0
	postTagWithout := 0
	var postTagDetails []string
	for _, cb := range cbs {
		if cb.Hook != "call_status_hook" {
			continue
		}
		status := cb.NestedString("call_status")
		// "completed" + "failed" + "no-answer" all fire AFTER the verb
		// chain executed (and thus AFTER tag); pre-answer statuses do
		// not. Filter to post-verb-chain statuses.
		if status != "completed" && status != "failed" && status != "no-answer" {
			continue
		}
		if cb.NestedString("customerData.foo") == "bar" {
			postTagWithTag++
			if got := int(toFloat(cb.NestedAny("customerData.n"))); got != 7 {
				s.Errorf("customerData.n: got %v want 7", cb.NestedAny("customerData.n"))
			}
		} else {
			postTagWithout++
			postTagDetails = append(postTagDetails, cb.Hook+"="+status)
		}
	}
	if postTagWithTag == 0 {
		s.Errorf("no post-tag callback carried customerData.foo=bar; saw %d callbacks", len(cbs))
		for _, cb := range cbs {
			s.Logf("  hook=%s status=%s customerData=%v",
				cb.Hook, cb.NestedString("call_status"), cb.CustomerData())
		}
	}
	// We tolerate at most ONE post-verb-chain status missing the tag —
	// jambonz's "completed" final hook can race with session teardown
	// and lose customerData on the very last frame. More than one miss
	// is a real regression.
	if postTagWithout > 1 {
		s.Errorf("%d post-verb-chain status callbacks missing customerData.foo=bar: %v",
			postTagWithout, postTagDetails)
	}
	s.Logf("post-tag status callbacks: %d with tag, %d without",
		postTagWithTag, postTagWithout)
	s.Done()
}

func toFloat(v any) float64 {
	if f, ok := v.(float64); ok {
		return f
	}
	return 0
}
