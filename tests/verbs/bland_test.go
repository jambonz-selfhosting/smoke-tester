// Bland AI TTS: one-shot say over POST /v2/tts (raw pcm), streaming say and
// the agent over the /v2/tts/ws socket (one context per turn, cancel on clear).
// The voice UUID picks the model; there is no language parameter. TTS only.
// Never t.Parallel: the default bland org allows one concurrent request.
// Every test passes without exercising bland when BLAND_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Built-in Matthew voice (btts-3).
const blandMatthewVoice = "1d4dc605-071b-4eee-81b3-099b2894d0d1"

// requireBland logs and reports false when the key is unset; callers return.
func requireBland(t *testing.T) bool {
	t.Helper()
	if !cfg.HasBland() || blandLabel == "" {
		t.Log("BLAND_API_KEY not set — passing without exercising bland")
		return false
	}
	return true
}

func blandSynth(voice string, options map[string]any) map[string]any {
	s := map[string]any{
		"vendor":   "bland",
		"label":    blandLabel,
		"voice":    voice,
		"language": "en-US",
	}
	if options != nil {
		s["options"] = options
	}
	return s
}

// blandControls sets every documented control, each inside 0.0–1.0.
func blandControls() map[string]any {
	return map[string]any{
		"expressiveness": 0.8,
		"stability":      0.4,
	}
}

// TestVerb_SpeechCredential_Bland — the api-server's credential test
// (synthAudio with the cache off) passes for the provisioned key.
//
// Steps:
//  1. test-credential
func TestVerb_SpeechCredential_Bland(t *testing.T) {
	if !requireBland(t) {
		return
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "test-credential")
	if _, err := client.TestAccountSpeechCredentialTTS(ctx, suite.AccountSID, blandSID); err != nil {
		s.Fatalf("credential test: %v", err)
	}
	s.Done()
}

// TestVerb_Say_Bland — one-shot say with the default voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Bland(t *testing.T) {
	if !requireBland(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-bland",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", blandSynth(cfg.BlandVoiceID, nil)),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Bland_CachedReplay — the same prompt twice on one call;
// both must be heard. The harness can't see whether the second came from cache.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Bland_CachedReplay(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	synth := blandSynth(cfg.BlandVoiceID, nil)
	text := "Your table for four is confirmed for tonight."
	runBlandScript(t, "say-bland-cache", 2*time.Second, 16*time.Second,
		[]string{"confirmed", "confirmed"},
		V("say", "text", text, "synthesizer", synth),
		V("say", "text", text, "synthesizer", synth))
}

// TestVerb_Say_Bland_AllOptions — one-shot say with expressiveness + stability.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Bland_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-bland-options",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Your order ships today from our warehouse.",
			"synthesizer", blandSynth(cfg.BlandVoiceID, blandControls())),
		wantWords: []string{"warehouse"},
	})
}

// TestVerb_Say_Bland_OtherVoice — one-shot say on the Matthew voice.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Bland_OtherVoice(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-bland-matthew",
		minDur:     1 * time.Second,
		maxDur:     12 * time.Second,
		verb: V("say", "text", "Please call the front desk on Monday morning.",
			"synthesizer", blandSynth(blandMatthewVoice, nil)),
		wantWords: []string{"monday", "morning"},
	})
}

// TestVerb_Say_Stream_Bland — streaming say over the websocket.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Bland(t *testing.T) {
	if !requireBland(t) {
		return
	}
	runStreamingSay(t, "say-stream-bland", blandSynth(cfg.BlandVoiceID, nil))
}

// TestVerb_Say_Stream_Bland_AllOptions — streaming say with the init controls.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Bland_AllOptions(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	runStreamingSay(t, "say-stream-bland-options", blandSynth(cfg.BlandVoiceID, blandControls()))
}

// TestVerb_Say_Stream_Bland_TwoTurns — two streaming says on one socket;
// the second turn rotates to a fresh context after the first is done.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Bland_TwoTurns(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	synth := blandSynth(cfg.BlandVoiceID, nil)
	runBlandScript(t, "say-stream-bland-2turns", 1*time.Second, 15*time.Second,
		[]string{"weather", "garden"},
		V("say", "text", "The first sentence is about the weather.", "stream", true, "synthesizer", synth),
		V("say", "text", "The second sentence is about the garden.", "stream", true, "synthesizer", synth))
}

// TestVerb_Say_Stream_Bland_MultiSentence — one streaming say of several
// sentences (several speak frames in one context).
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript-ordered
func TestVerb_Say_Stream_Bland_MultiSentence(t *testing.T) {
	vendorExtra(t)
	if !requireBland(t) {
		return
	}
	runBlandScript(t, "say-stream-bland-multi", 3*time.Second, 20*time.Second,
		[]string{"monday", "wednesday", "friday"},
		V("say", "text", "We open at nine on Monday. On Wednesday we close early. "+
			"Friday is our late night, until ten.",
			"stream", true, "synthesizer", blandSynth(cfg.BlandVoiceID, nil)))
}

// TestVerb_Agent_Bland_BargeIn — a live agent on bland: the caller barges
// into the greeting (cancel + fresh context, stale audio dropped) and the
// reply must be heard.
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
func TestVerb_Agent_Bland_BargeIn(t *testing.T) {
	if !requireBland(t) {
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
		TTS:               blandSynth(cfg.BlandVoiceID, nil),
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
	rec := filepath.Join(t.TempDir(), "bland-barge-reply.pcm")
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
			s.Logf("bland turn_end tts_ms=%v", lat["tts_ms"])
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// runBlandScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in order.
func runBlandScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
