//go:build detail

package verbs

import (
	"testing"
)

// TestVerb_Pause_3s — longer pause to be sure duration is actually honored.
func TestVerb_Pause_3s(t *testing.T) { runPauseTest(t, 3) }
