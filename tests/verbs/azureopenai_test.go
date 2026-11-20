// Azure OpenAI: TTS over POST /openai/v1/audio/speech (one-shot say, and
// streaming say and agent sent per sentence) and realtime transcription over
// /openai/v1/realtime (gather, transcribe, agent). One credential, with the
// resource endpoint, serves both. Every test passes without exercising
// azureopenai unless AZUREOPENAI_API_KEY and AZUREOPENAI_API_URI are set.
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
	azureopenaiVoice    = "alloy"
	azureopenaiAltVoice = "nova"
)

// azureopenaiGermanText is "Hello, this voice test is working correctly." in German.
const azureopenaiGermanText = "Hallo, dieser Sprachtest funktioniert einwandfrei."

// requireAzureopenai logs and reports false when key or endpoint is unset; callers return.
func requireAzureopenai(t *testing.T) bool {
	t.Helper()
	if !cfg.HasAzureopenai() || azureopenaiLabel == "" {
		t.Log("AZUREOPENAI_API_KEY/AZUREOPENAI_API_URI not set — passing without exercising azureopenai")
		return false
	}
	return true
}

func azureopenaiRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "azureopenai",
		"label":    azureopenaiLabel,
		"language": language,
	}
	if options != nil {
		r["azureopenaiOptions"] = options
	}
	return r
}

func azureopenaiSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "azureopenai",
		"label":    azureopenaiLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Azureopenai — the api-server's STT (realtime
// transcription of test audio) and TTS (real synthesis) credential tests pass.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Azureopenai(t *testing.T) {
	if !requireAzureopenai(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, azureopenaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, azureopenaiSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Azureopenai — one-shot say on the credential's TTS deployment.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Azureopenai(t *testing.T) {
	if !requireAzureopenai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-azureopenai",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", azureopenaiSynth(azureopenaiVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Azureopenai_Options — speed, instructions and an explicit
// model_id reach the request body; a rejected one fails it and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Azureopenai_Options(t *testing.T) {
	vendorExtra(t)
	if !requireAzureopenai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-azureopenai-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse.",
			"synthesizer", azureopenaiSynth(azureopenaiAltVoice, "en-US", map[string]any{
				"model_id":     cfg.AzureopenaiTTSDeployment,
				"speed":        1.2,
				"instructions": "Speak in a calm, friendly customer support tone.",
			})),
		wantWords: []string{"warehouse", "ships today"},
	})
}

// TestVerb_Say_Azureopenai_German — no language is sent; the voice follows
// the text. Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Azureopenai_German(t *testing.T) {
	vendorExtra(t)
	if !requireAzureopenai(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-azureopenai-de",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", azureopenaiGermanText,
			"synthesizer", azureopenaiSynth(azureopenaiVoice, "de-DE", nil)),
	})
}

// azureopenaiGather runs one gather on azureopenai with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func azureopenaiGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-azureopenai")
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

// TestVerb_Gather_Speech_Azureopenai — gather input=[speech] on the default
// gpt-live-transcribe family returns the phrase; en-US is sent as "en".
//
// Steps:
//   - script-gather-speech-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Azureopenai(t *testing.T) {
	if !requireAzureopenai(t) {
		return
	}
	transcript := azureopenaiGather(t, azureopenaiRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Azureopenai_Options — every live-family option
// reaches the session.update and still yields the phrase: deployment, model
// family, prompt, languages, keywords, delay, noise reduction and the local
// VAD settings (a rejected session fails the gather).
//
// Steps:
//   - script-gather-speech-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Azureopenai_Options(t *testing.T) {
	vendorExtra(t)
	if !requireAzureopenai(t) {
		return
	}
	r := azureopenaiRecognizer("en-US", map[string]any{
		"deployment":                  cfg.AzureopenaiSTTDeployment,
		"model":                       "gpt-live-transcribe",
		"prompt":                      "A short sentence about the weather.",
		"languages":                   []string{"en"},
		"keywords":                    []string{"sun", "shining"},
		"delay":                       "low",
		"input_audio_noise_reduction": "near_field",
		"vadMode":                     2,
		"vadSilenceMs":                600,
		"vadVoiceMs":                  200,
	})
	transcript := azureopenaiGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Azureopenai_Hints — with no keywords option, the
// recognizer hints become the live family's keywords.
//
// Steps:
//   - script-gather-speech-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Azureopenai_Hints(t *testing.T) {
	vendorExtra(t)
	if !requireAzureopenai(t) {
		return
	}
	r := azureopenaiRecognizer("en-US", nil)
	r["hints"] = []string{"sun", "shining"}
	transcript := azureopenaiGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Azureopenai_AnyLanguage — language "any" is not
// sent, so Azure detects the language itself.
//
// Steps:
//   - script-gather-speech-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Azureopenai_AnyLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireAzureopenai(t) {
		return
	}
	transcript := azureopenaiGather(t, azureopenaiRecognizer("any", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// azureopenaiTranscribe runs transcribe on azureopenai with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func azureopenaiTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-azureopenai")
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

// TestVerb_Transcribe_Azureopenai — transcribe posts each turn's final
// (transcription.completed); the interim count is logged, not asserted.
//
// Steps:
//   - script-transcribe-pause-hangup-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Azureopenai(t *testing.T) {
	if !requireAzureopenai(t) {
		return
	}
	r := azureopenaiRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := azureopenaiTranscribe(t, r)

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

// TestVerb_Agent_Echo_Azureopenai — two-turn agent echo with azureopenai STT.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-azureopenai
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Azureopenai(t *testing.T) {
	if !requireAzureopenai(t) {
		return
	}
	runAzureopenaiAgentEcho(t, nil)
}

func runAzureopenaiAgentEcho(t *testing.T, synth map[string]any) {
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

	s = Step(t, "script-agent-verb-azureopenai")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          azureopenaiRecognizer("en-US", nil),
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

// runAzureopenaiScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runAzureopenaiScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
