//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
