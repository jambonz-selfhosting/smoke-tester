//go:build failover

package failover

import (
	"context"
	"errors"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

// With an unreachable feature server in active-fs, round-robin sends some calls
// to it first; every call must still be answered by trying the live one next.
func TestFailover_NewCallsSkipDeadFS(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	box.liveFS(t)
	box.addDeadFS(t)
	stk := claimUAC(t, ctx)

	const calls = 4
	slow := 0
	for i := 0; i < calls; i++ {
		start := time.Now()
		callCtx, callCancel := context.WithTimeout(ctx, 45*time.Second)
		call, err := inviteApp(callCtx, stk)
		callCancel()
		if err != nil {
			t.Fatalf("call %d/%d not answered: %v", i+1, calls, err)
		}
		setup := time.Since(start)
		if setup > time.Second {
			slow++
		}
		t.Logf("call %d/%d answered in %s", i+1, calls, setup.Round(time.Millisecond))
		_ = call.Hangup()
	}
	// round-robin over two members puts the dead one first for half the calls
	if slow == 0 {
		t.Errorf("no call was slowed by the dead feature server, so failover was never exercised")
	}
}

// A real rejection from the first feature server must reach the caller as-is,
// not be retried elsewhere and not be swallowed.
func TestFailover_RejectionIsRelayed(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	box.liveFS(t)
	box.addDeadFS(t)
	stk := claimUAC(t, ctx)

	for i := 0; i < 4; i++ {
		start := time.Now()
		callCtx, callCancel := context.WithTimeout(ctx, 45*time.Second)
		call, err := inviteTo(callCtx, stk, declineSID)
		callCancel()
		if err == nil {
			_ = call.Hangup()
			t.Fatalf("call %d was answered, want 486", i+1)
		}
		var rejected *jsip.InviteRejected
		if !errors.As(err, &rejected) || rejected.StatusCode != 486 {
			t.Fatalf("call %d: got %v, want 486", i+1, err)
		}
		if got := rejected.RejectedHeader("X-Reason"); got != "declined by app" {
			t.Errorf("call %d: 486 X-Reason = %q, want the app's %q", i+1, got, "declined by app")
		}
		t.Logf("call %d rejected with 486 in %s", i+1, time.Since(start).Round(time.Millisecond))
	}
}

// A 503 the feature server sends on purpose is an answer, not a sign it is
// unreachable, so the SBC must not replay the call on another feature server.
func TestFailover_DeliberateBusyNotRetried(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	live := box.liveFS(t)
	box.addDeadFS(t)
	stk := claimUAC(t, ctx)
	before := box.inboundRetriesFrom(t, live)

	for i := 0; i < 4; i++ {
		callCtx, callCancel := context.WithTimeout(ctx, 45*time.Second)
		call, err := inviteTo(callCtx, stk, busySID)
		callCancel()
		if err == nil {
			_ = call.Hangup()
			t.Fatalf("call %d was answered, want 503", i+1)
		}
		var rejected *jsip.InviteRejected
		if !errors.As(err, &rejected) || rejected.StatusCode != 503 {
			t.Fatalf("call %d: got %v, want 503", i+1, err)
		}
	}
	if after := box.inboundRetriesFrom(t, live); after != before {
		t.Errorf("sbc-inbound retried %d calls after the live feature server's own 503", after-before)
	}
}

// Control case: a healthy call that sits in a long pause must not be mistaken for a dead feature server.
func TestFailover_HealthyCallStaysUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	stk := claimUAC(t, ctx)
	call, err := inviteApp(ctx, stk)
	if err != nil {
		t.Fatalf("call not answered: %v", err)
	}
	defer func() { _ = call.Hangup() }()
	stillUp(t, call, 40*time.Second)
}

// Freezing the feature server and its media server is what a dead host looks
// like: no BYE, no RTP. Every call on it must be hung up by the SBC.
func TestFailover_DeadFSHangsUpCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	live := box.liveFS(t)
	stk := claimUAC(t, ctx)

	const calls = 3
	var active []*jsip.Call
	for i := 0; i < calls; i++ {
		call, err := inviteApp(ctx, stk)
		if err != nil {
			t.Fatalf("call %d not answered: %v", i+1, err)
		}
		defer func() { _ = call.Hangup() }()
		active = append(active, call)
	}

	box.freeze(t, live, box.featureServerPID(t), box.mediajamPID(t))
	frozenAt := time.Now()
	for i, call := range active {
		took := endedBySBC(t, call, frozenAt, 60*time.Second)
		t.Logf("call %d hung up %s after the feature server died", i+1, took.Round(time.Millisecond))
		if took > 30*time.Second {
			t.Errorf("call %d took %s to hang up, want < 30s", i+1, took)
		}
	}
}

// A draining feature server leaves active-fs but keeps serving its calls;
// those calls must not be cut.
func TestFailover_DrainingFSKeepsCalls(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	live := box.liveFS(t)
	stk := claimUAC(t, ctx)
	call, err := inviteApp(ctx, stk)
	if err != nil {
		t.Fatalf("call not answered: %v", err)
	}
	defer func() { _ = call.Hangup() }()

	box.run(t, "kill -USR1 "+box.featureServerPID(t))
	t.Cleanup(func() {
		// there is no undrain signal, so a restart puts it back in service
		box.run(t, "pm2 restart feature-server >/dev/null")
		box.waitForFS(t, live, true, 60*time.Second)
	})
	box.waitForFS(t, live, false, 30*time.Second)
	t.Logf("feature server %s left active-fs while draining", live)
	stillUp(t, call, 40*time.Second)
}

// A call the caller puts on hold gets no RTP back; that must not look like a dead feature server.
func TestFailover_HeldCallStaysUp(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	stk := claimUAC(t, ctx)
	call, err := inviteApp(ctx, stk)
	if err != nil {
		t.Fatalf("call not answered: %v", err)
	}
	defer func() { _ = call.Hangup() }()

	sdp, err := jsip.EarlyMediaSDPWithDirection("PCMU", "127.0.0.1", 40000, int(time.Now().Unix()), "inactive")
	if err != nil {
		t.Fatal(err)
	}
	res, err := call.SendReinviteWithSDP(ctx, sdp, nil)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("hold re-INVITE failed: res=%v err=%v", res, err)
	}
	stillUp(t, call, 40*time.Second)
}

// The feature-server process crashes and pm2 restarts it while the media server
// stays up. Records whether the SBC notices.
func TestFailover_FSProcessCrash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	live := box.liveFS(t)
	stk := claimUAC(t, ctx)
	call, err := inviteApp(ctx, stk)
	if err != nil {
		t.Fatalf("call not answered: %v", err)
	}
	defer func() { _ = call.Hangup() }()

	box.run(t, "kill -9 "+box.featureServerPID(t))
	killedAt := time.Now()
	t.Cleanup(func() { box.waitForFS(t, live, true, 60*time.Second) })

	waitCtx, waitCancel := context.WithTimeout(ctx, 60*time.Second)
	defer waitCancel()
	if err := call.WaitState(waitCtx, jsip.StateEnded); err != nil {
		t.Fatalf("caller still up 60s after the feature-server process crashed")
	}
	t.Logf("call ended %s after the crash (X-Reason %q, end reason %s)",
		time.Since(killedAt).Round(time.Millisecond), byeReason(call), call.EndReason())
}
