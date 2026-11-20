//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Speechmatics_CachedReplay — the same prompt twice on one call:
// the second say plays the r16 cache file the first one wrote.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechmatics_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	synth := speechmaticsTtsSynth(speechmaticsTtsVoice, "en-GB", nil)
	text := "Your table for four is confirmed for tonight."
	runSpeechmaticsTtsScript(t, "say-speechmatics-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Speechmatics_USVoice — the en-US voice megan.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechmatics_USVoice(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechmatics-megan",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", speechmaticsTtsSynth("megan", "en-US", nil)),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Speechmatics_Options — api_uri with a scheme and trailing
// slash still resolves to the default host, and sm_app reaches the query.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechmatics_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechmatics-options",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "The library opens at nine tomorrow.",
			"synthesizer", speechmaticsTtsSynth(speechmaticsTtsVoice, "en-GB", map[string]any{
				"api_uri": "https://preview.tts.speechmatics.com/",
				"sm_app":  "jambonz-smoke",
			})),
		wantWords: []string{"library", "tomorrow"},
	})
}

// TestVerb_Say_Speechmatics_LanguageIgnored — language is never sent, so a
// non-English jambonz language must not break the call.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechmatics_LanguageIgnored(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSpeechmaticsTtsScript(t, "say-speechmatics-lang", 1*time.Second, 12*time.Second,
		[]string{"garden", "weather"},
		V("say", "text", "The garden looks lovely in this weather.",
			"synthesizer", speechmaticsTtsSynth(speechmaticsTtsVoice, "de-DE", nil)))
}
