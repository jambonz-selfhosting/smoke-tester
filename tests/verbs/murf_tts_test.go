package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_Stream_Murf — the same streaming say path as
// TestVerb_Say_Stream, but pinned to the Murf.ai vendor (which has its own
// WebSocket streaming module). Optional vendor: skips (passes) with a
// credential-missing log when MURF_API_KEY is unset, exactly like
// TestVerb_Say_Murf. When the key is set it drives real Murf streaming over
// the WS app and asserts the transcript.
func TestVerb_Say_Stream_Murf(t *testing.T) {
	t.Parallel()
	if !cfg.HasMurf() || murfLabel == "" {
		t.Skip("Murf streaming say test needs MURF_API_KEY (credential missing — skipping, not a failure)")
	}
	runStreamingSay(t, "say-stream-murf", map[string]any{
		"vendor":   "murf",
		"label":    murfLabel,
		"voice":    murfVoice,
		"language": "en-US",
	})
}

// TestVerb_Say_Murf — speak through the Murf.ai TTS vendor via a
// synthesizer override, using the Murf speech credential provisioned at
// TestMain. Murf is an optional vendor: when MURF_API_KEY is not set no
// credential is provisioned, and this test skips (which counts as a pass)
// with a credential-missing log rather than failing.
//
// Transcript verifies the override didn't break content; Murf voice
// identity isn't checkable via STT. Startup overhead / duration bounds
// mirror the Deepgram override test.
func TestVerb_Say_Murf(t *testing.T) {
	t.Parallel()
	if !cfg.HasMurf() || murfLabel == "" {
		t.Skip("Murf say test needs MURF_API_KEY (credential missing — skipping, not a failure)")
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-murf",
		minDur:     1 * time.Second,
		// ~1.5s spoken + ~1.5s startup overhead; 9s leaves headroom.
		maxDur: 9 * time.Second,
		// Plain prose only — no "Murf" brand word: telephony-quality STT
		// mangles the coined name ("mers"), which is a transcription
		// artifact, not a synthesis failure. The words below transcribe
		// reliably and still prove the Murf voice spoke the text.
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", map[string]any{
				"vendor":   "murf",
				"label":    murfLabel,
				"voice":    murfVoice,
				"language": "en-US",
			}),
		wantWords: []string{"voice test", "working correctly"},
	})
}
