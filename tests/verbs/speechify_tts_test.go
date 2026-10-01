// Speechify TTS. Speechify has no input-streaming socket, so mediajam POSTs
// each sentence to /v1/audio/stream (or the SSE /with-timestamps variant when
// the agent needs word alignment). All tests skip without SPEECHIFY_API_KEY.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func requireSpeechify(t *testing.T) {
	t.Helper()
	if !cfg.HasSpeechify() || speechifyLabel == "" {
		t.Skip("Speechify tests need SPEECHIFY_API_KEY (credential missing — skipping, not a failure)")
	}
}

func speechifySynth(voice, language string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "speechify",
		"label":    speechifyLabel,
		"voice":    voice,
		"language": language,
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// TestVerb_SpeechCredential_Speechify — the api-server's credential test
// passes for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Speechify(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, speechifySID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Speechify — one-shot say, on simba-3.2 (picked for English).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechify(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechify",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", speechifySynth(speechifyVoice, "en-US", nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Speechify_CachedReplay — the same prompt twice on one call:
// the second say plays the cache file the first one wrote (raw 24 kHz).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Speechify_CachedReplay(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	synth := speechifySynth(speechifyVoice, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runSpeechifyScript(t, "say-speechify-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Speechify_ModelOverride — a verb-level model_id (simba-3.0 on
// an English voice) overrides the language-picked default.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechify_ModelOverride(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechify-simba30",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "The multilingual model is speaking now.",
			"synthesizer", speechifySynth(speechifyVoice, "en-US", map[string]any{"model_id": "simba-3.0"})),
		wantWords: []string{"model", "speaking"},
	})
}

// TestVerb_Say_Speechify_Options — loudness and text normalization reach the
// API (an unknown or malformed option would fail the request: no audio).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Speechify_Options(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-speechify-options",
		minDur:     1 * time.Second,
		maxDur:     10 * time.Second,
		verb: V("say", "text", "Your order number is 42 and it ships today.",
			"synthesizer", speechifySynth(speechifyVoice, "en-US", map[string]any{
				"loudness_normalization": true, "text_normalization": true,
			})),
		wantWords: []string{"order number", "ships today"},
	})
}

// TestVerb_Say_Stream_Speechify — streaming say.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Speechify(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runStreamingSay(t, "say-stream-speechify", speechifySynth(speechifyVoice, "en-US", nil))
}

// TestVerb_Say_Stream_Speechify_TwoTurns — two streaming says on one call;
// the second must still speak after the first turn's flush completed.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Speechify_TwoTurns(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	synth := speechifySynth(speechifyVoice, "en-US", nil)
	runSpeechifyScript(t, "say-stream-speechify-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Speechify_MultiSentence — one streaming say of several
// sentences: each is its own request, and all of them must be heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Speechify_MultiSentence(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSpeechifyScript(t, "say-stream-speechify-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", speechifySynth(speechifyVoice, "en-US", nil)))
}

// TestVerb_Say_Speechify_German — the English voice speaks German text: de-DE
// gets simba-3.0, the English-only simba-3.2 would reject it. Duration is the
// check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Speechify_German(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSpeechifyScript(t, "say-speechify-de", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", "Guten Tag, dieser Sprachtest funktioniert einwandfrei.",
			"synthesizer", speechifySynth(speechifyVoice, "de-DE", nil)))
}

// TestVerb_Say_Stream_Speechify_German — as above on the streaming path.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Speechify_German(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runSpeechifyScript(t, "say-stream-speechify-de", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", "Guten Tag, dieser Sprachtest funktioniert einwandfrei.", "stream", true,
			"synthesizer", speechifySynth(speechifyVoice, "de-DE", nil)))
}

// runSpeechifyScript runs the says then hangs up; wantOrdered, when set, must
// appear in the transcript in that order.
func runSpeechifyScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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

// TestVerb_Agent_Speechify_BargeIn — a live agent on speechify: the caller
// barges into the greeting (the in-flight request is cancelled) and the reply
// to the barge-in must be heard.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. hangup-and-wait-ended
func TestVerb_Agent_Speechify_BargeIn(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting:          true,
		BargeIn:           true,
		NoResponseTimeout: &noReprompt,
		TTS:               speechifySynth(speechifyVoice, "en-US", nil),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-into-greeting", 4*time.Second)

	s = Step(t, "barge-in-and-record-reply")
	rec := filepath.Join(t.TempDir(), "speechify-barge-reply.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(bargeWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-barge-in-reply")
	AssertTranscriptHasMost(s, ctx, rec, 2, "alpha", "bravo", "charlie", "delta")
	s.Done()

	s = Step(t, "assert-user-interruption")
	cbs := DrainCallbacks(sess, time.Second)
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		s.Errorf("no user_interruption event: %s", summarizeEventTypes(cbs))
	}
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if lat, ok := te.JSON["latency"].(map[string]any); ok {
			s.Logf("speechify turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Speechify_HistoryTrimmedToSpoken — speechify is an alignment
// vendor (speech marks), so after a barge-in turn_end.response must be
// trimmed to what the caller heard.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. request-count
//  8. wait-into-count
//  9. send-interrupt-wav
//
// 10. collect-events
// 11. assert-interruption-confirmed
// 12. assert-response-matches-spoken
func TestVerb_Agent_Speechify_HistoryTrimmedToSpoken(t *testing.T) {
	requireSpeechify(t)
	t.Parallel()
	runHistoryTrimAfterBargeIn(t, speechifySynth(speechifyVoice, "en-US", nil))
}
