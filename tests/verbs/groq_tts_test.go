// Groq TTS (Orpheus): HTTP only, so mediajam POSTs each sentence to
// /openai/v1/audio/speech (wav, header stripped) and serves one-shot say the
// same way. TTS only; every test passes without exercising groq when
// GROQ_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	groqMaleVoice   = "troy"
	groqArabicVoice = "fahad"
	// "Hello, how are you today?" in Arabic.
	groqArabicText = "مرحبا، كيف حالك اليوم؟"
)

// requireGroq logs and reports false when the key is unset; callers return.
func requireGroq(t *testing.T) bool {
	t.Helper()
	if !cfg.HasGroq() || groqLabel == "" {
		t.Log("GROQ_API_KEY not set — passing without exercising groq")
		return false
	}
	return true
}

func groqSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "groq",
		"label":    groqLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Groq — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Groq(t *testing.T) {
	if !requireGroq(t) {
		return
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, groqSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Groq — one-shot say (en-US picks orpheus-v1-english).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Groq(t *testing.T) {
	if !requireGroq(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-groq",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", groqSynth(cfg.GroqVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// runGroqScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runGroqScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
