// ai-coustics noise isolation via config.noiseIsolation vendor "aicoustics".
// The license lives on the media server, so these run only with AICOUSTICS_ENABLED=true.
package verbs

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/wav"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// the default 8 kHz model only takes ~2 dB off white noise, so it must cut the lead RMS by >= 10%
const maxAicDefaultRMSRatio = 0.9

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

// leadRMS is the RMS of the forked audio inside the prompt's noise-only lead (same window as leadLoudShare).
func leadRMS(msgs []webhook.WSMessage, sent time.Time, np noisyPrompt) float64 {
	from, to := sent.Add(leadSkip), sent.Add(np.lead-200*time.Millisecond)
	var e float64
	n := 0
	for _, m := range msgs {
		if m.Kind != webhook.WSBinary {
			continue
		}
		pcm := wav.Samples(m.Binary)
		for i, v := range pcm {
			at := m.Received.Add(-time.Duration(len(pcm)-i) * time.Second / wav.SampleRate)
			if at.Before(from) || !at.Before(to) {
				continue
			}
			e += float64(v) * float64(v)
			n++
		}
	}
	if n == 0 {
		return 0
	}
	return math.Sqrt(e / float64(n))
}

func assertSpeechKept(s *StepCtx, ctx context.Context, t *testing.T, name string, f forkResult) {
	pcm := filepath.Join(t.TempDir(), name+".pcm")
	if err := os.WriteFile(pcm, webhook.BinaryConcat(f.msgs), 0o644); err != nil {
		s.Fatalf("write fork audio: %v", err)
	}
	AssertTranscriptHasMost(s, ctx, pcm, krispMinKeywordHits, krispEchoKeywords...)
}

// TestVerb_NoiseIsolation_Aicoustics_Listen — default model (quail-s-8khz unless AIC_NC_MODEL
// is set) vs a control call: the lead gets quieter, the speech survives, and a later call works.
//
// Steps:
//   - build-noisy-prompt
//   - control:fork-noisy-prompt
//   - aicoustics:fork-noisy-prompt
//   - assert-noise-reduced
//   - assert-speech-kept
//   - after:fork-noisy-prompt
//   - assert-media-server-still-serving
func TestVerb_NoiseIsolation_Aicoustics_Listen(t *testing.T) {
	requireAicoustics(t)
	t.Parallel()
	ctx := WithTimeout(t, 150*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "aicoustics", noisy, map[string]any{
		"enable": true, "vendor": "aicoustics",
	})

	s := Step(t, "assert-noise-reduced")
	if cShare := leadLoudShare(s, "control", control.msgs, control.sent, np); cShare < minLoudUnfiltered {
		s.Fatalf("control lead only %.0f%% loud: the fork lost the noise, nothing to compare", 100*cShare)
	}
	leadLoudShare(s, "aicoustics", filtered.msgs, filtered.sent, np)
	cRMS, fRMS := leadRMS(control.msgs, control.sent, np), leadRMS(filtered.msgs, filtered.sent, np)
	s.Logf("lead RMS control=%.0f aicoustics=%.0f ratio=%.2f", cRMS, fRMS, fRMS/cRMS)
	if cRMS == 0 || fRMS/cRMS > maxAicDefaultRMSRatio {
		s.Errorf("aicoustics lead RMS ratio %.2f (want <= %.2f): the enhancer did not run", fRMS/cRMS, maxAicDefaultRMSRatio)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	assertSpeechKept(s, ctx, t, "aicoustics", filtered)
	s.Done()

	// only meaningful on a single media server, where the stuck enhancer would have wedged it
	after := forkNoisyPrompt(t, ctx, "after", noisy, nil)
	s = Step(t, "assert-media-server-still-serving")
	if share := leadLoudShare(s, "after", after.msgs, after.sent, np); share < minLoudUnfiltered {
		s.Errorf("call after aicoustics lead only %.0f%% loud: fork audio missing", 100*share)
	}
	s.Done()
}

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
