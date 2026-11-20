//go:build detail

package verbs

import (
	"slices"
	"testing"
	"time"
)

// TestVerb_Gather_Speech_Assemblyai_CredentialModel — with no verb override
// the credential's speech_model is used: a 3.7 Realtime Agent credential
// transcribes, an invalid-model credential is rejected by AssemblyAI.
//
// Steps:
//   - provision-stt-credential
//   - script-gather-speech-assemblyai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-invalid-model-rejected
func TestVerb_Gather_Speech_Assemblyai_CredentialModel(t *testing.T) {
	if !assemblyaiSttReady(t) {
		return
	}
	ctx := WithTimeout(t, 5*time.Minute)
	good := assemblyaiSttCredential(t, ctx, "agent", assemblyaiRealtimeAgent)
	bad := assemblyaiSttCredential(t, ctx, "invalid", assemblyaiInvalidModel)
	wav := resolveFixture(t, speechWAV)

	r := assemblyaiGather(t, ctx, map[string]any{"vendor": "assemblyai", "label": good, "language": "en-US"}, wav)
	assertHits(t, r.Transcript, 1, "sun", "shining")

	r = assemblyaiGather(t, ctx, map[string]any{"vendor": "assemblyai", "label": bad, "language": "en-US"}, wav)
	assertAssemblyaiRejectedModel(t, r)
}

// TestVerb_Gather_Speech_Assemblyai_Options — every assemblyAiOptions field
// is accepted on 3.7 Realtime Agent (AssemblyAI documents some as no-ops on
// 3.7; an unsupported one fails the session).
//
// Steps:
//   - provision-stt-credential
//   - script-gather-speech-assemblyai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Assemblyai_Options(t *testing.T) {
	if !assemblyaiSttReady(t) {
		return
	}
	ctx := WithTimeout(t, 3*time.Minute)
	label := assemblyaiSttCredential(t, ctx, "options", assemblyaiRealtimeAgent)
	r := assemblyaiGather(t, ctx, map[string]any{
		"vendor": "assemblyai", "label": label, "language": "en-US",
		"assemblyAiOptions": map[string]any{
			"speechModel":                      assemblyaiRealtimeAgent,
			"formatTurns":                      true,
			"endOfTurnConfidenceThreshold":     0.5,
			"minEndOfTurnSilenceWhenConfident": 400,
			"minTurnSilence":                   200,
			"maxTurnSilence":                   1500,
			"keyterms":                         []string{"jambonz", "AssemblyAI"},
			"prompt":                           "A caller describing the weather.",
			"languageCodes":                    []string{"en", "es"},
			"voiceFocus":                       "near-field",
			"vadThreshold":                     0.4,
			"inactivityTimeout":                30,
		},
	}, resolveFixture(t, speechWAV))
	assertHits(t, r.Transcript, 1, "sun", "shining")
}

// TestVerb_Gather_Speech_Assemblyai_Spanish — 3.7 Realtime with
// languageDetection reports Spanish and returns "una mesa para hoy, por favor".
//
// Steps:
//   - provision-stt-credential
//   - script-gather-speech-assemblyai
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-detected-language
func TestVerb_Gather_Speech_Assemblyai_Spanish(t *testing.T) {
	if !assemblyaiSttReady(t) {
		return
	}
	ctx := WithTimeout(t, 3*time.Minute)
	label := assemblyaiSttCredential(t, ctx, "es", assemblyaiRealtime)
	r := assemblyaiGather(t, ctx, map[string]any{
		"vendor": "assemblyai", "label": label, "language": "es-ES",
		"assemblyAiOptions": map[string]any{
			"speechModel":       assemblyaiRealtime,
			"languageDetection": true,
		},
	}, resolveFixture(t, spanishWAV))
	assertHits(t, r.Transcript, 2, "mesa", "hoy", "favor")

	s := Step(t, "assert-detected-language")
	if !slices.Contains(r.Languages, "es") {
		s.Errorf("speech.languages = %v, want it to contain \"es\"", r.Languages)
	}
	s.Done()
}
