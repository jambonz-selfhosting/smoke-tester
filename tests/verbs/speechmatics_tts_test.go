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
