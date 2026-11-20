//go:build detail

package verbs

import (
	"strings"
	"testing"
	"time"
)

// TestVerb_Say_Kokoro_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Kokoro_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	synth := kokoroSynth(cfg.KokoroVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runKokoroScript(t, "say-kokoro-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Kokoro_AllOptions — lang_code, speed, volume_multiplier and
// normalization_options reach the body (a bad type would 422: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", kokoroSynth(cfg.KokoroVoiceID, "en-US", kokoroAllOptions())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Kokoro_ModelAndVoiceMix — the OpenAI alias model tts-1 and a
// mixed voice are accepted as given.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_ModelAndVoiceMix(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-mix",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", kokoroSynth("af_bella+af_sky", "en-US",
				map[string]any{"model_id": "tts-1"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Kokoro_ApiUri — a verb-level api_uri with a trailing /v1/
// still resolves to the same server.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "The library opens again on Thursday.",
			"synthesizer", kokoroSynth(cfg.KokoroVoiceID, "en-US",
				map[string]any{"api_uri": strings.TrimRight(cfg.KokoroAPIURI, "/") + "/v1/"})),
		wantWords: []string{"library", "thursday"},
	})
}

// TestVerb_Say_Kokoro_Spanish — es-ES with a Spanish voice; lang_code comes
// from the voice prefix. Duration is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Kokoro_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runKokoroScript(t, "say-kokoro-es", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Buenos días, ¿cómo está usted hoy?",
			"synthesizer", kokoroSynth(kokoroSpanishVoice, "es-ES", nil)))
}
