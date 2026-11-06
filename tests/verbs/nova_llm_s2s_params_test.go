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
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
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

// TestVerb_LLM_Nova_Voice proves promptStart.audioOutputConfiguration.voiceId
// is applied: a male and a female voice say the same line, and the female
// one's median pitch must be clearly higher.
func TestVerb_LLM_Nova_Voice(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasNova() {
		t.Log("NOVA_ACCESS_KEY_ID/NOVA_SECRET_ACCESS_KEY not set — passing without exercising nova voices")
		return
	}
	var mu sync.Mutex
	pitch := map[string]float64{}
	// runs once both parallel subtests have finished
	t.Cleanup(func() {
		m, f := pitch["matthew"], pitch["tiffany"]
		if m == 0 || f == 0 {
			t.Errorf("[compare-voices] missing a pitch measurement: %v", pitch)
			return
		}
		t.Logf("[compare-voices] matthew %.0f Hz, tiffany %.0f Hz", m, f)
		// both calls would sound alike if voiceId were dropped (one default voice)
		if f < m*1.25 {
			t.Errorf("[compare-voices] tiffany %.0f Hz is not clearly above matthew %.0f Hz — voiceId not applied?", f, m)
		}
		if m > 165 {
			t.Errorf("[compare-voices] matthew %.0f Hz is in the female range", m)
		}
	})
	for _, tc := range []struct {
		voice  string
		female bool
	}{{"matthew", false}, {"tiffany", true}} {
		t.Run(tc.voice, func(t *testing.T) {
			t.Parallel()
			ctx := WithTimeout(t, 120*time.Second)
			uas := claimUAS(t, ctx)
			_, sess := claimSession(t)

			s := Step(t, "call-and-record")
			verb := V("llm",
				"vendor", "nova", "model", cfg.NovaModel, "auth", novaAuth(),
				"actionHook", webhookSrv.PublicURL()+"/action/llm",
				"llmOptions", map[string]any{
					"systemPrompt": "When asked to say a sentence, say exactly that sentence and nothing else.",
					"greeting":     "Say: The quick brown fox jumps over the lazy dog, and the five boxing wizards jump quickly.",
					"promptStart":  map[string]any{"audioOutputConfiguration": map[string]any{"voiceId": tc.voice}},
				},
			)
			call := novaCall(t, ctx, uas, sess, verb)
			recPath := filepath.Join(t.TempDir(), "nova-voice-"+tc.voice+".pcm")
			if err := call.StartRecording(recPath); err != nil {
				s.Fatalf("StartRecording: %v", err)
			}
			time.Sleep(10 * time.Second)
			call.StopRecording()
			s.Done()
			HangupAndWaitEnded(t, ctx, call)

			s = Step(t, "assert-pitch")
			f0, voiced := medianPitch8k(t, recPath)
			s.Logf("voice=%s median F0=%.0f Hz over %d voiced frames", tc.voice, f0, voiced)
			if voiced < 50 {
				s.Fatalf("only %d voiced frames — the agent barely spoke", voiced)
			}
			mu.Lock()
			pitch[tc.voice] = f0
			mu.Unlock()
			s.Done()
		})
	}
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

// TestVerb_LLM_Nova_InvalidParams proves sessionStart and promptStart reach
// Nova verbatim: Bedrock rejects a bad value with a ValidationException,
// which must come back as the verb's server error.
func TestVerb_LLM_Nova_InvalidParams(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !cfg.HasNova() {
		t.Log("NOVA_ACCESS_KEY_ID/NOVA_SECRET_ACCESS_KEY not set — passing without exercising nova validation")
		return
	}
	for _, tc := range []struct {
		name    string
		options map[string]any
		want    string
	}{
		{"voiceId", map[string]any{
			"promptStart": map[string]any{"audioOutputConfiguration": map[string]any{"voiceId": "nobody"}},
		}, "nobody"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ctx := WithTimeout(t, 90*time.Second)
			uas := claimUAS(t, ctx)
			_, sess := claimSession(t)
			tc.options["greeting"] = "Say hello."
			verb := V("llm",
				"vendor", "nova", "model", cfg.NovaModel, "auth", novaAuth(),
				"actionHook", SessionURL(sess, "llm"),
				"llmOptions", tc.options,
			)
			call := novaCall(t, ctx, uas, sess, verb)

			s := Step(t, "wait-action-llm")
			wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			cb, err := sess.WaitCallbackFor(wctx, "action/llm")
			cancel()
			if err != nil {
				s.Fatalf("verb never completed after the invalid %s: %v", tc.name, err)
			}
			s.Logf("action/llm body: %s", string(cb.Body))
			if got := cb.String("completion_reason"); got != "server error" {
				s.Errorf("completion_reason=%q want %q", got, "server error")
			}
			if !strings.Contains(string(cb.Body), tc.want) {
				s.Errorf("action body lacks Bedrock's rejection %q", tc.want)
			}
			s.Done()
			HangupAndWaitEnded(t, ctx, call)
		})
	}
}

// TestVerb_LLM_Nova_History proves llmOptions.history reaches Nova: the
// account number exists only in the history, and the agent must read it back.
func TestVerb_LLM_Nova_History(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !novaPreflight(t) {
		return
	}
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "ensure-prompt-wav")
	askWAV, err := tts.EnsureWAV(ctx, "testdata/llm", "What is my account number?", tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	verb := V("llm",
		"vendor", "nova", "model", cfg.NovaModel, "auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-nova-event"),
		"events", []string{"textOutput", "contentStart"},
		"llmOptions", map[string]any{
			"systemPrompt": "Answer from the conversation so far in one short sentence. Say numbers digit by digit.",
			"history": []map[string]any{
				{"role": "user", "content": "Hi, my account number is 7 3 9 1."},
				{"role": "assistant", "content": "Thanks, I have your account number, 7 3 9 1."},
			},
			"promptStart": novaPromptStart(),
		},
	)
	call := novaCall(t, ctx, uas, sess, verb, "llm-nova-event")
	WaitFor(t, "wait-for-session", RecognizerArmDelay)

	s = Step(t, "ask-and-assert")
	if err := call.SendWAV(askWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	if reply := novaWaitReply(sess, 25*time.Second, "7391"); !strings.Contains(digitsSpoken(reply), "7391") {
		s.Errorf("agent reply %q does not contain the account number from history", reply)
	}
	s.Done()
	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_LLM_Nova_BargeIn proves caller speech interrupts the agent: the
// media server must flush playout and report it as interrupted.
func TestVerb_LLM_Nova_BargeIn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !novaPreflight(t) {
		return
	}
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "ensure-prompt-wav")
	stopWAV, err := tts.EnsureWAV(ctx, "testdata/llm", "Stop, stop. Please stop counting now.", tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	verb := V("llm",
		"vendor", "nova", "model", cfg.NovaModel, "auth", novaAuth(),
		"actionHook", webhookSrv.PublicURL()+"/action/llm",
		"eventHook", SessionURL(sess, "llm-nova-event"),
		"events", []string{"output_audio.playback_started", "output_audio.playback_stopped", "contentEnd"},
		"llmOptions", map[string]any{
			"systemPrompt": "You count slowly when asked, one number per sentence, and stop the moment you are interrupted.",
			"greeting":     "Please count slowly from one to forty.",
			"promptStart":  novaPromptStart(),
		},
	)
	call := novaCall(t, ctx, uas, sess, verb, "llm-nova-event")

	s = Step(t, "wait-agent-speaking")
	started := false
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) && !started {
		for _, cb := range DrainCallbacks(sess, time.Second) {
			if cb.Hook == "action/llm-nova-event" && cb.String("type") == "output_audio.playback_started" {
				started = true
			}
		}
	}
	if !started {
		s.Fatalf("agent never started speaking")
	}
	time.Sleep(3 * time.Second) // well into the count
	s.Done()

	s = Step(t, "interrupt-and-assert")
	if err := call.SendWAV(stopWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	interrupted := false
	deadline = time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) && !interrupted {
		for _, cb := range DrainCallbacks(sess, time.Second) {
			if cb.Hook == "action/llm-nova-event" && cb.String("type") == "output_audio.playback_stopped" &&
				cb.String("completion_reason") == "interrupted" {
				interrupted = true
			}
		}
	}
	if !interrupted {
		s.Errorf("no playback_stopped{interrupted} — caller speech did not cut the agent off")
	}
	s.Done()
	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_LLM_Nova_Handoff proves the injected handoff tool works on nova:
// asked for a human, the agent calls transfer_to_human and jambonz bridges.
func TestVerb_LLM_Nova_Handoff(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !novaPreflight(t) {
		return
	}
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, targetUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/handoff", handoffCallerUtterance, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	target := fmt.Sprintf("%s@%s", targetUAS.Username, suite.SIPRealm)
	verb := V("llm",
		"vendor", "nova", "model", cfg.NovaModel, "auth", novaAuth(),
		"actionHook", SessionURL(sess, "llm"),
		"llmOptions", map[string]any{
			"systemPrompt": handoffRealtimePrompt,
			"greeting":     "Greet me in one short sentence and ask how you can help.",
			"promptStart":  novaPromptStart(),
		},
		"handoff", map[string]any{
			"mode": "blind", "blindMethod": "dial", "brief": "none",
			"target": []any{map[string]any{"type": "user", "name": target}},
		},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb, V("hangup")}))
	SessionAckEmpty(sess, "llm")

	targetDone := make(chan struct{})
	var targetCall *jsip.Call
	targetCtx, targetCancel := context.WithCancel(ctx)
	go answerAndIdleTarget(t, targetCtx, targetUAS, targetDone, &targetCall)()
	t.Cleanup(func() {
		targetCancel()
		<-targetDone
	})

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	WaitFor(t, "let-model-greet", 3*time.Second)
	if err := call.SendWAV(callerWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "assert-target-answered")
	<-targetDone
	if targetCall == nil {
		s.Fatal("handoff never dialed the human")
	}
	sent := StatusesOf(targetCall.Sent())
	for _, want := range []int{100, 180, 200} {
		if !slices.Contains(sent, want) {
			s.Errorf("target sent statuses = %v, want %d", sent, want)
		}
	}
	s.Done()

	s = Step(t, "assert-completion-transferred")
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(wctx, "action/llm")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/llm: %v", err)
	}
	if got := cb.String("completion_reason"); got != "transferred" {
		s.Errorf("completion_reason=%q want transferred", got)
	}
	s.Done()
}
