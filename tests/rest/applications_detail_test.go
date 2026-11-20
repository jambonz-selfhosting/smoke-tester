//go:build detail

package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestApplication_CRUD_SP exercises Applications using the SP-scoped token.
// SP tokens have cross-account authority — this creates an Application under
// the main account SID and verifies the SP key can list and fetch it.
//
// Steps:
//  1. create-application-sp — POST /Applications under suite.AccountSID via SP client
//  2. get-application-sp — SP client fetches by sid and asserts account_sid
//  3. list-and-find-account-scope — account-scope client sees the SP-created app
func TestApplication_CRUD_SP(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-application-sp")
	sid := spClient.ManagedApplication(t, ctx, provision.ApplicationCreate{
		Name:       provision.Name("app-sp"),
		AccountSID: suite.AccountSID,
		CallHook: provision.Webhook{
			URL:    "https://example.invalid/hook",
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    "https://example.invalid/status",
			Method: "POST",
		},
	})
	s.Logf("sp created application sid=%s", sid)
	s.Done()

	s = Step(t, "get-application-sp")
	app, err := spClient.GetApplication(ctx, sid)
	if err != nil {
		s.Fatalf("sp get application: %v", err)
	}
	if app.AccountSID != suite.AccountSID {
		s.Errorf("account_sid mismatch: got %q want %q", app.AccountSID, suite.AccountSID)
	}
	s.Done()

	s = Step(t, "list-and-find-account-scope")
	apps, err := client.ListApplications(ctx)
	if err != nil {
		s.Fatalf("account-scope list applications: %v", err)
	}
	var found bool
	for _, a := range apps {
		if a.ApplicationSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Errorf("account-scope list did not include SP-created app %q", sid)
	}
	s.Done()
}
