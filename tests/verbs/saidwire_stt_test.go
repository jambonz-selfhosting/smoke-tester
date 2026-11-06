// Saidwire realtime STT (Deepgram /v1/listen protocol): credential test,
// gather, transcribe and agent echo. Every test passes without exercising
// saidwire when SAIDWIRE_API_KEY is unset.
package verbs

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// requireSaidwire logs and reports false when the key is unset; callers return.
func requireSaidwire(t *testing.T) bool {
	t.Helper()
	if !cfg.HasSaidwire() || saidwireLabel == "" {
		t.Log("SAIDWIRE_API_KEY not set — passing without exercising saidwire")
		return false
	}
	return true
}

func saidwireRecognizer(label string, options map[string]any) map[string]any {
	r := map[string]any{
		"vendor":   "saidwire",
		"label":    label,
		"language": "en-US",
	}
	if options != nil {
		r["saidwireOptions"] = options
	}
	return r
}

// TestVerb_SpeechCredential_Saidwire — the api-server's STT credential test
// passes for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential-stt
func TestVerb_SpeechCredential_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, saidwireSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// saidwireGather runs one gather on saidwire with recognizer, plays wavPath,
// and returns the recognized transcript ("" when the action has none).
func saidwireGather(t *testing.T, recognizer map[string]any, wavPath string) string {
	t.Helper()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-gather-speech-saidwire")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", recognizer),
		// keep the call up while the clip is still streaming; the test hangs up
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
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	_ = call.Hangup()
	return extractTranscript(cb)
}

// TestVerb_Gather_Speech_Saidwire — gather input=[speech] on saidwire with no
// options returns the spoken phrase (server default endpointing).
//
// Steps:
//   - script-gather-speech-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
		return
	}
	transcript := saidwireGather(t, saidwireRecognizer(saidwireLabel, nil), resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Saidwire_Options — every saidwireOptions field
// reaches Saidwire and the phrase still comes back (a rejected query fails
// the gather): endpointing 500, utteranceEndMs, vadEvents, punctuate,
// smartFormatting, plus interim results.
//
// Steps:
//   - script-gather-speech-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Saidwire_Options(t *testing.T) {
	vendorExtra(t)
	if !requireSaidwire(t) {
		return
	}
	r := saidwireRecognizer(saidwireLabel, map[string]any{
		"endpointing":     500,
		"utteranceEndMs":  1000,
		"vadEvents":       true,
		"punctuate":       true,
		"smartFormatting": true,
	})
	r["interim"] = true
	transcript := saidwireGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Saidwire_ApiUriCredential — the credential's api_uri
// selects the STT host: the explicit default host transcribes, an unreachable
// one does not (so the field is really used, not ignored).
//
// Steps:
//   - provision-credentials
//   - script-gather-speech-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
//   - assert-no-transcript-unreachable
func TestVerb_Gather_Speech_Saidwire_ApiUriCredential(t *testing.T) {
	vendorExtra(t)
	if !requireSaidwire(t) {
		return
	}

	s := Step(t, "provision-credentials")
	labels := map[string]string{}
	for name, uri := range map[string]string{
		"explicit":    "https://api.saidwire.com",
		"unreachable": "https://unreachable.invalid",
	} {
		label := provision.Name("saidwire-" + name)
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
			Vendor: "saidwire", Label: label, APIKey: cfg.SaidwireAPIKey, UseForSTT: true, APIURI: uri,
		})
		cancel()
		if err != nil {
			s.Fatalf("create %s credential: %v", name, err)
		}
		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, sid)
		})
		labels[name] = label
	}
	s.Done()

	wav := resolveFixture(t, speechWAV)
	opts := map[string]any{"endpointing": 500}
	good := saidwireGather(t, saidwireRecognizer(labels["explicit"], opts), wav)
	assertHits(t, good, 2, "sun", "shining")

	bad := saidwireGather(t, saidwireRecognizer(labels["unreachable"], opts), wav)
	s = Step(t, "assert-no-transcript-unreachable")
	if bad != "" {
		s.Errorf("unreachable api_uri still transcribed %q: api_uri is not reaching the STT connection", bad)
	}
	s.Done()
}

// TestVerb_Transcribe_Saidwire — transcribe with interim results posts
// interims and the final utterance to transcriptionHook.
//
// Steps:
//   - script-transcribe-pause-hangup-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
		return
	}
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	r := saidwireRecognizer(saidwireLabel, map[string]any{"endpointing": 500})
	r["interim"] = true

	s := Step(t, "script-transcribe-pause-hangup-saidwire")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", r),
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

	s = Step(t, "assert-transcript")
	s.Logf("interim results: %d", interims)
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	// Saidwire streams a first-pass interim while the speaker talks.
	if interims == 0 {
		s.Errorf("interim=true but no interim results arrived")
	}
	s.Done()
}

// TestVerb_Agent_Echo_Saidwire — two-turn agent echo with saidwire STT;
// turns end on Saidwire's speech_final (endpointing 500), as with deepgram.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-saidwire
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Saidwire(t *testing.T) {
	if !requireSaidwire(t) {
		return
	}
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

	s = Step(t, "script-agent-verb-saidwire")
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		STT:          saidwireRecognizer(saidwireLabel, map[string]any{"endpointing": 500}),
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
