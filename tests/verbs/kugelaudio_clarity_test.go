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
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// clarityMaxEnergyRatio: Clarity must leave at most this share of the
// fork energy the unfiltered control call carries for the same noisy prompt.
const clarityMaxEnergyRatio = 0.6

// TestVerb_NoiseIsolation_Kugelaudio_Listen — the same noisy prompt (speech
// over white noise, with noise-only lead and tail) is sent on two calls that
// fork their inbound audio with `listen`: a control call, and one with
// config.noiseIsolation vendor=kugelaudio and the credential's label. Noise isolation runs ahead of the
// fork, so the filtered fork must carry far less energy than the control
// while the speech in it stays intelligible.
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
	sentEnergy, err := writeNoisyWAV(clean, noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	control := forkNoisyPrompt(t, ctx, "control", noisy, nil)
	filtered := forkNoisyPrompt(t, ctx, "clarity", noisy, map[string]any{
		"enable": true, "vendor": "kugelaudio", "label": kugelaudioLabel,
	})

	s = Step(t, "assert-noise-removed")
	ce, fe := pcmEnergy(control), pcmEnergy(filtered)
	s.Logf("energy: sent %.3g, control fork %.3g, clarity fork %.3g (ratio %.2f)",
		sentEnergy, ce, fe, fe/ce)
	if ce < sentEnergy*0.5 {
		s.Fatalf("control fork carried %.3g of %.3g sent: fork lost the audio, nothing to compare", ce, sentEnergy)
	}
	if fe == 0 {
		s.Fatalf("clarity fork is silent")
	}
	if fe > ce*clarityMaxEnergyRatio {
		s.Errorf("clarity fork kept %.0f%% of the control energy (want <= %.0f%%): noise was not removed",
			100*fe/ce, 100*clarityMaxEnergyRatio)
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
	testID, sess := claimSession(t)

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

	select {
	case <-sess.WSClosed():
	case <-time.After(10 * time.Second):
		s.Logf("WS still open 10s after hangup; using what arrived")
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	audio := webhook.BinaryConcat(sess.CollectWS(dctx))
	if len(audio) < 8000 {
		s.Fatalf("fork carried only %d bytes", len(audio))
	}
	s.Logf("fork audio: %d bytes", len(audio))
	s.Done()
	return audio
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
// proves isolation actually ran: it must carry far less energy than was sent.
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
	sentEnergy, err := writeNoisyWAV(krispEnsurePromptWAV(ctx, s), noisy)
	if err != nil {
		s.Fatalf("noisy prompt: %v", err)
	}
	s.Done()

	testID, sess := claimSession(t)

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
	select {
	case <-sess.WSClosed():
	case <-time.After(10 * time.Second):
	}
	dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	fe := pcmEnergy(webhook.BinaryConcat(sess.CollectWS(dctx)))
	s.Logf("energy: sent %.3g, forked caller audio %.3g (ratio %.2f)", sentEnergy, fe, fe/sentEnergy)
	if fe == 0 {
		s.Fatalf("caller fork is silent")
	}
	if fe > sentEnergy*clarityMaxEnergyRatio {
		s.Errorf("caller fork kept %.0f%% of the sent energy (want <= %.0f%%): noise isolation did not run",
			100*fe/sentEnergy, 100*clarityMaxEnergyRatio)
	}
	s.Done()
}

// writeNoisyWAV writes src (16-bit 8 kHz mono) over white noise at about
// 6 dB SNR, with 2s of noise before and 1s after. Returns the written energy.
func writeNoisyWAV(src, dst string) (float64, error) {
	speech, err := readPCM16WAV(src)
	if err != nil {
		return 0, err
	}
	var e float64
	for _, v := range speech {
		e += float64(v) * float64(v)
	}
	amp := math.Sqrt(e/float64(len(speech))) * 0.5 * math.Sqrt(3) // uniform noise RMS = amp/sqrt(3)
	rng := rand.New(rand.NewSource(7))
	out := make([]int16, 0, len(speech)+3*8000)
	mix := func(v int16) {
		n := float64(v) + amp*(rng.Float64()*2-1)
		out = append(out, int16(max(-32768, min(32767, n))))
	}
	for range 2 * 8000 {
		mix(0)
	}
	for _, v := range speech {
		mix(v)
	}
	for range 8000 {
		mix(0)
	}
	if err := writePCM16WAV(dst, out); err != nil {
		return 0, err
	}
	var sent float64
	for _, v := range out {
		sent += float64(v) * float64(v)
	}
	return sent, nil
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

// pcmEnergy sums the squares of L16 little-endian samples.
func pcmEnergy(b []byte) float64 {
	var e float64
	for i := 0; i+1 < len(b); i += 2 {
		v := float64(int16(binary.LittleEndian.Uint16(b[i:])))
		e += v * v
	}
	return e
}
