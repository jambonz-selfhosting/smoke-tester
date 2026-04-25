// Package stt provides thin helpers for running a captured PCMU call
// recording through Deepgram's pre-recorded transcription API. Used by verb
// tests that need to assert "TTS actually said the right words" — our
// RMS/duration checks are too weak to catch vendor misconfiguration where
// jambonz produces plausible audio of the wrong content.
//
// Credential surfacing: DEEPGRAM_API_KEY env var. HasKey / Transcribe
// return ErrNoCredentials when unset so callers can t.Skip cleanly.
package stt

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/rest"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"
)

// EnvKey is the environment variable we read the Deepgram API key from.
const EnvKey = "DEEPGRAM_API_KEY"

// ErrNoCredentials is returned when DEEPGRAM_API_KEY is unset.
var ErrNoCredentials = errors.New("stt: DEEPGRAM_API_KEY not set")

// HasKey reports whether a Deepgram API key is available. Callers should
// t.Skip when false.
func HasKey() bool { return os.Getenv(EnvKey) != "" }

// Transcribe uploads a raw linear-16 (signed little-endian 16-bit) 8 kHz
// mono PCM recording to Deepgram's pre-recorded REST API and returns the
// best-alternative transcript of the first channel, lowercased and
// stripped to alphanumerics + spaces.
//
// The harness's call.StartRecording decodes incoming µ-law RTP into linear
// PCM16 before writing to disk (see internal/sip/call.go), so the .pcm
// files on disk are linear16 — not raw µ-law. Deepgram needs both the
// Encoding and SampleRate set explicitly for raw-PCM uploads.
func Transcribe(ctx context.Context, pcmuPath string) (string, error) {
	key := os.Getenv(EnvKey)
	if key == "" {
		return "", ErrNoCredentials
	}
	// The SDK reads DEEPGRAM_API_KEY from env at client init, so we just
	// pass an empty string and it'll pick it up.
	c := client.NewREST("", &interfaces.ClientOptions{
		Host: "https://api.deepgram.com",
	})
	dg := api.New(c)

	opts := &interfaces.PreRecordedTranscriptionOptions{
		Model:      "nova-3",
		Language:   "en-US",
		Encoding:   "linear16",
		SampleRate: 8000,
		Punctuate:  true,
		// SmartFormat intentionally OFF: it rewrites "one two three" →
		// "1 2 3" and similar, which breaks word-substring assertions.
		// Bias toward project-specific proper nouns Deepgram wouldn't know.
		Keyterm: []string{"jambonz"},
	}
	res, err := dg.FromFile(ctx, pcmuPath, opts)
	if err != nil {
		return "", fmt.Errorf("deepgram transcribe %s: %w", pcmuPath, err)
	}
	if res == nil || res.Results == nil || len(res.Results.Channels) == 0 {
		return "", fmt.Errorf("deepgram: no channels in response")
	}
	ch := res.Results.Channels[0]
	if len(ch.Alternatives) == 0 {
		return "", fmt.Errorf("deepgram: no alternatives in channel")
	}
	return Normalize(ch.Alternatives[0].Transcript), nil
}

// Normalize lowercases and strips to alphanumerics + single spaces so
// substring matches ignore punctuation and casing differences between
// input text and recognized transcript.
func Normalize(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	b.Grow(len(s))
	prevSpace := true
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevSpace = false
		case r == ' ' || r == '\t' || r == '\n':
			if !prevSpace {
				b.WriteByte(' ')
				prevSpace = true
			}
		}
	}
	return strings.TrimSpace(b.String())
}
