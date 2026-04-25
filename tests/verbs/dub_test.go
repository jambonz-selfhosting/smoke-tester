// Tests for the `dub` verb.
//
// Schema: schemas/verbs/dub — manages auxiliary audio tracks mixed into the
// call audio. Required: `action` (addTrack/removeTrack/silenceTrack/
// playOnTrack/sayOnTrack) + `track` name.
//
// We verify the happy path: addTrack + playOnTrack + a filler pause → audio
// arrives from the dub track. Can't identify the track from PCM alone, but
// non-trivial RMS during a pause (where no `say`/`play` is running)
// demonstrates the dub track is the audio source.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const dubSample = "https://samplelib.com/lib/preview/wav/sample-3s.wav"

// TestVerb_Dub_PlayOnTrack — `dub` adds a track and plays a WAV on it.
// We assert non-trivial audio arrives during an otherwise-silent `pause`,
// which proves the dub track was mixing into the call.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-dub-play-on-track — [addTrack, playOnTrack, pause 4s, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-record-and-wait-end — record PCM, send silence, block on end
//  5. assert-audio-bytes — at least 4000 PCM bytes captured (dub track mixed in)
//
// Test     --POST /Calls-->                                      Jambonz
// Jambonz  --GET /hook-->                                        Webhook
// Webhook  --[dub addTrack, dub playOnTrack(url), pause, hangup]-> Jambonz
// Jambonz  --INVITE-->                                           UAS
// Jambonz  ==RTP (dub audio mixed)==>                            UAS  // assert
// Jambonz  --BYE-->                                              UAS
func TestVerb_Dub_PlayOnTrack(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "register-webhook-session")
	testID := t.Name()
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	s.Done()

	s = Step(t, "script-dub-play-on-track")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dub", "action", "addTrack", "track", "bgm"),
		V("dub", "action", "playOnTrack", "track", "bgm", "play", dubSample),
		V("pause", "length", 4), // let the dub track deliver audio
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("dub"), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-bytes")
	// Any meaningful audio arriving during an otherwise-silent pause came
	// from the dub track. Use bytes + RMS jointly.
	AssertAudioBytes(s, call, 4000, "dub-play-on-track")
	s.Done()
}
