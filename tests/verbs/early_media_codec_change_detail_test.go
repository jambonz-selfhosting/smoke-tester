//go:build detail

package verbs

import (
	"testing"
)

// TestVerb_Dial_EarlyMedia_SameCodec is the control: identical flow, identical
// number of 183s, but the codec never changes. Proves the harness, early media
// and the advertised RTP address are all fine — so a CodecChange failure is
// attributable to the codec change alone.
func TestVerb_Dial_EarlyMedia_SameCodec(t *testing.T) {
	runEarlyMediaDial(t, []string{"PCMA", "PCMA", "PCMA"})
}
