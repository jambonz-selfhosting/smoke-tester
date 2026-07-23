package verbs

import (
	"testing"
	"time"
)

// TestVerb_Say_DeepgramFlux — speak through the Deepgram Flux TTS vendor
// ("deepgramflux", wss://api.deepgram.com/v2/speak) via a synthesizer
// override, using the credential provisioned at TestMain (reuses
// DEEPGRAM_API_KEY). The transcript-back verifies Flux synthesized the text.
//
// Plain prose only — no brand/coined words: telephony-grade STT mangles
// those, which is a transcription artifact, not a synthesis failure. Duration
// bounds mirror the other override say tests (~1.5s spoken + startup overhead).
func TestVerb_Say_DeepgramFlux(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-deepgramflux",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", map[string]any{
				"vendor":   "deepgramflux",
				"label":    deepgramFluxLabel,
				"voice":    deepgramFluxVoice,
				"language": "en-US",
			}),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_DeepgramFlux — the streaming synthesis path for Flux
// (the mediajam deepgramflux dialect over /v2/speak). Flux is a
// conversation-native, streaming-first model, so the streaming path is the
// primary one to exercise.
func TestVerb_Say_Stream_DeepgramFlux(t *testing.T) {
	t.Parallel()
	runStreamingSay(t, "say-stream-deepgramflux", map[string]any{
		"vendor":   "deepgramflux",
		"label":    deepgramFluxLabel,
		"voice":    deepgramFluxVoice,
		"language": "en-US",
	})
}
