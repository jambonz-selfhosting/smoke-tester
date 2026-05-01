// Tests for `enqueue` + `dequeue` + `leave` — the queue trio.
//
// Schema: schemas/verbs/enqueue  — required `name`. Caller sits in the
//                                  named queue until dequeued.
//         schemas/verbs/dequeue  — required `name`. Pulls the next caller
//                                  off the queue and bridges to the current
//                                  call. `actionHook` fires when the
//                                  bridged call ends.
//         schemas/verbs/leave    — no required fields. Exits the caller's
//                                  current enqueue/conference.
//
// Test shape mirrors dial: one side streams the reference WAV, the other
// records from the bridged call, Deepgram verifies content made it
// through. Difference from dial: the match-making is queue-based, not
// target-based.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Enqueue_Dequeue_Bridge — enqueuer waits in a queue, agent
// dequeues, they bridge. Same audio-passthrough proof as dial, but via
// queue match-making.
//
// Steps:
//  1. register-webhook-sessions — separate sessions for enqueuer + agent
//  2. resolve-fixture — resolve testdata/test_audio.wav path
//  3. script-enqueue-and-dequeue — enqueuer=[enqueue Q], agent=[dequeue Q]
//  4. claim-agent-channel — reserve callee-uas inbound channel
//  5. place-enqueuer — POST /Calls; answer, SendSilence so it idles in queue
//  6. wait-enqueuer-settles — 1s so jambonz matures the queue state
//  7. place-agent-call — POST /Calls to=callee-uas; dequeue matches enqueuer
//  8. spawn-agent-goroutine — async: answer, record, wait for end
//  9. wait-bridge-settles — 4s so jambonz's queue bridge fully forms
// 10. enqueuer-sends-wav — stream testdata/test_audio.wav from caller leg
// 11. post-speech-silence-and-hangup — trailing silence, enqueuer hangs up
// 12. stop-agent-call — DELETE /Calls/<agentSID> to flush recording
// 13. wait-agent-done — wait for agent goroutine to finish recording
// 14. assert-bridge-audio-transcript — Deepgram transcript has "sun" + "shining"
//
// Test     --POST /Calls to=caller-uas (enqueuer)-->  Jambonz
// Test     --POST /Calls to=callee-uas (agent)-->    Jambonz
// Webhook  --[enqueue name=Q]-->                   Jambonz (enqueuer waits)
// Webhook  --[dequeue name=Q]-->                   Jambonz (agent dequeues)
// Jambonz  --bridges both legs (same as dial)-->
// UAS (enqueuer) ==test_audio.wav==>               Jambonz ==> UAS2 (agent)
// UAS2 records PCM16 from queue bridge
//                                                   // Deepgram: sun + shining
func TestVerb_Enqueue_Dequeue_Bridge(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	enqueuerUAS, agentUAS := claimUAS2(t, ctx)

	s := Step(t, "register-webhook-sessions")
	queue := fmt.Sprintf("jambonz-it-q-%d", time.Now().UnixNano())
	enqueuerID := t.Name() + "-enqueuer"
	agentID := t.Name() + "-agent"
	enqueuerSess := webhookReg.New(enqueuerID)
	agentSess := webhookReg.New(agentID)
	t.Cleanup(func() {
		webhookReg.Release(enqueuerID)
		webhookReg.Release(agentID)
	})
	s.Done()

	s = Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-enqueue-and-dequeue")
	enqueuerSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("enqueue", "name", queue),
	}))
	agentSess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dequeue", "name", queue, "timeout", 30),
	}))
	s.Done()

	s = Step(t, "claim-agent-channel")
	// agentUAS.Inbound is already a per-test channel; nothing to claim.
	s.Done()

	s = Step(t, "place-enqueuer")
	// Start the enqueuer (caller leg) first so it's waiting in the queue
	// before the agent dequeues. Otherwise dequeue's timeout kicks in on
	// an empty queue and the agent hangs up without bridging.
	enqueuer := placeWebhookCallTo(ctx, t, enqueuerUAS, enqueuerSess, withTimeLimit(90))
	if err := enqueuer.Answer(); err != nil {
		s.Fatalf("enqueuer Answer: %v", err)
	}
	if err := enqueuer.SendSilence(); err != nil {
		s.Fatalf("enqueuer SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "place-agent-call")
	// Drop the old "wait-enqueuer-settles" 1s pad — the enqueuer's
	// Answer() above already returned synchronously after the 200 OK.
	// jambonz's queue accepts the row before we POST /Calls for the
	// agent.
	agentSID := placeWebhookCallToNoWait(ctx, t, agentUAS, agentSess)
	s.Logf("agent call sid=%s", agentSID)
	s.Done()

	s = Step(t, "spawn-agent-goroutine")
	type agentResult struct {
		wav string
		err error
	}
	agentResultCh := make(chan agentResult, 1)
	// answeredCh fires the moment the agent leg's 200 OK has gone out
	// (i.e. the queue has dequeued and bridged us). Lets the bridge-
	// settle step block on that event instead of a fixed 4s sleep.
	agentAnsweredCh := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var c *jsip.Call
		select {
		case c = <-agentUAS.Inbound:
		case <-ctx.Done():
			agentResultCh <- agentResult{err: ctx.Err()}
			return
		}
		as := Step(t, "agent:answer-record-and-wait-end")
		if err := c.Answer(); err != nil {
			as.Errorf("Answer: %v", err)
			agentResultCh <- agentResult{err: err}
			return
		}
		select {
		case agentAnsweredCh <- struct{}{}:
		default:
		}
		wav := filepath.Join(t.TempDir(), "enqueue-agent.pcm")
		if err := c.StartRecording(wav); err != nil {
			as.Errorf("StartRecording: %v", err)
			agentResultCh <- agentResult{err: err}
			return
		}
		if err := c.SendSilence(); err != nil {
			as.Errorf("SendSilence: %v", err)
		}
		_ = c.WaitState(ctx, jsip.StateEnded)
		as.Done()
		agentResultCh <- agentResult{wav: wav}
	}()
	s.Done()

	s = Step(t, "wait-bridge-settles")
	// Block on the agent leg's Answered signal. Bridge needs ~500ms
	// after the 200 OK to wire the cross-leg media path; 500ms is the
	// smallest pad that's been stable across runs (vs the old 4s blind
	// sleep).
	select {
	case <-agentAnsweredCh:
	case <-ctx.Done():
		s.Fatalf("agent never answered (queue match-up failed): %v", ctx.Err())
	}
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "enqueuer-sends-wav")
	if err := enqueuer.SendWAV(wavPath); err != nil {
		s.Fatalf("enqueuer SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence-and-hangup")
	if err := enqueuer.SendSilence(); err != nil {
		s.Fatalf("enqueuer post-SendSilence: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	_ = enqueuer.Hangup()
	s.Done()

	s = Step(t, "stop-agent-call")
	if err := client.DeleteCall(ctx, agentSID); err != nil {
		s.Logf("delete agent call: %v (may have ended already)", err)
	}
	s.Done()

	s = Step(t, "wait-agent-done")
	wg.Wait()
	res := <-agentResultCh
	if res.err != nil {
		s.Fatalf("agent: %v", res.err)
	}
	s.Logf("agent recording: %s", res.wav)
	s.Done()

	s = Step(t, "assert-bridge-audio-transcript")
	AssertTranscriptHasMost(s, ctx, res.wav, 1, "sun", "shining")
	s.Done()
}

// TestVerb_Leave_FromWaitHook — `leave` inside a waitHook pops the caller
// back to the main script; asserted by detecting the post-enqueue say's
// phrase in the recording.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-enqueue-waithook-leave — call_hook=[enqueue Q waitHook, say, hangup],
//     action/wait=[leave]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-record-and-wait-end — record PCM, send silence, block on end
//  5. assert-transcript-after-leave — Deepgram transcript contains "after leave"
//
// Test     --POST /Calls-->           Jambonz
// Webhook  --[enqueue Q, say]-->      Jambonz
// (caller enters queue, `leave` in waitHook would pop them back to the
//  main script; we assert by round-tripping through an empty waitHook
//  verb list — jambonz interprets "no verbs" as "keep waiting". Rather
//  than implement a full leave-from-waithook scenario we test `leave`
//  with a simpler shape: enqueue, then immediately leave on waitHook,
//  continue to the next verb.)
//
// The `leave` verb's integration surface is minimal — it's a control
// statement that only makes sense inside a waitHook. We verify it runs
// without error and the caller ends up executing the post-enqueue verb
// (a `say`) — proving leave returned control to the main script.
func TestVerb_Leave_FromWaitHook(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	queue := fmt.Sprintf("jambonz-it-q-%d", time.Now().UnixNano())
	_, sess := claimSession(t)

	s := Step(t, "script-enqueue-waithook-leave")
	// Main script: enqueue (with a waitHook that calls leave), then say
	// something distinctive. If `leave` works, we get the post-enqueue
	// audio out.
	waitURL := SessionURL(sess, "wait")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("enqueue", "name", queue, "waitHook", waitURL),
		V("say", "text", "After leave."),
		V("hangup"),
	}))
	// waitHook fires immediately on enqueue; returning `leave` pops the
	// caller back to the main script.
	sess.ScriptActionHook("wait", webhook.Script{V("leave")})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call,
		WithRecord("leave"), WithSilence())
	s.Done()

	s = Step(t, "assert-transcript-after-leave")
	AssertTranscriptContains(s, ctx, wav, "after leave")
	s.Done()
}
