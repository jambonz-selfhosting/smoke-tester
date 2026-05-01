// Package tts provides a tiny helper to pre-generate WAV fixtures via
// Deepgram's TTS REST API. Used by the agent verb test to produce the
// "user-side" prompt that we stream into the call via Call.SendWAV.
//
// Why pre-generate instead of using a fixed checked-in fixture: the agent
// verb test asserts on the LLM's reply transcript, which depends on the
// exact phrasing fed to STT. Deriving the WAV from a known-good text means
// we own both ends of the loop — text in, text expected back — without
// committing megabytes of binary into git.
//
// The output WAV is telephony-grade (8 kHz, mono, 16-bit linear PCM, RIFF
// header) so internal/sip/Call.SendWAV accepts it directly.
package tts

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

const EnvKey = "DEEPGRAM_API_KEY"

// ErrNoCredentials is returned when DEEPGRAM_API_KEY is unset.
var ErrNoCredentials = errors.New("tts: DEEPGRAM_API_KEY not set")

// HasKey reports whether a Deepgram API key is available.
func HasKey() bool { return os.Getenv(EnvKey) != "" }

// PromptOptions configure the TTS request. Defaults: aura-asteria-en voice,
// 8 kHz / mono / linear-16, 30s HTTP timeout.
type PromptOptions struct {
	// Model selects the voice (Deepgram aura-* family). Defaults to
	// "aura-asteria-en" — feminine American English, matches the in-jambonz
	// default voice we provision in TestMain.
	Model string
	// HTTPTimeout caps the round-trip. Defaults to 30s.
	HTTPTimeout time.Duration
}

// EnsureWAV returns a path to a telephony-grade WAV file containing TTS for
// text. Idempotent: cache key is sha1(model|text), so identical (model,
// text) pairs reuse the on-disk file without re-hitting the network.
//
// cacheDir must already exist; callers usually pass tests/verbs/testdata/agent.
//
// Skips with ErrNoCredentials if DEEPGRAM_API_KEY is unset (callers should
// t.Skip).
func EnsureWAV(ctx context.Context, cacheDir, text string, opts PromptOptions) (string, error) {
	if opts.Model == "" {
		opts.Model = "aura-asteria-en"
	}
	if opts.HTTPTimeout == 0 {
		opts.HTTPTimeout = 30 * time.Second
	}

	key := os.Getenv(EnvKey)
	if key == "" {
		return "", ErrNoCredentials
	}

	hash := sha1.Sum([]byte(opts.Model + "|" + text))
	name := hex.EncodeToString(hash[:8]) + ".wav"
	out := filepath.Join(cacheDir, name)

	if fi, err := os.Stat(out); err == nil && fi.Size() > 44 {
		return out, nil
	}

	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		return "", fmt.Errorf("tts: mkdir cache %s: %w", cacheDir, err)
	}

	pcm, err := fetchLPCM(ctx, key, text, opts)
	if err != nil {
		return "", err
	}

	if err := writeTelephonyWAV(out, pcm); err != nil {
		return "", err
	}
	return out, nil
}

// fetchLPCM hits Deepgram's /v1/speak with encoding=linear16, sample_rate=8000
// and returns the raw little-endian PCM body. Deepgram returns audio data as
// the response body — not JSON wrapped — so we just io.ReadAll it.
func fetchLPCM(ctx context.Context, apiKey, text string, opts PromptOptions) ([]byte, error) {
	body, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return nil, fmt.Errorf("tts: marshal: %w", err)
	}

	q := url.Values{}
	q.Set("model", opts.Model)
	q.Set("encoding", "linear16")
	q.Set("sample_rate", "8000")
	endpoint := "https://api.deepgram.com/v1/speak?" + q.Encode()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("tts: build request: %w", err)
	}
	req.Header.Set("Authorization", "Token "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	httpCli := &http.Client{Timeout: opts.HTTPTimeout}
	resp, err := httpCli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("tts: deepgram request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, fmt.Errorf("tts: deepgram %d: %s", resp.StatusCode, string(errBody))
	}

	pcm, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("tts: read response: %w", err)
	}
	if len(pcm) < 1600 { // <100ms of audio is almost certainly an error
		return nil, fmt.Errorf("tts: deepgram returned only %d bytes (suspiciously short)", len(pcm))
	}
	return pcm, nil
}

// writeTelephonyWAV wraps raw 8 kHz / mono / 16-bit LPCM in a RIFF/WAVE
// header that internal/sip/Call.SendWAV's readTelephonyWAV() accepts.
func writeTelephonyWAV(path string, pcm []byte) error {
	const (
		sampleRate    = 8000
		numChannels   = 1
		bitsPerSample = 16
	)
	byteRate := uint32(sampleRate * numChannels * bitsPerSample / 8)
	blockAlign := uint16(numChannels * bitsPerSample / 8)

	var buf bytes.Buffer
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")
	// fmt chunk
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))                // chunk size
	binary.Write(&buf, binary.LittleEndian, uint16(1))                 // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(numChannels))       //
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))        //
	binary.Write(&buf, binary.LittleEndian, byteRate)                  //
	binary.Write(&buf, binary.LittleEndian, blockAlign)                //
	binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))     //
	// data chunk
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)

	return os.WriteFile(path, buf.Bytes(), 0o644)
}
