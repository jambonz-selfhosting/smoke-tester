// Phonic TTS: HTTP only (its websocket is speech-to-speech), so mediajam POSTs
// each sentence to /v1/tts/stream (raw pcm_24000) and serves one-shot say.
// Every test passes without exercising phonic when PHONIC_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	phonicGermanVoice = "karoline"
	// "Good morning, your appointment is confirmed for tomorrow."
	phonicGermanText = "Guten Morgen, Ihr Termin ist für morgen bestätigt."
)

// requirePhonic logs and reports false when the key is unset; callers return.
func requirePhonic(t *testing.T) bool {
	t.Helper()
	if !cfg.HasPhonic() || phonicLabel == "" {
		t.Log("PHONIC_API_KEY not set — passing without exercising phonic")
		return false
	}
	return true
}

func phonicSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "phonic",
		"label":    phonicLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Phonic — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Phonic(t *testing.T) {
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, phonicSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Phonic — one-shot say (en-US sent as languages [en]).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Phonic(t *testing.T) {
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-phonic",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", phonicSynth(cfg.PhonicVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Phonic_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Phonic_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	synth := phonicSynth(cfg.PhonicVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runPhonicScript(t, "say-phonic-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Phonic_Speed — speed and model_id reach the body (an invalid
// value would 400: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Phonic_Speed(t *testing.T) {
	vendorExtra(t)
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-phonic-speed",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", phonicSynth(cfg.PhonicVoiceID, "en-US", map[string]any{"speed": 1.0, "model_id": "merritt"})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Phonic_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Phonic_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-phonic-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", phonicSynth(cfg.PhonicVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.phonic.ai/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Phonic_German — de-DE is sent as languages [de]. Duration is
// the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Phonic_German(t *testing.T) {
	vendorExtra(t)
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runPhonicScript(t, "say-phonic-de", 1*time.Second, 12*time.Second, nil,
		V("say", "text", phonicGermanText,
			"synthesizer", phonicSynth(phonicGermanVoice, "de-DE", nil)))
}

// TestVerb_Say_Phonic_UnknownLanguage — a language Phonic lacks is left out,
// so the voice speaks English.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Phonic_UnknownLanguage(t *testing.T) {
	vendorExtra(t)
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-phonic-xx",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your package arrives on Friday afternoon.",
			"synthesizer", phonicSynth(cfg.PhonicVoiceID, "yo-NG", nil)),
		wantWords: []string{"friday", "afternoon"},
	})
}

// runPhonicScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runPhonicScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
