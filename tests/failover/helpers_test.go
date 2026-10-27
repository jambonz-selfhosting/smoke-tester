//go:build failover

package failover

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

const activeFsSet = "default:active-fs"

// boxSSH runs commands on the all-in-one test box as the user that owns pm2.
type boxSSH struct {
	target, key, deadFS string
}

func newBoxSSH() (*boxSSH, error) {
	b := &boxSSH{
		target: os.Getenv("FAILOVER_SSH"),
		key:    os.Getenv("FAILOVER_SSH_KEY"),
		deadFS: os.Getenv("FAILOVER_DEAD_FS"),
	}
	if b.target == "" || b.key == "" || b.deadFS == "" {
		return nil, errors.New("FAILOVER_SSH, FAILOVER_SSH_KEY and FAILOVER_DEAD_FS must be set")
	}
	return b, nil
}

func (b *boxSSH) run(t *testing.T, cmd string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-i", b.key, b.target, cmd).CombinedOutput()
	if err != nil {
		t.Fatalf("ssh %q: %v\n%s", cmd, err, out)
	}
	return strings.TrimSpace(string(out))
}

func (b *boxSSH) activeFS(t *testing.T) []string {
	t.Helper()
	return strings.Fields(b.run(t, "redis-cli smembers "+activeFsSet))
}

// addDeadFS puts a feature server that does not exist into active-fs. The
// sidecar only expires members it has heard from, so it stays until removed.
func (b *boxSSH) addDeadFS(t *testing.T) {
	t.Helper()
	b.run(t, fmt.Sprintf("redis-cli sadd %s %s", activeFsSet, b.deadFS))
	t.Cleanup(func() { b.run(t, fmt.Sprintf("redis-cli srem %s %s", activeFsSet, b.deadFS)) })
}

// liveFS returns the real feature server: the active-fs member that is not the fake one.
func (b *boxSSH) liveFS(t *testing.T) string {
	t.Helper()
	for _, m := range b.activeFS(t) {
		if m != b.deadFS {
			return m
		}
	}
	t.Fatalf("no live feature server in %s", activeFsSet)
	return ""
}

func (b *boxSSH) waitForFS(t *testing.T, member string, present bool, within time.Duration) time.Duration {
	t.Helper()
	start := time.Now()
	for time.Since(start) < within {
		found := false
		for _, m := range b.activeFS(t) {
			found = found || m == member
		}
		if found == present {
			return time.Since(start)
		}
		time.Sleep(time.Second)
	}
	t.Fatalf("%s present=%v not reached in %s after %s", member, present, activeFsSet, within)
	return 0
}

func claimUAC(t *testing.T, ctx context.Context) *jsip.Stack {
	t.Helper()
	_, username, password := client.ManagedSIPClient(t, ctx)
	stk, err := jsip.Start(context.Background(), jsip.Config{
		SIPDomain: suite.SIPRealm,
		User:      username,
		Pass:      password,
		Transport: "tcp",
		LogLevel:  cfg.LogLevel,
		Resolver:  sipResolver.Resolver(),
		Owner:     t.Name(),
	}, func(_ context.Context, call *jsip.Call) error {
		_ = call.Reject(486, "Busy Here")
		return nil
	})
	if err != nil {
		t.Fatalf("claimUAC: SIP stack start (user=%s): %v", username, err)
	}
	t.Cleanup(stk.Stop)
	return stk
}

func inviteApp(ctx context.Context, stk *jsip.Stack) (*jsip.Call, error) {
	return inviteTo(ctx, stk, appSID)
}

func inviteTo(ctx context.Context, stk *jsip.Stack, sid string) (*jsip.Call, error) {
	return stk.Invite(ctx, fmt.Sprintf("sip:app-%s@%s", sid, suite.SIPRealm), jsip.InviteOptions{Transport: "tcp"})
}

// inboundRetriesFrom counts sbc-inbound log lines where it gave up on fs and tried the next one.
func (b *boxSSH) inboundRetriesFrom(t *testing.T, fs string) int {
	t.Helper()
	out := b.run(t, fmt.Sprintf(`cat $(pm2 describe inbound | awk '/out log path/ {print $(NF-1)}') | grep -c "feature server %s failed" || true`, fs))
	n, _ := strconv.Atoi(strings.TrimSpace(out))
	return n
}

func (b *boxSSH) featureServerPID(t *testing.T) string {
	return b.run(t, "pm2 pid feature-server")
}

func (b *boxSSH) mediajamPID(t *testing.T) string {
	return b.run(t, "systemctl show -p MainPID --value mediajam")
}

// freeze stops the processes as if their host had died; cleanup resumes them
// and waits for the feature server to check in again.
func (b *boxSSH) freeze(t *testing.T, live string, pids ...string) {
	t.Helper()
	b.run(t, "kill -STOP "+strings.Join(pids, " "))
	t.Cleanup(func() {
		b.run(t, "kill -CONT "+strings.Join(pids, " "))
		b.waitForFS(t, live, true, 60*time.Second)
	})
}

// stillUp fails the test if the call ends within d.
func stillUp(t *testing.T, call *jsip.Call, d time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	if err := call.WaitState(ctx, jsip.StateEnded); err == nil {
		t.Fatalf("call ended after %s, want it still up after %s (reason: %s, BYE X-Reason: %q)",
			call.Duration().Round(time.Second), d, call.EndReason(), byeReason(call))
	}
}

func byeReason(call *jsip.Call) string {
	if byes := call.ReceivedByMethod("BYE"); len(byes) > 0 {
		return byes[0].Header("X-Reason")
	}
	return ""
}

// endedBySBC waits for the SBC to hang the call up for a dead feature server and returns how long it took.
func endedBySBC(t *testing.T, call *jsip.Call, since time.Time, within time.Duration) time.Duration {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), within)
	defer cancel()
	if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
		t.Fatalf("caller still up %s after the feature server died", time.Since(since).Round(time.Second))
	}
	took := time.Since(since)
	if got := byeReason(call); got != "feature server unavailable" {
		t.Errorf("BYE X-Reason = %q, want %q (end reason: %s)", got, "feature server unavailable", call.EndReason())
	}
	return took
}
