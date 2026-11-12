// Gather digit entry keyed at a human pace for longer than `timeout`.
package verbs

import (
	"context"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_SlowKeyedEntry — two gathers on one call, both
// {minDigits 9, maxDigits 9, timeout 5, interDigitTimeout 3}, keyed every 800ms.
// The 5s no-input timer must hand over to the inter-digit timer on the first key.
//
// Steps:
//  1. script-two-gathers — [gather full, gather short, hangup] + empty acks
//  2. place-call — POST /Calls with application_sid=webhookApp
//  3. answer-and-silence — 200 OK + SendSilence
//  4. key-full-entry — 9 digits, 1500ms after answer (gather detector arms)
//  5. assert-full-entry — reason=dtmfDetected digits=123456789
//  6. key-short-entry — 5 digits then stop
//  7. assert-short-entry — reason=timeout digits=12345, after interDigitTimeout
func TestVerb_Gather_SlowKeyedEntry(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	const interDigit = 3 * time.Second
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	gather := func(hook string) map[string]any {
		return V("gather",
			"input", []any{"digits"},
			"minDigits", 9,
			"maxDigits", 9,
			"interDigitTimeout", int(interDigit/time.Second),
			"timeout", 5,
			"actionHook", SessionURL(sess, hook))
	}
	s := Step(t, "script-two-gathers")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{gather("gather-full"), gather("gather-short"), V("hangup")}))
	SessionAckEmpty(sess, "gather-full", "gather-short")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	time.Sleep(1500 * time.Millisecond)
	s = Step(t, "key-full-entry")
	keySlowly(s, call, "123456789")
	s.Done()

	s = Step(t, "assert-full-entry")
	cb := waitGatherCallback(s, ctx, sess, "gather-full")
	assertGather(s, cb, "dtmfDetected", "123456789")
	s.Done()

	// the second gather starts as soon as the first action hook is acked
	time.Sleep(500 * time.Millisecond)
	s = Step(t, "key-short-entry")
	lastKey := keySlowly(s, call, "12345")
	s.Done()

	s = Step(t, "assert-short-entry")
	cb = waitGatherCallback(s, ctx, sess, "gather-short")
	assertGather(s, cb, "timeout", "12345")
	// the old 5s timer ends it about 1.3s after the last key; the inter-digit timer needs 3s+
	if elapsed := cb.Received.Sub(lastKey); elapsed < interDigit {
		s.Errorf("resolved %v after the last key, want at least %v (interDigitTimeout)", elapsed, interDigit)
	}
	s.Done()

	_ = call.Hangup()
}

// keySlowly sends one RFC 2833 digit every 800ms, keeping outbound RTP flowing,
// and returns when the last key went out.
func keySlowly(s *StepCtx, call *jsip.Call, keys string) time.Time {
	var last time.Time
	for i, d := range keys {
		if i > 0 {
			time.Sleep(time.Until(last.Add(800 * time.Millisecond)))
		}
		last = time.Now()
		// the gather may already have ended the call; the callback says why
		if err := call.SendDTMF(string(d)); err != nil {
			s.Logf("SendDTMF(%q) after %d digits: %v", d, i, err)
			break
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
	}
	return last
}

func waitGatherCallback(s *StepCtx, ctx context.Context, sess *webhook.Session, hook string) webhook.Callback {
	waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/"+hook)
	if err != nil {
		s.Fatalf("WaitCallbackFor action/%s: %v", hook, err)
	}
	s.Logf("action/%s: %s", hook, cb.Body)
	return cb
}

func assertGather(s *StepCtx, cb webhook.Callback, reason, digits string) {
	if got := cb.String("reason"); got != reason {
		s.Errorf("reason: got %q want %q", got, reason)
	}
	if got := cb.String("digits"); got != digits {
		s.Errorf("digits: got %q want %q (keyed digits lost)", got, digits)
	}
}
