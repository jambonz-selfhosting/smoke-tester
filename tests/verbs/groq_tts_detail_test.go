//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
