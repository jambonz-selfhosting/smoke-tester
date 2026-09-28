// ElevenLabs streaming TTS. eleven_v3* models are served only on the Text to
// Dialogue websocket, the rest on TTS stream-input; mediajam picks the
// transport from model_id. All tests skip without ELEVENLABS_API_KEY.
package verbs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func requireElevenlabs(t *testing.T) {
	t.Helper()
	if !cfg.HasElevenlabs() || elevenlabsLabel == "" {
		t.Skip("ElevenLabs tests need ELEVENLABS_API_KEY (credential missing — skipping, not a failure)")
	}
}

func elevenlabsSynth(model string) map[string]any {
	return map[string]any{
		"vendor":  "elevenlabs",
		"label":   elevenlabsLabel,
		"voice":   elevenlabsVoice,
		"options": map[string]any{"model_id": model},
	}
}

// The [excited] audio tag must be performed, not read aloud.
func TestVerb_Say_Stream_ElevenlabsV3Conversational(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsStreamingSay(t, "say-stream-11labs-v3conv", "eleven_v3_conversational",
		"[excited] Streaming synthesis is working correctly.", "excited")
}

func TestVerb_Say_Stream_ElevenlabsV3(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsStreamingSay(t, "say-stream-11labs-v3", "eleven_v3",
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// Regression guard: a stream-input model still takes the old transport.
func TestVerb_Say_Stream_ElevenlabsFlash(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsStreamingSay(t, "say-stream-11labs-flash", "eleven_flash_v2_5",
		"Streaming synthesis is working correctly.", "")
}

func runElevenlabsStreamingSay(t *testing.T, tag, model, text, notSpoken string) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-streaming-say")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say", "text", text, "stream", true, "synthesizer", elevenlabsSynth(model)),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(30))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, 1*time.Second, 12*time.Second, tag)
	s.Done()

	if wav == "" {
		return
	}
	s = Step(t, "assert-transcript")
	AssertTranscriptContains(s, ctx, wav, "streaming", "working correctly")
	if notSpoken != "" && stt.HasKey() {
		transcript, err := stt.Transcribe(ctx, wav)
		if err != nil {
			s.Fatalf("stt.Transcribe: %v", err)
		}
		if strings.Contains(transcript, stt.Normalize(notSpoken)) {
			s.Errorf("audio tag was read aloud: %q", transcript)
		}
	}
	s.Done()
}

// A live agent on eleven_v3_conversational: a barge-in into the greeting
// (the socket is rotated), then a turn after 25s of silence, which outlasts
// the Text to Dialogue 20s receive timeout unless keepalives are flowing.
func TestVerb_Agent_ElevenlabsV3Conversational(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wavs")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	idlePrompt := agentEchoTurns[1].prompt
	idleWAV, err := tts.EnsureWAV(ctx, "testdata/agent", idlePrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting: true,
		BargeIn:  true,
		TTS:      elevenlabsSynth("eleven_v3_conversational"),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-into-greeting", 4*time.Second)

	s = Step(t, "barge-in-and-record-reply")
	bargeRec := filepath.Join(t.TempDir(), "11labs-barge-reply.pcm")
	if err := call.StartRecording(bargeRec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(bargeWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-barge-in-reply")
	AssertTranscriptHasMost(s, ctx, bargeRec, 2, "alpha", "bravo", "charlie", "delta")
	s.Done()

	s = Step(t, "assert-user-interruption")
	cbs := DrainCallbacks(sess, time.Second)
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		s.Errorf("no user_interruption event: %s", summarizeEventTypes(cbs))
	}
	s.Done()

	WaitFor(t, "idle-past-ttd-timeout", 25*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "11labs-idle-reply.pcm")
	if err := call.StartRecording(idleRec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	if err := call.SendWAV(idleWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-idle-turn-reply")
	AssertTranscriptHasMost(s, ctx, idleRec, 2, contentWords(idlePrompt)...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
