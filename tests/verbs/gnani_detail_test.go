//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
