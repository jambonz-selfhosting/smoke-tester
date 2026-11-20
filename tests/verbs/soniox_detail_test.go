//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_SpeechCredential_Soniox — the api-server's STT and TTS credential
// tests (real audio, real synthesis with the cache off) pass for the key.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Soniox(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, sonioxSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, sonioxSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Soniox_Options — speed, reduce_silence and client_reference_id
// reach the HTTP body, and es-ES is sent as "es". Duration only: the offline
// STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Soniox_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSoniox(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-soniox-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", sonioxSpanishText,
			"synthesizer", sonioxSynth(sonioxSpanishVoice, "es-ES", map[string]any{
				"speed":               1.2,
				"reduce_silence":      true,
				"client_reference_id": "jambonz-smoke",
			})),
	})
}

// TestVerb_Say_Stream_Soniox_TwoTurns — two streaming says on one call: the
// first stream's text_end closes it, so the second turn opens a new stream id
// on the same socket and must still be heard.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Soniox_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireSoniox(t) {
		return
	}
	t.Parallel()
	synth := sonioxSynth(sonioxVoice, "en-US", nil)
	runSonioxScript(t, "say-stream-soniox-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}
