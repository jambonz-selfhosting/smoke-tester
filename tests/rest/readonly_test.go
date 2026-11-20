package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestRecentCalls_List exercises the paginated envelope of RecentCalls.
// Covers Tier 2 row 2.3.
//
// Steps:
//  1. list-recent-calls — GET /RecentCalls?page=1&count=25&days=7
//  2. assert-page-is-one — response envelope reports page==1
func TestRecentCalls_List(t *testing.T) {
	ctx := WithTimeout(t, 15*time.Second)

	s := Step(t, "list-recent-calls")
	page, err := client.ListRecentCalls(ctx, provision.RecentCallsQuery{
		Page:  1,
		Count: 25,
		Days:  7,
	})
	if err != nil {
		s.Fatalf("list recent_calls: %v", err)
	}
	s.Logf("recent_calls total=%d len(data)=%d", page.Total.Int(), len(page.Data))
	s.Done()

	s = Step(t, "assert-page-is-one")
	if page.Page.Int() != 1 {
		s.Errorf("page mismatch: got %d want 1", page.Page.Int())
	}
	s.Done()

	s = Step(t, "assert-pagination-respected")
	// Re-query with Count=1 and assert the data slice is bounded by the
	// requested count. Catches a regression where jambonz silently
	// ignores Count and returns the default-sized page.
	page2, err := client.ListRecentCalls(ctx, provision.RecentCallsQuery{
		Page:  1,
		Count: 1,
		Days:  7,
	})
	if err != nil {
		s.Fatalf("list recent_calls (count=1): %v", err)
	}
	if len(page2.Data) > 1 {
		s.Errorf("Count=1 ignored: got %d rows", len(page2.Data))
	}
	s.Done()
}

// TestAlerts_List exercises the Alerts paginated envelope. Covers Tier 2
// row 2.4.
//
// Steps:
//  1. list-alerts — GET /Alerts?page=1&count=25&days=7
//  2. assert-page-is-one — response envelope reports page==1
func TestAlerts_List(t *testing.T) {
	ctx := WithTimeout(t, 15*time.Second)

	s := Step(t, "list-alerts")
	page, err := client.ListAlerts(ctx, provision.AlertsQuery{
		Page:  1,
		Count: 25,
		Days:  7,
	})
	if err != nil {
		s.Fatalf("list alerts: %v", err)
	}
	s.Logf("alerts total=%d len(data)=%d", page.Total.Int(), len(page.Data))
	s.Done()

	s = Step(t, "assert-page-is-one")
	if page.Page.Int() != 1 {
		s.Errorf("page mismatch: got %d want 1", page.Page.Int())
	}
	s.Done()

	s = Step(t, "assert-pagination-respected")
	page2, err := client.ListAlerts(ctx, provision.AlertsQuery{
		Page:  1,
		Count: 1,
		Days:  7,
	})
	if err != nil {
		s.Fatalf("list alerts (count=1): %v", err)
	}
	if len(page2.Data) > 1 {
		s.Errorf("Count=1 ignored: got %d rows", len(page2.Data))
	}
	s.Done()
}
