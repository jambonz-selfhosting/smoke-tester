//go:build detail

package verbs

import (
	"path/filepath"
	"testing"
	"time"
)

// TestVerb_NoiseIsolation_Kugelaudio_TtsOnlyCredentialIgnored — noise isolation
// pointed at the TTS-only kugelaudio credential must not start: the same key,
// but without use_for_noise_isolation. The noise-only lead stays loud in the
// forked caller audio. Not parallel: see the file comment. It cannot tell
// "ignored" from "Clarity is down"; the Listen test covers the positive path.
//
// Steps:
//  1. build-noisy-prompt
//  2. tts-only:fork-noisy-prompt
//  3. assert-noise-kept
func TestVerb_NoiseIsolation_Kugelaudio_TtsOnlyCredentialIgnored(t *testing.T) {
	requireKugelaudioNoise(t)
	ctx := WithTimeout(t, 60*time.Second)

	s := Step(t, "build-noisy-prompt")
	noisy := filepath.Join(t.TempDir(), "noisy-prompt.wav")
	np, err := writeNoisyWAV(krispEnsurePromptWAV(ctx, s), noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	fork := forkNoisyPrompt(t, ctx, "tts-only", noisy, map[string]any{
		"enable": true, "vendor": "kugelaudio", "label": kugelaudioLabel,
	})

	s = Step(t, "assert-noise-kept")
	if share := leadLoudShare(s, "tts-only", fork.msgs, fork.sent, np); share < minLoudUnfiltered {
		s.Errorf("lead only %.0f%% loud (want >= %.0f%%): noise isolation ran on a TTS-only credential",
			100*share, 100*minLoudUnfiltered)
	}
	s.Done()
}
