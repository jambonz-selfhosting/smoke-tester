// SBC rtpengine blocks that must hit one direction only: rtpengine 12.4+
// ignores from-tag on block media/DTMF without the 'directional' flag.
package verbs

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// five9PairHeader is what sbc-inbound keys its Voicestream special case on.
const five9PairHeader = "X-Five9-StreamingPairId"

const (
	// minSpeechRMS proves the reference WAV actually crossed the bridge.
	minSpeechRMS = 500
	// blockPollAttempts bounds the wait for an async mute/unmute to land.
	blockPollAttempts = 4
)

// TestFive9_Voicestream_ListenGetsAudio — a Five9 Voicestream INVITE
// (a=sendonly + X-Five9-StreamingPairId, as Five9 sends it) into a `listen`
// app must stream the caller's audio to the WebSocket.
//
// Steps:
//   - script-listen
//   - provision-application
//   - invite-as-five9
//   - send-speech
//   - hangup-and-wait-ws-close
//   - assert-ws-audio
func TestFive9_Voicestream_ListenGetsAudio(t *testing.T) {
	t.Parallel()
	runFive9ListenTest(t, "sendonly")
}

// TestFive9_Voicestream_ReverseBlocked — same flow with a sendrecv offer, so
// the SBC's feature-server -> Five9 block is observable at the caller.
//
// Steps:
//   - script-listen
//   - provision-application
//   - invite-as-five9
//   - send-speech
//   - hangup-and-wait-ws-close
//   - assert-ws-audio
//   - assert-reverse-blocked
func TestFive9_Voicestream_ReverseBlocked(t *testing.T) {
	t.Parallel()
	runFive9ListenTest(t, "sendrecv")
}

func runFive9ListenTest(t *testing.T, sdpMode string) {
	t.Helper()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-listen")
	// listen blocks until its WebSocket closes, so the test hangs up itself
	sess.ScriptCallHook(webhook.Script{
		V("listen",
			"url", wssURL(webhookSrv.PublicURL(), "/ws/"+testID),
			"mixType", "mono",
			"sampleRate", 8000),
	})
	s.Done()

	s = Step(t, "provision-application")
	appSID := provisionWebhookApp(t, ctx, "five9-"+sdpMode)
	s.Logf("provisioned Application sid=%s", appSID)
	s.Done()

	s = Step(t, "invite-as-five9")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	call, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  uas.Username,
		Username:  uas.Username,
		Password:  uas.Password,
		SDPMode:   sdpMode,
		Headers: jsip.H{
			webhook.CorrelationHeader: testID,
			five9PairHeader:           "smoke-" + testID,
		},
	})
	if err != nil {
		s.Fatalf("Invite(SDPMode=%s): %v", sdpMode, err)
	}
	t.Cleanup(func() { _ = call.Hangup() })
	if got := call.AnsweredStatus(); got != 200 {
		s.Fatalf("answered status: got %d want 200", got)
	}
	s.Done()

	s = Step(t, "send-speech")
	if err := call.StartRecording(filepath.Join(t.TempDir(), "five9-caller.pcm")); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(RecognizerArmDelay)
	if err := call.SendWAV(resolveFixture(t, speechWAV)); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	s.Done()
	callerBytesIn := call.PCMBytesIn()

	s = Step(t, "hangup-and-wait-ws-close")
	_ = call.Hangup()
	select {
	case <-sess.WSClosed():
	case <-time.After(10 * time.Second):
		s.Logf("WS still open 10s after hangup — collecting what's been received")
	}
	s.Done()

	s = Step(t, "assert-ws-audio")
	drainCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	audio := webhook.BinaryConcat(sess.CollectWS(drainCtx))
	s.Logf("WS audio: %d bytes", len(audio))
	if len(audio) == 0 {
		s.Fatalf("listen WebSocket received zero bytes — WS never streamed (connect/listen failure)")
	}
	// the media server streams silence frames when no caller RTP arrives
	if distinct := countDistinctBytes(audio); distinct < 32 {
		s.Fatalf("REPRODUCED: listen WebSocket got no caller audio on a Five9 Voicestream "+
			"call (%d bytes, %d distinct byte values) — the SBC's reverse-audio block hit "+
			"the entire call (rtpengine block media without 'directional')", len(audio), distinct)
	}
	if len(audio) < 6000 {
		s.Errorf("WS audio too short: %d bytes (want >= 6000)", len(audio))
	}
	s.Done()

	if sdpMode != "sendrecv" {
		return
	}
	s = Step(t, "assert-reverse-blocked")
	// Under 0.2s of PCM16: the SBC must not forward feature-server audio to Five9.
	s.Logf("caller received %d bytes", callerBytesIn)
	if callerBytesIn > 3200 {
		s.Errorf("caller received %d bytes of audio; the SBC should block the "+
			"feature-server -> Five9 direction", callerBytesIn)
	}
	s.Done()
}

// TestMute_ParentLeg_Directional — mute_status on the parent (caller) leg is
// handled by sbc-inbound: the caller must go unheard while still hearing the callee.
//
// Steps:
//   - script-dial-to-callee
//   - provision-application
//   - invite-caller
//   - resolve-call-sids
//   - baseline-audio-both-ways
//   - mute
//   - assert-muted-leg-silenced
//   - assert-muted-leg-still-hears
//   - assert-dtmf-directional
//   - unmute
//   - assert-unmute-restores-audio
func TestMute_ParentLeg_Directional(t *testing.T) {
	t.Parallel()
	runMuteDirectionalTest(t, false)
}

// TestMute_ChildLeg_Directional — mute_status on the dial child leg is
// handled by sbc-outbound: the callee must go unheard while still hearing the caller.
//
// Steps:
//   - script-dial-to-callee
//   - provision-application
//   - invite-caller
//   - resolve-call-sids
//   - baseline-audio-both-ways
//   - mute
//   - assert-muted-leg-silenced
//   - assert-muted-leg-still-hears
//   - assert-dtmf-directional
//   - unmute
//   - assert-unmute-restores-audio
func TestMute_ChildLeg_Directional(t *testing.T) {
	t.Parallel()
	runMuteDirectionalTest(t, true)
}

// Caller --INVITE--> SBC-inbound --> app [dial anchorMedia -> callee] --> SBC-outbound --> Callee
// Test   --PUT /Calls/{sid} mute_status=mute|unmute--> feature-server --INFO X-Reason--> SBC
func runMuteDirectionalTest(t *testing.T, muteChild bool) {
	t.Helper()
	requireWebhook(t)

	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	testID, sess := claimSession(t)
	wavPath := resolveFixture(t, speechWAV)

	s := Step(t, "script-dial-to-callee")
	sess.ScriptCallHook(webhook.Script{
		V("dial",
			"target", []any{map[string]any{
				"type": "user",
				"name": fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm),
			}},
			"timeout", 20,
			"anchorMedia", true),
		V("hangup"),
	})
	s.Done()

	s = Step(t, "provision-application")
	suffix := "mute-parent"
	if muteChild {
		suffix = "mute-child"
	}
	appSID := provisionWebhookApp(t, ctx, suffix)
	s.Done()

	// closed without a value if the callee leg fails, so the main select fails fast
	calleeCh := make(chan *jsip.Call, 1)
	go func() {
		defer close(calleeCh)
		select {
		case c := <-calleeUAS.Inbound:
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			calleeCh <- c
		case <-ctx.Done():
		}
	}()

	s = Step(t, "invite-caller")
	dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
	caller, err := callerUAS.Stack.Invite(ctx, dest, jsip.InviteOptions{
		Transport: "tcp",
		FromUser:  callerUAS.Username,
		Username:  callerUAS.Username,
		Password:  callerUAS.Password,
		Headers:   jsip.H{webhook.CorrelationHeader: testID},
	})
	if err != nil {
		s.Fatalf("Invite: %v", err)
	}
	t.Cleanup(func() { _ = caller.Hangup() })
	callee, ok := <-calleeCh
	if !ok {
		s.Fatalf("callee leg not answered (see callee:* failure, or ctx: %v)", ctx.Err())
	}
	t.Cleanup(func() { _ = callee.Hangup() })
	dir := t.TempDir()
	for name, c := range map[string]*jsip.Call{"caller": caller, "callee": callee} {
		if err := c.StartRecording(filepath.Join(dir, name+".pcm")); err != nil {
			s.Fatalf("StartRecording(%s): %v", name, err)
		}
		if err := c.SendSilence(); err != nil {
			s.Fatalf("SendSilence(%s): %v", name, err)
		}
	}
	s.Done()

	s = Step(t, "resolve-call-sids")
	cb, _, err := WaitCallbackForCollecting(ctx, sess, "call_hook")
	if err != nil {
		s.Fatalf("no call_hook callback: %v", err)
	}
	targetSID := cb.String("call_sid")
	if muteChild {
		// the dial child leg's sid rides on the INVITE the callee received
		targetSID = callee.Header("X-Call-Sid")
	}
	if targetSID == "" {
		s.Fatalf("could not resolve the call_sid to mute (muteChild=%v)", muteChild)
	}
	s.Logf("muting call_sid=%s (child=%v)", targetSID, muteChild)
	time.Sleep(BridgeSettleDelay)
	s.Done()

	// the muted leg must go unheard; the other leg is what it still hears
	muted, other, mutedName := caller, callee, "caller"
	if muteChild {
		muted, other, mutedName = callee, caller, "callee"
	}

	s = Step(t, "baseline-audio-both-ways")
	baseFromMuted := speakRMS(s, muted, other, wavPath)
	baseToMuted := speakRMS(s, other, muted, wavPath)
	s.Logf("baseline RMS: from %s=%.0f to %s=%.0f", mutedName, baseFromMuted, mutedName, baseToMuted)
	if baseFromMuted < minSpeechRMS || baseToMuted < minSpeechRMS {
		s.Fatalf("speech did not cross the bridge before mute: from %s RMS %.0f, to %s RMS %.0f (want >= %d)",
			mutedName, baseFromMuted, mutedName, baseToMuted, minSpeechRMS)
	}
	s.Done()

	s = Step(t, "mute")
	if err := client.UpdateCall(ctx, targetSID, map[string]any{"mute_status": "mute"}); err != nil {
		s.Fatalf("UpdateCall(mute) sid=%s: %v", targetSID, err)
	}
	s.Done()

	s = Step(t, "assert-muted-leg-silenced")
	// the 202 returns before the INFO reaches the SBC, so poll for the block
	silenced := pollRMS(s, muted, other, wavPath, func(rms float64) bool { return rms <= baseFromMuted/10 })
	if silenced > baseFromMuted/10 {
		s.Fatalf("mute did not silence the %s: RMS %.0f vs baseline %.0f — wrong from-tag?",
			mutedName, silenced, baseFromMuted)
	}
	s.Done()

	s = Step(t, "assert-muted-leg-still-hears")
	heard := speakRMS(s, other, muted, wavPath)
	s.Logf("muted %s still hears RMS=%.0f (baseline %.0f)", mutedName, heard, baseToMuted)
	if heard < baseToMuted/3 {
		s.Errorf("REPRODUCED: the muted %s can no longer hear the other party (RMS %.0f vs baseline %.0f) — "+
			"the SBC blocked the entire call (rtpengine block media without 'directional')",
			mutedName, heard, baseToMuted)
	}
	s.Done()

	s = Step(t, "assert-dtmf-directional")
	if err := muted.SendDTMF("1"); err != nil {
		s.Fatalf("muted SendDTMF: %v", err)
	}
	if err := other.SendDTMF("2"); err != nil {
		s.Fatalf("other SendDTMF: %v", err)
	}
	time.Sleep(time.Second)
	if hasDTMF(other, "1") {
		s.Errorf("the muted %s's DTMF still reached the other party; the DTMF block was not applied", mutedName)
	}
	if !hasDTMF(muted, "2") {
		s.Errorf("REPRODUCED: DTMF no longer reaches the muted %s — the SBC blocked DTMF for the "+
			"entire call (rtpengine block DTMF without 'directional')", mutedName)
	}
	s.Done()

	s = Step(t, "unmute")
	if err := client.UpdateCall(ctx, targetSID, map[string]any{"mute_status": "unmute"}); err != nil {
		s.Fatalf("UpdateCall(unmute) sid=%s: %v", targetSID, err)
	}
	s.Done()

	s = Step(t, "assert-unmute-restores-audio")
	restored := pollRMS(s, muted, other, wavPath, func(rms float64) bool { return rms >= baseFromMuted/3 })
	if restored < baseFromMuted/3 {
		s.Errorf("%s stuck muted after unmute: RMS %.0f vs baseline %.0f", mutedName, restored, baseFromMuted)
	}
	s.Done()
}

// speakRMS streams the WAV from speaker and returns the RMS of what the
// listener recorded meanwhile (plus a short tail for bridge latency); 0 when
// no packets arrived at all.
func speakRMS(s *StepCtx, speaker, listener *jsip.Call, wavPath string) float64 {
	start := listener.PCMBytesIn()
	if err := speaker.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := speaker.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(400 * time.Millisecond)
	end := listener.PCMBytesIn()

	pcm, err := os.ReadFile(listener.RawPCMPath())
	if err != nil {
		s.Fatalf("read recording: %v", err)
	}
	end = min(end, int64(len(pcm)))
	var sum float64
	var cnt int
	for j := start; j+1 < end; j += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(pcm[j : j+2])))
		sum += v * v
		cnt++
	}
	if cnt == 0 {
		return 0
	}
	return math.Sqrt(sum / float64(cnt))
}

// pollRMS repeats speakRMS until done(rms) holds or the attempts run out,
// returning the last RMS.
func pollRMS(s *StepCtx, speaker, listener *jsip.Call, wavPath string, done func(float64) bool) float64 {
	var rms float64
	for i := 1; i <= blockPollAttempts; i++ {
		rms = speakRMS(s, speaker, listener, wavPath)
		s.Logf("attempt %d: RMS %.0f", i, rms)
		if done(rms) {
			break
		}
	}
	return rms
}

func hasDTMF(c *jsip.Call, digit string) bool {
	return slices.ContainsFunc(c.ReceivedDTMF(), func(e jsip.DTMFEvent) bool { return e.Digit == digit })
}
