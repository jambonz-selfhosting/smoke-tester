// Vakyam TTS: HTTP only, so mediajam POSTs each sentence to /v1/tts/stream
// (raw pcm 24 kHz) and serves one-shot say the same way. TTS only.
// Never t.Parallel: the free plan allows one concurrent request.
// Every test passes without exercising vakyam when VAKYAM_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	vakyamMaleVoice = "Vikram"
	// "Hello, how are you today?" in Hindi.
	vakyamHindiText = "नमस्ते, आज आप कैसे हैं?"
)

// requireVakyam logs and reports false when the key is unset; callers return.
func requireVakyam(t *testing.T) bool {
	t.Helper()
	if !cfg.HasVakyam() || vakyamLabel == "" {
		t.Log("VAKYAM_API_KEY not set — passing without exercising vakyam")
		return false
	}
	return true
}

func vakyamSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "vakyam",
		"label":    vakyamLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Vakyam — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Vakyam(t *testing.T) {
	if !requireVakyam(t) {
		return
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, vakyamSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Vakyam — one-shot say (en-US sent as language en-IN).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Vakyam(t *testing.T) {
	if !requireVakyam(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-vakyam",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", vakyamSynth(cfg.VakyamVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// runVakyamScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runVakyamScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
