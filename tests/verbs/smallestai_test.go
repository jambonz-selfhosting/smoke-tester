// Smallest AI: Lightning TTS (one-shot say over HTTP /waves/v1/tts, streaming
// say and agent over the /waves/v1/tts/live socket) and Pulse realtime STT
// (gather, transcribe, agent). One credential serves both. Every test passes without
// exercising smallestai when SMALLESTAI_API_KEY is unset.
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
	smallestaiSTTModel   = "pulse"
	smallestaiSTTModelV2 = "pulse-2"
	smallestaiTTSModel   = "lightning_v3.1_pro"
	// default voices of the bundled Pro catalog for en-US and hi-IN
	smallestaiVoice      = "kaitlyn"
	smallestaiHindiVoice = "rhea"
)

// smallestaiHindiText is "Hello, this voice test is working correctly." in Hindi.
const smallestaiHindiText = "नमस्ते, यह आवाज़ परीक्षण ठीक से काम कर रहा है।"

// requireSmallestai logs and reports false when the key is unset; callers return.
func requireSmallestai(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSmallestai() || smallestaiLabel == "" {
		t.Log("SMALLESTAI_API_KEY not set — passing without exercising smallestai")
		return false
	}
	return true
}

func smallestaiRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "smallestai",
		"label":    smallestaiLabel,
		"language": language,
	}
	if options != nil {
		r["smallestaiOptions"] = options
	}
	return r
}

func smallestaiSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "smallestai",
		"label":    smallestaiLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Smallestai — the api-server's STT (real audio to a
// final) and TTS (synthAudio) credential tests pass for the provisioned key.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, smallestaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, smallestaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Smallestai — one-shot say over the HTTP leg; en-US maps to en.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-smallestai",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", smallestaiSynth(smallestaiVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Smallestai_Options — every HTTP synthesizer option reaches the
// body; a rejected option fails the request and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Smallestai_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-smallestai-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse.",
			"synthesizer", smallestaiSynth(smallestaiVoice, "en-US", map[string]any{
				"model_id":                      smallestaiTTSModel,
				"language":                      "en",
				"speed":                         1.1,
				"number_pronunciation_language": "en",
				"date_format":                   "US",
				"math_notation":                 false,
				"content_filter":                map[string]any{"enabled": true, "action": "flag"},
			})),
		wantWords: []string{"warehouse", "ships today"},
	})
}

// TestVerb_Say_Smallestai_Hindi — one-shot Hindi say on a hi-IN voice.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Smallestai_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-smallestai-hi",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", smallestaiHindiText,
			"synthesizer", smallestaiSynth(smallestaiHindiVoice, "hi-IN", nil)),
	})
}

// TestVerb_Say_Stream_Smallestai — streaming say over the websocket leg.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-smallestai", smallestaiSynth(smallestaiVoice, "en-US", nil))
}

// TestVerb_Say_Stream_Smallestai_Options — the WS-only buffering options and
// the shared ones in every frame; a rejected frame yields an error status and
// nothing is heard.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Smallestai_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-smallestai-options", smallestaiSynth(smallestaiVoice, "en-US", map[string]any{
		"speed":               0.9,
		"language":            "en",
		"date_format":         "US",
		"max_buffer_flush_ms": 200,
		"complete_backoff_ms": 500,
		"timeout":             60,
	}))
}

// TestVerb_Say_Stream_Smallestai_TwoTurns — two streaming says on one call; the
// socket closes after each turn's complete, so the second says on a new one.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Smallestai_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	synth := smallestaiSynth(smallestaiVoice, "en-US", nil)
	runSmallestaiScript(t, "say-stream-smallestai-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Smallestai_MultiSentence — one streaming say of several
// sentences, all heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Smallestai_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runSmallestaiScript(t, "say-stream-smallestai-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", smallestaiSynth(smallestaiVoice, "en-US", nil)))
}

// TestVerb_Say_Stream_Smallestai_Hindi — Hindi over the socket on a hi-IN voice.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Smallestai_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	runSmallestaiScript(t, "say-stream-smallestai-hi", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", smallestaiHindiText, "stream", true,
			"synthesizer", smallestaiSynth(smallestaiHindiVoice, "hi-IN", nil)))
}

// TestVerb_Agent_Smallestai_BargeIn — a live agent on smallestai TTS: the caller barges
// into the greeting (the socket is redialed per turn) and the reply must be
// heard on the new socket. A turn after 65s with no tokens proves nothing
// is left on a socket past Smallest AI's 60s idle close.
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
// 10. idle-no-tokens
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. hangup-and-wait-ended
func TestVerb_Agent_Smallestai_BargeIn(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 220*time.Second)
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
		TTS:               smallestaiSynth(smallestaiVoice, "en-US", nil),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(190))
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
	rec := filepath.Join(t.TempDir(), "smallestai-barge-reply.pcm")
	if err := call.StartRecording(rec); err != nil {
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
	AssertTranscriptHasMost(s, ctx, rec, 2, "alpha", "bravo", "charlie", "delta")
	s.Done()

	s = Step(t, "assert-user-interruption")
	cbs := DrainCallbacks(sess, time.Second)
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		s.Errorf("no user_interruption event: %s", summarizeEventTypes(cbs))
	}
	s.Done()

	WaitFor(t, "idle-no-tokens", 65*time.Second)

	s = Step(t, "idle-turn-record-and-speak")
	idleRec := filepath.Join(t.TempDir(), "smallestai-idle-reply.pcm")
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

// smallestaiGather runs one gather on smallestai with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func smallestaiGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-smallestai")
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

// TestVerb_Gather_Speech_Smallestai — gather on pulse with the jambonz en-US
// language, which mediajam maps to en, returns the spoken phrase.
//
// Steps:
//   - script-gather-speech-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	transcript := smallestaiGather(t, smallestaiRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Smallestai_Options — every Pulse query option and the
// expire-content header reach Smallest AI and still yield the phrase.
//
// Steps:
//   - script-gather-speech-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Smallestai_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	r := smallestaiRecognizer("en-US", map[string]any{
		"language":           "en",
		"wordTimestamps":     true,
		"diarize":            false,
		"vadEvents":          true,
		"endpointing":        true,
		"eouTimeoutMs":       800,
		"format":             true,
		"punctuate":          true,
		"capitalize":         true,
		"itnNormalize":       true,
		"noFormatDelay":      false,
		"finalizeOnWords":    false,
		"maxWords":           60,
		"redactPii":          false,
		"redactPci":          false,
		"sentenceTimestamps": false,
		"emotionDetection":   false,
		"genderDetection":    false,
		"keywords":           []string{"sun:5", "shining"},
		"timeout":            120,
		"expireContent":      "3600",
	})
	r["model"] = smallestaiSTTModel
	transcript := smallestaiGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Smallestai_Hints — hints with a boost become the
// keywords query param (term:intensifier).
//
// Steps:
//   - script-gather-speech-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Smallestai_Hints(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	r := smallestaiRecognizer("en-US", nil)
	r["hints"] = []string{"sun", "shining"}
	r["hintsBoost"] = 5
	transcript := smallestaiGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Smallestai_Pulse2 — the English-only pulse-2 model
// selected per verb; en-US still maps to en.
//
// Steps:
//   - script-gather-speech-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Smallestai_Pulse2(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	r := smallestaiRecognizer("en-US", nil)
	r["model"] = smallestaiSTTModelV2
	transcript := smallestaiGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Smallestai_AutoLanguage — language "any" omits the
// language param, so Pulse uses its own default for the English turn.
//
// Steps:
//   - script-gather-speech-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Smallestai_AutoLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	transcript := smallestaiGather(t, smallestaiRecognizer("any", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// smallestaiTranscribe runs transcribe on smallestai with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func smallestaiTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-smallestai")
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

// TestVerb_Transcribe_Smallestai — transcribe on pulse posts each final;
// interims are requested, but their count is logged, not asserted.
//
// Steps:
//   - script-transcribe-pause-hangup-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	r := smallestaiRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := smallestaiTranscribe(t, r)

	s := Step(t, "assert-transcript")
	s.Logf("interim results: %d", interims)
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Smallestai — two-turn agent echo with smallestai STT: the
// agent's own turn detection ends each turn (pulse emits no turn events).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Smallestai(t *testing.T) {
	if !requireSmallestai(t) {
		return
	}
	runSmallestaiAgentEcho(t, nil)
}

// TestVerb_Agent_Echo_Smallestai_TTS — the same echo with smallestai on both sides, so
// the replies heard are LLM tokens streamed into the smallestai TTS socket.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-smallestai
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Smallestai_TTS(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
		return
	}
	runSmallestaiAgentEcho(t, smallestaiSynth(smallestaiVoice, "en-US", nil))
}

func runSmallestaiAgentEcho(t *testing.T, synth map[string]any) {
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

	s = Step(t, "script-agent-verb-smallestai")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          smallestaiRecognizer("en-US", nil),
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

// runSmallestaiScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runSmallestaiScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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

// TestVerb_Agent_Smallestai_Pulse2_Turn — on pulse-2 Pulse's end of turn ends
// the agent turn: one prompt yields exactly one turn_end with its transcript.
// turn_detection is not in the eventHook payload, so stt_native is not asserted.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-smallestai-pulse2
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - speak-prompt
//   - wait-for-turn-end
//   - assert-one-turn-end
func TestVerb_Agent_Smallestai_Pulse2_Turn(t *testing.T) {
	vendorExtra(t)
	if !requireSmallestai(t) {
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

	s = Step(t, "script-agent-verb-smallestai-pulse2")
	stt := smallestaiRecognizer("en-US", nil)
	stt["model"] = smallestaiSTTModelV2
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:      agentEchoSystemPrompt,
		NoResponseTimeout: &noReprompt,
		STT:               stt,
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

	s = Step(t, "speak-prompt")
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
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-one-turn-end")
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) != 1 {
		s.Fatalf("turn_end count = %d, want 1: %s", len(turnEnds), summarizeEventTypes(cbs))
	}
	transcript := strings.ToLower(turnEnds[0].String("transcript"))
	if strings.TrimSpace(transcript) == "" {
		s.Fatalf("turn_end has empty transcript: %s", string(turnEnds[0].Body))
	}
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
}
