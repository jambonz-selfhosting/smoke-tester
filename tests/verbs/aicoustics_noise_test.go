// ai-coustics noise isolation: vendor "aicoustics" on config.noiseIsolation. The
// license lives on the media server (AIC_SDK_LICENSE), so the tests run only when
// AICOUSTICS_ENABLED=true. Each test forks a noisy prompt (speech over white
// noise, with a noise-only lead) through `listen` and measures the lead.
package verbs

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func requireAicoustics(t *testing.T) {
	t.Helper()
	requireWebhook(t)
	if !cfg.HasAicoustics() {
		t.Skip("AICOUSTICS_ENABLED not set — skipping, not a failure")
	}
}

func aicNoisyPrompt(t *testing.T, ctx context.Context) (string, noisyPrompt) {
	t.Helper()
	s := Step(t, "build-noisy-prompt")
	noisy := filepath.Join(t.TempDir(), "noisy-prompt.wav")
	np, err := writeNoisyWAV(krispEnsurePromptWAV(ctx, s), noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()
	return noisy, np
}

// TestVerb_NoiseIsolation_Aicoustics_Listen — control call vs aicoustics with the
// default model (quail-s-8khz on these 8 kHz calls): the speech stays intelligible
// and a last plain call proves the media server still takes calls once the
// filtered one has ended (a stuck enhancer used to wedge every later command).
// The 8 kHz model barely touches white noise (~2 dB offline), so the lead is only
// logged here; Model16k asserts the noise removal.
func TestVerb_NoiseIsolation_Aicoustics_Listen(t *testing.T) {
	requireAicoustics(t)
	ctx := WithTimeout(t, 150*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "aicoustics", noisy, map[string]any{
		"enable": true, "vendor": "aicoustics",
	})

	s := Step(t, "measure-noise-lead")
	if cShare := leadLoudShare(s, "control", control.msgs, control.sent, np); cShare < minLoudUnfiltered {
		s.Fatalf("control lead only %.0f%% loud: the fork lost the noise, nothing to compare", 100*cShare)
	}
	leadLoudShare(s, "aicoustics", filtered.msgs, filtered.sent, np)
	s.Done()

	s = Step(t, "assert-speech-kept")
	pcmPath := filepath.Join(t.TempDir(), "aicoustics-fork.pcm")
	if err := os.WriteFile(pcmPath, webhook.BinaryConcat(filtered.msgs), 0o644); err != nil {
		s.Fatalf("write fork audio: %v", err)
	}
	AssertTranscriptHasMost(s, ctx, pcmPath, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	after := forkNoisyPrompt(t, ctx, "after", noisy, nil)
	s = Step(t, "assert-media-server-still-serving")
	if share := leadLoudShare(s, "after", after.msgs, after.sent, np); share < minLoudUnfiltered {
		s.Errorf("call after aicoustics lead only %.0f%% loud: fork audio missing", 100*share)
	}
	s.Done()
}

// TestVerb_NoiseIsolation_Aicoustics_Model16k — pins the 16 kHz model on an 8 kHz
// call: proves the model option reaches the media server and the resampled path
// still removes the noise and keeps the speech.
func TestVerb_NoiseIsolation_Aicoustics_Model16k(t *testing.T) {
	requireAicoustics(t)
	ctx := WithTimeout(t, 90*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	f := forkNoisyPrompt(t, ctx, "model16k", noisy, map[string]any{
		"enable": true, "vendor": "aicoustics", "model": "quail-s-16khz.aicmodel",
	})

	s := Step(t, "assert-noise-removed")
	if share := leadLoudShare(s, "model16k", f.msgs, f.sent, np); share > maxLoudFiltered {
		s.Errorf("16k model lead %.0f%% loud (want <= %.0f%%): noise was not removed",
			100*share, 100*maxLoudFiltered)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	pcmPath := filepath.Join(t.TempDir(), "aicoustics-16k-fork.pcm")
	if err := os.WriteFile(pcmPath, webhook.BinaryConcat(f.msgs), 0o644); err != nil {
		s.Fatalf("write fork audio: %v", err)
	}
	AssertTranscriptHasMost(s, ctx, pcmPath, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()
}
