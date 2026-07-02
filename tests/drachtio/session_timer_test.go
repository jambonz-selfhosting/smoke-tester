//go:build drachtio

package drachtio

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// sessionInterval is the Session-Expires delta-seconds the harness offers in
// both tests below. Bump this in one place if the SBC's Min-SE ever forces a
// 422 (Session Interval Too Small).
// SBC (drachtio) floors the session timer at Min-SE=90 and ignores a lower
// offer, so 90 is the practical minimum; tests take ~90-120s each.
const sessionInterval = 90 // seconds offered in Session-Expires

// TestDrachtio_SessionTimer_UASRefresher — RFC 4028 refresher=uas path.
//
// When the SBC is configured sip/session-timers/@default-refresher=uas,
// drachtio (as the refresher) MUST proactively send a refreshing re-INVITE
// at ~half the negotiated interval, well before the session would otherwise
// expire. Silence (or a teardown BYE instead of a re-INVITE) is the bug.
//
// Steps:
//  1. UAC INVITEs the inline-app_json Application with Session-Expires: 90
//     and Supported: timer, but NO refresher param — so the SBC's
//     configured default decides who refreshes.
//  2. Assert the 200 OK and read back the negotiated Session-Expires.
//  3. If the SBC negotiated refresher=uac, skip (deployment config
//     difference, not a code bug) — this test only covers refresher=uas.
//  4. Otherwise wait for a refresh re-INVITE (or a premature BYE, which is
//     the bug) within delta+15s, and assert it lands before the full
//     interval elapses.
func TestDrachtio_SessionTimer_UASRefresher(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteApp(t, ctx, uas, jsip.H{
		"Session-Expires": strconv.Itoa(sessionInterval),
		"Supported":       "timer",
	})
	if err != nil {
		t.Fatalf("inviteApp: %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	resp, ok := call.AnsweredResponse()
	if !ok {
		t.Fatalf("no recorded 200 OK for INVITE")
	}

	se := resp.Header("Session-Expires")
	if se == "" {
		t.Fatalf("200 OK has no Session-Expires — SBC is not doing session timers; RFC4028 §9 requires the 2xx to echo it when timer negotiated")
	}

	delta, refresher, err := jsip.ParseSessionExpires(se)
	if err != nil {
		t.Fatalf("ParseSessionExpires(%q): %v", se, err)
	}

	if refresher == "uac" {
		t.Skipf("SBC negotiated refresher=uac (Session-Expires: %q); this test requires drachtio default-refresher=uas. Set sip/session-timers/@default-refresher=uas in drachtio.conf.xml.", se)
	}
	if refresher != "uas" {
		t.Fatalf("negotiated refresher=%q (Session-Expires: %q); expected \"uas\"", refresher, se)
	}

	start := time.Now()
	subCtx, subCancel := context.WithTimeout(ctx, time.Duration(delta+15)*time.Second)
	defer subCancel()

	msg, err := call.AwaitReceivedRequest(subCtx, "INVITE", "BYE")
	if err != nil {
		t.Fatalf("drachtio (refresher=uas) sent NEITHER a refresh re-INVITE NOR a BYE within %ds — session timer not honored", delta+15)
	}

	if msg.Method == "BYE" {
		t.Fatalf("expected a proactive refresh re-INVITE at ~%ds, but drachtio tore the call down with BYE Reason=%q", delta/2, msg.Header("Reason"))
	}

	elapsed := time.Since(start)
	if elapsed >= time.Duration(delta)*time.Second {
		t.Fatalf("refresh re-INVITE arrived after %s, at or beyond the full interval (delta=%ds) — expected it before full expiry", elapsed, delta)
	}
	t.Logf("refresh re-INVITE arrived after %s (delta=%ds)", elapsed, delta)

	// diago auto-ACKs the inbound re-INVITE; we only observe it here.
}

// TestDrachtio_SessionTimer_UACRefresherExpiry — RFC 4028 refresher=uac path
// where the UAC (harness) never refreshes.
//
// The harness offers refresher=uac and then deliberately sends no refresh.
// drachtio MUST enforce the session timer and BYE at ~the full negotiated
// interval with Reason "Session timer expired" — anything else (no BYE, a
// BYE for a different reason, or a BYE far too early/late) is the bug.
func TestDrachtio_SessionTimer_UACRefresherExpiry(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteApp(t, ctx, uas, jsip.H{
		"Session-Expires": strconv.Itoa(sessionInterval) + ";refresher=uac",
		"Supported":       "timer",
	})
	if err != nil {
		t.Fatalf("inviteApp: %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	resp, ok := call.AnsweredResponse()
	if !ok {
		t.Fatalf("no recorded 200 OK for INVITE")
	}

	delta, refresher, err := jsip.ParseSessionExpires(resp.Header("Session-Expires"))
	if err != nil {
		t.Fatalf("ParseSessionExpires(%q): %v", resp.Header("Session-Expires"), err)
	}
	if refresher != "uac" {
		t.Fatalf("offered refresher=uac but SBC negotiated %q", refresher)
	}

	// Deliberately send no refresh — wait for drachtio to enforce the timer.
	start := time.Now()
	subCtx, subCancel := context.WithTimeout(ctx, time.Duration(delta+30)*time.Second)
	defer subCancel()

	if err := call.WaitState(subCtx, jsip.StateEnded); err != nil {
		t.Fatalf("no BYE within %ds — drachtio did not enforce the session timer (UAC never refreshed)", delta+30)
	}

	if reason := call.EndReason(); reason != "remote-bye" {
		t.Fatalf("end reason: got %q want %q", reason, "remote-bye")
	}

	byes := call.ReceivedByMethod("BYE")
	if len(byes) == 0 {
		t.Fatalf("call ended but no BYE was recorded in Received()")
	}
	reason := byes[0].Header("Reason")
	if !strings.Contains(reason, "Session timer expired") {
		t.Fatalf("BYE Reason=%q, expected it to contain \"Session timer expired\" — a different BYE reason means the call ended for another cause (e.g. the app pause elapsed), not the session timer", reason)
	}

	elapsed := time.Since(start)
	lo := time.Duration(delta-15) * time.Second
	hi := time.Duration(delta+30) * time.Second
	if elapsed < lo || elapsed > hi {
		t.Fatalf("BYE arrived after %s, outside expected window [%s, %s] for delta=%ds — too early suggests the app pause ended the call, not the session timer; too late suggests drachtio delayed enforcement", elapsed, lo, hi, delta)
	}
	t.Logf("session-timer BYE arrived after %s (delta=%ds)", elapsed, delta)

	// Guard against a spurious refresh re-INVITE: no request-only INVITE
	// accessor exists on Call (ReceivedByMethod also matches responses'
	// synthesized Method from CSeq is only for BYE lookups here, but INVITE
	// matches would double as the 200 OK's CSeq method too), so we
	// deliberately skip this guard rather than add a loose/incorrect assert.
}

// TestDrachtio_SessionTimer_UACRefresherKeepalive — happy-path counterpart of
// UACRefresherExpiry.
//
// The harness offers refresher=uac and, unlike UACRefresherExpiry, actually
// does its job: it proactively sends a refreshing re-INVITE (re-offering
// Session-Expires) at ~half the negotiated interval, twice in a row. drachtio
// cancels and re-arms its session-expiry timer from the Session-Expires
// header on each received re-INVITE, so as long as the refreshes keep
// landing before the timer fires, the call must stay up — a session-timer
// BYE here is the bug.
func TestDrachtio_SessionTimer_UACRefresherKeepalive(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	refreshHdrs := jsip.H{
		"Session-Expires": strconv.Itoa(sessionInterval) + ";refresher=uac",
		"Supported":       "timer",
	}

	call, err := inviteApp(t, ctx, uas, refreshHdrs)
	if err != nil {
		t.Fatalf("inviteApp: %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	resp, ok := call.AnsweredResponse()
	if !ok {
		t.Fatalf("no recorded 200 OK for INVITE")
	}

	// Capture delta here, before any refresh — AnsweredResponse() reports the
	// LAST INVITE 200 OK once a refresh has gone out, not this initial one.
	delta, refresher, err := jsip.ParseSessionExpires(resp.Header("Session-Expires"))
	if err != nil {
		t.Fatalf("ParseSessionExpires(%q): %v", resp.Header("Session-Expires"), err)
	}
	if refresher != "uac" {
		t.Fatalf("offered refresher=uac but SBC negotiated %q", refresher)
	}

	for i := 1; i <= 2; i++ {
		select {
		case <-time.After(time.Duration(delta/2) * time.Second):
		case <-ctx.Done():
			t.Fatalf("ctx expired before refresh #%d", i)
		}

		// The call must still be up at refresh time.
		if call.State() == jsip.StateEnded {
			reason := call.EndReason()
			byes := call.ReceivedByMethod("BYE")
			extra := ""
			if len(byes) > 0 {
				extra = byes[0].Header("Reason")
			}
			t.Fatalf("call ended before refresh #%d (endReason=%q, BYE Reason=%q) — drachtio tore it down instead of accepting refresh", i, reason, extra)
		}

		rctx, rcancel := context.WithTimeout(ctx, 15*time.Second)
		res, err := call.SendReinvite(rctx, refreshHdrs)
		rcancel()
		if err != nil {
			st := 0
			if res != nil {
				st = res.StatusCode
			}
			t.Fatalf("refresh re-INVITE #%d failed: %v (status=%d)", i, err, st)
		}
		// SendReinvite already errors on non-2xx, so reaching here means 2xx.
		t.Logf("refresh re-INVITE #%d accepted (status=%d) at ~%ds", i, res.StatusCode, i*(delta/2))
	}

	// Now at ~t=delta (2 * delta/2). Prove the call SURVIVES past the full
	// interval with no timer BYE.
	wctx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	werr := call.WaitState(wctx, jsip.StateEnded)
	if werr == nil {
		reason := call.EndReason()
		byes := call.ReceivedByMethod("BYE")
		extra := ""
		if len(byes) > 0 {
			extra = byes[0].Header("Reason")
		}
		t.Fatalf("call ended despite refreshes (endReason=%q, BYE Reason=%q)", reason, extra)
	}
	if !errors.Is(werr, context.DeadlineExceeded) {
		t.Fatalf("WaitState returned unexpected error (want context.DeadlineExceeded == still alive): %v", werr)
	}

	// Final assertions before the deferred Hangup runs.
	if call.State() == jsip.StateEnded {
		t.Fatalf("call is StateEnded at end")
	}
	if reason := call.EndReason(); reason != "" {
		t.Fatalf("unexpected EndReason %q", reason)
	}
	if n := len(call.ReceivedByMethod("BYE")); n > 0 {
		t.Fatalf("received %d BYE(s); a session-timer BYE means the refresh keepalive failed", n)
	}
	t.Logf("call stayed up through 2 refreshes over ~%ds with no session-timer BYE", delta)
}
