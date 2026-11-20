// Phonic TTS: HTTP only (its websocket is speech-to-speech), so mediajam POSTs
// each sentence to /v1/tts/stream (raw pcm_24000) and serves one-shot say.
// Every test passes without exercising phonic when PHONIC_API_KEY is unset.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	phonicGermanVoice = "karoline"
	// "Good morning, your appointment is confirmed for tomorrow."
	phonicGermanText = "Guten Morgen, Ihr Termin ist für morgen bestätigt."
)

// requirePhonic logs and reports false when the key is unset; callers return.
func requirePhonic(t *testing.T) bool {
	t.Helper()
	if !cfg.HasPhonic() || phonicLabel == "" {
		t.Log("PHONIC_API_KEY not set — passing without exercising phonic")
		return false
	}
	return true
}

func phonicSynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "phonic",
		"label":    phonicLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Phonic — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Phonic(t *testing.T) {
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, phonicSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Phonic — one-shot say (en-US sent as languages [en]).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Phonic(t *testing.T) {
	if !requirePhonic(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-phonic",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", phonicSynth(cfg.PhonicVoiceID, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// runPhonicScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runPhonicScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
