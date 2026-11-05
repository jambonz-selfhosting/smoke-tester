// Gnani (Vachana): TTS posted per sentence to /api/v1/tts/sse (streaming say
// and agent; one-shot say via /api/v1/tts/inference) and finals-only STT over
// /stt/v3/stream with vendor VAD (gather, transcribe, agent). One credential
// serves both. Every test passes without exercising gnani when GNANI_API_KEY
// is unset.
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
	gnaniSTTModel   = "prisma-v2.5"
	gnaniSTTModelV3 = "prisma-v3.0"
	gnaniTTSModel   = "timbre-v2.5"
	gnaniTTSModelV3 = "timbre-v3.0"
	// first listed voice for en-IN and hi-IN on timbre-v2.5, and a v3.0 voice
	gnaniVoice      = "Kaveri"
	gnaniHindiVoice = "Nalini"
	gnaniV3Voice    = "Shivani"
)

// gnaniHindiText is "Hello, this voice test is working correctly." in Hindi.
const gnaniHindiText = "नमस्ते, यह आवाज़ परीक्षण ठीक से काम कर रहा है।"

// requireGnani logs and reports false when the key is unset; callers return.
func requireGnani(t *testing.T) bool {
	t.Helper()
	if !cfg.HasGnani() || gnaniLabel == "" {
		t.Log("GNANI_API_KEY not set — passing without exercising gnani")
		return false
	}
	return true
}

func gnaniRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "gnani",
		"label":    gnaniLabel,
		"language": language,
	}
	if options != nil {
		r["gnaniOptions"] = options
	}
	return r
}

func gnaniSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "gnani",
		"label":    gnaniLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Gnani — the api-server's STT (real audio to a
// final) and TTS (synthAudio) credential tests pass for the provisioned key.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Gnani(t *testing.T) {
	if !requireGnani(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, gnaniSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, gnaniSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Gnani — one-shot say over /api/v1/tts/inference; en-US maps to en-IN.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Gnani(t *testing.T) {
	if !requireGnani(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-gnani",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", gnaniSynth(gnaniVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Gnani_Options — model_id, language and a numeric speed reach
// the body; a rejected value fails the request and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Gnani_Options(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-gnani-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse.",
			"synthesizer", gnaniSynth(gnaniVoice, "en-US", map[string]any{
				"model_id": gnaniTTSModel,
				"language": "en-IN",
				"speed":    1.1,
			})),
		wantWords: []string{"warehouse", "ships today"},
	})
}

// TestVerb_Say_Gnani_V3 — timbre-v3.0 model override with a v3 voice and a
// named speed (sent as a string).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Gnani_V3(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-gnani-v3",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", gnaniSynth(gnaniV3Voice, "en-US", map[string]any{
				"model_id": gnaniTTSModelV3,
				"speed":    1.1,
			})),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Gnani_Hindi — one-shot Hindi say on a hi-IN voice.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Gnani_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-gnani-hi",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", gnaniHindiText,
			"synthesizer", gnaniSynth(gnaniHindiVoice, "hi-IN", nil)),
	})
}

// gnaniGather runs one gather on gnani with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func gnaniGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-gnani")
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

// TestVerb_Gather_Speech_Gnani — gather on gnani with the jambonz en-US
// language, sent as en-IN, returns the phrase on prisma-v2.5.
//
// Steps:
//   - script-gather-speech-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gnani(t *testing.T) {
	if !requireGnani(t) {
		return
	}
	transcript := gnaniGather(t, gnaniRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Gnani_Options — every prisma-v2.5 header option
// reaches the upgrade and still yields the phrase (a bad header closes 1008).
//
// Steps:
//   - script-gather-speech-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gnani_Options(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	r := gnaniRecognizer("en-US", map[string]any{
		"format":             "transcribe",
		"itnNativeNumerals":  false,
		"enableSubstitution": true,
		"substitutionMap": []any{
			map[string]any{"patterns": []string{"sunny"}, "replacement": "sun"},
		},
		"biasList":       []string{"sun", "shining"},
		"biasScore":      1.5,
		"topic":          "A short weather remark.",
		"previousText":   "Tell me about the weather.",
		"idleTimeoutSec": 30,
		"requestId":      "smoke-gnani-options",
		"vadThreshold":   0.6,
		"minSilenceMs":   600,
		"minSpeechMs":    150,
		"minSegmentMs":   200,
		"maxSpeechS":     30,
	})
	r["model"] = gnaniSTTModel
	transcript := gnaniGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Gnani_V3 — prisma-v3.0 with the v3-only keyterms and
// speakerNames headers.
//
// Steps:
//   - script-gather-speech-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gnani_V3(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	r := gnaniRecognizer("en-US", map[string]any{
		"keyterms":     []string{"sun is shining"},
		"speakerNames": []string{"Caller"},
	})
	r["model"] = gnaniSTTModelV3
	transcript := gnaniGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Gnani_Hints — hints become bias_list (+ bias_score 1)
// on prisma-v2.5 and keyterms on prisma-v3.0.
//
// Steps:
//   - script-gather-speech-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gnani_Hints(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	for _, model := range []string{gnaniSTTModel, gnaniSTTModelV3} {
		t.Run(model, func(t *testing.T) {
			r := gnaniRecognizer("en-US", nil)
			r["model"] = model
			r["hints"] = []string{"sun", "shining"}
			transcript := gnaniGather(t, r, resolveFixture(t, speechWAV))
			assertHits(t, transcript, 2, "sun", "shining")
		})
	}
}

// TestVerb_Gather_Speech_Gnani_AutoLanguage — language "any" is sent as
// indic-auto, so Gnani detects the English turn itself.
//
// Steps:
//   - script-gather-speech-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Gnani_AutoLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireGnani(t) {
		return
	}
	r := gnaniRecognizer("any", nil)
	r["model"] = gnaniSTTModelV3
	transcript := gnaniGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// gnaniTranscribe runs transcribe on gnani with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func gnaniTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-gnani")
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

// TestVerb_Transcribe_Gnani — transcribe on gnani posts each VAD-cut final;
// the vendor sends no partials, so none may arrive.
//
// Steps:
//   - script-transcribe-pause-hangup-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Gnani(t *testing.T) {
	if !requireGnani(t) {
		return
	}
	r := gnaniRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := gnaniTranscribe(t, r)

	s := Step(t, "assert-transcript")
	if interims != 0 {
		s.Errorf("got %d interim results; gnani is finals only", interims)
	}
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Gnani — two-turn agent echo with gnani STT: each vendor
// final ends the turn (native turn-taking, no local endpointing).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-gnani
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Gnani(t *testing.T) {
	if !requireGnani(t) {
		return
	}
	runGnaniAgentEcho(t, nil)
}

func runGnaniAgentEcho(t *testing.T, synth map[string]any) {
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

	s = Step(t, "script-agent-verb-gnani")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          gnaniRecognizer("en-US", nil),
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

// runGnaniScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in order.
func runGnaniScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
