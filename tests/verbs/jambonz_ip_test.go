package verbs

import (
	"testing"
	"time"
)

// TestVerb_Jambonz_InterruptPrediction — turnDetection "jambonz" with
// bargeIn.strategy=interruptPrediction, vendor "jambonz": the in-house
// interrupt model in mediajam's jambonz turn-taking session must judge real
// speech over the agent's greeting a genuine interruption.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (turnDetection=jambonz, bargeIn interruptPrediction/jambonz)
//  4. place-call
//  5. answer-and-arm
//  6. wait-into-greeting
//  7. speak-over-greeting
//  8. drain-agent-events
//  9. assert-user-interruption
//  10. hangup-and-wait-ended
func TestVerb_Jambonz_InterruptPrediction(t *testing.T) {
	jambonzInterrupt(t, map[string]any{"turnDetection": "jambonz"})
}

// TestVerb_Jambonz_InterruptPredictionOnly — the same barge-in with STT turn
// detection: feature-server starts an interrupt-only jambonz session.
func TestVerb_Jambonz_InterruptPredictionOnly(t *testing.T) {
	jambonzInterrupt(t, map[string]any{})
}

func jambonzInterrupt(t *testing.T, extra map[string]any) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	fields := map[string]any{
		"bargeIn": map[string]any{
			"enable":   true,
			"strategy": "interruptPrediction",
			"vendor":   "jambonz",
		},
	}
	for k, v := range extra {
		fields[k] = v
	}
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting: true,
	}, fields))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-arm")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-greeting")
	time.Sleep(3 * time.Second) // LLM first token + a couple of seconds of TTS in flight
	s.Done()

	s = Step(t, "speak-over-greeting")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "drain-agent-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	cbs := DrainCallbacks(sess, 15*time.Second)
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-user-interruption")
	if intr := findAgentEvents(cbs, "user_interruption"); len(intr) == 0 {
		s.Errorf("no user_interruption in %d events (%s): the jambonz interrupt model scored no barge-in; "+
			"check mediajam for 'jambonz ip' errors (model /usr/local/share/jambonz/jambonz-ip-v1.onnx)",
			len(cbs), summarizeEventTypes(cbs))
	} else {
		s.Logf("user_interruption fired %d time(s) under jambonz interruptPrediction", len(intr))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
