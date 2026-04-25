package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestApplication_CRUD exercises the Applications resource end-to-end:
// create → get → list → delete, with contract validation on every response.
// Covers Tier 1 row 1.2 (see docs/coverage-matrix.md).
//
// Steps:
//  1. create-application — POST /Applications via managed helper (auto cleanup)
//  2. get-application — fetch by sid and assert fields match the create body
//  3. list-and-find — GET /Applications and confirm sid is in the list
func TestApplication_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	body := provision.ApplicationCreate{
		Name:       provision.Name("app-crud"),
		AccountSID: cfg.AccountSID,
		CallHook: provision.Webhook{
			URL:    "https://example.invalid/hook", // placeholder; no call is placed in this test
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    "https://example.invalid/status",
			Method: "POST",
		},
	}

	s := Step(t, "create-application")
	sid := client.ManagedApplication(t, ctx, body)
	if sid == "" {
		s.Fatal("create returned empty SID")
	}
	s.Logf("created application sid=%s", sid)
	s.Done()

	s = Step(t, "get-application")
	app, err := client.GetApplication(ctx, sid)
	if err != nil {
		s.Fatalf("get application: %v", err)
	}
	if app.ApplicationSID != sid {
		s.Fatalf("sid mismatch: got %q want %q", app.ApplicationSID, sid)
	}
	if app.Name != body.Name {
		s.Errorf("name mismatch: got %q want %q", app.Name, body.Name)
	}
	if app.AccountSID != cfg.AccountSID {
		s.Errorf("account_sid mismatch: got %q want %q", app.AccountSID, cfg.AccountSID)
	}
	if app.CallHook.URL != body.CallHook.URL {
		s.Errorf("call_hook.url mismatch: got %q want %q", app.CallHook.URL, body.CallHook.URL)
	}
	s.Done()

	s = Step(t, "list-and-find")
	apps, err := client.ListApplications(ctx)
	if err != nil {
		s.Fatalf("list applications: %v", err)
	}
	var found bool
	for _, a := range apps {
		if a.ApplicationSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d apps)", sid, len(apps))
	}
	s.Done()

	// delete is handled by t.Cleanup registered in ManagedApplication.
}

// TestApplication_CRUD_SP exercises Applications using the SP-scoped token.
// SP tokens have cross-account authority — this creates an Application under
// the main account SID and verifies the SP key can list and fetch it.
//
// Steps:
//  1. create-application-sp — POST /Applications under cfg.AccountSID via SP client
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
		AccountSID: cfg.AccountSID,
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
	if app.AccountSID != cfg.AccountSID {
		s.Errorf("account_sid mismatch: got %q want %q", app.AccountSID, cfg.AccountSID)
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

// TestApplication_PUT covers the update path: create, mutate, get, verify.
// Covers Tier 2 row 2.1.
//
// Steps:
//  1. create-application — POST /Applications via managed helper (auto cleanup)
//  2. update-application — PUT /Applications/{sid} with new name + call_hook
//  3. get-and-assert-updated — GET /Applications/{sid} and assert fields changed
func TestApplication_PUT(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-application")
	sid := client.ManagedApplication(t, ctx, provision.ApplicationCreate{
		Name:       provision.Name("app-put"),
		AccountSID: cfg.AccountSID,
		CallHook: provision.Webhook{
			URL:    "https://example.invalid/hook",
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    "https://example.invalid/status",
			Method: "POST",
		},
	})
	s.Done()

	s = Step(t, "update-application")
	newName := provision.Name("app-put-renamed")
	newHook := provision.Webhook{URL: "https://example.invalid/hook2", Method: "POST"}
	if err := client.UpdateApplication(ctx, sid, provision.ApplicationUpdate{
		Name:     newName,
		CallHook: &newHook,
	}); err != nil {
		s.Fatalf("update application: %v", err)
	}
	s.Done()

	s = Step(t, "get-and-assert-updated")
	after, err := client.GetApplication(ctx, sid)
	if err != nil {
		s.Fatalf("get after update: %v", err)
	}
	if after.Name != newName {
		s.Errorf("name not updated: got %q want %q", after.Name, newName)
	}
	if after.CallHook.URL != newHook.URL {
		s.Errorf("call_hook.url not updated: got %q want %q", after.CallHook.URL, newHook.URL)
	}
	s.Done()
}
