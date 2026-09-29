// Tests for the `gather` and `transcribe` verbs with STT vendor "zoom"
// (Zoom Scribe Live mode).
//
// zoom is an OPTIONAL vendor (see config.HasZoom / provisionZoomCredential in
// verbsmain_test.go): when ZOOM_API_KEY is unset zoomLabel stays "" and both
// tests pass immediately after a log — never t.Skip, never a failure.
//
// Clones of the xai STT tests with the recognizer swapped to zoom. Zoom
// endpoints turns server-side and sends only finals (no interims), so the
// gather/transcribe flows are unchanged; timings stay on the LONG pad.
package verbs

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Gather_Speech_Zoom — stream a WAV into `gather input=[speech]`
// using recognizer vendor "zoom", assert the returned transcript contains
// the expected phrase. Clone of TestVerb_Gather_Speech (gather_speech_test.go)
// with the recognizer swapped to zoom.
func TestVerb_Gather_Speech_Zoom(t *testing.T) {
	if !cfg.HasZoom() || zoomLabel == "" {
		t.Log("ZOOM_API_KEY not set — passing without exercising zoom STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "load-ground-truth")
	wavPath, truthPath := resolveFixture(t, speechWAV), resolveFixture(t, speechTranscriptTxt)
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		s.Fatalf("read truth transcript: %v", err)
	}
	truth := strings.ToLower(strings.TrimSpace(string(truthBytes)))
	s.Logf("ground truth: %q", truth)
	s.Done()

	s = Step(t, "script-gather-speech-zoom")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "zoom",
				"label":    zoomLabel,
				"language": "en-US",
			}),
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
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
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
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	normalized := strings.ToLower(transcript)
	hits := 0
	for _, want := range []string{"sun", "shining"} {
		if strings.Contains(normalized, want) {
			hits++
		}
	}
	if hits == 0 {
		s.Errorf("transcript %q matched neither sun nor shining (truth=%q)", transcript, truth)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_Zoom — `transcribe` runs continuous STT via recognizer
// vendor "zoom" and posts each utterance to transcriptionHook. Clone of
// TestVerb_Transcribe_Basic (transcribe_test.go) with the recognizer
// swapped to zoom.
func TestVerb_Transcribe_Zoom(t *testing.T) {
	if !cfg.HasZoom() || zoomLabel == "" {
		t.Log("ZOOM_API_KEY not set — passing without exercising zoom STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-zoom")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":          "zoom",
				"label":           zoomLabel,
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
	// Zoom finalizes on its own end-of-turn; LONG pad so it is armed first.
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
	s.Done()

	s = Step(t, "post-speech-silence")
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
			if cb, err := tryPop(sess); err == nil {
				if cb.Hook != "action/transcription" {
					continue
				}
				s.Logf("action/transcription body: %s", string(cb.Body))
				if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
					parts = append(parts, seg)
					transcript = strings.Join(parts, " ")
				}
			}
		}
		if transcript != "" && transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	s = Step(t, "assert-transcript-sun-shining")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2",
			transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Agent_Echo_Zoom — the agent verb with zoom as its STT. Zoom sends
// speech_started (forwarded as the agent's speechStarted) plus one final per
// server-detected turn, and nothing else, so this proves those two signals
// alone drive a full turn: user speaks, LLM replies, TTS echoes it back.
func TestVerb_Agent_Echo_Zoom(t *testing.T) {
	if !cfg.HasZoom() || zoomLabel == "" {
		t.Log("ZOOM_API_KEY not set — passing without exercising zoom STT")
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	wavs := make([]string, len(agentEchoTurns))
	for i, turn := range agentEchoTurns {
		s = Step(t, "ensure-prompt-wav")
		path, err := tts.EnsureWAV(ctx, "testdata/agent", turn.prompt, tts.PromptOptions{
			Model: "aura-asteria-en",
		})
		if err != nil {
			s.Fatalf("EnsureWAV turn %d: %v", i+1, err)
		}
		wavs[i] = path
		s.Done()
	}

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-zoom")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT: map[string]any{
			"vendor":   "zoom",
			"label":    zoomLabel,
			"language": "en-US",
		},
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
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

	for i, turn := range agentEchoTurns {
		recPath := filepath.Join(t.TempDir(), formatAgentTurnRecPath(i+1))

		s = Step(t, formatAgentTurnStep(i+1, "record-and-speak"))
		if err := call.StartRecording(recPath); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (pre): %v", err)
		}
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, formatAgentTurnStep(i+1, "assert-echo"))
		AssertTranscriptHasMost(s, ctx, recPath, 2, contentWords(turn.prompt)...)
		s.Done()
	}

	HangupAndWaitEnded(t, ctx, call)
}
