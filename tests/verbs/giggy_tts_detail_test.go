//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Giggy_CachedReplay — the same prompt twice on one call; both
// must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Giggy_CachedReplay(t *testing.T) {
	vendorExtra(t)
	requireGiggy(t)
	t.Parallel()
	synth := giggySynth(nil)
	text := "Your table for four is confirmed for tonight."
	runGiggyScript(t, "say-giggy-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Giggy_Options — speed reaches the API: at 2.0 this text is
// 4.9s of speech (9.8s at 1.0, measured), so normal speed overruns maxDur.
// seed rides along but has no observable effect.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Giggy_Options(t *testing.T) {
	vendorExtra(t)
	requireGiggy(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-giggy-options",
		minDur:     4 * time.Second,
		maxDur:     9500 * time.Millisecond,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse, "+
			"and the courier will text you a delivery window tomorrow morning.",
			"synthesizer", giggySynth(map[string]any{"speed": 2.0, "seed": 42})),
		// STT drops some of the opening short sentence at 2x, so check the long one
		wantWords: []string{"warehouse", "delivery window"},
	})
}
