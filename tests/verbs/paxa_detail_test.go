//go:build detail

package verbs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_SpeechCredential_Paxa — the api-server's STT and TTS credential
// tests pass for the provisioned key; creating a credential never validates it.
//
// Steps:
//  1. test-credential-stt
//  2. test-credential-tts
func TestVerb_SpeechCredential_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	ctx := WithTimeout(t, 45*time.Second)

	s := Step(t, "test-credential-stt")
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, paxaSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()

	s = Step(t, "test-credential-tts")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, paxaSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Paxa_Options — speed and an explicit language reach the HTTP
// leg; an out-of-range or rejected option fails the request and nothing is heard.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Paxa_Options(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-paxa-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order number is forty two. It ships today from our warehouse.",
			"synthesizer", paxaSynth(paxaVoice, "en-US", map[string]any{
				"speed":    1.2,
				"language": "en",
			})),
		wantWords: []string{"warehouse", "ships today"},
	})
}

// TestVerb_Say_Paxa_V1 — a model override to paxa-tts-flash-v1, which only
// serves 24 kHz; the engine resamples. Duration only: v1 voices are Thai.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Paxa_V1(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-paxa-v1",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", paxaThaiText,
			"synthesizer", paxaSynth(paxaThaiVoice, "th-TH", map[string]any{
				"model_id": "paxa-tts-flash-v1",
			})),
	})
}

// TestVerb_Say_Paxa_Thai — one-shot Thai say; th-TH maps to language "th".
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Paxa_Thai(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-paxa-th",
		minDur:     1500 * time.Millisecond,
		maxDur:     12 * time.Second,
		verb: V("say", "text", paxaThaiText,
			"synthesizer", paxaSynth(paxaThaiVoice, "th-TH", nil)),
	})
}

// TestVerb_Say_Stream_Paxa — streaming say over the websocket leg.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-paxa", paxaSynth(paxaVoice, "en-US", nil))
}

// TestVerb_Say_Stream_Paxa_Options — speed and language in the socket's start
// message; a rejected start closes the socket with 4400 and nothing is heard.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Paxa_Options(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runStreamingSay(t, "say-stream-paxa-options", paxaSynth(paxaVoice, "en-US", map[string]any{
		"speed":    0.9,
		"language": "en",
	}))
}

// TestVerb_Say_Stream_Paxa_TwoTurns — two streaming says on one call; the
// second must speak after the first turn's flush completed on the same socket.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Paxa_TwoTurns(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	synth := paxaSynth(paxaVoice, "en-US", nil)
	runPaxaScript(t, "say-stream-paxa-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Paxa_MultiSentence — one streaming say of several
// sentences; the server synthesizes them as separate legs, all heard in order.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Paxa_MultiSentence(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runPaxaScript(t, "say-stream-paxa-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", paxaSynth(paxaVoice, "en-US", nil)))
}

// TestVerb_Say_Stream_Paxa_Thai — Thai over the socket: th-TH must reach the
// service as language "th". Duration only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Paxa_Thai(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	t.Parallel()
	runPaxaScript(t, "say-stream-paxa-th", 1500*time.Millisecond, 12*time.Second, nil,
		V("say", "text", paxaThaiText, "stream", true,
			"synthesizer", paxaSynth(paxaThaiVoice, "th-TH", nil)))
}

// TestVerb_Agent_Paxa_BargeIn — a live agent on paxa TTS: the caller barges
// into a long reply. The socket's cancel drops it, the reply to the barge-in
// is heard on the same socket, and alignment trims the interrupted turn to
// what was actually spoken (turn_end response ends with " -").
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. ask-for-long-reply
//  7. wait-into-reply
//  8. barge-in-and-record-reply
//  9. assert-barge-in-reply
//
// 10. assert-user-interruption
// 11. assert-reply-trimmed
// 12. hangup-and-wait-ended
func TestVerb_Agent_Paxa_BargeIn(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
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
	askWAV := resolveFixture(t, speechWAV)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"If the user says a list of code words, repeat them back verbatim. " +
			"Otherwise answer with a long, slow reply of at least six full sentences " +
			"about what the user said.",
		BargeIn:           true,
		NoResponseTimeout: &noReprompt,
		TTS:               paxaSynth(paxaVoice, "en-US", nil),
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

	WaitFor(t, "settle", 2*time.Second)

	s = Step(t, "ask-for-long-reply")
	if err := call.SendWAV(askWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// paxa's final lands ~2s after speech ends; leave a few seconds of reply
	WaitFor(t, "wait-into-reply", 8*time.Second)

	s = Step(t, "barge-in-and-record-reply")
	rec := filepath.Join(t.TempDir(), "paxa-barge-reply.pcm")
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
	s.Done()

	s = Step(t, "assert-reply-trimmed")
	trimmed := ""
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			trimmed, _ = te.JSON["response"].(string)
			break
		}
	}
	if !strings.HasSuffix(trimmed, " -") {
		s.Errorf("interrupted turn_end response not trimmed: %q (%s)", trimmed, summarizeEventTypes(cbs))
	} else {
		s.Logf("reply heard before barge-in: %q", trimmed)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Gather_Speech_Paxa_Options — every start option reaches Paxa and
// still yields the phrase: recognizer model, hints (vocabulary), style,
// convention and word timestamps (a rejected start fails the gather).
//
// Steps:
//   - script-gather-speech-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Paxa_Options(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	r := paxaRecognizer("en-US", map[string]any{
		"style":      "clean",
		"convention": "written",
		"timestamps": true,
	})
	r["model"] = paxaSTTModel
	r["hints"] = []string{"sun", "shining"}
	transcript := paxaGather(t, r, resolveFixture(t, speechWAV))
	assertHits(t, transcript, 2, "sun", "shining")
}

// TestVerb_Gather_Speech_Paxa_Thai — Thai speech with language th-TH.
//
// Steps:
//   - script-gather-speech-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Paxa_Thai(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	transcript := paxaGather(t, paxaRecognizer("th-TH", nil), resolveFixture(t, paxaThaiWAV))
	assertHits(t, transcript, 2, "จองโต๊ะ", "สองคน", "พรุ่งนี้")
}

// TestVerb_Gather_Speech_Paxa_AnyLanguage — language "any" is not sent, so
// Paxa detects the language itself and the Thai turn still comes back in Thai.
//
// Steps:
//   - script-gather-speech-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - wait-action-gather-callback
//   - assert-transcript
func TestVerb_Gather_Speech_Paxa_AnyLanguage(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	transcript := paxaGather(t, paxaRecognizer("any", nil), resolveFixture(t, paxaThaiWAV))
	assertHits(t, transcript, 2, "จองโต๊ะ", "สองคน", "พรุ่งนี้")
}

// TestVerb_Transcribe_Paxa — transcribe on paxa posts each turn's final
// utterance. Paxa sends interims only at pauses, so their count is logged,
// not asserted.
//
// Steps:
//   - script-transcribe-pause-hangup-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-recognizer
//   - send-wav
//   - collect-transcription-hook
//   - assert-transcript
func TestVerb_Transcribe_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	r := paxaRecognizer("en-US", nil)
	r["interim"] = true
	transcript, interims := paxaTranscribe(t, r)

	s := Step(t, "assert-transcript")
	s.Logf("interim results: %d", interims)
	if transcript == "" {
		s.Fatalf("no final transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2", transcript, hits, transcribeWords)
	}
	s.Done()
}

// TestVerb_Agent_Echo_Paxa — two-turn agent echo with paxa STT: Paxa's own
// turn detection ends each turn (native turn-taking, no local endpointing).
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Paxa(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	runPaxaAgentEcho(t, nil)
}

// TestVerb_Agent_Echo_Paxa_TTS — the same echo with paxa on both sides, so the
// replies heard are LLM tokens streamed into the paxa TTS socket.
//
// Steps:
//   - preflight-skips
//   - ensure-prompt-wav
//   - script-agent-verb-paxa
//   - place-call
//   - answer-and-silence
//   - wait-for-stt
//   - turn-N-record-and-speak
//   - turn-N-assert-echo
func TestVerb_Agent_Echo_Paxa_TTS(t *testing.T) {
	if !requirePaxa(t) {
		return
	}
	runPaxaAgentEcho(t, paxaSynth(paxaVoice, "en-US", nil))
}
