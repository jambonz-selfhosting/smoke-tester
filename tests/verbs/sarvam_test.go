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

// TestVerb_SpeechCredential_Sarvam — the api-server's STT (real audio to a
// final) and TTS (synthAudio) credential tests pass for the provisioned key.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, sarvamSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, sarvamSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
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

// TestVerb_Say_Sarvam_Options — every v4-flash HTTP option reaches the body;
// a rejected option fails the request and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Sarvam_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-sarvam-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse.",
			"synthesizer", sarvamSynth(sarvamVoice, "en-US", map[string]any{
				"model_id":                sarvamTTSModel,
				"language":                "en-IN",
				"pace":                    1.1,
				"temperature":             0.6,
				"enable_preprocessing":    true,
				"enable_cached_responses": false,
			})),
		wantWords: []string{"warehouse", "ships today"},
	})
}

// TestVerb_Say_Sarvam_Hindi — one-shot Hindi say on a hi-IN persona.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Sarvam_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-sarvam-hi",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", sarvamHindiText,
			"synthesizer", sarvamSynth(sarvamHindiVoice, "hi-IN", nil)),
	})
}

// TestVerb_Say_Stream_Sarvam — streaming say over the websocket leg.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-sarvam", sarvamSynth(sarvamVoice, "en-US", nil))
}

// TestVerb_Say_Stream_Sarvam_Options — the WS-only buffering options and the
// shared ones in the config message; a rejected config yields an error frame
// and nothing is heard.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Sarvam_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-sarvam-options", sarvamSynth(sarvamVoice, "en-US", map[string]any{
		"pace":                 0.9,
		"temperature":          0.6,
		"enable_preprocessing": true,
		"min_buffer_size":      30,
		"max_chunk_length":     150,
	}))
}

// TestVerb_Say_Stream_Sarvam_TwoTurns — two streaming says on one call; the
// second must speak after the first turn's flush completed on the same socket.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Sarvam_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	synth := sarvamSynth(sarvamVoice, "en-US", nil)
	runSarvamScript(t, "say-stream-sarvam-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Sarvam_MultiSentence — one streaming say of several
// sentences, all heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Sarvam_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runSarvamScript(t, "say-stream-sarvam-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", sarvamSynth(sarvamVoice, "en-US", nil)))
}

// TestVerb_Say_Stream_Sarvam_Hindi — Hindi over the socket on a hi-IN persona.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Sarvam_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	t.Parallel()
	runSarvamScript(t, "say-stream-sarvam-hi", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", sarvamHindiText, "stream", true,
			"synthesizer", sarvamSynth(sarvamHindiVoice, "hi-IN", nil)))
}

// TestVerb_Agent_Sarvam_BargeIn — a live agent on sarvam TTS: the caller barges
// into the greeting (no cancel message, so the socket is redialed) and the
// reply must be heard on the new socket. A turn after 65s with no tokens
// proves the ping keeps the socket past Sarvam's ~60s idle close.
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
func TestVerb_Agent_Sarvam_BargeIn(t *testing.T) {
	if !requireSarvam(t) {
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
		TTS:               sarvamSynth(sarvamVoice, "en-US", nil),
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
	rec := filepath.Join(t.TempDir(), "sarvam-barge-reply.pcm")
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
	idleRec := filepath.Join(t.TempDir(), "sarvam-idle-reply.pcm")
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

// TestVerb_Gather_Speech_Sarvam_Options — every realtime query option reaches
// Sarvam and still yields the phrase (a rejected config closes the socket).
//
// Steps:
//   - script-gather-speech-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Sarvam_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	r := sarvamRecognizer("en-US", map[string]any{
		"model":               sarvamSTTModel,
		"language":            "en-IN",
		"streamType":          "balanced",
		"mode":                "transcribe",
		"endpointing":         "vad",
		"threshold":           0.3,
		"prefixPaddingMs":     300,
		"silenceDurationMs":   600,
		"minSpeechDurationMs": 250,
		"returnTimestamps":    true,
		"prompt":              "A short weather remark.",
	})
	transcript := sarvamGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Sarvam_Keyterms — saaras:v4 with hints, which become
// the keyterms query param (sent only for v4).
//
// Steps:
//   - script-gather-speech-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Sarvam_Keyterms(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	r := sarvamRecognizer("en-US", nil)
	r["model"] = sarvamSTTModelV4
	r["hints"] = []string{"sun", "shining"}
	transcript := sarvamGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Sarvam_AutoLanguage — language "any" is sent as
// auto, so Sarvam detects the English turn itself.
//
// Steps:
//   - script-gather-speech-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Sarvam_AutoLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	transcript := sarvamGather(t, sarvamRecognizer("any", nil), resolveFixture(t, speechWAV))
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

// TestVerb_Transcribe_Sarvam — transcribe on sarvam posts each VAD-cut final;
// partials are requested, but their count is logged, not asserted.
//
// Steps:
//   - script-transcribe-pause-hangup-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	r := sarvamRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := sarvamTranscribe(t, r)

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

// TestVerb_Agent_Echo_Sarvam — two-turn agent echo with sarvam STT: the
// agent's own turn detection ends each turn (sarvam only signals speech start).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Sarvam(t *testing.T) {
	if !requireSarvam(t) {
		return
	}
	runSarvamAgentEcho(t, nil)
}

// TestVerb_Agent_Echo_Sarvam_TTS — the same echo with sarvam on both sides, so
// the replies heard are LLM tokens streamed into the sarvam TTS socket.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-sarvam
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Sarvam_TTS(t *testing.T) {
	vendorExtra(t)
	if !requireSarvam(t) {
		return
	}
	runSarvamAgentEcho(t, sarvamSynth(sarvamVoice, "en-US", nil))
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
