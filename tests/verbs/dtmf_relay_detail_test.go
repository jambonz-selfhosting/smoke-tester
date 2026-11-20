//go:build detail

package verbs

import (
	"testing"
)

func TestVerb_Dial_DTMFRelay_AnchorMedia(t *testing.T) {
	expectDTMFRelay(t, "dtmf-relay-anchored", []any{"anchorMedia", true})
}
