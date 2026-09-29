// ElevenLabs TTS. eleven_v3* and eleven_v4* stream only on the Text to
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

// TestVerb_Say_Stream_ElevenlabsV3Conversational — streaming say over Text to
// Dialogue; the [excited] audio tag must be performed, not read aloud.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV3Conversational(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v3conv", "eleven_v3_conversational", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsV3 — as above on eleven_v3.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV3(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v3", "eleven_v3", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsFlash — regression guard: a stream-input model
// still takes the old transport.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsFlash(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-flash", "eleven_flash_v2_5", true,
		"Streaming synthesis is working correctly.", "")
}

// TestVerb_Say_Stream_ElevenlabsV4Turbo — streaming say on eleven_v4_turbo over
// Text to Dialogue, with an audio tag that must not be read aloud.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV4Turbo(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v4turbo", "eleven_v4_turbo", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsV4 — as above on eleven_v4.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV4(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v4", "eleven_v4", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_ElevenlabsV4AndV3 — one-shot (HTTP) say on each v3/v4 model;
// /v1/text-to-speech serves them although stream-input does not.
//
// Steps (per model subtest):
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_ElevenlabsV4AndV3(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	for _, model := range []string{"eleven_v4_turbo", "eleven_v4", "eleven_v3"} {
		t.Run(model, func(t *testing.T) {
			t.Parallel()
			runElevenlabsSay(t, "say-11labs-"+model, model, false,
				"[excited] Speech synthesis is working correctly.", "excited")
		})
	}
}

func runElevenlabsSay(t *testing.T, tag, model string, stream bool, text, notSpoken string) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-streaming-say")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say", "text", text, "stream", stream, "synthesizer", elevenlabsSynth(model)),
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
	if !stt.HasKey() {
		s.Logf("skipping transcript assertion: %s unset", stt.EnvKey)
		s.Done()
		return
	}
	transcript, err := stt.Transcribe(ctx, wav)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", wav, err)
	}
	s.Logf("transcript: %q", transcript)
	for _, want := range []string{"synthesis", "working correctly"} {
		if !strings.Contains(transcript, stt.Normalize(want)) {
			s.Errorf("transcript missing %q", want)
		}
	}
	if notSpoken != "" && strings.Contains(transcript, stt.Normalize(notSpoken)) {
		s.Errorf("audio tag was read aloud: %q", transcript)
	}
	s.Done()
}

// TestVerb_Agent_ElevenlabsV3Conversational — a live agent on
// eleven_v3_conversational: a barge-in into the greeting (the socket is
// rotated), then a turn after 25s of silence. noResponseTimeout is off so no
// reprompt fills that gap; it outlasts the 20s TTD receive timeout unless
// keepalives are flowing.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. idle-past-ttd-timeout
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. hangup-and-wait-ended
func TestVerb_Agent_ElevenlabsV3Conversational(t *testing.T) {
	runElevenlabsAgent(t, "eleven_v3_conversational")
}

// TestVerb_Agent_ElevenlabsV4Turbo — the same agent flow on eleven_v4_turbo,
// the low-latency model built for agent loops.
//
// Steps: as TestVerb_Agent_ElevenlabsV3Conversational.
func TestVerb_Agent_ElevenlabsV4Turbo(t *testing.T) {
	runElevenlabsAgent(t, "eleven_v4_turbo")
}

func runElevenlabsAgent(t *testing.T, model string) {
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
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting:          true,
		BargeIn:           true,
		NoResponseTimeout: &noReprompt,
		TTS:               elevenlabsSynth(model),
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
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if lat, ok := te.JSON["latency"].(map[string]any); ok {
			s.Logf("%s turn_end tts_ms=%v", model, lat["tts_ms"])
		}
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
