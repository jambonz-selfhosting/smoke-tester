//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
