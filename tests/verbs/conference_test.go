// Tests for the `conference` verb.
//
// Schema: schemas/verbs/conference — required `name` (room name). Two
// callers joining the same named room are bridged. This test places two
// calls into a uniquely-named conference room, has one participant stream
// the reference WAV, and asserts the other participant recorded the same
// audio — proving jambonz's conference media bridge actually mixes our
// audio into the room.
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

// TestVerb_Conference_TwoParty — two legs join the same conference room.
// One streams the reference WAV; the other records mixed audio from the
// room; Deepgram verifies the audio traversed the mixer.
//
// Steps:
//  1. register-webhook-sessions — separate sessions for caller + listener
//  2. resolve-fixture — resolve testdata/test_audio.wav path
//  3. script-conference-both-legs — [conference name=jambonz-it-<id>] on both
//  4. claim-listener-channel — reserve callee-uas inbound channel
//  5. place-listener-call — POST /Calls to=callee-uas so listener joins first
//  6. spawn-listener-goroutine — async: answer, record, wait for end
//  7. wait-listener-settles — 2s so listener is in room before speaker joins
//  8. place-speaker-call — POST /Calls to=caller-uas; answer, stream WAV, hangup
//  9. stop-listener-call — DELETE /Calls/<listenerSID> to end conference
// 10. wait-listener-done — wait for listener goroutine to finish recording
// 11. assert-conference-audio-transcript — Deepgram transcript has "sun" + "shining"
//
// Test     --POST /Calls to=caller-uas-->            Jambonz (caller leg, speaker)
// Test     --POST /Calls to=callee-uas-->           Jambonz (callee leg, listener)
// Jambonz  --GET /hook-->                         Webhook  (both legs resolve the same hook)
// Webhook  --[conference name=jambonz-it-<id>]--> Jambonz (each leg joins the same room)
// Jambonz  --INVITE-->                            UAS, UAS2   (both answered)
// UAS      ==test_audio.wav==>                    Jambonz ==> (mixed into room) ==> UAS2
// UAS2     records PCM16 from conference bridge
//                                                             // Deepgram:
//                                                             //   transcript has
//                                                             //   "sun" + "shining"
func TestVerb_Conference_TwoParty(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	speakerUAS, listenerUAS := claimUAS2(t, ctx)

	s := Step(t, "register-webhook-sessions")
	// Unique room name per run — jambonz rooms are cluster-global, so a
	// stale room from a previous test could otherwise collide.
	room := fmt.Sprintf("jambonz-it-%d", time.Now().UnixNano())
	callerID := t.Name() + "-caller"
	listenerID := t.Name() + "-listener"
	callerSess := webhookReg.New(callerID)
	listenerSess := webhookReg.New(listenerID)
	t.Cleanup(func() {
		webhookReg.Release(callerID)
		webhookReg.Release(listenerID)
	})
	s.Done()

	s = Step(t, "resolve-fixture")
	wavPath := resolveFixture(t, speechWAV)
	s.Done()

	s = Step(t, "script-conference-both-legs")
	script := webhook.Script{V("conference", "name", room)}
	callerSess.ScriptCallHook(WithWarmupScript(script))
	listenerSess.ScriptCallHook(WithWarmupScript(script))
	s.Done()

	s = Step(t, "place-listener-call")
	// Place the listener's call first so it joins the room before the
	// speaker starts streaming. Uses the listener UAS (its own dynamic
	// /Clients user); inbound INVITE is delivered on listenerUAS.Inbound.
	listenerSID := placeWebhookCallToNoWait(ctx, t, listenerUAS, listenerSess)
	s.Logf("listener call sid=%s", listenerSID)
	s.Done()

	s = Step(t, "spawn-listener-goroutine")
	type listenerResult struct {
		wavPath string
		err     error
	}
	listenerResultCh := make(chan listenerResult, 1)
	// answeredCh fires the moment the listener leg's 200 OK has gone out
	// (i.e. it's in the conference room and ready to mix audio). Lets the
	// "wait-listener-settles" step block on that signal instead of a
	// fixed 2s sleep.
	answeredCh := make(chan struct{}, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var c *jsip.Call
		select {
		case c = <-listenerUAS.Inbound:
		case <-ctx.Done():
			listenerResultCh <- listenerResult{err: ctx.Err()}
			return
		}
		ls := Step(t, "listener:answer-record-and-wait-end")
		if err := c.Answer(); err != nil {
			ls.Errorf("Answer: %v", err)
			listenerResultCh <- listenerResult{err: err}
			return
		}
		// Signal "listener is in the conference" — at this point jambonz
		// has accepted the leg into the room. The bridge needs a small
		// settle to wire mixer → caller path; we add 300ms of pad in the
		// caller's wait step (still ~1.7s faster than the old 2s sleep).
		select {
		case answeredCh <- struct{}{}:
		default:
		}
		wav := filepath.Join(t.TempDir(), "conference-listener.pcm")
		if err := c.StartRecording(wav); err != nil {
			ls.Errorf("StartRecording: %v", err)
			listenerResultCh <- listenerResult{err: err}
			return
		}
		if err := c.SendSilence(); err != nil {
			ls.Errorf("SendSilence: %v", err)
		}
		_ = c.WaitState(ctx, jsip.StateEnded)
		ls.Done()
		listenerResultCh <- listenerResult{wavPath: wav}
	}()
	s.Done()

	s = Step(t, "wait-listener-settles")
	// Block on the listener's Answered signal instead of a 2s wall
	// timer. The bridge needs ~300ms after the 200 OK to wire mixer →
	// caller-leg media path; without that pad the speaker's WAV prefix
	// can land before mixing is live.
	select {
	case <-answeredCh:
	case <-ctx.Done():
		s.Fatalf("listener never answered: %v", ctx.Err())
	}
	time.Sleep(300 * time.Millisecond)
	s.Done()

	s = Step(t, "place-speaker-call")
	// Speaker leg — its own dynamic UAS.
	speaker := placeWebhookCallTo(ctx, t, speakerUAS, callerSess, withTimeLimit(60))
	if err := speaker.Answer(); err != nil {
		s.Fatalf("speaker Answer: %v", err)
	}
	if err := speaker.SendSilence(); err != nil {
		s.Fatalf("speaker SendSilence: %v", err)
	}
	// Let both legs fully bridge before speech; same pattern as dial/gather_speech.
	time.Sleep(RecognizerArmDelay)
	if err := speaker.SendWAV(wavPath); err != nil {
		s.Fatalf("speaker SendWAV: %v", err)
	}
	if err := speaker.SendSilence(); err != nil {
		s.Fatalf("speaker post-SendSilence: %v", err)
	}
	_ = speaker.Hangup()
	s.Done()

	s = Step(t, "stop-listener-call")
	// Tell jambonz to hang up the listener so the conference ends and we
	// can finalize the recording.
	if err := client.DeleteCall(ctx, listenerSID); err != nil {
		s.Logf("delete listener call: %v (may have ended already)", err)
	}
	s.Done()

	s = Step(t, "wait-listener-done")
	wg.Wait()
	res := <-listenerResultCh
	if res.err != nil {
		s.Fatalf("listener: %v", res.err)
	}
	s.Logf("listener recording: %s", res.wavPath)
	s.Done()

	s = Step(t, "assert-conference-audio-transcript")
	AssertTranscriptHasMost(s, ctx, res.wavPath, 1, "sun", "shining")
	s.Done()
}

