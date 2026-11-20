//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Spitch_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Spitch_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	synth := spitchSynth(cfg.SpitchVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSpitchScript(t, "say-spitch-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Spitch_Speed — speed reaches the body as a number (an invalid
// one would 422: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Spitch_Speed(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-spitch-speed",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", spitchSynth(cfg.SpitchVoiceID, "en-US", map[string]any{"speed": 0.9})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Spitch_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Spitch_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-spitch-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", spitchSynth(cfg.SpitchVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.spitch.app/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Spitch_Yoruba — yo-NG is sent as language yo. Duration is the
// check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Spitch_Yoruba(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSpitchScript(t, "say-spitch-yo", 1*time.Second, 12*time.Second, nil,
		V("say", "text", spitchYorubaText,
			"synthesizer", spitchSynth(spitchYorubaVoice, "yo-NG", nil)))
}

// TestVerb_Say_Spitch_Pidgin — pcm-NG has no documented code, so language is
// omitted and the voice still speaks.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Spitch_Pidgin(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSpitchScript(t, "say-spitch-pcm", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "How you dey? Everything go better today.",
			"synthesizer", spitchSynth(spitchPidginVoice, "pcm-NG", nil)))
}
