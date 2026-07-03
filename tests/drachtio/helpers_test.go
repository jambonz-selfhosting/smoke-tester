//go:build drachtio

package drachtio

import (
	"context"
	"errors"
	"fmt"
	"testing"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// claimUAS provisions a one-shot SIP user, brings up a registered stack
// against the suite's realm, and returns the resulting *jsip.Stack. Cleanup
// (deregister + stop the stack) runs on test exit via t.Cleanup.
//
// This is a minimal port of tests/verbs/helpers_test.go's claimUAS: the
// drachtio suite only needs a registered UAC to originate INVITEs against
// the harness's inline-app_json Application, so it drops the Inbound-call
// channel plumbing that verb tests need for jambonz-initiated legs.
func claimUAS(t *testing.T, ctx context.Context) *jsip.Stack {
	t.Helper()
	_, username, password := client.ManagedSIPClient(t, ctx)

	stk, err := jsip.Start(context.Background(), jsip.Config{
		SIPDomain: suite.SIPRealm,
		User:      username,
		Pass:      password,
		Transport: "tcp",
		LogLevel:  cfg.LogLevel,
		Resolver:  sipResolver.Resolver(),
		// Owner ties calls on this per-test stack to t for per-leg
		// recording archives (RECORD_LEGS, ADR-0016).
		Owner: t.Name(),
	}, func(_ context.Context, call *jsip.Call) error {
		// This suite never expects an inbound INVITE from jambonz — reject
		// anything that lands here rather than leak the goroutine.
		_ = call.Reject(486, "Busy Here")
		return nil
	})
	if err != nil {
		t.Fatalf("claimUAS: SIP stack start (user=%s): %v", username, err)
	}
	t.Cleanup(func() {
		// Stop the stack before the test's bounded ctx runs out so diago
		// has time to send DEREGISTER before we close the UA.
		stk.Stop()
	})
	return stk
}

// inviteApp places an outbound INVITE from uas to the suite's inline-
// app_json Application (sip:app-<appSID>@<suite.SIPRealm>) over TCP, with
// the given custom headers (e.g. Session-Expires / Min-SE / Supported).
//
// If jambonz/the SBC rejects the INVITE with a 422 (Session Interval Too
// Small), this fails loudly with the rejected Min-SE header value and a
// hint to raise Session-Expires above the SBC's Min-SE — that rejection
// otherwise surfaces as an opaque "invite rejected: 422" that doesn't tell
// the operator what to fix.
func inviteApp(t *testing.T, ctx context.Context, uas *jsip.Stack, headers jsip.H) (*jsip.Call, error) {
	t.Helper()
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		Headers:   headers,
	})
	if err != nil {
		var rejected *jsip.InviteRejected
		if errors.As(err, &rejected) && rejected.StatusCode == 422 {
			t.Fatalf("inviteApp: rejected 422 (Session Interval Too Small): Min-SE=%s"+
				" — raise Session-Expires above the SBC Min-SE",
				rejected.RejectedHeader("Min-SE"))
		}
		return nil, err
	}
	return call, nil
}
