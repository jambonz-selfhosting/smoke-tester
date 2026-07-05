// Tests for the `say` verb with TTS vendor "xai".
//
// xai is an OPTIONAL vendor (see config.HasXai / provisionXaiCredential in
// verbsmain_test.go): TestMain only provisions the xai SpeechCredential
// when XAI_API_KEY is set, and that credential is dual-use (STT+TTS) — the
// tests below reuse the same xaiLabel the xai STT tests use. When the key is
// unset, xaiLabel stays "" and BOTH tests below pass immediately without
// exercising xai TTS — a plain `return` after a log, never t.Skip, never a
// failure, so the suite stays green with or without the key (mirrors
// xai_stt_test.go's guard).
//
// Clones of TestVerb_Say_Murf / TestVerb_Say_Stream_Murf (say_test.go) with
// the vendor swapped to xai.
package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Xai — speak through the xai TTS vendor via a synthesizer
// override, using the xai speech credential provisioned at TestMain (the
// same dual-use credential the xai STT tests use). xai is an optional
// vendor: when XAI_API_KEY is not set no credential is provisioned, and
// this test passes without exercising xai TTS (plain return, not t.Skip).
//
// Transcript verifies the override didn't break content; xai voice
// identity isn't checkable via STT. Startup overhead / duration bounds
// mirror the Murf override test.
func TestVerb_Say_Xai(t *testing.T) {
	if !cfg.HasXai() || xaiLabel == "" {
		t.Log("XAI_API_KEY not set — passing without exercising xai TTS")
		return
	}

	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-xai",
		minDur:     1 * time.Second,
		// ~1.5s spoken + ~1.5s startup overhead; 9s leaves headroom.
		maxDur: 9 * time.Second,
		// Plain prose only — no coined brand words: telephony-quality STT
		// mangles those, which is a transcription artifact, not a synthesis
		// failure. The words below transcribe reliably and still prove the
		// xai voice spoke the text.
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", map[string]any{
				"vendor":   "xai",
				"label":    xaiLabel,
				"voice":    xaiVoice,
				"language": "en-US",
			}),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Xai — the same streaming say path as
// TestVerb_Say_Stream, but pinned to the xai vendor. Optional vendor: passes
// without exercising xai TTS (plain return, not t.Skip) when XAI_API_KEY is
// unset, exactly like TestVerb_Say_Xai. When the key is set it drives real
// xai streaming over the WS app and asserts the transcript.
func TestVerb_Say_Stream_Xai(t *testing.T) {
	if !cfg.HasXai() || xaiLabel == "" {
		t.Log("XAI_API_KEY not set — passing without exercising xai TTS")
		return
	}

	t.Parallel()
	runStreamingSay(t, "say-stream-xai", map[string]any{
		"vendor":   "xai",
		"label":    xaiLabel,
		"voice":    xaiVoice,
		"language": "en-US",
	})
}
