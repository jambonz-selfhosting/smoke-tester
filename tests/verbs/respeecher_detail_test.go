//go:build detail

package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_SpeechCredential_Respeecher — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Respeecher(t *testing.T) {
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, respeecherSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Respeecher_CachedReplay — the same prompt twice on one call;
// both must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Respeecher_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	synth := respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil)
	text := "Your table for four is confirmed for tonight."
	runRespeecherScript(t, "say-respeecher-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Respeecher_AllOptions — one-shot say with every sampling param.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Respeecher_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-respeecher-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", respeecherSynth(cfg.RespeecherVoiceID, "en-US", respeecherSampling())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Respeecher_ModelOverride — options.model_id wins over the
// language map (en-GB would map to en-rt anyway; set explicitly here).
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Respeecher_ModelOverride(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-respeecher-model",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call Acme Corporation on Monday morning.",
			"synthesizer", respeecherSynth(cfg.RespeecherVoiceID, "en-GB",
				map[string]any{"model_id": "en-rt"})),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Respeecher_Ukrainian — one-shot uk-UA (mapped to ua-rt).
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
func TestVerb_Say_Respeecher_Ukrainian(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-respeecher-uk",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Добрий день, ця голосова перевірка працює правильно.",
			"synthesizer", respeecherSynth(respeecherUkVoice, "uk-UA", nil)),
	})
}

// TestVerb_Say_Stream_Respeecher_AllOptions — streaming say with every
// sampling param in voice.sampling_params plus an explicit model_id.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Respeecher_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	opts := respeecherSampling()
	opts["model_id"] = "en-rt"
	runStreamingSay(t, "say-stream-respeecher-options", respeecherSynth(cfg.RespeecherVoiceID, "en-US", opts))
}

// TestVerb_Say_Stream_Respeecher_TwoTurns — two streaming says on one
// socket; the second turn rotates to a fresh context after the first is done.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Respeecher_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	synth := respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil)
	runRespeecherScript(t, "say-stream-respeecher-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Respeecher_MultiSentence — one streaming say of several
// sentences (several continue:true frames in one context).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Respeecher_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runRespeecherScript(t, "say-stream-respeecher-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil)))
}

// TestVerb_Say_Stream_Respeecher_Ukrainian — uk-UA (ua-rt) over the socket.
// Duration only: the offline STT is English-only.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
func TestVerb_Say_Stream_Respeecher_Ukrainian(t *testing.T) {
	vendorExtra(t)
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	runRespeecherScript(t, "say-stream-respeecher-uk", 1*time.Second, 12*time.Second, nil,
		V("say", "text", "Добрий день, ця голосова перевірка працює правильно.", "stream", true,
			"synthesizer", respeecherSynth(respeecherUkVoice, "uk-UA", nil)))
}

// TestVerb_Agent_Respeecher_BargeIn — a live agent on respeecher: the caller
// barges into the greeting (cancel + fresh context, stale audio dropped) and
// the reply must be heard.
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
func TestVerb_Agent_Respeecher_BargeIn(t *testing.T) {
	if !requireRespeecher(t) {
		return
	}
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
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
		TTS:               respeecherSynth(cfg.RespeecherVoiceID, "en-US", nil),
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
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
	rec := filepath.Join(t.TempDir(), "respeecher-barge-reply.pcm")
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
			s.Logf("respeecher turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
