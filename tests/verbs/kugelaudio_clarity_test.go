// KugelAudio Clarity (clarity-1) noise isolation: vendor "kugelaudio" on the
// noiseIsolation config/agent option. The key comes from the account's
// kugelaudio speech credential. All tests skip without KUGELAUDIO_API_KEY.
//
// The free plan allows two concurrent Clarity streams, which is exactly what
// this file opens at peak (one per test).
package verbs

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// A frame counts as loud above half the noise RMS: the noise keeps every
// unfiltered frame loud, while a plain gain drop of a few dB would not
// silence any. Clarity must silence at least half the noise-only frames.
const (
	loudFraction       = 0.5
	minNoiseFramesGone = 0.5
)

// TestVerb_NoiseIsolation_Kugelaudio_Listen — the same noisy prompt (speech
// over white noise, with noise-only lead and tail) is sent on two calls that
// fork their inbound audio with `listen`: a control call, and one with
// config.noiseIsolation vendor=kugelaudio and the credential's label. Noise
// isolation runs ahead of the fork, so the noise-only stretches must go quiet
// in the filtered fork while the speech in it stays intelligible.
//
// Steps:
//  1. build-noisy-prompt
//  2. control:fork-noisy-prompt
//  3. clarity:fork-noisy-prompt
//  4. assert-noise-removed
//  5. assert-speech-kept
func TestVerb_NoiseIsolation_Kugelaudio_Listen(t *testing.T) {
	requireKugelaudio(t)
	requireWebhook(t)
	t.Parallel()
	ctx := WithTimeout(t, 150*time.Second)

	s := Step(t, "build-noisy-prompt")
	clean := krispEnsurePromptWAV(ctx, s)
	noisy := filepath.Join(t.TempDir(), "noisy-prompt.wav")
	np, err := writeNoisyWAV(clean, noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "clarity", noisy, map[string]any{
		"enable": true, "vendor": "kugelaudio", "label": kugelaudioLabel,
	})

	s = Step(t, "assert-noise-removed")
	thr := np.noiseRMS * loudFraction
	cl, fl := loudFrames(control, thr), loudFrames(filtered, thr)
	s.Logf("loud frames: sent %d (noise-only %d), control fork %d, clarity fork %d",
		np.frames, np.noiseOnlyFrames, cl, fl)
	if cl < np.frames*9/10 {
		s.Fatalf("control fork has only %d of %d loud frames: fork lost the audio, nothing to compare", cl, np.frames)
	}
	if fl == 0 {
		s.Fatalf("clarity fork is silent")
	}
	if want := int(float64(np.noiseOnlyFrames) * minNoiseFramesGone); cl-fl < want {
		s.Errorf("clarity silenced %d frames, want >= %d of the %d noise-only ones: noise was not removed",
			cl-fl, want, np.noiseOnlyFrames)
	}
	s.Done()

	s = Step(t, "assert-speech-kept")
	pcmPath := filepath.Join(t.TempDir(), "clarity-fork.pcm")
	if err := os.WriteFile(pcmPath, filtered, 0o644); err != nil {
		s.Fatalf("write fork audio: %v", err)
	}
	AssertTranscriptHasMost(s, ctx, pcmPath, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()
}

// forkNoisyPrompt runs one call: optional config.noiseIsolation, a listen
// fork to our WS, the noisy prompt from the UAS, hangup. Returns the forked
// L16 audio.
func forkNoisyPrompt(t *testing.T, ctx context.Context, leg, wav string, noise map[string]any) []byte {
	t.Helper()
	s := Step(t, leg+":fork-noisy-prompt")
	uas := claimUAS(t, ctx)
	// claimSession keys by test name; each leg needs its own session and WS
	testID := t.Name() + "-" + leg
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
	// the Clarity socket opens after config; give it time before speaking
	time.Sleep(2 * time.Second)
	sendAndHangup(s, call, wav)

	audio := webhook.BinaryConcat(collected())
	if len(audio) < 8000 {
		s.Fatalf("fork carried only %d bytes", len(audio))
	}
	s.Logf("fork audio: %d bytes", len(audio))
	s.Done()
	return audio
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

func sendAndHangup(s *StepCtx, call *jsip.Call, wav string) {
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(time.Second) // let the delayed tail of the filtered audio reach the fork
	_ = call.Hangup()
}

// TestVerb_Agent_NoiseIsolation_Kugelaudio — an agent with the shorthand
// noiseIsolation:"kugelaudio" (no label, so jambonz must find the suite's
// labelled credential) understands a prompt spoken over loud white noise and
// echoes the four words back. A background `listen` fork of the caller audio
// proves isolation actually ran: the noise-only stretches must go quiet in it.
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
	requireKugelaudio(t)
	requireWebhook(t)
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

	rec := RunAudioRoundtrip(t, ctx, call, AudioRoundtripOpts{
		PromptWAV: noisy,
		RecordTag: "kugelaudio-noise",
	})
	call.StopRecording()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-noise-removed")
	fork := webhook.BinaryConcat(collected())
	forkPath := filepath.Join(t.TempDir(), "caller-fork.pcm")
	if err := os.WriteFile(forkPath, fork, 0o644); err == nil && stt.HasKey() {
		if tr, err := stt.Transcribe(ctx, forkPath); err == nil {
			s.Logf("what the agent's STT was fed: %q", tr)
		}
	}
	fl := loudFrames(fork, np.noiseRMS*loudFraction)
	s.Logf("loud frames: sent %d (noise-only %d), caller fork %d", np.frames, np.noiseOnlyFrames, fl)
	if fl == 0 {
		s.Fatalf("caller fork is silent")
	}
	if limit := np.frames - int(float64(np.noiseOnlyFrames)*minNoiseFramesGone); fl > limit {
		s.Errorf("caller fork has %d loud frames, want <= %d: noise isolation did not run", fl, limit)
	}
	s.Done()
}

type noisyPrompt struct {
	noiseRMS        float64
	frames          int // 20ms frames written
	noiseOnlyFrames int // in the lead and tail
}

// writeNoisyWAV writes src (16-bit 8 kHz mono) over white noise at about
// 6 dB SNR, with 3s of noise before and 1s after.
func writeNoisyWAV(src, dst string) (noisyPrompt, error) {
	const lead, tail = 3 * 8000, 8000
	speech, err := readPCM16WAV(src)
	if err != nil {
		return noisyPrompt{}, err
	}
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
	if err := writePCM16WAV(dst, out); err != nil {
		return noisyPrompt{}, err
	}
	return noisyPrompt{noiseRMS: rms, frames: len(out) / 160, noiseOnlyFrames: (lead + tail) / 160}, nil
}

func readPCM16WAV(path string) ([]int16, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(raw) < 12 || string(raw[:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		return nil, fmt.Errorf("%s: not a WAV", path)
	}
	for off := 12; off+8 <= len(raw); {
		sz := int(binary.LittleEndian.Uint32(raw[off+4:]))
		body := raw[off+8 : min(off+8+sz, len(raw))]
		switch string(raw[off : off+4]) {
		case "fmt ":
			if binary.LittleEndian.Uint16(body[0:]) != 1 || binary.LittleEndian.Uint16(body[2:]) != 1 ||
				binary.LittleEndian.Uint32(body[4:]) != 8000 || binary.LittleEndian.Uint16(body[14:]) != 16 {
				return nil, fmt.Errorf("%s: want 16-bit 8 kHz mono PCM", path)
			}
		case "data":
			pcm := make([]int16, len(body)/2)
			for i := range pcm {
				pcm[i] = int16(binary.LittleEndian.Uint16(body[2*i:]))
			}
			return pcm, nil
		}
		off += 8 + sz + sz%2
	}
	return nil, fmt.Errorf("%s: no data chunk", path)
}

func writePCM16WAV(path string, pcm []int16) error {
	b := make([]byte, 0, 44+2*len(pcm))
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+2*len(pcm)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, 1) // mono
	b = binary.LittleEndian.AppendUint32(b, 8000)
	b = binary.LittleEndian.AppendUint32(b, 16000)
	b = binary.LittleEndian.AppendUint16(b, 2)
	b = binary.LittleEndian.AppendUint16(b, 16)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(2*len(pcm)))
	for _, v := range pcm {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return os.WriteFile(path, b, 0o644)
}

// loudFrames counts 20ms L16 frames whose RMS exceeds thr.
func loudFrames(b []byte, thr float64) int {
	n := 0
	for off := 0; off+320 <= len(b); off += 320 {
		var e float64
		for i := off; i < off+320; i += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(b[i:])))
			e += v * v
		}
		if math.Sqrt(e/160) > thr {
			n++
		}
	}
	return n
}
