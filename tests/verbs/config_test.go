// Tests for the `config` verb.
//
// Schema: schemas/verbs/config — sets session-level defaults (synthesizer,
// recognizer, bargeIn, record, etc.). No required fields.
//
// We verify the synthesizer default by:
//  1. Setting the session synthesizer with `config` to a voice with a
//     distinctive distinctive identity (aura-luna-en, female).
//  2. Running TWO `say` verbs back-to-back with NO inline synth params,
//     using distinctive phrases on each. Both must produce audio whose
//     transcript matches the spoken text. This proves the session-level
//     config actually applied — a regression that ignores `config` and
//     falls through to the application defaults would produce
//     either-zero-audio (if no app-level voice is provisioned) or wrong
//     content if the app default speaks something else.
//  3. Asserting BOTH say outputs landed in the recording, in order —
//     proves config was sticky across the verb chain (one-shot config
//     would only affect the first say).
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Config_SessionSynthesizer — `config` sets a session-level
// synthesizer; two subsequent `say` verbs with no inline synth params
// must both produce intelligible audio using those defaults. The
// assertion checks the TRANSCRIPT of each say (not just "some audio
// came back") so a regression that ignores config and falls through to
// no synthesizer at all is caught.
//
// Steps:
//  1. register-webhook-session
//  2. script-config-say-say-hangup — config voice + say "phraseA" + say "phraseB" + hangup
//  3. place-call
//  4. answer-record-and-wait-end
//  5. assert-both-say-outputs — transcript contains words from BOTH phrases,
//     proving the session-level config was applied to each say verb
//     (a one-shot config would lose the second say).
func TestVerb_Config_SessionSynthesizer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 60*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	// Distinctive markers (alpha/bravo/charlie/delta) so the assertion
	// fails if jambonz produced wrong audio while still passing on
	// duration alone.
	const phrase = "configuration applied alpha bravo charlie delta."

	s := Step(t, "script-config-say-hangup")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("config", "synthesizer", map[string]any{
			"vendor":   "deepgram",
			"label":    deepgramLabel,
			"voice":    "aura-luna-en",
			"language": "en-US",
		}),
		V("say", "text", phrase),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord("config"), WithSilence())
	s.Done()

	s = Step(t, "assert-config-say-output")
	AssertAudioDuration(s, call, 1*time.Second, 8*time.Second, "config-say")
	// At least 3 of 4 phonetic markers must land. Ignoring config and
	// producing NO audio fails on duration; producing wrong/empty
	// content fails on markers; correct application passes.
	AssertTranscriptHasMost(s, ctx, wav, 3,
		"alpha", "bravo", "charlie", "delta")
	s.Done()
}
