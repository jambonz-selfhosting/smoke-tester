// Vakyam TTS: HTTP only, so mediajam POSTs each sentence to /v1/tts/stream
// (raw pcm 24 kHz) and serves one-shot say the same way. TTS only.
// Never t.Parallel: the free plan allows one concurrent request.
// Every test passes without exercising vakyam when VAKYAM_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	vakyamMaleVoice = "Vikram"
	// "Hello, how are you today?" in Hindi.
	vakyamHindiText = "नमस्ते, आज आप कैसे हैं?"
)

// requireVakyam logs and reports false when the key is unset; callers return.
func requireVakyam(t *testing.T) bool {
	t.Helper()
	if !cfg.HasVakyam() || vakyamLabel == "" {
		t.Log("VAKYAM_API_KEY not set — passing without exercising vakyam")
		return false
	}
	return true
}

func vakyamSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "vakyam",
		"label":    vakyamLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Vakyam — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Vakyam(t *testing.T) {
	if !requireVakyam(t) {
		return
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, vakyamSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Vakyam — one-shot say (en-US sent as language en-IN).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Vakyam(t *testing.T) {
	if !requireVakyam(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-vakyam",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", vakyamSynth(cfg.VakyamVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Vakyam_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Vakyam_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireVakyam(t) {
		return
	}
	synth := vakyamSynth(cfg.VakyamVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runVakyamScript(t, "say-vakyam-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Vakyam_Options — speed, explicit language and model_id reach
// the body (an invalid field would 422: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Vakyam_Options(t *testing.T) {
	vendorExtra(t)
	if !requireVakyam(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-vakyam-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", vakyamSynth(vakyamMaleVoice, "en-US", map[string]any{
				"speed":    0.9,
				"language": "en-IN",
				"model_id": "raaga-v1",
			})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Vakyam_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Vakyam_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireVakyam(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-vakyam-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", vakyamSynth(cfg.VakyamVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.vakyam.ai/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Vakyam_Hindi — hi-IN passes through as the language. Duration
// is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Vakyam_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireVakyam(t) {
		return
	}
	runVakyamScript(t, "say-vakyam-hi", 1*time.Second, 12*time.Second, nil,
		V("say", "text", vakyamHindiText,
			"synthesizer", vakyamSynth(cfg.VakyamVoiceID, "hi-IN", nil)))
}

// runVakyamScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runVakyamScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
