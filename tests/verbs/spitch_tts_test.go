// Spitch TTS: HTTP only, so mediajam POSTs each sentence to /v1/speech (raw
// pcm_s16le 24 kHz) and serves one-shot say the same way. TTS only.
// Every test passes without exercising spitch when SPITCH_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	spitchYorubaVoice = "sade"
	spitchPidginVoice = "justice"
	// "Good morning, how are you today?" in Yoruba.
	spitchYorubaText = "Ẹ káàrọ̀, báwo ni o ṣe wà lónìí?"
)

// requireSpitch logs and reports false when the key is unset; callers return.
func requireSpitch(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSpitch() || spitchLabel == "" {
		t.Log("SPITCH_API_KEY not set — passing without exercising spitch")
		return false
	}
	return true
}

func spitchSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "spitch",
		"label":    spitchLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Spitch — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Spitch(t *testing.T) {
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, spitchSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Spitch — one-shot say (en-US sent as language en).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Spitch(t *testing.T) {
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-spitch",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", spitchSynth(cfg.SpitchVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Spitch_CachedReplay — the same prompt twice on one call: the
// second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Spitch_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	synth := spitchSynth(cfg.SpitchVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSpitchScript(t, "say-spitch-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Spitch_Speed — speed reaches the body as a number (an invalid
// one would 422: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Spitch_Speed(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-spitch-speed",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", spitchSynth(cfg.SpitchVoiceID, "en-US", map[string]any{"speed": 0.9})),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Spitch_ApiUri — a verb-level api_uri with a scheme and a
// trailing slash still resolves to the default host.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Spitch_ApiUri(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-spitch-apiuri",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", spitchSynth(cfg.SpitchVoiceID, "en-US",
				map[string]any{"api_uri": "https://api.spitch.app/"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Spitch_Yoruba — yo-NG is sent as language yo. Duration is the
// check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Spitch_Yoruba(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSpitchScript(t, "say-spitch-yo", 1*time.Second, 12*time.Second, nil,
		V("say", "text", spitchYorubaText,
			"synthesizer", spitchSynth(spitchYorubaVoice, "yo-NG", nil)))
}

// TestVerb_Say_Spitch_Pidgin — pcm-NG has no documented code, so language is
// omitted and the voice still speaks.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Spitch_Pidgin(t *testing.T) {
	vendorExtra(t)
	if !requireSpitch(t) {
		return
	}
	t.Parallel()
	runSpitchScript(t, "say-spitch-pcm", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "How you dey? Everything go better today.",
			"synthesizer", spitchSynth(spitchPidginVoice, "pcm-NG", nil)))
}

// runSpitchScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runSpitchScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
