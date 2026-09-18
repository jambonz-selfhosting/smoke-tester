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
	"strconv"
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

// TestVerb_Krisp_TurnDetection_Replies — with `turnDetection: "krisp"` the
// agent uses Krisp's acoustic end-of-turn model instead of the STT
// vendor's silence detection. This test proves the model actually fires:
// we speak one prompt and assert the agent SPOKE A REPLY containing the
// prompt's content words.
//
// Why the assertion has to be on reply content: when Krisp's end-of-turn
// never crosses threshold, the LLM is never prompted, the agent never
// speaks, and the caller records pure silence — while RTP keeps flowing
// the whole time. Any assertion weaker than "the agent said the right
// words" (inbound byte counts, call completion, verb acceptance) passes
// in exactly that failure mode.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (turnDetection=krisp)
//  4. place-call
//  5. answer-record-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-reply
//  9. assert-agent-replied — independent STT over the reply recording
//  10. hangup-and-wait-ended
func TestVerb_Krisp_TurnDetection_Replies(t *testing.T) {
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
		"turnDetection": "krisp",
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	rec := RunAudioRoundtrip(t, ctx, call, AudioRoundtripOpts{
		PromptWAV: wavPath,
		RecordTag: "krisp-turn-detection",
	})
	call.StopRecording()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Krisp_NoiseIsolation_Replies — noise isolation runs Krisp over
// the caller's audio before it reaches STT. The risk this test covers is
// that the new default model (vi-tel-lite-v2.5, which replaced the
// dropped vi-tel-lite-v1) mangles the audio badly enough that the agent
// can no longer understand the caller.
//
// Turn detection is left at the default (STT) so this test isolates the
// noise-isolation path: if it fails while
// TestVerb_Krisp_TurnDetection_Replies also fails, the fault is shared;
// if only this one fails, it's the voice-isolation model.
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

// TestVerb_Krisp_NoiseIsolation_ModelOverride — the 9.20 voice-isolation
// pack ships three models. The default (lite-v2.5) is covered by
// TestVerb_Krisp_NoiseIsolation_Replies; this test pins each of the other
// two explicitly via `noiseIsolation.model` and asserts a full
// conversational turn still completes on each.
//
// A model the media server fails to load produces silence, so the
// per-model reply assertion doubles as proof the file was found and
// initialised — an assertion on call completion alone would not
// distinguish "loaded the pinned model" from "silently fell back to the
// default".
//
// Steps (N is the 1-based model index; see the per-step Logf for its name):
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. model-N-script-agent-verb
//  4. model-N-place-call
//  5. model-N-answer-and-arm
//  6. model-N-wait-for-stt
//  7. model-N-speak-and-record
//  8. model-N-assert-agent-replied
//  9. model-N-hangup-and-wait-ended
func TestVerb_Krisp_NoiseIsolation_ModelOverride(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 240*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	for i, model := range []string{krispModelVIFull, krispModelVIFullNewer} {
		prefix := "model-" + strconv.Itoa(i+1) + "-"
		_, sess := claimSession(t)

		s = Step(t, prefix+"script-agent-verb")
		s.Logf("pinning noiseIsolation.model=%s", model)
		scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{}, map[string]any{
			"noiseIsolation": krispNoiseIsolation(model),
		}))
		s.Done()

		s = Step(t, prefix+"place-call")
		call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
		s.Done()

		s = Step(t, prefix+"answer-and-arm")
		if err := call.Answer(); err != nil {
			s.Fatalf("Answer: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
		s.Done()

		WaitFor(t, prefix+"wait-for-stt", RecognizerArmDelay)

		rec := filepath.Join(t.TempDir(), "krisp-noise-model-"+strconv.Itoa(i+1)+".pcm")

		s = Step(t, prefix+"speak-and-record")
		if !krispSpeakAndRecord(s, call, wavPath, rec) {
			s.Fatalf("call ended before the prompt could be spoken")
		}
		s.Done()

		s = Step(t, prefix+"assert-agent-replied")
		AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
		s.Done()

		krispHangup(t, ctx, call, prefix+"hangup-and-wait-ended")
	}
}

// TestVerb_Krisp_NoiseIsolation_RemovedModel — the 9.20 pack drops
// vi-tel-lite-v1 (the old default). Any app still pinning it now hits a
// model-load failure. This test asserts the failure is CONTAINED: the
// call must not hang, and — the assertion that matters — a following call
// with a valid model on the same media server must still complete a full
// conversational turn.
//
// That second call is the real subject. A model-load path that faults the
// media server, leaks a session, or wedges the audio pipeline would take
// the next caller down with it; a clean failure leaves the next call
// untouched. This is the regression an operator actually cares about when
// a model name disappears from under a running deployment.
//
// The first call is deliberately NOT asserted to fail in any particular
// way. jambonz may reject the verb up-front, or run the agent with noise
// isolation disabled — both are defensible. Pinning one of them here
// would make this a change-detector for behaviour the platform has not
// committed to. The watchdog on ctx supplies the one assertion that does
// hold: the call must not wedge.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. removed-script-agent-verb (noiseIsolation.model = removed name)
//  4. removed-place-call
//  5. removed-answer-and-arm
//  6. removed-wait-for-stt
//  7. removed-speak-and-record — must return within budget, not hang
//  8. removed-log-outcome
//  9. removed-hangup-and-wait-ended
//  10. survivor-script-agent-verb (valid default model)
//  11. survivor-place-call
//  12. survivor-answer-and-arm
//  13. survivor-wait-for-stt
//  14. survivor-speak-and-record
//  15. survivor-assert-agent-replied — media server survived intact
//  16. survivor-hangup-and-wait-ended
func TestVerb_Krisp_NoiseIsolation_RemovedModel(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 240*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	// --- call 1: the removed model ---------------------------------------

	_, removedSess := claimSession(t)

	s = Step(t, "removed-script-agent-verb")
	scriptKrispAgent(removedSess, krispAgentVerb(removedSess, agentVerbOpts{}, map[string]any{
		"noiseIsolation": krispNoiseIsolation(krispModelVIRemoved),
	}))
	s.Done()

	s = Step(t, "removed-place-call")
	removedCall := placeWebhookCallTo(ctx, t, uas, removedSess, withTimeLimit(90))
	s.Done()

	s = Step(t, "removed-answer-and-arm")
	if err := removedCall.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := removedCall.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "removed-wait-for-stt", RecognizerArmDelay)

	removedRec := filepath.Join(t.TempDir(), "krisp-noise-removed.pcm")

	s = Step(t, "removed-speak-and-record")
	removedSpoke := krispSpeakAndRecord(s, removedCall, wavPath, removedRec)
	s.Done()

	s = Step(t, "removed-log-outcome")
	if removedSpoke {
		s.Logf("removed model %q: jambonz ran the call, %d inbound PCM bytes",
			krispModelVIRemoved, removedCall.PCMBytesIn())
	} else {
		s.Logf("removed model %q: jambonz tore the call down before the prompt "+
			"(also a clean outcome)", krispModelVIRemoved)
	}
	s.Done()

	krispHangup(t, ctx, removedCall, "removed-hangup-and-wait-ended")

	// --- call 2: the survivor --------------------------------------------

	_, survivorSess := claimSession(t)

	s = Step(t, "survivor-script-agent-verb")
	scriptKrispAgent(survivorSess, krispAgentVerb(survivorSess, agentVerbOpts{}, map[string]any{
		"noiseIsolation": krispNoiseIsolation(krispModelVIDefault),
	}))
	s.Done()

	s = Step(t, "survivor-place-call")
	survivorCall := placeWebhookCallTo(ctx, t, uas, survivorSess, withTimeLimit(90))
	s.Done()

	s = Step(t, "survivor-answer-and-arm")
	if err := survivorCall.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := survivorCall.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "survivor-wait-for-stt", RecognizerArmDelay)

	survivorRec := filepath.Join(t.TempDir(), "krisp-noise-survivor.pcm")

	s = Step(t, "survivor-speak-and-record")
	if !krispSpeakAndRecord(s, survivorCall, wavPath, survivorRec) {
		s.Fatalf("survivor call ended before the prompt could be spoken — " +
			"the removed model did not stay contained")
	}
	s.Done()

	s = Step(t, "survivor-assert-agent-replied")
	AssertTranscriptHasMost(s, ctx, survivorRec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	krispHangup(t, ctx, survivorCall, "survivor-hangup-and-wait-ended")
}

// TestVerb_Krisp_InterruptPrediction — `bargeIn.strategy:
// "interruptPrediction"` swaps the default VAD barge-in for Krisp's ML
// interruption model, which scores whether caller speech is a genuine
// attempt to take the floor rather than backchannel ("uh-huh"). That
// model is `ip-v1.1` in the 9.20 pack — RENAMED from `ip-v1`, one of the
// three headline changes in mediajam PR #99. If the rename were not
// carried through, the model would fail to load (the media server logs
// `krisp ip create: failed to create IP instance`) and no interruption
// would ever be scored.
//
// The assertion is on the `user_interruption` eventHook rather than on
// reply audio: an interruption is a control-plane event, and asserting it
// directly is both cheaper and sharper than inferring it from the agent's
// audio going quiet. The harness has no inbound-energy helper anyway.
//
// The agent is prompted to greet at length so there is a long window of
// agent speech to cut into; we let it get ~3s in, then speak over it for
// 4s — well past the model's decision window, and unambiguous speech
// rather than backchannel, so a working model must score it as a genuine
// interruption.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (bargeIn.strategy=interruptPrediction, vendor=krisp)
//  4. place-call
//  5. answer-and-arm
//  6. wait-into-greeting — let the agent get mid-TTS
//  7. speak-over-greeting — 4s of real speech across the agent's audio
//  8. drain-agent-events
//  9. assert-user-interruption — the ip-v1.1 model scored the barge-in
//  10. hangup-and-wait-ended
func TestVerb_Krisp_InterruptPrediction(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"On your first turn, greet the user with a long, slow welcome " +
			"of at least three full sentences so they have time to interrupt. " +
			"On subsequent turns, repeat the user's words back to them verbatim.",
		Greeting: true,
	}, map[string]any{
		"bargeIn": map[string]any{
			"enable":    true,
			"strategy":  "interruptPrediction",
			"vendor":    "krisp",
			"threshold": 0.5,
		},
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-arm")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-greeting")
	// ~3s = LLM first token (~1s) + a couple of seconds of TTS in flight.
	// Longer risks the greeting finishing, which turns this into an
	// ordinary second turn rather than an interruption.
	time.Sleep(3 * time.Second)
	s.Done()

	s = Step(t, "speak-over-greeting")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "drain-agent-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	cbs := DrainCallbacks(sess, 15*time.Second)
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-user-interruption")
	intr := findAgentEvents(cbs, "user_interruption")
	if len(intr) == 0 {
		s.Errorf("no user_interruption event in %d events (%s) — "+
			"interruptPrediction scored no barge-in; check the media server for "+
			"'krisp ip create: failed to create IP instance' (model %s missing)",
			len(cbs), summarizeEventTypes(cbs), "krisp-viva-ip-v1.1.kef")
	} else {
		s.Logf("user_interruption fired %d time(s) under interruptPrediction", len(intr))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Krisp_TurnDetection_AfterGreeting — the isolating experiment for
// the silence that TestVerb_Krisp_TurnDetection_Replies exhibits.
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
//   - PASS here + FAIL in _Replies ⇒ Krisp turn detection requires the
//     agent to have spoken at least once; a user-speaks-first agent is
//     broken. That is a real defect with a precise trigger.
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

// TestVerb_Krisp_InterruptPrediction_WeighsThreshold — the half of
// interrupt-prediction coverage that actually discriminates it from VAD.
//
// TestVerb_Krisp_InterruptPrediction proves an interruption fires when the
// caller talks over the agent — but plain `strategy:"vad"` does that too
// (TestVerb_Agent_BargeIn asserts exactly the same thing with the same
// audio and the same 3s timing). So on its own, the positive test passes
// unchanged if `strategy` is dropped, if the `ip-v1.1` rename was not
// carried through, or if the whole ML model silently fell back to VAD.
//
// What only the ML model can do is SCORE the speech and weigh it against a
// threshold; VAD sees energy alone and cannot weigh anything. This test
// runs the SAME audio down both strategies and asserts they disagree:
//
//   - vad → at least one user_interruption (the control; proves the audio
//     is loud and long enough to trip a barge-in at all, so a zero on the
//     krisp leg means the score was weighed, not that the plumbing is deaf)
//   - interruptPrediction at a threshold ABOVE the score → zero
//
// Both legs zero ⇒ the audio never reached the detector, and the test says
// so rather than passing on a vacuous negative.
//
// On the threshold: an earlier revision used a bare backchannel and a 0.5
// threshold, expecting the model to reject it outright. It did not — the
// media server logged `krisp interrupt-prediction crossed threshold
// prob=0.543` for "Uh huh. Mhm. Right. Yeah, okay.", and scoring three
// seconds of four separate acknowledgements as a genuine floor bid is a
// defensible call, not a defect. So the assertion moved to where the
// measured evidence actually supports one: pin the threshold above the
// observed score. That also covers `bargeIn.threshold`, which nothing else
// in this suite exercises — a threshold parsed and dropped (leaving every
// caller pinned at the 0.5 default) fails this test.
//
// What this test therefore does NOT establish: that backchannel proper
// ("mhm" alone) is suppressed where a genuine interruption is not. That
// needs audio the model scores well below threshold, and this suite has
// not yet found it.
//
// Steps:
//  1. preflight-skips
//  2. ensure-backchannel-wav
//  3. vad-script-agent-verb
//  4. vad-place-call
//  5. vad-answer-and-arm
//  6. vad-wait-into-greeting
//  7. vad-speak-backchannel
//  8. vad-drain-events
//  9. vad-hangup-and-wait-ended
//  10. krisp-script-agent-verb
//  11. krisp-place-call
//  12. krisp-answer-and-arm
//  13. krisp-wait-into-greeting
//  14. krisp-speak-backchannel
//  15. krisp-drain-events
//  16. assert-strategies-disagree
//  17. krisp-hangup-and-wait-ended
func TestVerb_Krisp_InterruptPrediction_WeighsThreshold(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 300*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-backchannel-wav")
	// Long enough to clear bargeIn's default minSpeechDuration (0.5s) so the
	// VAD leg definitely trips — the whole point is that VAD cannot tell this
	// apart from a real interruption.
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent",
		"Uh huh. Mhm. Right. Yeah, okay.", tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	counts := map[string]int{}
	for _, leg := range []struct{ name, strategy string }{
		{"vad", "vad"},
		{"krisp", "interruptPrediction"},
	} {
		_, sess := claimSession(t)
		bargeIn := map[string]any{"enable": true, "strategy": leg.strategy}
		if leg.strategy == "interruptPrediction" {
			bargeIn["vendor"] = "krisp"
			// Above the ~0.54 this audio measured, below certainty: the model
			// must run, score, and decline. Left at the 0.5 default it fires.
			bargeIn["threshold"] = 0.75
		}

		s = Step(t, leg.name+"-script-agent-verb")
		scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{
			SystemPrompt: "You are a friendly voice assistant. " +
				"On your first turn, greet the user with a long, slow welcome " +
				"of at least three full sentences so they have time to interrupt. " +
				"On subsequent turns, repeat the user's words back to them verbatim.",
			Greeting: true,
		}, map[string]any{"bargeIn": bargeIn}))
		s.Done()

		s = Step(t, leg.name+"-place-call")
		call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
		s.Done()

		s = Step(t, leg.name+"-answer-and-arm")
		if err := call.Answer(); err != nil {
			s.Fatalf("Answer: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
		s.Done()

		s = Step(t, leg.name+"-wait-into-greeting")
		// Event-driven rather than a blind sleep: LLM first-token latency
		// varies by seconds, and a "barge-in" delivered before the greeting
		// starts is just an ordinary first turn.
		gctx, cancel := context.WithTimeout(ctx, 45*time.Second)
		greet := WaitCallbacksUntil(gctx, sess, func(cbs []webhook.Callback) bool {
			return len(findAgentEvents(cbs, "llm_response")) > 0
		})
		cancel()
		if len(findAgentEvents(greet, "llm_response")) == 0 {
			s.Fatalf("greeting never produced an llm_response in %d events (%s)",
				len(greet), summarizeEventTypes(greet))
		}
		// Far enough into the greeting that it is definitely still speaking.
		time.Sleep(2 * time.Second)
		s.Done()

		s = Step(t, leg.name+"-speak-backchannel")
		if err := call.SendWAV(wavPath); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		s.Done()

		s = Step(t, leg.name+"-drain-events")
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		cbs := DrainCallbacks(sess, 12*time.Second)
		counts[leg.name] = len(findAgentEvents(cbs, "user_interruption"))
		s.Logf("strategy=%s → user_interruption=%d (of %d events: %s)",
			leg.strategy, counts[leg.name], len(cbs), summarizeEventTypes(cbs))
		s.Done()

		if leg.name == "krisp" {
			s = Step(t, "assert-strategies-disagree")
			switch {
			case counts["vad"] == 0:
				s.Errorf("control leg failed: strategy=vad saw no user_interruption "+
					"for the backchannel either, so this run cannot tell "+
					"discrimination from a barge-in path that never fires "+
					"(vad=%d krisp=%d)", counts["vad"], counts["krisp"])
			case counts["krisp"] > 0:
				s.Errorf("strategy=interruptPrediction with threshold 0.75 still "+
					"interrupted (%d user_interruption events) — same as strategy=vad "+
					"(%d) on identical audio the model scores ~0.54. Either `threshold` "+
					"was parsed and dropped (every caller stuck on the 0.5 default), or "+
					"`strategy`/`vendor` never reached the media server and this fell "+
					"back to VAD", counts["krisp"], counts["vad"])
			default:
				s.Logf("interruptPrediction at threshold 0.75 declined the barge-in "+
					"(krisp=0) where vad took it (vad=%d) — the score was weighed",
					counts["vad"])
			}
			s.Done()
		}

		krispHangup(t, ctx, call, leg.name+"-hangup-and-wait-ended")
	}
}
