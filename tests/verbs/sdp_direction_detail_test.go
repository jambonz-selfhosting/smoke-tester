//go:build detail

package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestSDP_Sendonly_ReinviteLoop — the second half of the Five9 sequence: when
// the answer is not the complement, Five9 re-INVITEs with a=sendonly to force
// it. That renegotiation must be answered 200 with the complement and must
// not tear the dialog down. Performed regardless of what the initial answer
// said, so the test stays meaningful on both fixed and buggy builds.
//
// Steps:
//   - script-answer-pause-hangup
//   - invite-sendonly-expect-200
//   - corrective-reinvite-sendonly
//   - assert-dialog-survived
func TestSDP_Sendonly_ReinviteLoop(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-answer-pause-hangup")
	appSID := sdpDirectionApp(t, ctx, sess, 20)
	s.Done()

	s = Step(t, "invite-sendonly-expect-200")
	call := inviteAppMode(s, ctx, uas, appSID, testID, "sendonly")
	t.Cleanup(func() { _ = call.Hangup() })
	if answer, ok := call.AnsweredResponse(); ok && answer.RawResponse != nil {
		s.Logf("initial answer direction: %q (\"\" = implied sendrecv)",
			sdpDirection(answer.RawResponse.Body()))
	}
	s.Done()

	s = Step(t, "corrective-reinvite-sendonly")
	res := reinviteDirection(s, ctx, call, "sendonly")
	requireComplementOfSendonly(s, "corrective re-INVITE", res.Body())
	s.Done()

	s = Step(t, "assert-dialog-survived")
	if state := call.State(); state != jsip.StateAnswered {
		s.Fatalf("dialog did not survive the a=sendonly re-INVITE: state=%v", state)
	}
	s.Done()
}

// TestSDP_OfferMode_EarlyAnswerDirectionChange — the OTHER side of the
// direction fix: the leg where jambonz is the OFFERER.
//
// Everything above drives the leg where jambonz answers our offer. When
// jambonz dials out (the `dial` verb's B leg) it offers, and the far end's
// SDP arrives as an ANSWER — possibly more than once, since a carrier may
// answer first in a 183 and again in the 200. A media server must never
// re-render its own offer as an answer on that path; doing so replaces the
// multi-codec offer with a single-codec body (and, once the direction
// differs between the two answers, flips its direction too), which corrupts
// the leg. mediajam's first cut of the direction fix had exactly that bug —
// it guarded only the FIRST answer.
//
// This drives the sequence over real SIP: our callee answers the dial leg
// twice with DIFFERENT directions — 183 carrying a=recvonly, then 200
// carrying sendrecv (diago's negotiated answer). The observable is
// end-to-end: the dial must complete and real audio must bridge caller <->
// callee afterwards, which it cannot do if the B-leg SDP was rewritten
// mid-sequence.
//
// Steps:
//   - resolve-fixture
//   - script-dial-to-callee
//   - spawn-callee-goroutine
//   - place-caller-and-record
//   - wait-callee-done
//   - wait-action-dial-callback
//   - assert-dial-status
//   - assert-bridge-audio
func TestSDP_OfferMode_EarlyAnswerDirectionChange(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	ctx := WithTimeout(t, 150*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-dial-to-callee")
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{"type": "user", "name": target}},
			"timeout", 30,
			"actionHook", SessionURL(sess, "dial"),
			// anchorMedia keeps both legs' RTP inside the cluster; without it
			// the two legs can negotiate a peer-to-peer SDP using each side's
			// private NAT address and no audio crosses (see dial_test.go).
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			// Answer #1: a 183 with SDP, deliberately a=recvonly — a
			// direction the eventual 200 will NOT carry.
			sdp, err := jsip.EarlyMediaSDPWithDirection("PCMU",
				earlyMediaSDPHost, earlyMediaSDPPort, 2000, "recvonly")
			if err != nil {
				GoroutineFailf(t, "callee:build-sdp", "EarlyMediaSDPWithDirection: %v", err)
				return
			}
			t.Logf("[callee:183] sending Session Progress with a=recvonly")
			if err := c.SendEarlyMedia183(sdp); err != nil {
				GoroutineFailf(t, "callee:183", "SendEarlyMedia183: %v", err)
				return
			}
			time.Sleep(2 * time.Second)
			// Answer #2: the real 200 OK, which diago answers sendrecv.
			t.Logf("[callee:answer] start (200 OK, sendrecv)")
			if err := c.Answer(); err != nil {
				t.Logf("[callee:answer] FAILED (leg may have been torn down): %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-prime", "SendSilence: %v", err)
				return
			}
			time.Sleep(RecognizerArmDelay)
			if err := c.SendWAV(wavPath); err != nil {
				GoroutineFailf(t, "callee:send-wav", "SendWAV: %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-trail", "SendSilence: %v", err)
				return
			}
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
			}
			<-c.Done()
		case <-calleeCtx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
		}
	}()
	t.Cleanup(func() {
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-caller-and-record")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("sdp-offermode-caller"), WithSilence())
	s.Done()

	s = Step(t, "wait-callee-done")
	<-calleeDone
	s.Done()

	s = Step(t, "wait-action-dial-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 30*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/dial")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/dial: %v", err)
	}
	s.Logf("action/dial body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-dial-status")
	dcs := cb.String("dial_call_status")
	if dcs != "completed" {
		s.Errorf("dial_call_status: got %q want \"completed\" (dial_sip_status=%d) — "+
			"the two-answers-different-direction sequence broke the outbound leg",
			dcs, cb.Int("dial_sip_status"))
	}
	s.Done()

	s = Step(t, "assert-bridge-audio")
	s.Logf("caller recorded pcm_bytes=%d rms=%.1f duration=%s",
		call.PCMBytesIn(), call.RMS(), call.AudioDuration())
	// If the offering side re-rendered its own offer as an answer partway
	// through, the B leg's media is wrong and the bridge carries silence.
	AssertTranscriptContains(s, ctx, wav, "sun", "shining")
	s.Done()
}
