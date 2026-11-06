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

// TestVerb_Say_Groq_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Groq_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireGroq(t) {
		return
	}
	synth := groqSynth(cfg.GroqVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runGroqScript(t, "say-groq-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Groq_Options — speed, sample_rate and model_id reach the body
// (an invalid field would 400: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Groq_Options(t *testing.T) {
	vendorExtra(t)
	if !requireGroq(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-groq-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", groqSynth(groqMaleVoice, "en-US", map[string]any{
				"speed":       0.9,
				"sample_rate": 24000,
				"model_id":    "canopylabs/orpheus-v1-english",
			})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Groq_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Groq_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireGroq(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-groq-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", groqSynth(cfg.GroqVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.groq.com/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Groq_LongSentence — one sentence over Orpheus's 200-char input
// cap is split into two POSTs; both halves must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Groq_LongSentence(t *testing.T) {
	vendorExtra(t)
	if !requireGroq(t) {
		return
	}
	runGroqScript(t, "say-groq-long", 6*time.Second, 30*time.Second,
		[]string{"library", "parking"},
		V("say", "text", "Thank you for calling the city library, where you can borrow books, "+
			"reserve a study room, join a reading group for children or adults, and ask our staff "+
			"about the free parking behind the building on weekdays and weekends alike.",
			"synthesizer", groqSynth(cfg.GroqVoiceID, "en-US", nil)))
}

// TestVerb_Say_Groq_Arabic — ar-SA picks orpheus-arabic-saudi. Duration is
// the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Groq_Arabic(t *testing.T) {
	vendorExtra(t)
	if !requireGroq(t) {
		return
	}
	runGroqScript(t, "say-groq-ar", 1*time.Second, 12*time.Second, nil,
		V("say", "text", groqArabicText,
			"synthesizer", groqSynth(groqArabicVoice, "ar-SA", nil)))
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
