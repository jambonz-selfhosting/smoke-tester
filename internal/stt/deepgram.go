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
	"net"
	"os"
	"strings"
	"time"

	api "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/rest"
	restapi "github.com/deepgram/deepgram-go-sdk/v3/pkg/api/listen/v1/rest/interfaces"
	interfaces "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/interfaces"
	client "github.com/deepgram/deepgram-go-sdk/v3/pkg/client/listen"
)

// transcribeAttemptTimeout caps a single Deepgram REST call. The SDK has
// no transport timeout of its own; without this, a flaky network can
// hang an attempt for the test's full ctx (often 60-180s). 15s is well
// past the API's typical 1-3s response time on a healthy day.
const transcribeAttemptTimeout = 15 * time.Second

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
	res, err := transcribeWithRetry(ctx, dg, pcmuPath, opts)
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

// TranscribeMulawWAV is Transcribe for µ-law-payload WAV files. The
// listen/stream verb tests capture WS frames and wrap them in a PCMU
// WAV header — Transcribe's encoding=linear16 hint would mis-decode
// those, so we send encoding=mulaw instead.
func TranscribeMulawWAV(ctx context.Context, wavPath string) (string, error) {
	key := os.Getenv(EnvKey)
	if key == "" {
		return "", ErrNoCredentials
	}
	c := client.NewREST("", &interfaces.ClientOptions{Host: "https://api.deepgram.com"})
	dg := api.New(c)
	opts := &interfaces.PreRecordedTranscriptionOptions{
		Model:      "nova-3",
		Language:   "en-US",
		Encoding:   "mulaw",
		SampleRate: 8000,
		Punctuate:  true,
		Keyterm:    []string{"jambonz"},
	}
	res, err := transcribeWithRetry(ctx, dg, wavPath, opts)
	if err != nil {
		return "", fmt.Errorf("deepgram transcribe %s: %w", wavPath, err)
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

// transcribeWithRetry runs a single Deepgram pre-recorded call with a
// per-attempt timeout, then retries once on context-deadline-exceeded
// or transient network errors. The SDK has no transport timeout of its
// own, so a sick TCP path can hang the call for the caller's full ctx
// (60-180s of test budget) — capping each attempt at
// transcribeAttemptTimeout and retrying once is the smallest fix that
// protects the suite without masking real outages.
func transcribeWithRetry(parent context.Context, dg *api.Client, path string, opts *interfaces.PreRecordedTranscriptionOptions) (*restapi.PreRecordedResponse, error) {
	const attempts = 2
	var lastErr error
	for i := 0; i < attempts; i++ {
		// Each attempt gets a fresh per-call deadline. Use the parent
		// ctx's deadline as a hard ceiling — never wait longer than the
		// caller asked for in total.
		attemptCtx, cancel := context.WithTimeout(parent, transcribeAttemptTimeout)
		res, err := dg.FromFile(attemptCtx, path, opts)
		cancel()
		if err == nil {
			return res, nil
		}
		lastErr = err
		if !isRetryableTranscribeErr(err) || parent.Err() != nil {
			return nil, err
		}
	}
	return nil, lastErr
}

// isRetryableTranscribeErr reports whether err looks like a transient
// network/timeout failure that's worth one retry. Returns false for
// 4xx-style API errors (auth, bad audio, missing file) — those will
// fail again identically.
func isRetryableTranscribeErr(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && (ne.Timeout() || ne.Temporary()) { //nolint:staticcheck // Temporary() is deprecated but still useful here
		return true
	}
	// Connection refused / reset / EOF surface as plain errors from the
	// SDK without a typed wrapper; match on substring as a last resort.
	msg := err.Error()
	for _, needle := range []string{"connection refused", "connection reset", "EOF", "no such host", "i/o timeout"} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
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
