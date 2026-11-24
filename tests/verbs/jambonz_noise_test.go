// jambonz noise isolation (the in-house model) via config.noiseIsolation vendor "jambonz".
// The model lives on the media server, so these run only with JAMBONZ_NC_ENABLED=true.
package verbs

import (
	"testing"
	"time"
)

// level 100 keeps 30% of each bin at minimum (about 10 dB), so white noise drops well below this
const maxJambonzNcRMSRatio = 0.6

func requireJambonzNc(t *testing.T) {
	t.Helper()
	requireWebhook(t)
	if !cfg.HasJambonzNc() {
		t.Skip("JAMBONZ_NC_ENABLED not set — skipping, not a failure")
	}
}

// TestVerb_NoiseIsolation_Jambonz_Listen — default level vs a control call: the noise-only lead
// gets quieter, the speech still transcribes, and a later call works.
//
// Steps:
//   - build-noisy-prompt
//   - control:fork-noisy-prompt
//   - jambonz:fork-noisy-prompt
//   - assert-noise-reduced
//   - assert-speech-kept
//   - after:fork-noisy-prompt
//   - assert-media-server-still-serving
func TestVerb_NoiseIsolation_Jambonz_Listen(t *testing.T) {
	requireJambonzNc(t)
	t.Parallel()
	ctx := WithTimeout(t, 150*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "jambonz", noisy, map[string]any{"enable": true, "vendor": "jambonz"})

	s := Step(t, "assert-noise-reduced")
	if cShare := leadLoudShare(s, "control", control.msgs, control.sent, np); cShare < minLoudUnfiltered {
		s.Fatalf("control lead only %.0f%% loud: the fork lost the noise, nothing to compare", 100*cShare)
	}
	leadLoudShare(s, "jambonz", filtered.msgs, filtered.sent, np)
	cRMS, fRMS := leadRMS(control.msgs, control.sent, np), leadRMS(filtered.msgs, filtered.sent, np)
	s.Logf("lead RMS control=%.0f jambonz=%.0f ratio=%.2f", cRMS, fRMS, fRMS/cRMS)
	if cRMS == 0 || fRMS/cRMS > maxJambonzNcRMSRatio {
		s.Errorf("jambonz lead RMS ratio %.2f (want <= %.2f): the model did not run", fRMS/cRMS, maxJambonzNcRMSRatio)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	assertSpeechKept(s, ctx, t, "jambonz", filtered)
	s.Done()

	after := forkNoisyPrompt(t, ctx, "after", noisy, nil)
	s = Step(t, "assert-media-server-still-serving")
	if share := leadLoudShare(s, "after", after.msgs, after.sent, np); share < minLoudUnfiltered {
		s.Errorf("call after jambonz lead only %.0f%% loud: fork audio missing", 100*share)
	}
	s.Done()
}

// TestVerb_NoiseIsolation_Jambonz_LevelAndBadModel — level 50 mixes back more of the input than
// the default, and a model name that is not installed leaves the call unfiltered but working.
//
// Steps:
//   - build-noisy-prompt
//   - control:fork-noisy-prompt
//   - level100:fork-noisy-prompt
//   - level50:fork-noisy-prompt
//   - badmodel:fork-noisy-prompt
//   - assert-level-orders-suppression
//   - assert-bad-model-passes-audio
func TestVerb_NoiseIsolation_Jambonz_LevelAndBadModel(t *testing.T) {
	requireJambonzNc(t)
	t.Parallel()
	ctx := WithTimeout(t, 180*time.Second)
	noisy, np := aicNoisyPrompt(t, ctx)

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	l100 := forkNoisyPrompt(t, ctx, "level100", noisy, map[string]any{"enable": true, "vendor": "jambonz", "level": 100})
	l50 := forkNoisyPrompt(t, ctx, "level50", noisy, map[string]any{"enable": true, "vendor": "jambonz", "level": 50})
	bad := forkNoisyPrompt(t, ctx, "badmodel", noisy, map[string]any{"enable": true, "vendor": "jambonz", "model": "no-such-model.onnx"})

	cRMS := leadRMS(control.msgs, control.sent, np)
	r100 := leadRMS(l100.msgs, l100.sent, np) / cRMS
	r50 := leadRMS(l50.msgs, l50.sent, np) / cRMS
	s := Step(t, "assert-level-orders-suppression")
	s.Logf("lead RMS ratio level100=%.2f level50=%.2f", r100, r50)
	if cRMS == 0 || !(r100 < r50 && r50 < 0.95) {
		s.Errorf("want level100 < level50 < 0.95, got %.2f / %.2f", r100, r50)
	}
	s.Done()

	s = Step(t, "assert-bad-model-passes-audio")
	if share := leadLoudShare(s, "badmodel", bad.msgs, bad.sent, np); share < minLoudUnfiltered {
		s.Errorf("bad model lead only %.0f%% loud: want unfiltered audio, the call must not go silent", 100*share)
	}
	s.Done()
}
