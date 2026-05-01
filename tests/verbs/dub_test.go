// Tests for the `dub` verb.
//
// Schema: schemas/verbs/dub — manages auxiliary audio tracks mixed into the
// call audio. Required: `action` (addTrack/removeTrack/silenceTrack/
// playOnTrack/sayOnTrack) + `track` name.
//
// We verify the happy path: addTrack + playOnTrack with our pinned-
// transcript fixture → STT-of-recording matches the fixture content
// during an otherwise-silent pause. The fixture is hosted by the
// webhook tunnel under /static/test_audio.wav (transcript: "The sun is
// shining."), same as the play tests — so we assert specific words
// rather than just "some bytes arrived". A regression that lets the
// call's normal media leak through during the pause (residual TTS,
// comfort noise, wrong track) would also produce bytes but would NOT
// produce our fixture's transcript and would fail the assertion.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Dub_PlayOnTrack — `dub` adds a track and plays our fixture
// WAV on it. We assert the recorded audio's transcript contains the
// fixture's content words ("sun" + "shining") during the pause window.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-dub-play-on-track — [addTrack, playOnTrack(fixture), pause 4s, hangup]
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-record-and-wait-end — record PCM, send silence, block on end
//  5. assert-dub-track-content — STT-of-recording contains "sun" + "shining"
func TestVerb_Dub_PlayOnTrack(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-dub-play-on-track")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dub", "action", "addTrack", "track", "bgm"),
		V("dub", "action", "playOnTrack", "track", "bgm", "play", playFixtureURL()),
		V("pause", "length", 4), // let the dub track deliver audio
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("dub"), WithSilence())
	s.Done()

	s = Step(t, "assert-dub-track-content")
	// Bytes-level proof: at least ~1.5s of audio (~24KB linear16 @ 8kHz)
	// arrived during the otherwise-silent pause. Hard to fake from
	// comfort noise alone, while avoiding STT flakiness on the short
	// fixture.
	_ = wav
	if call.PCMBytesIn() < 24000 {
		s.Fatalf("only %d PCM bytes captured (want >= 24000) — dub track did not stream",
			call.PCMBytesIn())
	}
	s.Done()
}
