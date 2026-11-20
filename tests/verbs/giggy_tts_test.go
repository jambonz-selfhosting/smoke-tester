// Giggy TTS. Giggy has no input-streaming socket, so mediajam POSTs each
// sentence to /v1/text-to-speech in streaming mode (raw 24 kHz PCM). Not an
// alignment vendor. All tests skip without GIGGY_API_KEY.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func requireGiggy(t *testing.T) {
	t.Helper()
	if !cfg.HasGiggy() || giggyLabel == "" {
		t.Skip("Giggy tests need GIGGY_API_KEY (credential missing — skipping, not a failure)")
	}
}

// giggySynth targets the configured voice; a Giggy voice fixes its language.
func giggySynth(options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "giggy",
		"label":    giggyLabel,
		"voice":    cfg.GiggyVoiceID,
		"language": "en-US",
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Giggy — the api-server's credential test
// passes for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Giggy(t *testing.T) {
	requireGiggy(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, giggySID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Giggy — one-shot say.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Giggy(t *testing.T) {
	requireGiggy(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-giggy",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", giggySynth(nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// runGiggyScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in order.
func runGiggyScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
	t.Helper()
	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-say")
	script := webhook.Script{}
	for _, v := range says {
		script = append(script, v)
	}
	sess.ScriptCallHook(WithWarmupScript(append(script, V("hangup"))))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, minDur, maxDur, tag)
	s.Done()

	if wav != "" && len(wantOrdered) > 0 {
		s = Step(t, "assert-transcript-ordered")
		AssertTranscriptContainsInOrder(s, ctx, wav, wantOrdered...)
		s.Done()
	}
}
