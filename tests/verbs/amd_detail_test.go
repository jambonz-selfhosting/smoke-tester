//go:build detail

package verbs

import (
	"testing"
	"time"
)

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
