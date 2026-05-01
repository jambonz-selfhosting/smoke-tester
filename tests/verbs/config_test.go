// Tests for the `config` verb.
//
// Schema: schemas/verbs/config — sets session-level defaults (synthesizer,
// recognizer, bargeIn, record, etc.). No required fields.
//
// We verify the synthesizer default by running `config` with a voice
// override, then `say` with *no* inline synthesizer params — the speech
// that comes back should be non-empty audio. This doesn't verify the voice
// identity from PCM alone (can't), but confirms the session-default path
// is wired (if it weren't, the say verb would fail and no audio arrive).
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Config_SessionSynthesizer — `config` sets a session-level
// synthesizer, and a subsequent `say` with no inline synth params should
// still produce audio using those defaults.
//
// Steps:
//  1. register-webhook-session — webhook.Registry.New + cleanup
//  2. script-config-say-hangup — register [config synth, say, hangup] as call_hook
//  3. place-call — POST /Calls (application_sid=webhookApp, tag.x_test_id)
//  4. answer-record-and-wait-end — record PCM, send silence, block on end
//  5. assert-audio-duration — say produced 1-6s of audio (session defaults worked)
//
// Test     --POST /Calls [app=webhookApp]-->    Jambonz
// Jambonz  --GET /hook-->                       Webhook
// Webhook  --[config synth=..., say, hangup]--> Jambonz
// Jambonz  --INVITE -> media -> BYE-->          UAS     // audio arrived
func TestVerb_Config_SessionSynthesizer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-config-say-hangup")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("config", "synthesizer", map[string]any{
			"vendor":   "deepgram",
			"label":    deepgramLabel,
			"voice":    "aura-luna-en",
			"language": "en-US",
		}),
		V("say", "text", "Speaking with the session default voice."),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("config"), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, 1*time.Second, 6*time.Second, "config-say-session-default")
	s.Done()
}
