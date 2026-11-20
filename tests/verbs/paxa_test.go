// Paxa Labs: TTS (one-shot say over HTTP, streaming say and agent over the
// /v1/tts/live socket) and realtime STT with native turn detection (gather,
// transcribe, agent). One credential serves both. Every test passes without
// exercising paxa when PAXA_API_KEY is unset.
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
	paxaSTTModel = "paxa-stt-lite-realtime-v1-preview"
	paxaTTSModel = "paxa-tts-flash-v2"
	// flash-v2 voices speak Thai, English and Mandarin; cookie is described
	// as an English support-line voice, nomyen is the Thai female lead
	paxaVoice     = "cookie"
	paxaThaiVoice = "nomyen"
)

// paxaThaiText is "Hello, this voice test is working correctly." in Thai.
const paxaThaiText = "สวัสดีค่ะ การทดสอบเสียงนี้ทำงานได้ถูกต้อง"

// requirePaxa logs and reports false when the key is unset; callers return.
func requirePaxa(t *testing.T) bool {
	t.Helper()
	if !cfg.HasPaxa() || paxaLabel == "" {
		t.Log("PAXA_API_KEY not set — passing without exercising paxa")
		return false
	}
	return true
}

func paxaRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "paxa",
		"label":    paxaLabel,
		"language": language,
	}
	if options != nil {
		r["paxaOptions"] = options
	}
	return r
}

func paxaSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "paxa",
		"label":    paxaLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_Say_Paxa — one-shot say over the HTTP leg on the default model.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-paxa",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", paxaSynth(paxaVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// paxaGather runs one gather on paxa with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func paxaGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-paxa")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		// keep the call up while a long clip is still streaming; the test hangs up
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
	// skip any early non-final speechDetected action; the gather's result is the final one
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

// TestVerb_Gather_Speech_Paxa — gather input=[speech] on paxa returns the
// spoken phrase; the final is Paxa's one is_final transcript for the turn.
//
// Steps:
//   - script-gather-speech-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	transcript := paxaGather(t, paxaRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// paxaThaiWAV is one Thai turn: "I'd like to book a table for two tomorrow night".
const paxaThaiWAV = "testdata/th_reservation.wav"

// paxaTranscribe runs transcribe on paxa with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func paxaTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-paxa")
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

func runPaxaAgentEcho(t *testing.T, synth map[string]any) {
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

	s = Step(t, "script-agent-verb-paxa")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          paxaRecognizer("en-US", nil),
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

// runPaxaScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runPaxaScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
