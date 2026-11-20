//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Simplismart_CachedReplay — the same prompt twice on one call:
// the second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Simplismart_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	synth := simplismartSynth(cfg.SimplismartVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSimplismartScript(t, "say-simplismart-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Simplismart_AllOptions — every sampling param reaches the
// body (a malformed one would fail the request: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Simplismart_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-simplismart-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", simplismartSynth(cfg.SimplismartVoiceID, "en-US", simplismartSampling())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Simplismart_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the shared host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Simplismart_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-simplismart-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", simplismartSynth(cfg.SimplismartVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.simplismart.live/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Simplismart_VeenaModel — a verb-level model_id (Veena) on
// English text overrides the language-picked Orpheus.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Simplismart_VeenaModel(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSimplismartScript(t, "say-simplismart-veena", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "The second model is speaking now.",
			"synthesizer", simplismartSynth(simplismartHindiVoice, "en-US",
				map[string]any{"model_id": simplismartVeenaModel})))
}

// TestVerb_Say_Simplismart_Hindi — hi-IN with no model picks Veena. Duration
// is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Simplismart_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSimplismartScript(t, "say-simplismart-hi", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", simplismartHindiText,
			"synthesizer", simplismartSynth(simplismartHindiVoice, "hi-IN", nil)))
}
