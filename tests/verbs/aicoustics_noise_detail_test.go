//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_NoiseIsolation_Aicoustics_Model16k — config.noiseIsolation.model pins quail-s-16khz on an
// 8 kHz call: the resampled path removes the noise and keeps the speech.
//
// Steps:
//   - build-noisy-prompt
//   - control:fork-noisy-prompt
//   - model16k:fork-noisy-prompt
//   - assert-noise-removed
//   - assert-speech-kept
func TestVerb_NoiseIsolation_Aicoustics_Model16k(t *testing.T) {
	requireAicoustics(t)
	t.Parallel()
	ctx := WithTimeout(t, 120*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	f := forkNoisyPrompt(t, ctx, "model16k", noisy, map[string]any{
		"enable": true, "vendor": "aicoustics", "model": "quail-s-16khz.aicmodel",
	})

	s := Step(t, "assert-noise-removed")
	if cShare := leadLoudShare(s, "control", control.msgs, control.sent, np); cShare < minLoudUnfiltered {
		s.Fatalf("control lead only %.0f%% loud: the fork lost the noise, nothing to compare", 100*cShare)
	}
	if share := leadLoudShare(s, "model16k", f.msgs, f.sent, np); share > maxLoudFiltered {
		s.Errorf("16k model lead %.0f%% loud (want <= %.0f%%): noise was not removed", 100*share, 100*maxLoudFiltered)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	assertSpeechKept(s, ctx, t, "model16k", f)
	s.Done()
}
