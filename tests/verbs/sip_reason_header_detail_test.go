//go:build detail

package verbs

import (
	"fmt"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestSIPReasonHeader_InboundCancel — a UAC calls in and CANCELs before the call is
// answered. Needs early media (say + earlyMedia): once a call is answered there is no
// INVITE transaction left to CANCEL, so 183 + SDP is what keeps it cancelable while a
// verb runs.
func TestSIPReasonHeader_InboundCancel(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-say-early-media")
	// earlyMedia keeps the call unanswered: 183 + SDP and TTS instead of a 200 OK
	sess.ScriptCallHook(webhook.Script{
		V("say", "text", "This call is deliberately left unanswered while the smoke test "+
			"cancels it, so please keep talking for a good few seconds without stopping.",
			"earlyMedia", true),
	})
	appSID := provisionWebhookApp(t, ctx, "sipreason-cancel-app")
	s.Done()

	s = Step(t, "invite-and-await-early-media")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	pending, err := uas.Stack.InviteEarlyMedia(ctx, dest, inviteOpts(uas, testID))
	if err != nil {
		s.Fatalf("InviteEarlyMedia: %v", err)
	}
	t.Cleanup(func() { _ = pending.Close() })
	s.Done()

	s = Step(t, "cancel-with-reason-header")
	if err := pending.CancelWithHeaders(ctx, sip.NewHeader("Reason", cancelReasonHeader)); err != nil {
		s.Fatalf("CancelWithHeaders: %v", err)
	}
	s.Done()

	s = Step(t, "assert-sip-reason-header")
	// 487 and "Request Terminated" are jambonz's own on every abandoned call, which is
	// exactly why the header is the only thing that distinguishes them
	assertReasonHeader(s, DrainCallbacks(sess, 5*time.Second), "no-answer", 487, cancelReasonHeader)
	s.Done()
}
