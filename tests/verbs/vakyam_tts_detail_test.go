//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
