// Krisp coverage for the mediajam media server's audiofx path.
//
// Why this file exists separately from agent_test.go: the Krisp features
// (noise isolation, acoustic turn detection) are media-server-internal.
// They have no client-side handle, so the older tests in agent_test.go
// asserted only "the verb was accepted and RTP flowed" — an assertion
// that `PCMBytesIn() > 0` satisfies even when the agent never speaks a
// word, because silence is still PCM bytes. That gave a green run
// against a media server on which Krisp turn detection was completely
// dead.
//
// The tests here close that hole. Every one drives a real conversational
// turn and asserts on an INDEPENDENT transcription of the agent's reply
// audio. A media server whose Krisp path is broken produces silence, and
// silence transcribes to nothing — so these tests fail where the old ones
// passed.
//
// Coverage maps to the Krisp SDK upgrade surface (mediajam PR #99, Krisp
// VIVA UAR C SDK 9.17 → 9.20.0):
//
//   - the new default voice-isolation model (the 9.20 pack drops
//     vi-tel-lite-v1; lite-v2.5 replaces it as the default)
//   - the renamed interrupt-prediction model (ip-v1 → ip-v1.1)
//   - the `noiseIsolation.model` override, and a removed model name whose
//     failure must stay contained
//   - `bargeIn.strategy: "interruptPrediction"`, tested BOTH ways: it must
//     fire on a genuine interruption and must NOT fire on backchannel.
//     Only the negative half distinguishes it from plain VAD barge-in
//
// Known limits of this suite, stated plainly so nobody over-reads a green run:
//
//   - Noise isolation is driven with clean TTS speech, so these tests show
//     that enabling it does not break a call — NOT that it removes noise.
//     A no-op denoiser passes them. Closing that needs a noise-mixed
//     fixture and an A/B on `user_transcript` recall.
//   - Nothing here proves a pinned `model` was the one loaded. When the
//     removed name is pinned the media server logs `krisp nc create:
//     failed to create noise cancellation instance` and carries on with
//     isolation disabled, so the call still completes and the client sees
//     no difference. That evidence is server-side only.
//
// Deliberately NOT covered: a `turnDetection.model` override. The only
// turn-taking model in the pack is the default (tp-v3), so pinning it
// cannot distinguish "the override reached the media server" from "the
// field was ignored" — the noiseIsolation pair above is the coverage that
// actually discriminates.
package verbs

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// Model names from the Krisp 9.20 pack. Kept as constants so a future
// pack bump has one place to edit, and so the "removed" names below stay
// visibly paired with their replacements.
const (
	// krispModelVIDefault is the 9.20 default voice-isolation model — the
	// successor to vi-tel-lite-v1, which the 9.20 pack drops entirely.
	krispModelVIDefault = "krisp-viva-vi-tel-lite-v2.5.kef"

	// krispModelVIFull and krispModelVIFullNewer are the other two
	// voice-isolation models shipped in the 9.20 pack. Exercised to prove
	// the `model` override reaches the media server rather than being
	// parsed and silently dropped.
	krispModelVIFull      = "krisp-viva-vi-tel-v2.5.1.kef"
	krispModelVIFullNewer = "krisp-viva-vi-tel-v2.7.kef"

	// krispModelVIRemoved no longer exists in the 9.20 pack. Any app that
	// pinned it breaks on upgrade; this suite asserts that it breaks
	// CLEANLY — a session-scoped error, not a media-server crash.
	krispModelVIRemoved = "krisp-viva-vi-tel-lite-v1.kef"
)

// krispEchoPrompt and krispEchoKeywords deliberately avoid the NATO
// phonetic alphabet that agentEchoPrompt uses. "alpha, bravo, charlie,
// delta" is the single most predictable word sequence in English: an LLM
// whose STT input degraded to just "repeat ... alpha" will very likely
// emit the rest from priors alone. For a suite whose whole premise is
// "did the caller's audio survive Krisp processing?", an assertion
// satisfiable without the audio surviving is worthless.
//
// These four are concrete, common enough to survive telephony-band STT,
// and — the point — carry no sequential relationship, so each one can
// only appear if it was actually heard. Being unique to this file, they
// also cannot collide with a parallel test's keywords.
const krispEchoPrompt = "Please repeat exactly these four words: walnut, harbor, velvet, trumpet."

var krispEchoKeywords = []string{"walnut", "harbor", "velvet", "trumpet"}

// krispMinKeywordHits mirrors the tolerance in TestVerb_Agent_Echo: STT
// over telephony-quality TTS-of-LLM-reply audio drops the occasional
// word, so we require a majority rather than all four. A silent or
// hallucinated reply still scores 0 and fails.
const krispMinKeywordHits = 2

// krispAgentVerb builds an agent verb wired to sess from opts, then
// applies overrides verbatim on top.
//
// The overrides map is the point of this helper: agentVerbOpts carries
// only the shorthand string form of turnDetection/noiseIsolation and only
// the `enable` flag of bargeIn, whereas the object forms — the ones
// carrying `model`, `threshold`, `strategy` and `vendor` — are precisely
// what this suite exists to exercise. A key left out of overrides is left
// off the verb, so jambonz applies its own default.
func krispAgentVerb(sess *webhook.Session, opts agentVerbOpts, overrides map[string]any) map[string]any {
	if opts.SystemPrompt == "" {
		opts.SystemPrompt = agentEchoSystemPrompt
	}
	opts.ActionURL = SessionURL(sess, "agent-complete")
	opts.EventURL = SessionURL(sess, "agent-turn")
	verb := buildAgentVerb(opts)
	for k, v := range overrides {
		verb[k] = v
	}
	return verb
}

// krispNoiseIsolation builds the object form of noiseIsolation. An empty
// model leaves the field off, so jambonz applies the media server's
// compiled-in default.
func krispNoiseIsolation(model string) map[string]any {
	ni := map[string]any{
		"mode":      "krisp",
		"level":     80,
		"direction": "read",
	}
	if model != "" {
		ni["model"] = model
	}
	return ni
}

// scriptKrispAgent installs verb as the call's script and acks the
// action/event hooks. Mirrors ScriptAgent, but takes a pre-built verb so
// tests can plug in object-form fields agentVerbOpts can't express.
func scriptKrispAgent(sess *webhook.Session, verb map[string]any) {
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		verb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
}

// krispEnsurePromptWAV pre-generates (or reuses the cached) TTS render of
// the echo prompt every test in this file speaks.
func krispEnsurePromptWAV(ctx context.Context, s *StepCtx) string {
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", krispEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	return wavPath
}

// krispSpeakAndRecord drives one conversational turn on an answered call
// under caller-chosen step names. RunAudioRoundtrip does the same job with
// fixed names; the multi-call tests below need per-call prefixes so the
// failure summary says WHICH call broke, which fixed names cannot express.
//
// The leading silence matters: without it the first ~100ms of the reply
// can land before the recording file is open.
// Returns false when the call is no longer answered — jambonz tore it down
// before we got to speak. That is a legitimate outcome for a test driving a
// verb the platform may reject up-front, and it must not abort the test:
// StartRecording and SendWAV both return invalidState on a dead dialog
// (internal/sip/call.go), so fataling here would skip whatever the test
// actually set out to assert afterwards.
func krispSpeakAndRecord(s *StepCtx, call *jsip.Call, wavPath, recPath string) bool {
	if st := call.State(); st != jsip.StateAnswered {
		s.Logf("call is %s, not answered — skipping the spoken turn", st)
		return false
	}
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	return true
}

// krispHangup is HangupAndWaitEnded under a caller-chosen step name, for
// the same reason krispSpeakAndRecord exists.
func krispHangup(t *testing.T, ctx context.Context, call *jsip.Call, stepName string) {
	t.Helper()
	s := Step(t, stepName)
	defer s.Done()
	_ = call.Hangup()
	endCtx, cancel := context.WithTimeout(ctx, EndedDrainTimeout)
	defer cancel()
	_ = call.WaitState(endCtx, jsip.StateEnded)
}

// krispReportTurnPipeline drains the agent eventHook after a user turn and
// logs how far the turn actually got. It asserts nothing; it exists so a
// failing reply assertion arrives with the reason attached instead of just
// "the transcript was empty".
//
// The counts separate the two failure modes this suite has to tell apart:
// `user_transcript=0` means the agent never processed the caller's speech at
// all (the end_of_turn event was not consumed), whereas a non-zero
// user_transcript with `llm_response=0` would put the fault after the
// transcript, in the LLM call.
func krispReportTurnPipeline(s *StepCtx, sess *webhook.Session, within time.Duration) {
	cbs := DrainCallbacks(sess, within)
	s.Logf("post-turn pipeline: user_transcript=%d llm_response=%d turn_end=%d (of %d events: %s)",
		len(findAgentEvents(cbs, "user_transcript")),
		len(findAgentEvents(cbs, "llm_response")),
		len(findAgentEvents(cbs, "turn_end")),
		len(cbs), summarizeEventTypes(cbs))
}

// TestVerb_Krisp_NoiseIsolation_Replies — noise isolation runs Krisp over
// the caller's audio before it reaches STT. The risk this test covers is
// that the new default model (vi-tel-lite-v2.5, which replaced the
// dropped vi-tel-lite-v1) mangles the audio badly enough that the agent
// can no longer understand the caller.
//
// Turn detection is left at the default (STT) so this test isolates the
// noise-isolation path: if it fails while the Krisp turn-detection tests
// pass, it's the voice-isolation model.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (noiseIsolation object form, default model)
//  4. place-call
//  5. answer-record-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-reply
//  9. assert-agent-replied
//  10. hangup-and-wait-ended
func TestVerb_Krisp_NoiseIsolation_Replies(t *testing.T) {
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
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{}, map[string]any{
		"noiseIsolation": krispNoiseIsolation(""),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	rec := RunAudioRoundtrip(t, ctx, call, AudioRoundtripOpts{
		PromptWAV: wavPath,
		RecordTag: "krisp-noise-default",
	})
	call.StopRecording()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Krisp_TurnDetection_AfterGreeting — Krisp turn detection with the
// agent speaking first. The user-speaks-first case (greeting:false) is a known
// open defect and is not tested.
//
// Observation that motivates it: on this cluster, `turnDetection:"krisp"`
// with `greeting:false` never logs `end-of-turn crossed threshold` and the
// agent stays mute, while TestVerb_Krisp_InterruptPrediction — which runs
// the same Krisp turn-taking session but with `greeting:true` — logs
// end-of-turn at prob 0.986 on the very same build. The two differ in two
// variables: whether the agent speaks first, and whether bargeIn is on.
//
// This test holds bargeIn OFF and flips ONLY the greeting, isolating the
// first variable. The hypothesis it tests comes from the media server's own
// comment on ttSession.Process: "Krisp TTv3 hard-resets its internal turn
// state on the botSpeaking edge". With greeting:false the agent never
// speaks before the caller's first turn, so that edge never occurs and the
// model's turn state is never armed.
//
//   - PASS here + FAIL with greeting:false (confirmed 2026-10-20) ⇒ Krisp
//     turn detection requires the agent to have spoken at least once; a
//     user-speaks-first agent is broken. That is a real defect with a
//     precise trigger.
//   - FAIL here too ⇒ the greeting is not the variable; bargeIn is the
//     remaining candidate.
//
// Recording starts only AFTER the greeting has played out, so the
// transcript covers the agent's REPLY and the keyword assertion cannot be
// satisfied by greeting audio.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (turnDetection=krisp, greeting=true, bargeIn off)
//  4. place-call
//  5. answer-and-arm
//  6. wait-out-greeting — let the agent's first turn finish before recording
//  7. speak-and-record — records the reply only
//  8. assert-agent-replied
//  9. hangup-and-wait-ended
func TestVerb_Krisp_TurnDetection_AfterGreeting(t *testing.T) {
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
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// A one-WORD greeting, not a one-sentence one: bargeIn is off for this
	// test, so anything the caller says while the agent is still talking is
	// discarded. The shorter the first turn, the smaller the window in which
	// our prompt could be swallowed and mistaken for a turn-detection fault.
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{
		SystemPrompt: "On your first turn, say only the single word: ready. " +
			"On every turn after that, repeat the user's words back to them verbatim.",
		Greeting: true,
	}, map[string]any{
		"turnDetection": "krisp",
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-arm")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-out-greeting")
	// Wait on the greeting's llm_response EVENT rather than on a guessed
	// duration: LLM first-token latency varies by seconds, and an earlier
	// revision of this test that slept a fixed 10s had our prompt land on
	// top of a still-playing greeting. With bargeIn off that prompt is
	// discarded, which reads exactly like a turn-detection failure — a
	// false positive this test exists to avoid producing.
	greetCtx, cancelGreet := context.WithTimeout(ctx, 45*time.Second)
	greetCbs := WaitCallbacksUntil(greetCtx, sess, func(cbs []webhook.Callback) bool {
		return len(findAgentEvents(cbs, "llm_response")) > 0
	})
	cancelGreet()
	if len(findAgentEvents(greetCbs, "llm_response")) == 0 {
		s.Fatalf("greeting never produced an llm_response in %d events (%s)",
			len(greetCbs), summarizeEventTypes(greetCbs))
	}
	// llm_response fires when the text is ready; TTS playout of the
	// one-word greeting still has to drain out of the media server.
	time.Sleep(6 * time.Second)
	s.Done()

	rec := filepath.Join(t.TempDir(), "krisp-turn-after-greeting.pcm")

	s = Step(t, "speak-and-record")
	krispSpeakAndRecord(s, call, wavPath, rec)
	s.Done()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
