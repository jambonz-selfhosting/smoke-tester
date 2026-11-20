//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Speechify_CachedReplay — the same prompt twice on one call:
// the second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechify_CachedReplay(t *testing.T) {
	vendorExtra(t)
	requireSpeechify(t)
	t.Parallel()
	synth := speechifySynth(speechifyVoice, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSpeechifyScript(t, "say-speechify-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Speechify_ModelOverride — a verb-level model_id (simba-3.0 on
// an English voice) overrides the language-picked default.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechify_ModelOverride(t *testing.T) {
	vendorExtra(t)
	requireSpeechify(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechify-simba30",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "The multilingual model is speaking now.",
			"synthesizer", speechifySynth(speechifyVoice, "en-US", map[string]any{"model_id": "simba-3.0"})),
		wantWords: []string{"model", "speaking"},
	})
}

// TestVerb_Say_Speechify_Options — loudness and text normalization reach the
// API (an unknown or malformed option would fail the request: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechify_Options(t *testing.T) {
	vendorExtra(t)
	requireSpeechify(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechify-options",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "Your order number is 42 and it ships today.",
			"synthesizer", speechifySynth(speechifyVoice, "en-US", map[string]any{
				"loudness_normalization": true, "text_normalization": true,
			})),
		wantWords: []string{"order number", "ships today"},
	})
}

// TestVerb_Say_Speechify_German — the English voice speaks German text: de-DE
// gets simba-3.0, the English-only simba-3.2 would reject it. Duration is the
// check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Speechify_German(t *testing.T) {
	vendorExtra(t)
	requireSpeechify(t)
	t.Parallel()
	runSpeechifyScript(t, "say-speechify-de", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", "Guten Tag, dieser Sprachtest funktioniert einwandfrei.",
			"synthesizer", speechifySynth(speechifyVoice, "de-DE", nil)))
}
