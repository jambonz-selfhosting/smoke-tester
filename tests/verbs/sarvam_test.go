// Sarvam AI: TTS (one-shot say over HTTP /text-to-speech/stream, streaming
// say and agent over the /text-to-speech/ws socket) and realtime STT (gather,
// transcribe, agent). One credential serves both. Every test passes without
// exercising sarvam when SARVAM_API_KEY is unset.
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

const (
	sarvamSTTModel   = "saaras:v3-realtime"
	sarvamSTTModelV4 = "saaras:v4"
	sarvamTTSModel   = "bulbul:v4-flash"
	// default personas of the bundled v4-flash voice list for en-IN and hi-IN
	sarvamVoice      = "simran_en_customer"
	sarvamHindiVoice = "kavya_hi_conversational"
)

// sarvamHindiText is "Hello, this voice test is working correctly." in Hindi.
const sarvamHindiText = "नमस्ते, यह आवाज़ परीक्षण ठीक से काम कर रहा है।"

// requireSarvam logs and reports false when the key is unset; callers return.
func requireSarvam(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSarvam() || sarvamLabel == "" {
		t.Log("SARVAM_API_KEY not set — passing without exercising sarvam")
		return false
	}
	return true
}

func sarvamRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "sarvam",
		"label":    sarvamLabel,
		"language": language,
	}
	if options != nil {
		r["sarvamOptions"] = options
	}
	return r
}

func sarvamSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "sarvam",
		"label":    sarvamLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Sarvam — one-shot say over the HTTP leg; en-US maps to en-IN.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-sarvam",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", sarvamSynth(sarvamVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// sarvamGather runs one gather on sarvam with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func sarvamGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-sarvam")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		V("pause", "length", 20),
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

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	var cb webhook.Callback
	for {
		var err error
		if cb, err = sess.WaitCallbackFor(waitCtx, "action/gather"); err != nil {
			s.Fatalf("WaitCallbackFor action/gather: %v", err)
		}
		if speech, _ := cb.JSON["speech"].(map[string]any); speech == nil || speech["is_final"] != false {
			break
		}
		s.Logf("early action/gather: %s", string(cb.Body))
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	_ = call.Hangup()
	return extractTranscript(cb)
}

// TestVerb_Gather_Speech_Sarvam — gather on sarvam with the jambonz en-US
// language, which mediajam maps to en-IN, returns the spoken phrase.
//
// Steps:
//   - script-gather-speech-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	transcript := sarvamGather(t, sarvamRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// sarvamTranscribe runs transcribe on sarvam with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func sarvamTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-sarvam")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", recognizer),
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

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

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
	transcript, interims := "", 0
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			cb, err := tryPop(sess)
			if err != nil || cb.Hook != "action/transcription" {
				continue
			}
			s.Logf("action/transcription body: %s", string(cb.Body))
			if strings.Contains(string(cb.Body), `"is_final":false`) {
				interims++
				continue
			}
			if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
				parts = append(parts, seg)
				transcript = strings.Join(parts, " ")
			}
		}
		if transcript != "" && transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	_ = call.Hangup()
	return transcript, interims
}

func runSarvamAgentEcho(t *testing.T, synth map[string]any) {
	t.Helper()
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

	s = Step(t, "script-agent-verb-sarvam")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          sarvamRecognizer("en-US", nil),
		TTS:          synth,
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

// runSarvamScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runSarvamScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
	t.Helper()
	ctx := WithTimeout(t, 45*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-say")
	script := webhook.Script{}
	for _, v := range says {
		script = append(script, v)
	}
	sess.ScriptCallHook(WithWarmupScript(append(script, V("hangup"))))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(45))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, minDur, maxDur, tag)
	s.Done()

	if wav != "" && len(wantOrdered) > 0 {
		s = Step(t, "assert-transcript-ordered")
		AssertTranscriptContainsInOrder(s, ctx, wav, wantOrdered...)
		s.Done()
	}
}
