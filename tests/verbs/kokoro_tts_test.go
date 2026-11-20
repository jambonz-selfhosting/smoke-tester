// Kokoro TTS: self-hosted Kokoro-FastAPI (OpenAI-compatible), HTTP only, so
// mediajam POSTs each sentence to /v1/audio/speech (raw pcm 24 kHz) and serves
// one-shot say the same way. Every test passes without exercising kokoro when
// KOKORO_API_URI is unset.
package verbs

import (
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const kokoroSpanishVoice = "ef_dora"

// requireKokoro logs and reports false when the server URL is unset; callers return.
func requireKokoro(t *testing.T) bool {
	t.Helper()
	if !cfg.HasKokoro() || kokoroLabel == "" {
		t.Log("KOKORO_API_URI not set — passing without exercising kokoro")
		return false
	}
	return true
}

func kokoroSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "kokoro",
		"label":    kokoroLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// kokoroAllOptions sets every body option the vendor exposes.
func kokoroAllOptions() map[string]any {
	return map[string]any{
		"lang_code":         "a",
		"speed":             1.1,
		"volume_multiplier": 1.2,
		"normalization_options": map[string]any{
			"normalize":           true,
			"unit_normalization":  false,
			"url_normalization":   true,
			"email_normalization": true,
			"phone_normalization": true,
		},
	}
}

// TestVerb_SpeechCredential_Kokoro — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned server.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Kokoro(t *testing.T) {
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, kokoroSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Kokoro — one-shot say; en-US is not sent as lang_code.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro(t *testing.T) {
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", kokoroSynth(cfg.KokoroVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Kokoro_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Kokoro_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	synth := kokoroSynth(cfg.KokoroVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runKokoroScript(t, "say-kokoro-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Kokoro_AllOptions — lang_code, speed, volume_multiplier and
// normalization_options reach the body (a bad type would 422: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", kokoroSynth(cfg.KokoroVoiceID, "en-US", kokoroAllOptions())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Kokoro_ModelAndVoiceMix — the OpenAI alias model tts-1 and a
// mixed voice are accepted as given.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_ModelAndVoiceMix(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-mix",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", kokoroSynth("af_bella+af_sky", "en-US",
				map[string]any{"model_id": "tts-1"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Kokoro_ApiUri — a verb-level api_uri with a trailing /v1/
// still resolves to the same server.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kokoro_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kokoro-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "The library opens again on Thursday.",
			"synthesizer", kokoroSynth(cfg.KokoroVoiceID, "en-US",
				map[string]any{"api_uri": strings.TrimRight(cfg.KokoroAPIURI, "/") + "/v1/"})),
		wantWords: []string{"library", "thursday"},
	})
}

// TestVerb_Say_Kokoro_Spanish — es-ES with a Spanish voice; lang_code comes
// from the voice prefix. Duration is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Kokoro_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireKokoro(t) {
		return
	}
	t.Parallel()
	runKokoroScript(t, "say-kokoro-es", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Buenos días, ¿cómo está usted hoy?",
			"synthesizer", kokoroSynth(kokoroSpanishVoice, "es-ES", nil)))
}

// runKokoroScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runKokoroScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
