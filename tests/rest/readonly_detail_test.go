//go:build detail

package rest

import (
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

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
