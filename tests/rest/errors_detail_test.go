//go:build detail

package rest

import (
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestError_404 verifies that GET on a non-existent SID returns 404 wrapped
// in our structured APIError. Covers Tier 2 row 2.12.
//
// Steps:
//  1. get-nonexistent-application — GET /Applications/<bogus-sid>; expect error
//  2. assert-404-apierror — err is *APIError with status 404
func TestError_404(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "get-nonexistent-application")
	bogus := "00000000-0000-0000-0000-000000000000"
	_, err := client.GetApplication(ctx, bogus)
	if err == nil {
		s.Fatal("expected 404, got nil")
	}
	s.Done()

	s = Step(t, "assert-404-apierror")
	var apiErr *provision.APIError
	if !errors.As(err, &apiErr) {
		s.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	if apiErr.Status != http.StatusNotFound {
		s.Errorf("status mismatch: got %d want 404", apiErr.Status)
	}
	s.Done()
}
