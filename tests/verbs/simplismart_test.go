// Simplismart TTS: HTTP only, so mediajam POSTs each sentence to /tts
// (Orpheus by default, Veena for hi) and serves one-shot say the same way.
// qwen-tts needs a dedicated deployment, so it is not exercised. TTS only.
// Every test passes without exercising simplismart when SIMPLISMART_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	simplismartHindiVoice = "kavya"
	simplismartVeenaModel = "maya-research/Veena"
	// "Hello, this voice test is working correctly." in Hindi.
	simplismartHindiText = "नमस्ते, यह आवाज़ परीक्षण ठीक से काम कर रहा है।"
)

// requireSimplismart logs and reports false when the key is unset; callers return.
func requireSimplismart(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSimplismart() || simplismartLabel == "" {
		t.Log("SIMPLISMART_API_KEY not set — passing without exercising simplismart")
		return false
	}
	return true
}

func simplismartSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "simplismart",
		"label":    simplismartLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// simplismartSampling sets every Orpheus/Veena body param (LiveKit's defaults).
func simplismartSampling() map[string]any {
	return map[string]any{
		"temperature":        0.7,
		"top_p":              0.9,
		"repetition_penalty": 1.5,
		"max_tokens":         1000,
	}
}

// TestVerb_SpeechCredential_Simplismart — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Simplismart(t *testing.T) {
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, simplismartSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Simplismart — one-shot say on Orpheus (picked for en-US).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Simplismart(t *testing.T) {
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-simplismart",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", simplismartSynth(cfg.SimplismartVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Simplismart_CachedReplay — the same prompt twice on one call:
// the second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Simplismart_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	synth := simplismartSynth(cfg.SimplismartVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSimplismartScript(t, "say-simplismart-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Simplismart_AllOptions — every sampling param reaches the
// body (a malformed one would fail the request: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Simplismart_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-simplismart-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", simplismartSynth(cfg.SimplismartVoiceID, "en-US", simplismartSampling())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Simplismart_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the shared host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Simplismart_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-simplismart-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", simplismartSynth(cfg.SimplismartVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.simplismart.live/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Simplismart_VeenaModel — a verb-level model_id (Veena) on
// English text overrides the language-picked Orpheus.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Simplismart_VeenaModel(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSimplismartScript(t, "say-simplismart-veena", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "The second model is speaking now.",
			"synthesizer", simplismartSynth(simplismartHindiVoice, "en-US",
				map[string]any{"model_id": simplismartVeenaModel})))
}

// TestVerb_Say_Simplismart_Hindi — hi-IN with no model picks Veena. Duration
// is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Simplismart_Hindi(t *testing.T) {
	vendorExtra(t)
	if !requireSimplismart(t) {
		return
	}
	t.Parallel()
	runSimplismartScript(t, "say-simplismart-hi", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", simplismartHindiText,
			"synthesizer", simplismartSynth(simplismartHindiVoice, "hi-IN", nil)))
}

// runSimplismartScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in that order.
func runSimplismartScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
