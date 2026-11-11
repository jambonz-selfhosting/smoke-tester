// Tests for the `gather` and `transcribe` verbs with STT vendor "elevenlabs"
// on Scribe v3 Live. Pass without exercising anything when ELEVENLABS_API_KEY
// is unset (plain return, same convention as the xai STT tests).
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

// elevenlabsSttModel is the credential's stt_model_id for every test here.
const elevenlabsSttModel = "scribe_v3_live"

// TestVerb_Gather_Speech_Elevenlabs — default (vad) commit strategy.
func TestVerb_Gather_Speech_Elevenlabs(t *testing.T) {
	runElevenlabsGather(t, nil)
}

// TestVerb_Gather_Speech_Elevenlabs_TurnPrediction — v3-only semantic turn
// detection; the API rejects it on scribe_v2_realtime.
func TestVerb_Gather_Speech_Elevenlabs_TurnPrediction(t *testing.T) {
	runElevenlabsGather(t, map[string]any{"commitStrategy": "turn_prediction"})
}

func runElevenlabsGather(t *testing.T, opts map[string]any) {
	if !cfg.HasElevenlabs() || elevenlabsSttLabel == "" {
		t.Log("ELEVENLABS_API_KEY not set — passing without exercising elevenlabs STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-elevenlabs")
	recognizer := map[string]any{
		"vendor":   "elevenlabs",
		"label":    elevenlabsSttLabel,
		"language": "en-US",
	}
	if opts != nil {
		recognizer["elevenlabsOptions"] = opts
	}
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(resolveFixture(t, speechWAV)); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-transcript-sun-shining")
	transcript := strings.ToLower(extractTranscript(cb))
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	if !strings.Contains(transcript, "sun") && !strings.Contains(transcript, "shining") {
		s.Errorf("transcript %q matched neither sun nor shining", transcript)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_Elevenlabs — continuous STT posting to transcriptionHook.
func TestVerb_Transcribe_Elevenlabs(t *testing.T) {
	if !cfg.HasElevenlabs() || elevenlabsSttLabel == "" {
		t.Log("ELEVENLABS_API_KEY not set — passing without exercising elevenlabs STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-elevenlabs")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":          "elevenlabs",
				"label":           elevenlabsSttLabel,
				"language":        "en-US",
				"singleUtterance": true,
			}),
		V("pause", "length", 15),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/transcribe", transcribeText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hook")
	deadline := time.Now().Add(30 * time.Second)
	var parts []string
	transcript := ""
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			if cb, err := tryPop(sess); err == nil && cb.Hook == "action/transcription" {
				s.Logf("action/transcription body: %s", string(cb.Body))
				if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
					parts = append(parts, seg)
					transcript = strings.Join(parts, " ")
				}
			}
		}
		if transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	s = Step(t, "assert-transcript")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
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
