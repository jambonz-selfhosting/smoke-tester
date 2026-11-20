// Kokoro TTS: self-hosted Kokoro-FastAPI (OpenAI-compatible), HTTP only, so
// mediajam POSTs each sentence to /v1/audio/speech (raw pcm 24 kHz) and serves
// one-shot say the same way. Every test passes without exercising kokoro when
// KOKORO_API_URI is unset.
package verbs

import (
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
