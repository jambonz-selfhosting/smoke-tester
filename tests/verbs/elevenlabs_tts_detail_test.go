//go:build detail

package verbs

import (
	"testing"
)

// TestVerb_Say_Stream_ElevenlabsV3Conversational — streaming say over Text to
// Dialogue; the [excited] audio tag must be performed, not read aloud.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV3Conversational(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v3conv", "eleven_v3_conversational", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsV3 — as above on eleven_v3.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV3(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v3", "eleven_v3", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsV4Turbo — streaming say on eleven_v4_turbo over
// Text to Dialogue, with an audio tag that must not be read aloud.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV4Turbo(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v4turbo", "eleven_v4_turbo", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Say_Stream_ElevenlabsV4 — as above on eleven_v4.
//
// Steps:
//  1. script-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_ElevenlabsV4(t *testing.T) {
	requireElevenlabs(t)
	t.Parallel()
	runElevenlabsSay(t, "say-stream-11labs-v4", "eleven_v4", true,
		"[excited] Streaming synthesis is working correctly.", "excited")
}

// TestVerb_Agent_ElevenlabsV3Conversational — a live agent on
// eleven_v3_conversational: a barge-in into the greeting (the socket is
// rotated), then a turn after 25s of silence. noResponseTimeout is off so no
// reprompt fills that gap; it outlasts the 20s TTD receive timeout unless
// keepalives are flowing.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. idle-past-ttd-timeout
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. assert-idle-turn-end
// 14. hangup-and-wait-ended
func TestVerb_Agent_ElevenlabsV3Conversational(t *testing.T) {
	runElevenlabsAgent(t, "eleven_v3_conversational")
}

// TestVerb_Agent_ElevenlabsV4Turbo — the same agent flow on eleven_v4_turbo,
// the low-latency model built for agent loops.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-into-greeting
//  7. barge-in-and-record-reply
//  8. assert-barge-in-reply
//  9. assert-user-interruption
//
// 10. idle-past-ttd-timeout
// 11. idle-turn-record-and-speak
// 12. assert-idle-turn-reply
// 13. assert-idle-turn-end
// 14. hangup-and-wait-ended
func TestVerb_Agent_ElevenlabsV4Turbo(t *testing.T) {
	runElevenlabsAgent(t, "eleven_v4_turbo")
}
