//go:build detail

package verbs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestProbe_TurnAudioPath
//
// Steps:
//  0. setup — create the out dir, list the wavs
//
// Per phrase subtest:
//  1. script-listen-16k — [listen ws mono 16k, pause wav+4s, hangup]
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-ws-connect
//  5. send-wav
//  6. post-speech-silence
//  7. hangup-and-wait-ws-close
//  8. save-capture
func TestProbe_TurnAudioPath(t *testing.T) {
	dir := os.Getenv("TURN_PROBE_WAV_DIR")
	if dir == "" {
		t.Skip("TURN_PROBE_WAV_DIR not set")
	}
	s := Step(t, "setup")
	outDir := os.Getenv("TURN_PROBE_OUT_DIR")
	if outDir == "" {
		outDir = t.TempDir()
	} else if err := os.MkdirAll(outDir, 0o755); err != nil {
		s.Fatalf("mkdir %s: %v", outDir, err)
	}
	wavs, err := filepath.Glob(filepath.Join(dir, "*.wav"))
	if err != nil {
		s.Fatalf("glob %s: %v", dir, err)
	}
	sort.Strings(wavs)
	if len(wavs) == 0 {
		s.Fatalf("no wavs in %s", dir)
	}
	s.Done()
	for _, wavPath := range wavs {
		name := strings.TrimSuffix(filepath.Base(wavPath), ".wav")
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runTurnProbe(t, wavPath, filepath.Join(outDir, name+".l16"))
		})
	}
}
