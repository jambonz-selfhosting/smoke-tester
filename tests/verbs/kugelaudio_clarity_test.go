// KugelAudio Clarity (clarity-1) noise isolation: vendor "kugelaudio" on the
// noiseIsolation config/agent option, using a kugelaudio speech credential with
// use_for_noise_isolation. All tests skip without KUGELAUDIO_API_KEY.
//
// The free plan allows two concurrent Clarity streams: the Listen and Agent
// tests open one each. The TTS-only test must open none, and runs serially so
// a regression that makes it open one cannot starve the others.
package verbs

import (
	"context"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/wav"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	// a frame is loud above half the noise RMS: the noise keeps every unfiltered
	// frame loud, while a plain gain drop of a few dB silences none
	loudFraction = 0.5
	// skip the start of the noise-only lead: Clarity startup and network jitter
	leadSkip = time.Second
	// minimum frames the lead window must hold, or the fork missed it
	minLeadFrames = 60

	maxLoudFiltered   = 0.5 // Clarity must silence at least half the noise-only lead
	minLoudUnfiltered = 0.9
)

func requireKugelaudioNoise(t *testing.T) {
	t.Helper()
	requireKugelaudio(t)
	requireWebhook(t)
	if kugelaudioNoiseLabel == "" || kugelaudioDefaultNoiseSID == "" {
		t.Skip("KugelAudio noise isolation credentials missing — skipping, not a failure")
	}
}

// TestVerb_NoiseIsolation_Kugelaudio_Listen — the same noisy prompt (speech
// over white noise, with a noise-only lead and tail) is sent on two calls, one
// after the other, that fork their inbound audio with `listen`: a control call, and one
// with config.noiseIsolation vendor=kugelaudio and the noise isolation
// credential's label. Noise isolation runs ahead of the fork, so in the
// filtered fork the noise-only lead must go quiet while the speech stays
// intelligible.
//
// Steps:
//  1. build-noisy-prompt
//  2. control:fork-noisy-prompt
//  3. clarity:fork-noisy-prompt
//  4. assert-noise-removed
//  5. assert-speech-kept
func TestVerb_NoiseIsolation_Kugelaudio_Listen(t *testing.T) {
	requireKugelaudioNoise(t)
	t.Parallel()
	ctx := WithTimeout(t, 150*time.Second)

	s := Step(t, "build-noisy-prompt")
	noisy := filepath.Join(t.TempDir(), "noisy-prompt.wav")
	np, err := writeNoisyWAV(krispEnsurePromptWAV(ctx, s), noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	// sequential on purpose: parallel subtests queue for a -parallel slot,
	// which in a full run can outlast this test's budget
	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "clarity", noisy, map[string]any{
		"enable": true, "vendor": "kugelaudio", "label": kugelaudioNoiseLabel,
	})

	s = Step(t, "assert-noise-removed")
	cShare := leadLoudShare(s, "control", control.msgs, control.sent, np)
	fShare := leadLoudShare(s, "clarity", filtered.msgs, filtered.sent, np)
	if cShare < minLoudUnfiltered {
		s.Fatalf("control lead only %.0f%% loud: the fork lost the noise, nothing to compare", 100*cShare)
	}
	if fShare > maxLoudFiltered {
		s.Errorf("clarity lead %.0f%% loud (want <= %.0f%%): noise was not removed",
			100*fShare, 100*maxLoudFiltered)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	pcmPath := filepath.Join(t.TempDir(), "clarity-fork.pcm")
	if err := os.WriteFile(pcmPath, webhook.BinaryConcat(filtered.msgs), 0o644); err != nil {
		s.Fatalf("write fork audio: %v", err)
	}
	AssertTranscriptHasMost(s, ctx, pcmPath, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()
}

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

// TestVerb_Agent_NoiseIsolation_Kugelaudio — an agent with the shorthand
// noiseIsolation:"kugelaudio" (no label: jambonz must pick the account's
// unlabelled credential with use_for_noise_isolation) understands a prompt
// spoken over loud white noise and echoes the four words back. A background
// `listen` fork of the caller audio proves isolation ran: the noise-only lead
// goes quiet in it.
//
// Steps:
//  1. preflight-skips
//  2. build-noisy-prompt
//  3. script-agent-verb
//  4. place-call
//  5. answer-record-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-reply
//  9. assert-agent-replied
//  10. hangup-and-wait-ended
//  11. assert-noise-removed
func TestVerb_Agent_NoiseIsolation_Kugelaudio(t *testing.T) {
	requireKugelaudioNoise(t)
	t.Parallel()

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "build-noisy-prompt")
	noisy := filepath.Join(t.TempDir(), "noisy-prompt.wav")
	np, err := writeNoisyWAV(krispEnsurePromptWAV(ctx, s), noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	testID, sess := claimSession(t)
	collected := collectWSInBackground(ctx, sess)

	s = Step(t, "script-agent-verb")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("config", "listen", map[string]any{
			"enable": true, "url": wssURL(webhookSrv.PublicURL(), "/ws/"+testID),
			"mixType": "mono", "sampleRate": 8000,
		}),
		krispAgentVerb(sess, agentVerbOpts{}, map[string]any{"noiseIsolation": "kugelaudio"}),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	var sent time.Time
	rec := RunAudioRoundtrip(t, ctx, call, AudioRoundtripOpts{
		PromptWAV:     noisy,
		RecordTag:     "kugelaudio-noise",
		PromptStarted: &sent,
	})
	call.StopRecording()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-noise-removed")
	if share := leadLoudShare(s, "caller", collected(), sent, np); share > maxLoudFiltered {
		s.Errorf("caller fork lead %.0f%% loud (want <= %.0f%%): noise isolation did not run",
			100*share, 100*maxLoudFiltered)
	}
	s.Done()
}

type forkResult struct {
	msgs []webhook.WSMessage
	sent time.Time // when the prompt started streaming
}

// forkNoisyPrompt runs one call: optional config.noiseIsolation, a listen fork
// to our WS, the noisy prompt from the UAS once the fork is connected, hangup.
func forkNoisyPrompt(t *testing.T, ctx context.Context, leg, wavPath string, noise map[string]any) forkResult {
	t.Helper()
	s := Step(t, leg+":fork-noisy-prompt")
	uas := claimUAS(t, ctx)
	// one test may run several legs; each needs its own session and WS
	testID := t.Name() + "/" + leg
	sess := webhookReg.New(testID)
	t.Cleanup(func() { webhookReg.Release(testID) })
	collected := collectWSInBackground(ctx, sess)

	script := webhook.Script{}
	if noise != nil {
		script = append(script, V("config", "noiseIsolation", noise))
	}
	script = append(script,
		V("listen", "url", wssURL(webhookSrv.PublicURL(), "/ws/"+testID), "mixType", "mono", "sampleRate", 8000),
		V("pause", "length", 20),
		V("hangup"))
	sess.ScriptCallHook(WithWarmupScript(script))

	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := sess.WSConnected(wctx); err != nil {
		s.Fatalf("listen fork never connected: %v", err)
	}
	sent := time.Now()
	sendAndHangup(s, call, wavPath)

	msgs := collected()
	s.Logf("fork audio: %d bytes", len(webhook.BinaryConcat(msgs)))
	s.Done()
	return forkResult{msgs: msgs, sent: sent}
}

// collectWSInBackground drains the session's WS while the call runs (its
// queue holds only 256 frames); the returned func waits for the WS to close.
func collectWSInBackground(ctx context.Context, sess *webhook.Session) func() []webhook.WSMessage {
	cctx, cancel := context.WithCancel(ctx)
	ch := make(chan []webhook.WSMessage, 1)
	go func() { ch <- sess.CollectWS(cctx) }()
	return func() []webhook.WSMessage {
		t := time.AfterFunc(10*time.Second, cancel)
		defer t.Stop()
		defer cancel()
		return <-ch
	}
}

func sendAndHangup(s *StepCtx, call *jsip.Call, wavPath string) {
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(time.Second) // let the delayed tail of the filtered audio reach the fork
	_ = call.Hangup()
}

// leadLoudShare is the share of forked frames inside the prompt's noise-only
// lead that are loud. Each WS message's last sample arrived at its Received
// time, which puts every frame on the wall clock, independent of how much
// of the call the fork carried.
func leadLoudShare(s *StepCtx, who string, msgs []webhook.WSMessage, sent time.Time, np noisyPrompt) float64 {
	from, to := sent.Add(leadSkip), sent.Add(np.lead-200*time.Millisecond)
	thr := np.noiseRMS * loudFraction
	loud, n := 0, 0
	const frame = 160 // 20ms at 8 kHz
	for _, m := range msgs {
		if m.Kind != webhook.WSBinary {
			continue
		}
		pcm := wav.Samples(m.Binary)
		for off := 0; off+frame <= len(pcm); off += frame {
			at := m.Received.Add(-time.Duration(len(pcm)-off) * time.Second / wav.SampleRate)
			if at.Before(from) || !at.Before(to) {
				continue
			}
			n++
			var e float64
			for _, v := range pcm[off : off+frame] {
				e += float64(v) * float64(v)
			}
			if math.Sqrt(e/frame) > thr {
				loud++
			}
		}
	}
	if n < minLeadFrames {
		s.Fatalf("%s fork holds only %d frames of the noise-only lead (want >= %d)", who, n, minLeadFrames)
	}
	share := float64(loud) / float64(n)
	s.Logf("%s: %d of %d noise-only lead frames loud (%.0f%%)", who, loud, n, 100*share)
	return share
}

type noisyPrompt struct {
	noiseRMS float64
	lead     time.Duration // noise-only, before the speech
}

// writeNoisyWAV writes src over white noise at about 6 dB SNR, with 3s of
// noise before the speech and 1s after.
func writeNoisyWAV(src, dst string) (noisyPrompt, error) {
	const lead, tail = 3 * wav.SampleRate, wav.SampleRate
	lpcm, err := wav.Read(src)
	if err != nil {
		return noisyPrompt{}, err
	}
	speech := wav.Samples(lpcm)
	var e float64
	for _, v := range speech {
		e += float64(v) * float64(v)
	}
	rms := math.Sqrt(e/float64(len(speech))) * 0.5
	amp := rms * math.Sqrt(3) // uniform noise RMS = amp/sqrt(3)
	rng := rand.New(rand.NewSource(7))
	out := make([]int16, 0, len(speech)+lead+tail)
	mix := func(v int16) {
		n := float64(v) + amp*(rng.Float64()*2-1)
		out = append(out, int16(max(-32768, min(32767, n))))
	}
	for range lead {
		mix(0)
	}
	for _, v := range speech {
		mix(v)
	}
	for range tail {
		mix(0)
	}
	if err := wav.WriteSamples(dst, out); err != nil {
		return noisyPrompt{}, err
	}
	return noisyPrompt{noiseRMS: rms, lead: time.Duration(lead) * time.Second / wav.SampleRate}, nil
}
