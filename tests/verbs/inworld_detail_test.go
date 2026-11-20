//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_SpeechCredential_Inworld — the api-server's STT credential test
// streams real audio to Inworld and needs a final; creating never validates.
//
// Steps:
//  1. test-credential-stt
func TestVerb_SpeechCredential_Inworld(t *testing.T) {
	if !requireInworld(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, inworldSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Gather_Speech_Inworld_Options — every inworldOptions field plus
// recognizer model and hints reaches Inworld and still yields the phrase (a
// rejected transcribeConfig fails the gather).
//
// Steps:
//   - script-gather-speech-inworld
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Inworld_Options(t *testing.T) {
	vendorExtra(t)
	if !requireInworld(t) {
		return
	}
	r := inworldRecognizer("en-US", map[string]any{
		"prompts":                          []string{"sunshine"},
		"includeWordTimestamps":            true,
		"enableSpeakerDiarization":         true,
		"endOfTurnConfidenceThreshold":     0.5,
		"minEndOfTurnSilenceWhenConfident": 300,
		"maxTurnSilence":                   1500,
		"vadThreshold":                     0.5,
		"inactivityTimeoutSeconds":         30,
		"enableVoiceProfile":               true,
		"voiceProfileTopN":                 1,
	})
	r["model"] = inworldSTTModel
	r["hints"] = []string{"sun", "shining"}
	transcript := inworldGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Inworld_AnyLanguage — language "any" is not sent, so
// Inworld auto-detects and the English phrase still comes back.
//
// Steps:
//   - script-gather-speech-inworld
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Inworld_AnyLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireInworld(t) {
		return
	}
	transcript := inworldGather(t, inworldRecognizer("any", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Agent_Echo_Inworld — two-turn agent echo with inworld STT:
// Inworld's own turn detection ends each turn (native turn-taking).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-inworld
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Inworld(t *testing.T) {
	if !requireInworld(t) {
		return
	}
	runInworldAgentEcho(t, nil)
}
