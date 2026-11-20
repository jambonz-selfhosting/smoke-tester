//go:build detail

package verbs

import (
	"testing"
)

// TestFive9_Voicestream_ReverseBlocked — same flow with a sendrecv offer, so
// the SBC's feature-server -> Five9 block is observable at the caller.
//
// Steps:
//   - script-listen
//   - provision-application
//   - invite-as-five9
//   - send-speech
//   - hangup-and-wait-ws-close
//   - assert-ws-audio
//   - assert-reverse-blocked
func TestFive9_Voicestream_ReverseBlocked(t *testing.T) {
	t.Parallel()
	runFive9ListenTest(t, "sendrecv")
}

// TestMute_ChildLeg_Directional — mute_status on the dial child leg is
// handled by sbc-outbound: the callee must go unheard while still hearing the caller.
//
// Steps:
//   - script-dial-to-callee
//   - provision-application
//   - invite-caller
//   - resolve-call-sids
//   - baseline-audio-both-ways
//   - mute
//   - assert-muted-leg-silenced
//   - assert-muted-leg-still-hears
//   - assert-dtmf-directional
//   - unmute
//   - assert-unmute-restores-audio
func TestMute_ChildLeg_Directional(t *testing.T) {
	t.Parallel()
	runMuteDirectionalTest(t, true)
}
