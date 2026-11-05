// Speechmatics TTS (preview). No input-streaming socket, so mediajam POSTs
// each sentence to /generate/<voice> as pcm_16000. Runs on a TTS-only
// credential (no speechmatics_stt_uri); passes without SPEECHMATICS_API_KEY.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireSpeechmaticsTts logs and reports false when the key is unset; callers return.
func requireSpeechmaticsTts(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSpeechmatics() {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics TTS")
		return false
	}
	if speechmaticsTtsLabel == "" {
		t.Fatalf("speechmatics TTS-only credential was not created: %v", speechmaticsTtsErr)
	}
	return true
}

func speechmaticsTtsSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "speechmatics",
		"label":    speechmaticsTtsLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Speechmatics_TTS — a TTS-only credential saves
// and the api-server's credential test (synthAudio, cache off) passes.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Speechmatics_TTS(t *testing.T) {
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, speechmaticsTtsSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Speechmatics — one-shot say on the default voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechmatics(t *testing.T) {
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechmatics",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", speechmaticsTtsSynth(speechmaticsTtsVoice, "en-GB", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Speechmatics_CachedReplay — the same prompt twice on one call:
// the second say plays the r16 cache file the first one wrote.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechmatics_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	synth := speechmaticsTtsSynth(speechmaticsTtsVoice, "en-GB", nil)
	text := "Your table for four is confirmed for tonight."
	runSpeechmaticsTtsScript(t, "say-speechmatics-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Speechmatics_USVoice — the en-US voice megan.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechmatics_USVoice(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechmatics-megan",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", speechmaticsTtsSynth("megan", "en-US", nil)),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Speechmatics_Options — api_uri with a scheme and trailing
// slash still resolves to the default host, and sm_app reaches the query.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechmatics_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechmatics-options",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "The library opens at nine tomorrow.",
			"synthesizer", speechmaticsTtsSynth(speechmaticsTtsVoice, "en-GB", map[string]any{
				"api_uri": "https://preview.tts.speechmatics.com/",
				"sm_app":  "jambonz-smoke",
			})),
		wantWords: []string{"library", "tomorrow"},
	})
}

// TestVerb_Say_Speechmatics_LanguageIgnored — language is never sent, so a
// non-English jambonz language must not break the call.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechmatics_LanguageIgnored(t *testing.T) {
	vendorExtra(t)
	if !requireSpeechmaticsTts(t) {
		return
	}
	t.Parallel()
	runSpeechmaticsTtsScript(t, "say-speechmatics-lang", 1*time.Second, 12*time.Second,
		[]string{"garden", "weather"},
		V("say", "text", "The garden looks lovely in this weather.",
			"synthesizer", speechmaticsTtsSynth(speechmaticsTtsVoice, "de-DE", nil)))
}

// runSpeechmaticsTtsScript runs the says then hangs up; wantOrdered, when
// set, must appear in the transcript in that order.
func runSpeechmaticsTtsScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
