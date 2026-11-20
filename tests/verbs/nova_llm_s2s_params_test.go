// Nova Sonic tests that prove parameters and voice behaviour reach Nova and
// change what it does, rather than merely being accepted:
//   - voiceId: the recorded voice's pitch tells matthew (male) from tiffany
//   - invalid voiceId: Bedrock's ValidationException comes back as the verb's
//     server error, so llmOptions reach Nova verbatim
//   - history: the agent answers from a fact only the history contains
//   - barge-in: caller speech cuts the agent off (playback interrupted)
//   - handoff: the injected tool dials and bridges the human
package verbs

import (
	"context"
	"encoding/binary"
	"math"
	"os"
	"sort"
	"testing"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// novaCall places a webhook call running verb, answers it and starts silence.
func novaCall(t *testing.T, ctx context.Context, uas *UAS, sess *webhook.Session, verb any, hooks ...string) *jsip.Call {
	t.Helper()
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, append([]string{"llm"}, hooks...)...)
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		t.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		t.Fatalf("SendSilence: %v", err)
	}
	return call
}

// medianPitch8k estimates the median F0 of voiced 40ms frames in an 8kHz
// linear16 recording by normalized autocorrelation (70-350 Hz).
func medianPitch8k(t *testing.T, path string) (float64, int) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recording: %v", err)
	}
	x := make([]float64, len(raw)/2)
	for i := range x {
		x[i] = float64(int16(binary.LittleEndian.Uint16(raw[i*2:])))
	}
	const frame, hop, minLag, maxLag = 320, 160, 23, 114
	var f0s []float64
	for start := 0; start+frame+maxLag < len(x); start += hop {
		w := x[start : start+frame+maxLag]
		var energy float64
		for _, v := range w[:frame] {
			energy += v * v
		}
		if math.Sqrt(energy/frame) < 300 {
			continue
		}
		bestLag, best := 0, 0.0
		for lag := minLag; lag <= maxLag; lag++ {
			var num, e1, e2 float64
			for i := 0; i < frame; i++ {
				num += w[i] * w[i+lag]
				e1 += w[i] * w[i]
				e2 += w[i+lag] * w[i+lag]
			}
			if r := num / math.Sqrt(e1*e2+1e-9); r > best {
				best, bestLag = r, lag
			}
		}
		if best > 0.6 {
			f0s = append(f0s, 8000/float64(bestLag))
		}
	}
	if len(f0s) == 0 {
		return 0, 0
	}
	sort.Float64s(f0s)
	return f0s[len(f0s)/2], len(f0s)
}
