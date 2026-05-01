package rest

import (
	"testing"
	"time"
	"unicode/utf8"

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

// TestRecentCalls_List_SP exercises the SP-scoped variant
// `/ServiceProviders/{sid}/RecentCalls`. Returns cross-account call records.
//
// Steps:
//  1. list-recent-calls-sp — GET /ServiceProviders/{sid}/RecentCalls via SP client
func TestRecentCalls_List_SP(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 15*time.Second)

	s := Step(t, "list-recent-calls-sp")
	page, err := spClient.ListRecentCallsBySP(ctx, cfg.SPSID, provision.RecentCallsQuery{
		Page:  1,
		Count: 25,
		Days:  7,
	})
	if err != nil {
		s.Fatalf("sp list recent_calls: %v", err)
	}
	s.Logf("sp recent_calls total=%d len(data)=%d", page.Total.Int(), len(page.Data))
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

// TestWebhookSecret_Get reads the webhook signing secret. No regenerate to
// avoid rotating a real shared secret on the cluster.
//
// Steps:
//  1. get-webhook-secret — GET /WebhookSecret (regenerate=false)
//  2. assert-secret-looks-real — utf-8, length>=8
func TestWebhookSecret_Get(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "get-webhook-secret")
	sec, err := client.GetWebhookSecret(ctx, false)
	if err != nil {
		s.Fatalf("get webhook_secret: %v", err)
	}
	s.Done()

	s = Step(t, "assert-secret-looks-real")
	if !utf8.ValidString(sec) || len(sec) < 8 {
		s.Errorf("webhook_secret looks bogus (len=%d)", len(sec))
	}
	s.Logf("webhook_secret length=%d (masked)", len(sec))
	s.Done()
}

// TestRegisteredSipUsers_List returns an empty or short list on a quiet
// cluster — just assert the schema + shape.
//
// Steps:
//  1. list-registered-sip-users — GET /RegisteredSipUsers
func TestRegisteredSipUsers_List(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "list-registered-sip-users")
	users, err := client.ListRegisteredSipUsers(ctx)
	if err != nil {
		s.Fatalf("list registered_sip_users: %v", err)
	}
	s.Logf("registered_sip_users count=%d", len(users))
	s.Done()
}
