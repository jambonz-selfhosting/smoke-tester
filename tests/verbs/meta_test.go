// Meta Muse Voice Transcribe: realtime STT over /v1/asr/realtime with Meta's
// own turn detection (gather, transcribe, agent). STT only — Meta has no TTS.
// Every test passes without exercising meta when META_API_KEY is unset.
package verbs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const metaSTTModel = "muse-voice-transcribe-1.0"

// requireMeta logs and reports false when the key is unset; callers return.
func requireMeta(t *testing.T) bool {
	t.Helper()
	if !cfg.HasMeta() || metaLabel == "" {
		t.Log("META_API_KEY not set — passing without exercising meta")
		return false
	}
	return true
}

func metaRecognizer(language string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "meta",
		"label":    metaLabel,
		"language": language,
	}
	if options != nil {
		r["metaOptions"] = options
	}
	return r
}

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

// metaGather runs one gather on meta with recognizer, plays wavPath, and
// returns the recognized transcript ("" when the action has none).
func metaGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-meta")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		// keep the call up while a long clip is still streaming; the test hangs up
		V("pause", "length", 20),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	// skip any early non-final speechDetected action; the gather's result is the final one
	var cb webhook.Callback
	for {
		var err error
		if cb, err = sess.WaitCallbackFor(waitCtx, "action/gather"); err != nil {
			s.Fatalf("WaitCallbackFor action/gather: %v", err)
		}
		if speech, _ := cb.JSON["speech"].(map[string]any); speech == nil || speech["is_final"] != false {
			break
		}
		s.Logf("early action/gather: %s", string(cb.Body))
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	_ = call.Hangup()
	return extractTranscript(cb)
}

// TestVerb_Gather_Speech_Meta — gather input=[speech] on meta returns the
// spoken phrase; en-US goes out as languageBias ["English"] and the final is
// Meta's speechComplete for the turn.
//
// Steps:
//   - script-gather-speech-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Meta(t *testing.T) {
	if !requireMeta(t) {
		return
	}
	transcript := metaGather(t, metaRecognizer("en-US", nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
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

// metaTranscribe runs transcribe on meta with recognizer and returns the
// joined transcript plus how many interim (is_final false) results arrived.
func metaTranscribe(t *testing.T, recognizer map[string]any) (string, int) {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-meta")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", recognizer),
		V("pause", "length", 15),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "send-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/transcribe", transcribeText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hook")
	deadline := time.Now().Add(30 * time.Second)
	var parts []string
	transcript, interims := "", 0
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			cb, err := tryPop(sess)
			if err != nil || cb.Hook != "action/transcription" {
				continue
			}
			s.Logf("action/transcription body: %s", string(cb.Body))
			if strings.Contains(string(cb.Body), `"is_final":false`) {
				interims++
				continue
			}
			if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
				parts = append(parts, seg)
				transcript = strings.Join(parts, " ")
			}
		}
		if transcript != "" && transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	_ = call.Hangup()
	return transcript, interims
}

// TestVerb_Transcribe_Meta — transcribe on meta posts interims while the
// caller speaks and one final per turn.
//
// Steps:
//   - script-transcribe-pause-hangup-meta
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Meta(t *testing.T) {
	if !requireMeta(t) {
		return
	}
	r := metaRecognizer("en-US", nil)
	r["interim"] = true
	assertMetaTranscribe(t, r, true)
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

func assertMetaTranscribe(t *testing.T, recognizer map[string]any, wantInterims bool) {
	t.Helper()
	transcript, interims := metaTranscribe(t, recognizer)

	s := Step(t, "assert-transcript")
	s.Logf("interim results: %d", interims)
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	if wantInterims && interims == 0 {
		s.Errorf("interim=true but no interim transcripts arrived")
	}
	s.Done()
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

func runMetaAgentEcho(t *testing.T, synth map[string]any) {
	t.Helper()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	wavs := make([]string, len(agentEchoTurns))
	for i, turn := range agentEchoTurns {
		s = Step(t, "ensure-prompt-wav")
		path, err := tts.EnsureWAV(ctx, "testdata/agent", turn.prompt, tts.PromptOptions{
			Model: "aura-asteria-en",
		})
		if err != nil {
			s.Fatalf("EnsureWAV turn %d: %v", i+1, err)
		}
		wavs[i] = path
		s.Done()
	}

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-meta")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          metaRecognizer("en-US", nil),
		TTS:          synth,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	for i, turn := range agentEchoTurns {
		recPath := filepath.Join(t.TempDir(), formatAgentTurnRecPath(i+1))

		s = Step(t, formatAgentTurnStep(i+1, "record-and-speak"))
		if err := call.StartRecording(recPath); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (pre): %v", err)
		}
		if err := call.SendWAV(wavs[i]); err != nil {
			s.Fatalf("SendWAV turn %d: %v", i+1, err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, formatAgentTurnStep(i+1, "assert-echo"))
		AssertTranscriptHasMost(s, ctx, recPath, 2, contentWords(turn.prompt)...)
		s.Done()
	}

	HangupAndWaitEnded(t, ctx, call)
}
