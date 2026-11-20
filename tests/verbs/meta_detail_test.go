//go:build detail

package verbs

import (
	"testing"
	"time"
)

// TestVerb_SpeechCredential_Meta — the api-server's STT credential test
// streams real audio and gets a final; creating a credential never validates it.
//
// Steps:
//  1. test-credential-stt
func TestVerb_SpeechCredential_Meta(t *testing.T) {
	if !requireMeta(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, metaSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Gather_Speech_Meta_Options — every handshake option reaches Meta
// and still yields the phrase (a rejected handshake fails the gather):
// recognizer model, mode, partialMode, emitAudioProgress, keywords merged
// with hints, an explicit languageBias and a sessionId query parameter.
//
// Steps:
//   - script-gather-speech-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Meta_Options(t *testing.T) {
	vendorExtra(t)
	if !requireMeta(t) {
		return
	}
	r := metaRecognizer("en-US", map[string]any{
		"mode":              "ENDPOINTING",
		"partialMode":       "CUMULATIVE",
		"emitAudioProgress": false,
		"keywords":          []string{"shining"},
		"languageBias":      []string{"English"},
		"sessionId":         "smoke-" + metaLabel,
	})
	r["model"] = metaSTTModel
	r["hints"] = []string{"sun"}
	transcript := metaGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Meta_Spanish — es-ES is mapped to languageBias
// ["Spanish"]; gather returns the clip's first sentence.
//
// Steps:
//   - script-gather-speech-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Meta_Spanish(t *testing.T) {
	vendorExtra(t)
	if !requireMeta(t) {
		return
	}
	transcript := metaGather(t, metaRecognizer("es-ES", nil), resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
}

// TestVerb_Gather_Speech_Meta_AnyLanguage — language "any" omits languageBias,
// so Meta auto-detects and the Spanish turn still comes back in Spanish.
//
// Steps:
//   - script-gather-speech-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Meta_AnyLanguage(t *testing.T) {
	vendorExtra(t)
	if !requireMeta(t) {
		return
	}
	transcript := metaGather(t, metaRecognizer("any", nil), resolveFixture(t, spanishWAV))
	assertHits(t, transcript, 2, "mesa", "hoy", "favor")
}

// TestVerb_Transcribe_Meta_Delta — partialMode DELTA makes interims
// increments; finals still carry the whole turn.
//
// Steps:
//   - script-transcribe-pause-hangup-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Meta_Delta(t *testing.T) {
	vendorExtra(t)
	if !requireMeta(t) {
		return
	}
	r := metaRecognizer("en-US", map[string]any{"partialMode": "DELTA"})
	r["interim"] = true
	assertMetaTranscribe(t, r, false)
}

// TestVerb_Transcribe_Meta_Diarization — mode DIARIZATION is accepted and
// still produces finals; speaker events are dropped.
//
// Steps:
//   - script-transcribe-pause-hangup-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Meta_Diarization(t *testing.T) {
	vendorExtra(t)
	if !requireMeta(t) {
		return
	}
	assertMetaTranscribe(t, metaRecognizer("en-US", map[string]any{"mode": "DIARIZATION"}), false)
}

// TestVerb_Agent_Echo_Meta — two-turn agent echo with meta STT: Meta's own
// turn detection (speechComplete) ends each turn, no local endpointing.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Meta(t *testing.T) {
	if !requireMeta(t) {
		return
	}
	runMetaAgentEcho(t, nil)
}
