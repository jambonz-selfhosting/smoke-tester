// KugelAudio TTS. Streaming goes over mediajam's /ws/tts/stream dialect, one
// socket per call; a one-shot say goes over POST /v1/tts/generate. All tests
// skip without KUGELAUDIO_API_KEY.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// German voice for the language test (Antonia Meier, de-DE).
const kugelaudioGermanVoice = "1930"

func requireKugelaudio(t *testing.T) {
	t.Helper()
	if !cfg.HasKugelaudio() || kugelaudioLabel == "" {
		t.Skip("KugelAudio tests need KUGELAUDIO_API_KEY (credential missing — skipping, not a failure)")
	}
}

func kugelaudioSynth(voice, language string) map[string]any {
	return map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioLabel,
		"voice":    voice,
		"language": language,
	}
}

// TestVerb_SpeechCredential_Kugelaudio — the api-server's credential test
// passes for the provisioned key. Creating a credential never validates it,
// so this is the only check that the key itself is good.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Kugelaudio(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, kugelaudioSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Kugelaudio — one-shot (HTTP) say.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kugelaudio(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kugelaudio",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", kugelaudioSynth(kugelaudioVoice, "en-US")),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Kugelaudio — streaming say over /ws/tts/stream.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Kugelaudio(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runStreamingSay(t, "say-stream-kugelaudio", kugelaudioSynth(kugelaudioVoice, "en-US"))
}

// TestVerb_Say_Stream_Kugelaudio_TwoTurns — two streaming says on one call
// share the socket; the second must still speak after the first's final frame.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Kugelaudio_TwoTurns(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	synth := kugelaudioSynth(kugelaudioVoice, "en-US")
	runKugelaudioScript(t, "say-stream-kugelaudio-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Kugelaudio_German — de-DE must reach the service as
// "de"; a language it rejects discards the whole config and no turn speaks.
// Audio duration is the check: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Kugelaudio_German(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runKugelaudioScript(t, "say-stream-kugelaudio-de", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", "Guten Tag, dieser Sprachtest funktioniert einwandfrei.", "stream", true,
			"synthesizer", kugelaudioSynth(kugelaudioGermanVoice, "de-DE")))
}

// runKugelaudioScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in that order.
func runKugelaudioScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
	t.Helper()
	ctx := WithTimeout(t, 40*time.Second)
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
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(40))
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

// TestVerb_Agent_Kugelaudio_BargeIn — a live agent on kugelaudio: the caller
// barges into the greeting ({"cancel"} on the socket) and the reply to the
// barge-in must be heard, i.e. the next turn is not swallowed while the
// cancel is acknowledged.
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
func TestVerb_Agent_Kugelaudio_BargeIn(t *testing.T) {
	requireKugelaudio(t)
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
		TTS:               kugelaudioSynth(kugelaudioVoice, "en-US"),
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
	rec := filepath.Join(t.TempDir(), "kugelaudio-barge-reply.pcm")
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
			s.Logf("kugelaudio turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Kugelaudio_HistoryTrimmedToSpoken — kugelaudio is an
// alignment vendor, so after a barge-in turn_end.response must be trimmed to
// what the caller heard. Same flow as the Defect1 test on deepgram.
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
func TestVerb_Agent_Kugelaudio_HistoryTrimmedToSpoken(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runHistoryTrimAfterBargeIn(t, kugelaudioSynth(kugelaudioVoice, "en-US"))
}
