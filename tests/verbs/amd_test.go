// Tests for Answering Machine Detection (AMD) — the `amd` object on the
// `dial` verb. Tier 4.
//
// AMD is NOT a standalone verb. It is configured as a nested object on
// `dial` (or on POST /Calls) and runs on the DIALED (callee) leg. It is
// STT-driven, not acoustic ML: feature-server transcribes the callee's
// audio and applies word-count heuristics (see
// feature-server/lib/utils/amd-utils.js Amd.evaluateTranscription):
//
//   - transcript wordCount >= thresholdWordCount (default 9)  -> amd_machine_detected (reason "long greeting")
//   - final transcript && wordCount < thresholdWordCount       -> amd_human_detected   (reason "short greeting")
//   - a voicemail-hint match                                   -> amd_machine_detected (reason "hint")
//   - a digit string                                           -> amd_machine_detected (reason "digit count")
//   - no speech within timers.noSpeechTimeoutMs (default 5s)   -> amd_no_speech_detected
//   - no decision within timers.decisionTimeoutMs (default 15s)-> amd_decision_timeout
//   - an acoustic beep detected by the media server's avmd     -> amd_tone_detected
//   - (after machine) greeting-completion silence timer        -> amd_machine_stopped_speaking
//
// Because AMD listens to OUR callee UAS, we drive each outcome by
// controlling what the callee plays after answering: a short greeting, a
// long greeting, or silence. The human / no-speech / decision-timeout tests
// also assert amd_stopped (feature-server tears AMD down and emits it on
// those paths). The machine path does NOT assert amd_stopped: after a
// machine is detected feature-server keeps avmd running for a beep
// (keepAvmd=true), so amd_stopped is deferred past the drain window.
//
// The amd actionHook payload merges callInfo (feature-server task.js
// performHook), so it satisfies schemas/callbacks/amd.schema.json and the
// webhook server contract-validates every event automatically.
//
// Phase-2 test; skipped without NGROK_AUTHTOKEN.
//
// Topology (same shape as dial_test.go, plus amd on the dial):
//
//	Test    --POST /Calls [tag.x_test_id, to=caller-uas]--> Jambonz
//	Jambonz --GET /hook-->                                  Webhook
//	Webhook --[answer, pause, dial{target:callee, amd{actionHook}}, hangup]--> Jambonz
//	Jambonz --INVITE (caller leg)-->                        UAS(caller)
//	Jambonz --INVITE (callee leg)-->                        UAS(callee)
//	UAS(callee) ==greeting | silence==>                     Jambonz  (AMD transcribes this leg)
//	Jambonz --POST /action/amd {type:amd_*}-->              Webhook  // assert
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// HISTORY on the STT-driven detection tests (amd_human_detected /
// amd_machine_detected): these initially produced NO detection against
// jambonz.me while the timer-based events (amd_no_speech_detected /
// amd_decision_timeout / amd_stopped) all fired. This test surfaced a real
// feature-server bug: after the media server moved from FreeSWITCH to
// mediajam (a Go server), transcription events were renamed to a normalized
// vocabulary (`stt.transcription`), and the gather/transcribe/listen paths
// were migrated to it via `sttEvents()` (lib/utils/media-events.js) — but
// `amd-utils.js` was NOT migrated and still registered a listener for the
// legacy FreeSWITCH name `deepgram_transcribe::transcription`, which mediajam
// never emits. So mediajam transcribed the greeting fine (verified in its
// logs) and delivered `stt.transcription`, but AMD's handler never fired and
// only its setTimeout-based timers produced events. Fixed by routing AMD's
// listener registration through `sttEvents()` in amd-utils.js (feature-server
// v11.0.0). With that fix all four detections below pass live on jambonz.me.

// amdScenario configures one AMD dial run.
type amdScenario struct {
	// amdConfig holds amd fields merged on top of the defaults
	// (actionHook + deepgram recognizer are always set by the runner).
	// Use it to set timers / thresholdWordCount per scenario.
	amdConfig map[string]any
	// calleeWAV is a greeting WAV the callee plays after answering;
	// "" means the callee stays silent for the whole hold window.
	calleeWAV string
	// hold is how long the callee keeps its leg answered so AMD's timers
	// run to completion before the callee hangs up.
	hold time.Duration
	// drain is how long we collect amd actionHook callbacks. Must exceed
	// hold so the trailing amd_stopped (fired on dial teardown) is caught.
	drain time.Duration
}

// runAMDDial places a call into jambonz that dials our callee UAS with an
// `amd` config on the `dial` verb, drives the callee's answering behaviour
// per sc, and returns every amd actionHook callback captured (in arrival
// order) together with the ordered list of their `type` values.
//
// Shared steps (each AMD test's Steps: doc comment references these):
//  1. script-dial-amd        — [answer, pause, dial{amd}, hangup] + empty acks
//  2. spawn-callee           — async: answer, (greeting|silence), hold, hangup
//  3. place-and-answer-caller— POST /Calls, answer caller leg, send silence
//  4. collect-amd-events     — drain the session, keep action/amd callbacks
//  5. teardown               — hang up caller, join callee goroutine
func runAMDDial(t *testing.T, ctx context.Context, sc amdScenario) ([]string, []webhook.Callback) {
	t.Helper()
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-dial-amd")
	amd := map[string]any{
		"actionHook": SessionURL(sess, "amd"),
		// No `recognizer` override: AMD then uses the session-default STT
		// (Deepgram, provisioned with use_for_stt at TestMain — the same
		// credential the gather tests rely on) via startAmd's rich default
		// path (enhancedModel etc.). Passing a minimal recognizer object
		// instead bypasses that config and Deepgram produces no transcripts,
		// so greeting-based human/machine detection never fires.
	}
	for k, v := range sc.amdConfig {
		amd[k] = v
	}
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{"type": "user", "name": target}},
			"timeout", 20,
			// anchorMedia keeps RTP inside the cluster data plane — see
			// dial_test.go for the full rationale. AMD attaches its STT
			// media bug to the callee endpoint regardless, but anchored
			// media keeps the leg reachable via the SBC public IP.
			"anchorMedia", true,
			"amd", amd,
			"actionHook", SessionURL(sess, "dial")),
		V("hangup"),
	}))
	// Both hooks return [] — amd events are notifications (feature-server
	// only redirects on a non-empty verb array), and we assert on the
	// captured payloads rather than chaining verbs.
	SessionAckEmpty(sess, "amd", "dial")
	s.Done()

	s = Step(t, "spawn-callee")
	calleeDone := make(chan struct{})
	// Dedicated context so the cleanup below can unblock the goroutine
	// independently of the test's WithTimeout ctx (whose cancel cleanup runs
	// last, LIFO). See the t.Cleanup after the goroutine for why.
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "callee:ringing", "Ringing: %v", err)
				return
			}
			if err := c.Answer(); err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			// Prime outbound RTP so the symmetric-RTP latch opens and AMD's
			// STT recognizer starts receiving the callee leg.
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence-prime", "SendSilence: %v", err)
				return
			}
			// Let AMD arm its STT before the greeting starts (a marginally
			// armed recognizer drops leading words — same pad the
			// single-utterance gather/transcribe tests use).
			time.Sleep(RecognizerArmDelayLong)
			if sc.calleeWAV != "" {
				if err := c.SendWAV(sc.calleeWAV); err != nil {
					GoroutineFailf(t, "callee:greeting", "SendWAV: %v", err)
					return
				}
				// Trailing silence: lets Deepgram close the utterance (so a
				// short greeting goes final -> human) and lets the machine
				// greeting-completion timer fire -> amd_machine_stopped_speaking.
				if err := c.SendSilence(); err != nil {
					GoroutineFailf(t, "callee:silence-trail", "SendSilence: %v", err)
					return
				}
			}
			// Hold the leg up so AMD's timers run to completion before BYE.
			select {
			case <-time.After(sc.hold):
			case <-calleeCtx.Done():
			}
			if err := c.Hangup(); err != nil {
				GoroutineFailf(t, "callee:hangup", "Hangup: %v", err)
			}
			<-c.Done()
		case <-calleeCtx.Done():
			GoroutineFailf(t, "callee", "never received INVITE: %v", calleeCtx.Err())
		}
	}()
	// Always join the callee goroutine, even if a later Step fatals
	// (t.Fatalf → runtime.Goexit skips the teardown join). Registered after
	// spawn so it runs (LIFO) BEFORE WithTimeout's ctx-cancel cleanup, while t
	// is still valid: calleeCancel() unblocks the goroutine's selects, then we
	// wait for it to exit. Without this, a fatal in place-and-answer-caller
	// (e.g. a "480 no available feature servers") orphans the goroutine, which
	// then calls GoroutineFailf after the test has completed → "Log in
	// goroutine after test completed" panic that crashes the whole binary.
	t.Cleanup(func() {
		calleeCancel()
		<-calleeDone
	})
	s.Done()

	s = Step(t, "place-and-answer-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	if err := call.Answer(); err != nil {
		s.Fatalf("caller Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("caller SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-amd-events")
	var amdCBs []webhook.Callback
	var types []string
	for _, cb := range DrainCallbacks(sess, sc.drain) {
		if cb.Hook == "action/amd" {
			amdCBs = append(amdCBs, cb)
			types = append(types, cb.String("type"))
		}
	}
	s.Logf("amd events (%d): %v", len(types), types)
	s.Done()

	s = Step(t, "teardown")
	_ = call.Hangup()
	<-calleeDone
	s.Done()

	return types, amdCBs
}

// firstAMD returns the first captured callback of the given type, or nil.
func firstAMD(cbs []webhook.Callback, typ string) *webhook.Callback {
	for i := range cbs {
		if cbs[i].String("type") == typ {
			return &cbs[i]
		}
	}
	return nil
}

// assertAMDType fails the step (non-fatally) if want is absent from types.
func assertAMDType(s *StepCtx, types []string, want string) {
	for _, got := range types {
		if got == want {
			return
		}
	}
	s.Errorf("expected amd event %q, got %v", want, types)
}

// shortGreeting is a <9-word greeting: feature-server classifies a final
// transcript below thresholdWordCount as a human.
const amdShortGreeting = "Hello, who is this?"

// longGreeting is a >=9-word greeting: feature-server classifies a
// transcript at or above thresholdWordCount as a machine.
const amdLongGreeting = "Hi, you have reached the voicemail of Jordan Smith, " +
	"please leave a message after the tone and I will call you back."

// TestVerb_Dial_AMD_HumanDetected — callee answers with a short (<9-word)
// greeting; AMD classifies it as a human and fires amd_human_detected,
// then amd_stopped.
//
// Steps:
//  1. ensure-greeting-wav — synthesize the short greeting via Deepgram TTS
//  2. script-dial-amd     — see runAMDDial
//  3. spawn-callee        — see runAMDDial
//  4. place-and-answer-caller — see runAMDDial
//  5. collect-amd-events  — see runAMDDial
//  6. teardown            — see runAMDDial
//  7. assert-human-detected — amd_human_detected present, reason "short greeting", greeting non-empty
//  8. assert-stopped      — amd_stopped present
func TestVerb_Dial_AMD_HumanDetected(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	s := Step(t, "ensure-greeting-wav")
	wav, err := tts.EnsureWAV(ctx, "testdata/amd", amdShortGreeting, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(short greeting): %v", err)
	}
	s.Done()

	types, cbs := runAMDDial(t, ctx, amdScenario{
		calleeWAV: wav,
		amdConfig: map[string]any{
			// Raise the no-speech timer well above the default 5s so the
			// callee's greeting (which starts ~1.5s after answer and takes
			// a couple seconds to play + transcribe over Deepgram) is heard
			// before no-speech could fire. Human/machine detection fires the
			// instant a qualifying transcript arrives (~3s), long before the
			// 15s decision timeout — this only removes the no-speech race.
			"timers": map[string]any{"noSpeechTimeoutMs": 20000},
		},
		hold:  8 * time.Second,
		drain: 13 * time.Second,
	})

	s = Step(t, "assert-human-detected")
	assertAMDType(s, types, "amd_human_detected")
	if cb := firstAMD(cbs, "amd_human_detected"); cb != nil {
		if got := cb.String("reason"); got != "short greeting" {
			s.Errorf("amd_human_detected reason: got %q want %q", got, "short greeting")
		}
		if cb.String("greeting") == "" {
			s.Errorf("amd_human_detected greeting is empty; body=%s", string(cb.Body))
		}
	}
	s.Done()

	s = Step(t, "assert-stopped")
	assertAMDType(s, types, "amd_stopped")
	s.Done()
}

// TestVerb_Dial_AMD_MachineDetected — callee answers with a long (>=9-word)
// voicemail-style greeting; AMD classifies it as a machine and fires
// amd_machine_detected, then (after the greeting completes) fires
// amd_machine_stopped_speaking and amd_stopped.
//
// Steps:
//  1. ensure-greeting-wav — synthesize the long greeting via Deepgram TTS
//  2. script-dial-amd     — see runAMDDial
//  3. spawn-callee        — see runAMDDial
//  4. place-and-answer-caller — see runAMDDial
//  5. collect-amd-events  — see runAMDDial
//  6. teardown            — see runAMDDial
//  7. assert-machine-detected — amd_machine_detected present, reason "long greeting", greeting non-empty
//  8. assert-machine-stopped-speaking — amd_machine_stopped_speaking present
//
// NB: amd_stopped is intentionally NOT asserted here. After a machine is
// detected, feature-server (amd-utils.js) calls stopAmd with keepAvmd=true
// and keeps listening for a voicemail beep until the tone timer (20s) or
// call teardown — so amd_stopped is deferred well past a normal drain
// window on the machine path. amd_stopped is covered by the human /
// no-speech / decision-timeout tests, which tear AMD down immediately.
func TestVerb_Dial_AMD_MachineDetected(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	s := Step(t, "ensure-greeting-wav")
	wav, err := tts.EnsureWAV(ctx, "testdata/amd", amdLongGreeting, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(long greeting): %v", err)
	}
	s.Done()

	types, cbs := runAMDDial(t, ctx, amdScenario{
		calleeWAV: wav,
		amdConfig: map[string]any{
			// See the human test: raise no-speech above the default 5s so
			// the greeting is transcribed before no-speech could fire.
			"timers": map[string]any{"noSpeechTimeoutMs": 20000},
		},
		// Longer hold than the human case: after amd_machine_detected the
		// greeting-completion timer (2s of trailing silence) must fire to
		// produce amd_machine_stopped_speaking.
		hold:  10 * time.Second,
		drain: 15 * time.Second,
	})

	s = Step(t, "assert-machine-detected")
	assertAMDType(s, types, "amd_machine_detected")
	if cb := firstAMD(cbs, "amd_machine_detected"); cb != nil {
		if got := cb.String("reason"); got != "long greeting" {
			s.Errorf("amd_machine_detected reason: got %q want %q", got, "long greeting")
		}
		if cb.String("greeting") == "" {
			s.Errorf("amd_machine_detected greeting is empty; body=%s", string(cb.Body))
		}
	}
	s.Done()

	s = Step(t, "assert-machine-stopped-speaking")
	assertAMDType(s, types, "amd_machine_stopped_speaking")
	s.Done()
}

// TestVerb_Dial_AMD_NoSpeechDetected — callee answers but stays silent past
// the default 5s noSpeechTimeoutMs; AMD fires amd_no_speech_detected, then
// amd_stopped.
//
// Steps:
//  1. script-dial-amd     — see runAMDDial
//  2. spawn-callee        — see runAMDDial
//  3. place-and-answer-caller — see runAMDDial
//  4. collect-amd-events  — see runAMDDial
//  5. teardown            — see runAMDDial
//  6. assert-no-speech    — amd_no_speech_detected present
//  7. assert-stopped      — amd_stopped present
func TestVerb_Dial_AMD_NoSpeechDetected(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	types, _ := runAMDDial(t, ctx, amdScenario{
		// No greeting -> silent leg. Default noSpeechTimeoutMs is 5s.
		calleeWAV: "",
		hold:      9 * time.Second,
		drain:     14 * time.Second,
	})

	s := Step(t, "assert-no-speech")
	assertAMDType(s, types, "amd_no_speech_detected")
	s.Done()

	s = Step(t, "assert-stopped")
	assertAMDType(s, types, "amd_stopped")
	s.Done()
}

// TestVerb_Dial_AMD_DecisionTimeout — callee stays silent, but we raise
// noSpeechTimeoutMs well above decisionTimeoutMs so the decision timer wins
// the race: AMD fires amd_decision_timeout (not amd_no_speech_detected),
// then amd_stopped.
//
// This is the deterministic way to force a decision timeout: with real
// speech, any interim transcript of >=9 words trips machine and any final
// of <9 words trips human, so a content-driven timeout is inherently
// flaky. Silence + (noSpeech >> decision) isolates the decision timer.
//
// Steps:
//  1. script-dial-amd     — see runAMDDial (timers override)
//  2. spawn-callee        — see runAMDDial
//  3. place-and-answer-caller — see runAMDDial
//  4. collect-amd-events  — see runAMDDial
//  5. teardown            — see runAMDDial
//  6. assert-decision-timeout — amd_decision_timeout present
//  7. assert-stopped      — amd_stopped present
func TestVerb_Dial_AMD_DecisionTimeout(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)

	types, _ := runAMDDial(t, ctx, amdScenario{
		calleeWAV: "",
		amdConfig: map[string]any{
			"timers": map[string]any{
				"noSpeechTimeoutMs": 60000, // don't let no-speech win
				"decisionTimeoutMs": 3000,  // decision timer fires first
			},
		},
		hold:  7 * time.Second,
		drain: 12 * time.Second,
	})

	s := Step(t, "assert-decision-timeout")
	assertAMDType(s, types, "amd_decision_timeout")
	s.Done()

	s = Step(t, "assert-stopped")
	assertAMDType(s, types, "amd_stopped")
	s.Done()
}

// TestVerb_Dial_AMD_ToneDetected — amd_tone_detected fires when the media
// server's avmd beep detector spots a voicemail beep on the callee leg
// (feature-server amd-utils.js onBeep -> task.emit('amd', {type:
// amd_tone_detected})). This path is acoustic and independent of the STT
// transcript path, so it does NOT share the human/machine gap above.
//
// SKIP-STUB. jambonz's media server (mediajam) DOES implement a Go port of
// mod_avmd's beep detector (internal/audiofx/avmd.go — a DESA-2 estimator
// that looks for a constant-amplitude sinusoid), so this is implementable.
// But a probe this session that played a clean 1000Hz sine (silence-tone-
// silence, 8kHz mono) on the callee leg did NOT produce amd_tone_detected.
// The likely cause is that the beep is transmitted as G.711 µ-law (PCMU):
// µ-law companding adds quantization noise that raises the amplitude
// variance the DESA-2/SMA detector keys on, so a naive sine may not read as
// a constant-amplitude beep after the codec. Enabling this needs a beep
// fixture tuned to survive µ-law and verified to fire avmd against the live
// stack — otherwise it would be a flaky release-gate test.
//
// TODO(tier4): craft a µ-law-robust beep (tune amplitude/frequency against
// mediajam internal/audiofx/avmd.go thresholds), commit it as
// testdata/amd/beep.wav, have the callee play it, and assert
// amd_tone_detected with frequency present.
func TestVerb_Dial_AMD_ToneDetected(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	t.Skip("amd_tone_detected: a clean 1000Hz sine over µ-law did not trip mediajam's avmd; needs a codec-robust beep fixture + live verification; see TODO(tier4)")
}

// TestVerb_Dial_AMD_Error — amd_error is documented in the amd actionHook
// schema (schemas/callbacks/amd.schema.json) but is NOT delivered to the
// actionHook by the current feature-server.
//
// SKIP-STUB (documents a feature-server bug). Two independent gaps:
//
//  1. amd-utils.js startTranscribing catch does `task.emit(AmdEvents.Error,
//     err)` — i.e. it emits on event name "amd_error". But dial.js wires
//     only `this.on('amd', this._onAmdEvent)` — every DELIVERED event uses
//     `task.emit('amd', {type: ...})`. The error path uses the wrong event
//     name, so it has no listener and never reaches the actionHook.
//
//  2. The other error source — missing STT credentials — throws inside the
//     `new Amd(...)` constructor, which dial.js catches at
//     _selectSingleDial and only logs ("Error calling startAmd"); it is
//     never surfaced as an amd_error either.
//
// So there is no code path today that delivers amd_error to the actionHook.
// Enabling a real test requires a feature-server fix to emit
// `task.emit('amd', {type: AmdEvents.Error, ...})` (and to route the
// constructor-throw through the same channel). File upstream, then wire the
// test (force the error by pointing recognizer.vendor at a credential-less
// vendor).
func TestVerb_Dial_AMD_Error(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	t.Skip("amd_error is emitted on the wrong event channel in feature-server (amd-utils.js) and never reaches the actionHook; needs an upstream fix — see doc comment")
}
