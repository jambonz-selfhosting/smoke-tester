// Turn-detection audio-path probe, not a release gate: captures what a 16 kHz
// `listen` consumer receives for each phrase in $TURN_PROBE_WAV_DIR.
package verbs

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/wav"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func runTurnProbe(t *testing.T, wavPath, out string) {
	t.Helper()
	requireWebhook(t)
	lpcm, err := wav.Read(wavPath)
	if err != nil {
		msg := fmt.Sprintf("read %s: %v", wavPath, err)
		recordFailure(t, "read-wav", msg)
		t.Fatal(msg)
	}
	secs := len(lpcm)/(2*wav.SampleRate) + 1
	ctx := WithTimeout(t, time.Duration(secs+90)*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "script-listen-16k")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("listen", "url", wssURL(webhookSrv.PublicURL(), "/ws/"+url.PathEscape(testID)),
			"mixType", "mono", "sampleRate", 16000),
		V("pause", "length", secs+4),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(secs+45))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// listen drops audio sent before the WS is up, which would clip the phrase onset
	s = Step(t, "wait-for-ws-connect")
	connCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	err = sess.WSConnected(connCtx)
	ccancel()
	if err != nil {
		s.Fatalf("WS never connected: %v", err)
	}
	time.Sleep(RecognizerArmDelay)
	s.Done()

	// drain while sending: the session's inbound queue holds only ~5 s of frames
	drainCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	captured := make(chan []webhook.WSMessage, 1)
	go func() { captured <- sess.CollectWS(drainCtx) }()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	time.Sleep(1500 * time.Millisecond)
	s.Done()

	s = Step(t, "hangup-and-wait-ws-close")
	_ = call.Hangup()
	select {
	case <-sess.WSClosed():
	case <-time.After(10 * time.Second):
		s.Logf("WS still open after 10s — collecting what's been received")
	}
	s.Done()

	s = Step(t, "save-capture")
	var msgs []webhook.WSMessage
	select {
	case msgs = <-captured:
	case <-time.After(5 * time.Second):
		cancel()
		msgs = <-captured
	}
	audio := webhook.BinaryConcat(msgs)
	if len(audio) < 2*len(lpcm) {
		s.Fatalf("capture too short: %d bytes, want >= %d (the phrase at 16 kHz)", len(audio), 2*len(lpcm))
	}
	if rate := byteRate(msgs); rate < 24000 {
		s.Fatalf("capture arrives at %.0f B/s, want ~32000 (16 kHz L16)", rate)
	}
	if peak := peakAbs(wav.Samples(audio)); peak < 1000 {
		s.Fatalf("capture is silent: peak %d", peak)
	}
	if err := os.WriteFile(out, audio, 0o644); err != nil {
		s.Fatalf("write: %v", err)
	}
	s.Logf("saved %d bytes (%.2fs @16k L16) to %s", len(audio), float64(len(audio))/32000, out)
	s.Done()
}

// byteRate is binary bytes per second between the first and last binary frame.
func byteRate(msgs []webhook.WSMessage) float64 {
	var first, last time.Time
	n := 0
	for _, m := range msgs {
		if m.Kind != webhook.WSBinary {
			continue
		}
		if first.IsZero() {
			first = m.Received
		} else {
			n += len(m.Binary)
		}
		last = m.Received
	}
	if d := last.Sub(first).Seconds(); d > 0 {
		return float64(n) / d
	}
	return 0
}

func peakAbs(pcm []int16) int {
	peak := 0
	for _, v := range pcm {
		a := int(v)
		if a < 0 {
			a = -a
		}
		peak = max(peak, a)
	}
	return peak
}
