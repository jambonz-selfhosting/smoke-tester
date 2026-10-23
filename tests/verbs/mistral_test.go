// Mistral Voxtral 3: realtime STT (gather / transcribe / agent turn-taking) and
// TTS (one-shot and streaming say, agent). One credential serves both. Every
// test passes without exercising mistral when MISTRAL_API_KEY is unset.
//
// Voxtral TTS has no input-streaming socket, so mediajam POSTs each sentence
// to /v1/audio/speech (SSE of 24 kHz PCM). Not an alignment vendor.
package verbs

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	mistralSTTModel = "voxtral-mini-transcribe-realtime-3"
	mistralTTSModel = "voxtral-mini-tts-3-0"
	// Voxtral 3 preset voices: Sheila (en-US) and Marie (French)
	mistralVoice       = "voice_478"
	mistralFrenchVoice = "voice_592"
)

// requireMistral logs and reports false when the key is unset; callers return.
func requireMistral(t *testing.T) bool {
	t.Helper()
	if !cfg.HasMistral() || mistralLabel == "" {
		t.Log("MISTRAL_API_KEY not set — passing without exercising mistral")
		return false
	}
	return true
}

func mistralRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "mistral",
		"label":    mistralLabel,
		"language": language,
	}
	if options != nil {
		r["mistralOptions"] = options
	}
	return r
}

// mistralSynth targets voice; a Voxtral voice fixes its language.
func mistralSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "mistral",
		"label":    mistralLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Mistral — the api-server's STT and TTS credential
// tests pass for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, mistralSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, mistralSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// mistralGather runs one gather on mistral with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func mistralGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-mistral")
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
	// an eager end posts an early, non-final speechDetected action first (as
	// deepgramflux's EagerEndOfTurn does); the gather's result is the final one
	var cb webhook.Callback
	for {
		var err error
		if cb, err = sess.WaitCallbackFor(waitCtx, "action/gather"); err != nil {
			s.Fatalf("WaitCallbackFor action/gather: %v", err)
		}
		if speech, _ := cb.JSON["speech"].(map[string]any); speech == nil || speech["is_final"] != false {
			break
		}
		s.Logf("early action/gather (eager end): %s", string(cb.Body))
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	_ = call.Hangup()
	return extractTranscript(cb)
}

// TestVerb_Gather_Speech_Mistral — gather input=[speech] on mistral returns
// the spoken phrase; the final comes from Voxtral's turn.end.
//
// Steps:
//   - script-gather-speech-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	transcript := mistralGather(t, mistralRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Mistral_Options — every turn and latency option
// reaches Voxtral and still yields the phrase: recognizer model and hints
// (context_bias), streaming delay, and the turn_detection thresholds (a
// rejected session.update fails the gather).
//
// Steps:
//   - script-gather-speech-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Mistral_Options(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	r := mistralRecognizer("en-US", map[string]any{
		"streamingDelayMs": 240,
		"turnDetection": map[string]any{
			"startThreshold":    0.5,
			"eagerEndThreshold": 0.4,
			"endThreshold":      0.33,
			"minSilenceMs":      300,
			"endTimeoutMs":      2500,
		},
	})
	r["model"] = mistralSTTModel
	r["hints"] = []string{"sun", "shining"}
	transcript := mistralGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Mistral_LongDelay — the longest streaming delay
// Voxtral takes is accepted. The final is cut at Voxtral's end of turn, and
// at this delay the last word can come after it, so only "sun" is required.
//
// Steps:
//   - script-gather-speech-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Mistral_LongDelay(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	transcript := mistralGather(t, mistralRecognizer("en-US", map[string]any{
		"streamingDelayMs": 1200,
	}), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Mistral_Spanish — es-ES maps to Voxtral's "es";
// gather returns the clip's first sentence, "una mesa para hoy, por favor".
//
// Steps:
//   - script-gather-speech-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Mistral_Spanish(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	transcript := mistralGather(t, mistralRecognizer("es-ES", nil), resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
}

// mistralTranscribe runs transcribe on mistral with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func mistralTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-mistral")
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

// TestVerb_Transcribe_Mistral — transcribe on mistral posts interim results
// built from Voxtral's text deltas, then the final utterance.
//
// Steps:
//   - script-transcribe-pause-hangup-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
//   - assert-interims
func TestVerb_Transcribe_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	r := mistralRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := mistralTranscribe(t, r)

	s := Step(t, "assert-transcript")
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "assert-interims")
	if interims == 0 {
		s.Errorf("interim:true but no is_final=false results arrived")
	}
	s.Done()
}

// TestVerb_Transcribe_Mistral_Diarize — recognizer.diarization replaces turn
// detection with Voxtral's speaker segments; each still reaches transcriptionHook.
//
// Steps:
//   - script-transcribe-pause-hangup-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Mistral_Diarize(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	r := mistralRecognizer("en-US", nil)
	r["diarization"] = true
	transcript, _ := mistralTranscribe(t, r)

	s := Step(t, "assert-transcript")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Mistral — two-turn agent echo with mistral STT: Voxtral's
// own turn.end ends each turn (native turn-taking, no local endpointing).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	runMistralAgentEcho(t, nil)
}

// TestVerb_Agent_Echo_Mistral_TTS — the same echo with Voxtral on both sides,
// so the replies heard are Voxtral speech streamed sentence by sentence.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Mistral_TTS(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	runMistralAgentEcho(t, mistralSynth(mistralVoice, "en-US", nil))
}

func runMistralAgentEcho(t *testing.T, synth map[string]any) {
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

	s = Step(t, "script-agent-verb-mistral")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          mistralRecognizer("en-US", nil),
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

// TestVerb_Agent_Mistral_EarlyGeneration — Voxtral's eager end drives
// preflight, as Flux's EagerEndOfTurn does. A hit needs the preflight text to
// equal the final's, which Voxtral's late full stop often prevents, so this
// asserts preflight ran, not that it hit.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-speak-and-drain
//   - assert-preflight-ran
func TestVerb_Agent_Mistral_EarlyGeneration(t *testing.T) {
	if !requireMistral(t) {
		return
	}
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

	s = Step(t, "script-agent-verb-mistral")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:    agentEchoSystemPrompt,
		STT:             mistralRecognizer("en-US", nil),
		EarlyGeneration: true,
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

	var cbs []webhook.Callback
	for i := range agentEchoTurns {
		s = Step(t, formatAgentTurnStep(i+1, "speak-and-drain"))
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
		cbs = append(cbs, DrainCallbacks(sess, LLMReplyWindow)...)
		s.Done()
	}

	s = Step(t, "assert-preflight-ran")
	var results []string
	for _, te := range findAgentEvents(cbs, "turn_end") {
		lat, _ := te.JSON["latency"].(map[string]any)
		pf, _ := lat["preflight"].(map[string]any)
		r, _ := pf["result"].(string)
		results = append(results, r)
	}
	s.Logf("preflight results per turn: %v", results)
	if !slices.ContainsFunc(results, func(r string) bool { return r == "hit" || r == "miss" }) {
		s.Errorf("no turn ran a preflight (results %v)", results)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Say_Mistral — one-shot say on the default model.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-mistral",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", mistralSynth(mistralVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Mistral_Options — a model override and expressiveness reach
// the API (a rejected field fails the say), and an emotion tag is not read aloud.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Mistral_Options(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-mistral-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "[emotion=happy]Your order shipped this morning and arrives on Thursday.",
			"synthesizer", mistralSynth(mistralVoice, "en-US", map[string]any{
				"model_id":       "voxtral-mini-tts-3",
				"expressiveness": 3,
			})),
		wantWords: []string{"shipped", "thursday"},
	})
}

// TestVerb_Say_Mistral_French — a French preset voice speaks French. Only the
// length is checked: the transcript assertion is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Mistral_French(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-mistral-fr",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Bonjour, votre commande a été expédiée ce matin.",
			"synthesizer", mistralSynth(mistralFrenchVoice, "fr-FR", nil)),
	})
}

// TestVerb_Say_Stream_Mistral — streaming say.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Mistral(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-mistral", mistralSynth(mistralVoice, "en-US", nil))
}

// TestVerb_Say_Stream_Mistral_MultiSentence — one streaming say of several
// sentences: each is its own request, and all must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Mistral_MultiSentence(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	t.Parallel()
	runMistralScript(t, "say-stream-mistral-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", mistralSynth(mistralVoice, "en-US", nil)))
}

// TestVerb_Agent_Mistral_BargeIn — a live agent on mistral TTS: the caller
// barges into the greeting (the in-flight request is cancelled) and the reply
// to the barge-in must be heard.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. hangup-and-wait-ended
func TestVerb_Agent_Mistral_BargeIn(t *testing.T) {
	if !requireMistral(t) {
		return
	}
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
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
		TTS:               mistralSynth(mistralVoice, "en-US", nil),
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

	WaitFor(t, "wait-into-greeting", 4*time.Second)

	s = Step(t, "barge-in-and-record-reply")
	rec := filepath.Join(t.TempDir(), "mistral-barge-reply.pcm")
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

	HangupAndWaitEnded(t, ctx, call)
}

// runMistralScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in order.
func runMistralScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
