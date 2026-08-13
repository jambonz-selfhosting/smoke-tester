//go:build drachtio

package drachtio

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// TestDrachtio_TportReconnect_ReferAfterPeerReconnects reproduces the
// 2026-08-05 production incident in which 64 SIP REFERs failed with
// 408 Request Timeout and every in-transit transfer died.
//
// # What happened in production
//
// A carrier SBC (10.222.30.173) held one long-lived TLS connection to jambonz
// sbc-inbound (10.185.0.9) carrying every call on that trunk — 55 distinct
// Call-IDs on the connection in a five-minute window. At 16:54:57 that
// connection went silent with no FIN and no RST, and ten seconds later the
// carrier reconnected from a new ephemeral source port. Inbound traffic was
// completely unaffected: SIP matches dialogs by Call-ID and tags, never by
// connection, so BYEs for calls set up on the old connection kept arriving on
// the new one and were answered normally (30 Call-IDs appear on both).
//
// Outbound was not. drachtio pins each dialog to the tport the INVITE arrived
// on (SipDialog::m_tp) and forces every in-dialog request onto it via
// NTATAG_TPORT. Nothing revisits that pin: a blackholed socket is
// indistinguishable from a healthy idle one, so tport_is_closed() stays false
// and checkTportState() never releases it. When the feature-server sent a
// REFER over UDP at 16:55:10, drachtio wrote it to the dead connection, no 202
// ever came back, and Timer F produced a 408 thirty-two seconds later.
//
// # What this test stages
//
// The same shape over TCP, which exercises the identical pin-and-force path
// (the transport-specific part of the production bug is only that TLS has no
// keepalive at all in sofia — tport_type_tls.c leaves both secondary-timer
// vtable slots NULL — so nothing was even trying to notice; TCP's CRLF
// pingpong would not have fired inside the 32s window either).
//
//	UAC ──(stable)──▶ tcpRelay ──(swappable)──▶ jambonz SBC
//
//  1. Provision an app that answers, waits, then emits `sip:refer`.
//  2. INVITE through the relay; jambonz answers. The dialog is now pinned to
//     the relay's first upstream connection, and drachtio has recorded "the
//     peer advertising this Contact is on that connection".
//  3. Mid-call, relay.Reconnect(): the first upstream is left open and
//     unread-from (no FIN, no RST) and a second is dialled. Same source IP,
//     new source port — the incident, staged.
//  4. Send an in-dialog INFO. It carries the same Contact as the INVITE (diago
//     adds one below internal/sip's doInDialog), so it re-points that Contact
//     at the new connection. This is the only way drachtio can learn the new
//     connection exists; in production the equivalent was the carrier's next
//     INVITE or BYE, arriving on some other call.
//  5. Wait for the REFER.
//
// Unfixed drachtio sends the REFER on the abandoned connection and the UAC
// never sees it. With DRACHTIO_TPORT_CONTACT_ALIAS=1 the send path looks up
// the address the REFER is aimed at — the dialog's remote target, i.e. that
// same Contact — finds the connection the peer is now on, and delivers it.
//
// Note this asserts on delivery, not on the 408: the 408 is generated toward
// the feature-server, which the harness cannot observe. "REFER never arrived"
// is the same defect seen from the only vantage point a UAC has.
func TestDrachtio_TportReconnect_ReferAfterPeerReconnects(t *testing.T) {
	runReferThroughRelay(t, true)
}

// TestDrachtio_TportReconnect_ReferControlNoReconnect is the control for the
// test above: identical in every respect except that the connection is never
// abandoned. It exists so a failure of the reconnect case can only be read one
// way.
//
// Without it, "no REFER arrived" is ambiguous — `sip:refer` might simply not
// survive being relayed, or the app might never have emitted it, and the
// reconnect would be getting the blame for something it did not cause. This
// test holds every other variable fixed (same relay, same app JSON, same
// timings, same assertions) and changes only whether Reconnect() is called.
//
// Read the pair together: control green + reconnect red isolates the defect to
// the abandoned connection. Both red means something upstream of the bug is
// broken and the reconnect result proves nothing.
func TestDrachtio_TportReconnect_ReferControlNoReconnect(t *testing.T) {
	runReferThroughRelay(t, false)
}

// runReferThroughRelay drives the shared scenario. withReconnect selects
// between the reproduction and its control.
func runReferThroughRelay(t *testing.T, withReconnect bool) {
	t.Helper()
	// Not parallel: the relay must own 127.0.0.1:5060, and the test reasons
	// about which connections exist to a single host.

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	const (
		pauseBeforeRefer = 20 * time.Second // app waits this long, then REFERs
		reconnectAt      = 5 * time.Second  // ...we kill the connection here
		referDeadline    = 45 * time.Second // 20s pause + 32s Timer F, with room
		referTarget      = "sip:transfer-target@example.invalid"
	)

	// 1. An app that answers, holds the call, then transfers. The pause has to
	// outlast the reconnect so the REFER is issued against an already-stale pin.
	appJSON := fmt.Sprintf(
		`[{"verb":"answer"},{"verb":"pause","length":%d},{"verb":"sip:refer","referTo":%q},{"verb":"pause","length":30},{"verb":"hangup"}]`,
		int(pauseBeforeRefer.Seconds()), referTarget)

	appCtx, appCancel := context.WithTimeout(ctx, 30*time.Second)
	referAppSID, err := client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("drachtio-refer-app"),
		AccountSID:     suite.AccountSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
		AppJSON:        appJSON,
	})
	appCancel()
	if err != nil {
		t.Fatalf("create sip:refer application: %v", err)
	}
	t.Cleanup(func() {
		delCtx, delCancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer delCancel()
		if err := client.DeleteApplication(delCtx, referAppSID); err != nil {
			t.Logf("cleanup: delete application %s: %v", referAppSID, err)
		}
	})

	// 2. Relay in the path, and a resolver that sends the realm to it rather
	// than straight to the SBC.
	relay := newTCPRelay(t, relayListenAddr, sbcAddr())

	relayResolver, err := jsip.NewStaticResolver(map[string]string{
		suite.SIPRealm: "127.0.0.1",
	})
	if err != nil {
		t.Fatalf("relay resolver: %v", err)
	}
	t.Cleanup(func() { _ = relayResolver.Close() })

	uas := claimUASVia(t, ctx, relayResolver)

	// 3. Place the call. Every byte crosses the relay, so the tport drachtio
	// pins to this dialog is the relay's first upstream connection.
	dest := fmt.Sprintf("sip:app-%s@%s", referAppSID, suite.SIPRealm)
	call, err := uas.Invite(ctx, dest, jsip.InviteOptions{Transport: "tcp"})
	if err != nil {
		t.Fatalf("INVITE %s through relay: %v", dest, err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}
	answeredAt := time.Now()
	t.Logf("call answered (call-id %s); dialog is now pinned to the relay's first upstream connection",
		call.CallID())

	// 4. Kill the connection the dialog is pinned to and reconnect. The control
	// skips exactly this and nothing else.
	select {
	case <-time.After(reconnectAt):
	case <-ctx.Done():
		t.Fatalf("context expired before the reconnect point")
	}
	if withReconnect {
		if n := relay.Reconnect(); n == 0 {
			t.Fatalf("relay.Reconnect swapped no connections — nothing was staged, the rest " +
				"of this test would prove nothing")
		}
	} else {
		t.Logf("control run: leaving the connection alone")
	}

	// 5. Put a SIP message on the connection. In the reconnect case this is how
	// drachtio learns the new connection exists at all — a TCP connection
	// carrying no SIP is invisible to the SIP layer, which is the whole reason
	// the old one stays pinned. The control sends it too, so the two runs differ
	// only in step 4.
	infoCtx, infoCancel := context.WithTimeout(ctx, 10*time.Second)
	res, err := call.SendInfo(infoCtx, "application/x-test", []byte("probe"))
	infoCancel()
	switch {
	case err != nil:
		// The request still reached drachtio even if the far end disliked it;
		// that is all this step needs.
		t.Logf("in-dialog INFO returned an error (%v) — harmless, the request still "+
			"crossed the connection", err)
	case res != nil:
		t.Logf("in-dialog INFO answered %d %s — drachtio has now seen traffic on this connection",
			res.StatusCode, res.Reason)
	}

	// 6. Wait for the transfer.
	waitFor := referDeadline - time.Since(answeredAt)
	if waitFor <= 0 {
		t.Fatalf("no time left to wait for the REFER; test timings are wrong")
	}
	t.Logf("waiting up to %s for the REFER (app pauses %s after answer, then transfers)",
		waitFor.Round(time.Second), pauseBeforeRefer)

	referCtx, referCancel := context.WithTimeout(ctx, waitFor)
	msg, err := call.AwaitReceivedRequest(referCtx, "REFER")
	referCancel()

	if err != nil {
		if !withReconnect {
			t.Fatalf("CONTROL FAILED: no REFER arrived within %s of answer even though the "+
				"connection was never touched.\n"+
				"Something other than the reconnect is broken — the app may not be emitting "+
				"sip:refer, or REFER may not survive the relay. Until this control passes, a "+
				"failure of TestDrachtio_TportReconnect_ReferAfterPeerReconnects proves nothing.\n"+
				"methods received on this leg: %v\n(await error: %v)",
				waitFor.Round(time.Second), call.MethodsReceived(), err)
		}
		t.Fatalf("REPRODUCED: no REFER arrived within %s of answer.\n"+
			"jambonz emitted it, but drachtio wrote it to the connection pinned at INVITE time — "+
			"the one this test abandoned — instead of the live one from the same host.\n"+
			"drachtio then generated 408 Request Timeout toward the feature-server after Timer F, "+
			"exactly as in the 2026-08-05 incident.\n"+
			"Check TestDrachtio_TportReconnect_ReferControlNoReconnect is green: that is what makes "+
			"this attributable to the reconnect rather than to the harness.\n"+
			"methods received on this leg: %v\n"+
			"fix: build drachtio with the Contact alias table and run it with "+
			"DRACHTIO_TPORT_CONTACT_ALIAS=1\n"+
			"(await error: %v)",
			waitFor.Round(time.Second), call.MethodsReceived(), err)
	}

	referTo := msg.Headers["Refer-To"]
	if !strings.Contains(referTo, referTarget) {
		t.Fatalf("REFER arrived but Refer-To is wrong: got %q want substring %q", referTo, referTarget)
	}
	if withReconnect {
		t.Logf("PASS: REFER arrived %s after answer, on the connection established after the "+
			"reconnect; Refer-To=%q", time.Since(answeredAt).Round(time.Second), referTo)
	} else {
		t.Logf("CONTROL PASS: REFER arrived %s after answer over an untouched connection; "+
			"Refer-To=%q. The scenario is sound, so a failure of the reconnect case is "+
			"attributable to the reconnect.", time.Since(answeredAt).Round(time.Second), referTo)
	}
}

// claimUASVia is claimUAS with a caller-supplied resolver, so a test can point
// the suite realm somewhere other than the SBC — here, at the relay.
func claimUASVia(t *testing.T, ctx context.Context, resolver *jsip.StaticResolver) *jsip.Stack {
	t.Helper()
	_, username, password := client.ManagedSIPClient(t, ctx)

	stk, err := jsip.Start(context.Background(), jsip.Config{
		SIPDomain: suite.SIPRealm,
		User:      username,
		Pass:      password,
		Transport: "tcp",
		LogLevel:  cfg.LogLevel,
		Resolver:  resolver.Resolver(),
		Owner:     t.Name(),
	}, func(_ context.Context, call *jsip.Call) error {
		_ = call.Reject(486, "Busy Here")
		return nil
	})
	if err != nil {
		t.Fatalf("claimUASVia: SIP stack start (user=%s): %v", username, err)
	}
	t.Cleanup(func() {
		// Deregister will travel over the reconnected connection, which the
		// registrar may not associate with the original binding. A 403 here is
		// expected fallout of the scenario, not a failure.
		stk.Stop()
	})
	return stk
}
