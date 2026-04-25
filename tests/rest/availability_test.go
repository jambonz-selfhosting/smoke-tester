package rest

import (
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestAvailability — read-only probe. Covers Tier 2 row 2.6 (moved into Tier 1
// since it's a trivial GET).
//
// Steps:
//  1. check-subdomain-availability — GET /Availability?type=subdomain&value=…
func TestAvailability(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "check-subdomain-availability")
	val := fmt.Sprintf("it-%s.example", provision.RunID())
	available, err := client.CheckAvailability(ctx, provision.AvailabilitySubdomain, val)
	if err != nil {
		s.Fatalf("check availability: %v", err)
	}
	s.Logf("subdomain %q available=%v", val, available)
	s.Done()
}
