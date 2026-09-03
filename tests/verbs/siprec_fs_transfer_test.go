// Test for a SIPREC recording surviving a cross-feature-server dequeue.
//
// A caller is answered, recording starts, and the call is enqueued on one
// feature server. The agent call is answered on a *different* feature server
// and dequeues by callSid, so jambonz moves the queued call with a REFER. The
// SIPREC session is anchored at the SBC and is supposed to keep forking the
// same conversation across that move, then be BYEd when the call ends.
//
// Both failure modes this covers were real:
//   - the media fork was not rebuilt after the transfer, so the recorder went
//     silent from the moment the call moved while the SIP dialog stayed up
//   - the transferred leg's teardown did not stop the recording, so the
//     recorder never got a BYE and had to wait out its own media timeout
//
// Needs JAMBONZ_FEATURE_SERVERS naming two feature servers on different hosts,
// the SBC running with JAMBONES_SERVER_CONTROL enabled (for the
// X-Jambonz-Feature-Server header), and the harness running somewhere the SBC
// can send SIP and RTP to - i.e. inside the cluster's network.
package verbs

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/siprec"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// featureServerHeader pins a call to one feature server. Honoured by
// sbc-inbound when JAMBONES_SERVER_CONTROL is enabled.
const featureServerHeader = "X-Jambonz-Feature-Server"

// how long the recording is watched after the call has moved
const postTransferWatch = 12 * time.Second

// TestVerb_Siprec_SurvivesFeatureServerTransfer — recording keeps receiving the
// conversation across a cross-feature-server dequeue, and is torn down cleanly.
//
// Steps:
//  1. start-siprec-recorder — SRS on an ephemeral port, captures each stream
//  2. script-caller — [answer, config record→SRS, enqueue Q waitHook]
//  3. provision-applications — one per leg
//  4. place-caller-pinned-fs-a — INVITE with X-Jambonz-Feature-Server: fsA
//  5. await-call-hook — pick the jambonz call_sid off the hook payload
//  6. await-siprec-invite — the SBC opens the recording session
//  7. await-queued — waitHook fetch #1, i.e. the caller is parked on fsA
//  8. place-agent-pinned-fs-b — INVITE pinned to fsB, [answer, dequeue callSid]
//  9. await-transfer — a status callback carrying fsB's address = the REFER landed
//
// 10. speak-after-transfer — caller streams the reference WAV post-bridge
// 11. assert-media-continuity — no long silence, rate held up vs before
// 12. assert-recorded-conversation — Deepgram on the fork: sun + shining
// 13. hangup-agent-leg — jambonz ends the moved leg (the no-BYE case)
// 14. assert-siprec-bye — the SBC stopped the recording
func TestVerb_Siprec_SurvivesFeatureServerTransfer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasFeatureServers() {
		t.Skip("needs JAMBONZ_FEATURE_SERVERS with two feature servers")
	}
	fsA, fsB := cfg.FeatureServers[0], cfg.FeatureServers[1]
	if hostOf(fsA) == hostOf(fsB) {
		t.Skipf("feature servers %s and %s are the same host; a call is only "+
			"moved with a REFER between hosts", fsA, fsB)
	}
	ctx := WithTimeout(t, 180*time.Second)
	callerUAS, agentUAS := claimUAS2(t, ctx)

	s := Step(t, "start-siprec-recorder")
	advertise, err := advertiseIP()
	if err != nil {
		s.Fatalf("resolve advertise ip: %v", err)
	}
	rec := siprec.New(advertise, cfg.SiprecSIPPort, cfg.SiprecRTPPortBase,
		filepath.Join(t.TempDir(), "siprec"))
	if err := rec.Start(); err != nil {
		s.Fatalf("start recorder: %v", err)
	}
	t.Cleanup(rec.Stop)
	s.Logf("siprec recorder at %s", rec.URL())
	s.Done()

	s = Step(t, "script-caller")
	queue := fmt.Sprintf("jambonz-it-siprec-q-%d", time.Now().UnixNano())
	callerID, callerSess := claimSession(t)
	agentID, agentSess := claimSession(t)
	SessionAckEmpty(callerSess, "wait")
	callerSess.ScriptCallHook(webhook.Script{
		V("answer"),
		V("config", "record", map[string]any{
			"action":          "startCallRecording",
			"type":            "siprec",
			"siprecServerURL": rec.URL(),
			"recordingID":     fmt.Sprintf("it-%d", time.Now().UnixNano()),
		}),
		V("enqueue", "name", queue, "waitHook", SessionURL(callerSess, "wait")),
	})
	s.Done()

	s = Step(t, "provision-applications")
	callerApp := provisionWebhookApp(t, ctx, "siprec-caller")
	agentApp := provisionWebhookApp(t, ctx, "siprec-agent")
	s.Done()

	s = Step(t, "place-caller-pinned-fs-a")
	callerCall, err := invitePinnedToFS(ctx, callerUAS, callerApp, callerID, fsA)
	if err != nil {
		s.Fatalf("caller Invite (pinned to %s): %v", fsA, err)
	}
	t.Cleanup(func() { _ = callerCall.Hangup() })
	// media has to flow while the caller is parked, or "the fork went silent"
	// cannot be told from "nobody was talking"
	if err := callerCall.SendSilence(); err != nil {
		s.Fatalf("caller SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "await-call-hook")
	// the jambonz call_sid comes off the hook payload; the UAC only ever sees
	// the headers of the INVITE it sent
	cb, err := callerSess.WaitCallbackFor(ctx, "call_hook")
	if err != nil {
		s.Fatalf("caller call_hook never arrived: %v", err)
	}
	callSid := cb.String("call_sid")
	if callSid == "" {
		s.Fatalf("no call_sid in the caller's call_hook payload: %s", cb.Body)
	}
	s.Logf("caller call_sid=%s", callSid)
	s.Done()

	s = Step(t, "await-siprec-invite")
	if !awaitTrue(ctx, 20*time.Second, func() bool { return rec.Saw("invite") }) {
		s.Fatalf("no SIPREC INVITE reached the recorder at %s - the SBC never "+
			"started the recording session", rec.URL())
	}
	s.Done()

	s = Step(t, "await-queued")
	if _, err := callerSess.WaitCallbackFor(ctx, "action/wait"); err != nil {
		s.Fatalf("caller never reached the queue (no waitHook fetch): %v", err)
	}
	s.Done()

	s = Step(t, "place-agent-pinned-fs-b")
	agentSess.ScriptCallHook(webhook.Script{
		V("answer"),
		V("dequeue", "name", queue, "callSid", callSid, "timeout", 30),
	})
	agentCall, err := invitePinnedToFS(ctx, agentUAS, agentApp, agentID, fsB)
	if err != nil {
		s.Fatalf("agent Invite (pinned to %s): %v", fsB, err)
	}
	t.Cleanup(func() { _ = agentCall.Hangup() })
	if err := agentCall.SendSilence(); err != nil {
		s.Fatalf("agent SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "await-transfer")
	// A moved call is answered again on the receiving feature server, which emits
	// a fresh status carrying its own address - the waitHook is NOT re-fetched,
	// because the transferred enqueue task bridges immediately instead of waiting.
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	var transferred time.Time
	seen := WaitCallbacksUntil(waitCtx, callerSess, func(cbs []webhook.Callback) bool {
		for _, c := range cbs {
			if c.Hook == "call_status_hook" && c.NestedString("call_sid") == callSid &&
				strings.HasPrefix(c.NestedString("fs_sip_address"), hostOf(fsB)) {
				transferred = c.Received
				return true
			}
		}
		return false
	})
	cancel()
	if transferred.IsZero() {
		s.Fatalf("the queued call was never moved to %s; callbacks seen: %s", fsB, summarize(seen))
	}
	s.Logf("call moved to %s", fsB)
	s.Done()

	s = Step(t, "speak-after-transfer")
	before := rec.Window(transferred.Add(-6*time.Second), transferred)
	WaitFor(t, "wait-bridge-settles", BridgeSettleDelay)
	wavPath := resolveFixture(t, speechWAV)
	if err := callerCall.SendWAV(wavPath); err != nil {
		s.Fatalf("caller SendWAV: %v", err)
	}
	if err := callerCall.SendSilence(); err != nil {
		s.Fatalf("caller SendSilence after wav: %v", err)
	}
	time.Sleep(postTransferWatch - BridgeSettleDelay)
	after := rec.Window(transferred.Add(BridgeSettleDelay), time.Now())
	s.Done()

	s = Step(t, "assert-media-continuity")
	s.Logf("packet rate before transfer: %s", formatWindows(before))
	s.Logf("packet rate after  transfer: %s", formatWindows(after))
	beforeRate, afterRate := totalRate(before), totalRate(after)
	switch {
	case beforeRate == 0:
		s.Fatalf("the recorder saw no media even before the transfer, so this "+
			"run proves nothing about the fix; check that rtpengine can reach %s",
			advertise)
	case afterRate == 0:
		s.Errorf("the recording went silent when the call moved to %s: the media "+
			"fork was not rebuilt after the transfer", fsB)
	case afterRate < beforeRate/2:
		s.Errorf("media dropped to %.0f%% of its pre-transfer rate (%.0f/s -> %.0f/s): "+
			"the fork was only partly rebuilt", afterRate/beforeRate*100, beforeRate, afterRate)
	}
	if gap := rec.LongestGap(transferred, time.Now()); gap > 2*time.Second {
		s.Errorf("recording had %v of silence after the transfer", gap)
	}
	if n := rec.Count("extra-invite"); n > 0 {
		s.Errorf("the SBC opened %d extra SIPREC session(s) for one call", n)
	}
	s.Done()

	s = Step(t, "assert-recorded-conversation")
	// packets alone would pass on a fork wired to the wrong media, so check the
	// words spoken after the move actually landed in the recording
	AssertMulawTranscriptContains(s, ctx, rec.StreamPath("1"), "sun", "shining")
	s.Done()

	s = Step(t, "hangup-agent-leg")
	// ending it from the agent side makes jambonz tear the moved leg down,
	// which is the path that used to leave the recorder without a BYE
	if err := agentCall.Hangup(); err != nil {
		s.Fatalf("agent Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "assert-siprec-bye")
	if !awaitTrue(ctx, 15*time.Second, func() bool { return rec.Saw("bye") }) {
		s.Errorf("no BYE for the SIPREC session after the call ended: the " +
			"recorder is left to time out on its own")
	}
	for _, e := range rec.Events() {
		s.Logf("recorder event %+d.%03ds %s %s",
			int(e.At.Sub(transferred).Seconds()),
			int(e.At.Sub(transferred).Milliseconds())%1000, e.Kind, e.Detail)
	}
	s.Done()
}

// invitePinnedToFS places an inbound call to an application, asking the SBC to
// route it to a specific feature server.
func invitePinnedToFS(ctx context.Context, uas *UAS, appSID, testID, fs string) (*jsip.Call, error) {
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	return uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		Headers: jsip.H{
			webhook.CorrelationHeader: testID,
			featureServerHeader:       fs,
		},
	})
}

// advertiseIP is the address the SBC should send SIPREC signalling and media
// to: whatever local address routes to the SBC, unless one was configured.
func advertiseIP() (net.IP, error) {
	if cfg.SiprecAdvertiseIP != nil {
		return cfg.SiprecAdvertiseIP, nil
	}
	c, err := net.Dial("udp4", net.JoinHostPort(cfg.SBCPublicIP.String(), "5060"))
	if err != nil {
		return nil, err
	}
	defer c.Close()
	return c.LocalAddr().(*net.UDPAddr).IP, nil
}

// summarize renders callbacks compactly, so a failure shows what did arrive.
func summarize(cbs []webhook.Callback) string {
	if len(cbs) == 0 {
		return "none"
	}
	out := make([]string, 0, len(cbs))
	for _, c := range cbs {
		out = append(out, fmt.Sprintf("%s[status=%s fs=%s]",
			c.Hook, c.NestedString("call_status"), c.NestedString("fs_sip_address")))
	}
	return strings.Join(out, " ")
}

func hostOf(fs string) string {
	if h, _, err := net.SplitHostPort(fs); err == nil {
		return h
	}
	return fs
}

func awaitTrue(ctx context.Context, budget time.Duration, cond func() bool) bool {
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		select {
		case <-ctx.Done():
			return cond()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return cond()
}

func totalRate(ws []siprec.Window) float64 {
	var sum float64
	for _, w := range ws {
		sum += w.Rate
	}
	return sum
}

func formatWindows(ws []siprec.Window) string {
	if len(ws) == 0 {
		return "no streams"
	}
	out := ""
	for i, w := range ws {
		if i > 0 {
			out += ", "
		}
		out += fmt.Sprintf("stream %s %.0f/s (%d pkts)", w.Label, w.Rate, w.Packets)
	}
	return out
}
