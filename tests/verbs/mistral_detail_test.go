//go:build detail

package verbs

import (
	"slices"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

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
	vendorExtra(t)
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
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Mistral_LongDelay — at a 1200ms streaming delay the
// last word lands after Voxtral's turn.end; the held final must still have it.
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
	vendorExtra(t)
	if !requireMistral(t) {
		return
	}
	transcript := mistralGather(t, mistralRecognizer("en-US", map[string]any{
		"streamingDelayMs": 1200,
	}), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
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
	vendorExtra(t)
	if !requireMistral(t) {
		return
	}
	transcript := mistralGather(t, mistralRecognizer("es-ES", nil), resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
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
	vendorExtra(t)
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

// TestVerb_Agent_Mistral_EarlyGeneration — Voxtral's eager end drives
// preflight. The eager end waits for the sentence to close, so the preflight
// text equals the final's and the speculative reply is used.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-mistral
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-speak-and-drain
//   - assert-preflight-hit
func TestVerb_Agent_Mistral_EarlyGeneration(t *testing.T) {
	vendorExtra(t)
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

	s = Step(t, "assert-preflight-hit")
	var results []string
	for _, te := range findAgentEvents(cbs, "turn_end") {
		lat, _ := te.JSON["latency"].(map[string]any)
		pf, _ := lat["preflight"].(map[string]any)
		r, _ := pf["result"].(string)
		results = append(results, r)
	}
	s.Logf("preflight results per turn: %v", results)
	if !slices.Contains(results, "hit") {
		s.Errorf("no turn used its preflight (results %v)", results)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
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
	vendorExtra(t)
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
	vendorExtra(t)
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
