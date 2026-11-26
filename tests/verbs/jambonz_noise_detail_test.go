//go:build detail

package verbs

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/wav"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_NoiseIsolation_Jambonz_ListenPairs — one call per 8 kHz WAV in JAMBONZ_NC_LISTEN_DIR
// with jambonz noise isolation on, saving <name>.sent.wav and <name>.recv.wav (the listen fork,
// aligned to the prompt start) to JAMBONZ_NC_LISTEN_OUT for a by-ear review.
// JAMBONZ_NC_LISTEN_MODEL picks the model file, JAMBONZ_NC_LISTEN_LEVEL the level and
// JAMBONZ_NC_LISTEN_VENDOR another vendor.
//
// Steps:
//   - setup
//   - <name>:fork-noisy-prompt (per clip)
//   - <name>:save-pair (per clip)
func TestVerb_NoiseIsolation_Jambonz_ListenPairs(t *testing.T) {
	requireJambonzNc(t)
	in, out := os.Getenv("JAMBONZ_NC_LISTEN_DIR"), os.Getenv("JAMBONZ_NC_LISTEN_OUT")
	if in == "" || out == "" {
		t.Skip("JAMBONZ_NC_LISTEN_DIR / JAMBONZ_NC_LISTEN_OUT not set")
	}
	clips, _ := filepath.Glob(filepath.Join(in, "*.wav"))
	sort.Strings(clips)
	ctx := WithTimeout(t, time.Duration(len(clips)+1)*40*time.Second)
	s := Step(t, "setup")
	if len(clips) == 0 {
		s.Fatalf("no WAVs in %s", in)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		s.Fatal(err)
	}
	vendor := "jambonz"
	if v := os.Getenv("JAMBONZ_NC_LISTEN_VENDOR"); v != "" {
		vendor = v // e.g. krisp, for a side-by-side with the same clips
	}
	noise := map[string]any{"enable": true, "vendor": vendor}
	if m := os.Getenv("JAMBONZ_NC_LISTEN_MODEL"); m != "" {
		noise["model"] = m
	}
	if l := os.Getenv("JAMBONZ_NC_LISTEN_LEVEL"); l != "" {
		lv, err := strconv.Atoi(l)
		if err != nil {
			s.Fatalf("JAMBONZ_NC_LISTEN_LEVEL: %v", err)
		}
		noise["level"] = lv
	}
	s.Done()
	for _, clip := range clips {
		name := strings.TrimSuffix(filepath.Base(clip), ".wav")
		f := forkNoisyPrompt(t, ctx, name, clip, noise)

		s := Step(t, name+":save-pair")
		sent, err := wav.Read(clip)
		if err != nil {
			s.Fatalf("read %s: %v", clip, err)
		}
		recv := alignedFork(f, len(sent)/2)
		if err := wav.Write(filepath.Join(out, name+".sent.wav"), sent); err != nil {
			s.Fatalf("write sent: %v", err)
		}
		if err := wav.WriteSamples(filepath.Join(out, name+".recv.wav"), recv); err != nil {
			s.Fatalf("write recv: %v", err)
		}
		s.Logf("%s: sent %d samples, received %d", name, len(sent)/2, len(recv))
		s.Done()
	}
}

// alignedFork returns n contiguous fork samples from the prompt start: the
// last message's Received time anchors the stream to the wall clock once, so
// per-message jitter does not tear the audio.
func alignedFork(f forkResult, n int) []int16 {
	all := wav.Samples(webhook.BinaryConcat(f.msgs))
	var last time.Time
	for _, m := range f.msgs {
		if m.Kind == webhook.WSBinary {
			last = m.Received
		}
	}
	start := len(all) - int(last.Sub(f.sent)*wav.SampleRate/time.Second)
	out := make([]int16, n)
	if -start >= n { // the fork carried less audio than the time elapsed
		return out
	}
	if start < 0 {
		copy(out[-start:], all)
	} else if start < len(all) {
		copy(out, all[start:])
	}
	return out
}
