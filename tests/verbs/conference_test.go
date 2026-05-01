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
	"sync"
	"testing"
	"time"

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
	speakerUAS := claimUAS(t, ctx)
	listenerUAS := claimUAS(t, ctx)

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
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		select {
		case c := <-listenerUAS.Inbound:
			// Inner step runs under a dedicated StepCtx so the goroutine's
			// failures also carry a step name.
			ls := Step(t, "listener:answer-record-and-wait-end")
			wav := AnswerRecordAndWaitEnded(ls, ctx, c,
				WithRecord("conference-listener"), WithSilence())
			ls.Done()
			listenerResultCh <- listenerResult{wavPath: wav}
		case <-ctx.Done():
			listenerResultCh <- listenerResult{err: ctx.Err()}
		}
	}()
	s.Done()

	s = Step(t, "wait-listener-settles")
	// Give the listener ~2s to settle into the room before the speaker
	// joins and starts streaming. Without this the speaker often ends up
	// in the room alone for the first ~500ms and the WAV's prefix gets
	// mixed into silence (verified empirically: 1s caused "the sun" to be
	// clipped, leaving only "is shining" in the recording).
	time.Sleep(2 * time.Second)
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
	time.Sleep(500 * time.Millisecond)
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
	AssertTranscriptContains(s, ctx, res.wavPath, "sun", "shining")
	s.Done()
}

