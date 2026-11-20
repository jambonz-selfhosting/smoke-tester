//go:build detail

package verbs

import "testing"

// TestVerb_Jambonz_InterruptPredictionOnly — the same barge-in with STT turn
// detection: feature-server starts an interrupt-only jambonz session.
func TestVerb_Jambonz_InterruptPredictionOnly(t *testing.T) {
	jambonzInterrupt(t, map[string]any{})
}
