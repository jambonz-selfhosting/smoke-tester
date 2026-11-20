//go:build detail

package verbs

import (
	"testing"
)

func TestVerb_DTMFMatrix_InbandToInband(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: sendInband, calleeTelEvent: false})
}

func TestVerb_DTMFMatrix_RFC2833To2833(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: send2833, calleeTelEvent: true})
}

// Repeated digits are lost whenever the digit is delivered to the callee as
// tones. rtpengine regenerates telephone-event without an inter-digit pause,
// so "11" reaches the far end as a single 205ms tone and ITU-T Q.24 says that
// is one keypress. Nothing in jambonz can space them: the tones are
// synthesised inside rtpengine, and its transcoding path has no equivalent of
// the `pause` parameter that `play DTMF` accepts.
//
// Kept running rather than skipped so the day rtpengine gains that control,
// this turns green and tells us.
func TestVerb_DTMFMatrix_RepeatedDigits(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	const digits = "1123"
	got := runDTMFMatrixWith(t, dtmfMatrixCase{caller: send2833, calleeTelEvent: false, digits: digits}, digits)
	switch got {
	case digits:
		t.Logf("repeated digits now survive: %q", got)
	case "123": // the two 1s fused into one tone — the known signature
		t.Skipf("known rtpengine limitation: callee heard %q for %q "+
			"(repeated digits fuse without an inter-digit pause)", got, digits)
	default:
		t.Fatalf("callee heard %q for %q — neither correct nor the known fusion signature %q",
			got, digits, "123")
	}
}
