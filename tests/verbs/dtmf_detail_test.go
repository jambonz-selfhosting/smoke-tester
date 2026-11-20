//go:build detail

package verbs

import (
	"testing"
)

// TestVerb_Dtmf_Symbols — non-numeric digits "*#0".
func TestVerb_Dtmf_Symbols(t *testing.T) { expectDTMF(t, "*#0", "*#0", "dtmf-symbols", 3) }
