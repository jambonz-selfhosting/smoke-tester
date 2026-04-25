package rest

import (
	"testing"
	"time"
)

// TestSbcs_List — read-only probe of the SBC address list. Covers Tier 1
// row 2.5 (moved from Tier 2 since it's trivially a Tier 1 read test).
//
// Steps:
//  1. list-sbcs — GET /Sbcs (no count assertion; cluster may have zero)
func TestSbcs_List(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "list-sbcs")
	sbcs, err := client.ListSbcs(ctx)
	if err != nil {
		s.Fatalf("list sbcs: %v", err)
	}
	s.Logf("cluster reports %d SBC addresses", len(sbcs))
	s.Done()
}
