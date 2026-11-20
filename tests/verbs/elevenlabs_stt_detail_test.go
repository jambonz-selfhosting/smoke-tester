//go:build detail

package verbs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Elevenlabs_TurnPrediction — v3-only semantic turn
// detection; the API rejects it on scribe_v2_realtime.
func TestVerb_Gather_Speech_Elevenlabs_TurnPrediction(t *testing.T) {
	runElevenlabsGather(t, map[string]any{"commitStrategy": "turn_prediction"})
}

// TestVerb_Agent_Elevenlabs_TurnPrediction — on scribe_v3_live the agent defaults to
// turn_prediction and each commit ends the turn: one prompt yields exactly one turn_end.
func TestVerb_Agent_Elevenlabs_TurnPrediction(t *testing.T) {
	if !cfg.HasElevenlabs() || elevenlabsSttLabel == "" {
		t.Log("ELEVENLABS_API_KEY not set — passing without exercising elevenlabs STT")
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	prompt := agentEchoTurns[0].prompt
	wav, err := tts.EnsureWAV(ctx, "testdata/agent", prompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-elevenlabs-v3")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:      agentEchoSystemPrompt,
		NoResponseTimeout: &noReprompt,
		STT: map[string]any{
			"vendor":   "elevenlabs",
			"label":    elevenlabsSttLabel,
			"language": "en-US",
		},
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	s = Step(t, "speak-prompt-and-record-reply")
	rec := filepath.Join(t.TempDir(), "elevenlabs-v3-reply.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-turn-end")
	teCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cbs := WaitCallbacksUntil(teCtx, sess, func(seen []webhook.Callback) bool {
		return len(findAgentEvents(seen, "turn_end")) > 0
	})
	// a second turn_end here would mean the one prompt was split into two turns
	cbs = append(cbs, DrainCallbacks(sess, 3*time.Second)...)
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-one-turn-end")
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) != 1 {
		s.Fatalf("turn_end count = %d, want 1: %s", len(turnEnds), summarizeEventTypes(cbs))
	}
	transcript := strings.ToLower(turnEnds[0].String("transcript"))
	s.Logf("turn_end transcript=%q latency=%v", transcript, turnEnds[0].JSON["latency"])
	hits := 0
	for _, w := range contentWords(prompt) {
		if strings.Contains(transcript, strings.ToLower(w)) {
			hits++
		}
	}
	if hits < 2 {
		s.Errorf("turn_end transcript %q matched %d of %v; want >= 2", transcript, hits, contentWords(prompt))
	}
	s.Done()

	s = Step(t, "assert-agent-reply")
	AssertTranscriptHasMost(s, ctx, rec, 2, contentWords(prompt)...)
	s.Done()
}
