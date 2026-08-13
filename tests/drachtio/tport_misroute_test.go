//go:build drachtio

package drachtio

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// TestDrachtio_TportMisroute_TwoPeersOneSourceAddress guards the half of the
// contact-alias design that the reconnect tests cannot reach: that following a
// peer to a new connection never follows the *wrong* peer there.
//
// This is a regression test in the literal sense. An earlier version of the
// feature keyed connection tracking on the peer's source address alone. That is
// correct for a carrier trunk, which owns its address, and wrong for everything
// else, because peers share addresses constantly — an office of phones behind
// one NAT, a CI machine running clients in parallel. Enabled on a live cluster
// it sent one agent's in-dialog request down another agent's socket; the
// receiving agent answered "Call/Transaction Does Not Exist" to an ACK for a
// dialog it had never heard of, the sending agent's transaction timed out, and
// most of the jambonz smoke-test suite went red. The current design keys on the
// address each peer *advertises* in its Contact, which differs even when the
// source address does not.
//
// The reconnect tests would pass under either design, so on their own they do
// not defend the property that actually broke. This one does:
//
//	UAC A ─┐                      ┌─ upstream A ─┐
//	       ├─ tcpRelay (one) ─────┤              ├─▶ jambonz SBC
//	UAC B ─┘                      └─ upstream B ─┘
//
// Both UACs reach the SBC from one source address, on separate connections and
// with separate Contacts. Only A's connection is abandoned and replaced. B is
// untouched throughout, and B's REFER must still arrive: if it does not, the
// send path let A's reconnect move a dialog that was never A's.
//
// The failure is asymmetric and worth stating: A's REFER arriving proves the
// alias works (the reconnect tests already prove that), while B's REFER
// arriving proves it is not over-eager. Only B is asserted here.
func TestDrachtio_TportMisroute_TwoPeersOneSourceAddress(t *testing.T) {
	// Not parallel: owns 127.0.0.1:5060, and reasons about which connections
	// exist to one address.

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	const (
		pauseBeforeRefer = 20 * time.Second
		reconnectAt      = 5 * time.Second
		referDeadline    = 45 * time.Second
		referTarget      = "sip:transfer-target@example.invalid"
	)

	appJSON := fmt.Sprintf(
		`[{"verb":"answer"},{"verb":"pause","length":%d},{"verb":"sip:refer","referTo":%q},{"verb":"pause","length":30},{"verb":"hangup"}]`,
		int(pauseBeforeRefer.Seconds()), referTarget)

	appCtx, appCancel := context.WithTimeout(ctx, 30*time.Second)
	appSID, err := client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("drachtio-misroute-app"),
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
		if err := client.DeleteApplication(delCtx, appSID); err != nil {
			t.Logf("cleanup: delete application %s: %v", appSID, err)
		}
	})

	// One relay for both peers: every upstream connection leaves from the same
	// source address, which is the whole point.
	relay := newTCPRelay(t, relayListenAddr, sbcAddr())

	relayResolver, err := jsip.NewStaticResolver(map[string]string{suite.SIPRealm: "127.0.0.1"})
	if err != nil {
		t.Fatalf("relay resolver: %v", err)
	}
	t.Cleanup(func() { _ = relayResolver.Close() })

	// Two independent SIP users, so each dialog's remote target is a distinct
	// Contact. They differ in port (and user), exactly as two softphones behind
	// one NAT would.
	uasA := claimUASVia(t, ctx, relayResolver)
	uasB := claimUASVia(t, ctx, relayResolver)

	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)

	callA, err := uasA.Invite(ctx, dest, jsip.InviteOptions{Transport: "tcp"})
	if err != nil {
		t.Fatalf("peer A INVITE: %v", err)
	}
	defer callA.Hangup()

	callB, err := uasB.Invite(ctx, dest, jsip.InviteOptions{Transport: "tcp"})
	if err != nil {
		t.Fatalf("peer B INVITE: %v", err)
	}
	defer callB.Hangup()

	if got := callA.AnsweredStatus(); got != 200 {
		t.Fatalf("peer A answered status: got %d want 200", got)
	}
	if got := callB.AnsweredStatus(); got != 200 {
		t.Fatalf("peer B answered status: got %d want 200", got)
	}
	answeredAt := time.Now()
	t.Logf("both peers up on one source address — A call-id %s, B call-id %s",
		callA.CallID(), callB.CallID())

	// Abandon and replace every upstream. Both peers' connections move, which is
	// the harshest version of the scenario: each peer must be followed to its own
	// replacement, not to the other's.
	select {
	case <-time.After(reconnectAt):
	case <-ctx.Done():
		t.Fatalf("context expired before the reconnect point")
	}
	if n := relay.Reconnect(); n < 2 {
		t.Fatalf("relay.Reconnect swapped %d connections, want both — the peers are not "+
			"sharing the relay as this test assumes", n)
	}

	// Each peer announces its own Contact on its own new connection. If the send
	// path keys on anything coarser than that Contact, these two announcements
	// overwrite each other and one peer ends up pointed at the other's socket.
	for _, c := range []struct {
		name string
		call *jsip.Call
	}{{"A", callA}, {"B", callB}} {
		infoCtx, infoCancel := context.WithTimeout(ctx, 10*time.Second)
		res, err := c.call.SendInfo(infoCtx, "application/x-test", []byte("probe"))
		infoCancel()
		if err != nil {
			t.Logf("peer %s in-dialog INFO returned %v — harmless, the request still crossed", c.name, err)
		} else if res != nil {
			t.Logf("peer %s in-dialog INFO answered %d %s", c.name, res.StatusCode, res.Reason)
		}
	}

	// B is the assertion. B was never singled out, so its REFER must arrive on
	// B's own connection; anything else means a peer's traffic followed another
	// peer's reconnect.
	waitFor := referDeadline - time.Since(answeredAt)
	if waitFor <= 0 {
		t.Fatalf("no time left to wait for the REFER; test timings are wrong")
	}
	t.Logf("waiting up to %s for peer B's REFER", waitFor.Round(time.Second))

	referCtx, referCancel := context.WithTimeout(ctx, waitFor)
	_, errB := callB.AwaitReceivedRequest(referCtx, "REFER")
	referCancel()

	if errB != nil {
		t.Fatalf("MISROUTE: peer B never received its REFER within %s of answer, "+
			"though nothing was done to B's connection.\n"+
			"Its dialog was pointed at a connection that is not B's — the send path is "+
			"matching peers on something they share (their source address) rather than on "+
			"something that distinguishes them (the address each advertises in its Contact).\n"+
			"This is the defect that took out the smoke-test suite when connection tracking "+
			"was keyed on source address alone.\n"+
			"methods received on B's leg: %v\n(await error: %v)",
			waitFor.Round(time.Second), callB.MethodsReceived(), errB)
	}
	t.Logf("PASS: peer B's REFER arrived %s after answer despite a peer on the same source "+
		"address reconnecting — peers are being told apart",
		time.Since(answeredAt).Round(time.Second))
}
