package rest

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestAccount_CRUD_SP exercises Account CRUD using the SP-scoped token.
// Skips when JAMBONZ_SP_API_KEY + JAMBONZ_SP_SID are unset.
// Covers Tier 1 row 1.1.
//
// Steps:
//  1. create-account — POST /Accounts via SP client (managed helper → auto cleanup)
//  2. get-account — SP client fetches by sid; assert fields match create body
//  3. list-and-find — SP client lists accounts and confirms sid is in the list
func TestAccount_CRUD_SP(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured (JAMBONZ_SP_API_KEY / JAMBONZ_SP_SID)")
	}
	ctx := WithTimeout(t, 30*time.Second)

	body := provision.AccountCreate{
		Name:               provision.Name("acct-crud"),
		ServiceProviderSID: cfg.SPSID,
	}

	s := Step(t, "create-account")
	sid := spClient.ManagedAccount(t, ctx, body)
	if sid == "" {
		s.Fatal("create returned empty SID")
	}
	s.Logf("created account sid=%s", sid)
	s.Done()

	s = Step(t, "get-account")
	acct, err := spClient.GetAccount(ctx, sid)
	if err != nil {
		s.Fatalf("get account: %v", err)
	}
	if acct.AccountSID != sid {
		s.Fatalf("sid mismatch: got %q want %q", acct.AccountSID, sid)
	}
	if acct.Name != body.Name {
		s.Errorf("name mismatch: got %q want %q", acct.Name, body.Name)
	}
	if acct.ServiceProviderSID != cfg.SPSID {
		s.Errorf("service_provider_sid mismatch: got %q want %q", acct.ServiceProviderSID, cfg.SPSID)
	}
	s.Done()

	s = Step(t, "list-and-find")
	accounts, err := spClient.ListAccounts(ctx)
	if err != nil {
		s.Fatalf("list accounts: %v", err)
	}
	var found bool
	for _, a := range accounts {
		if a.AccountSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d accounts)", sid, len(accounts))
	}
	s.Done()
}

// TestAccount_PUT exercises PUT on Accounts (SP scope).
// Covers Tier 2 row 2.1.
//
// Steps:
//  1. create-account — POST /Accounts via SP client (managed helper → auto cleanup)
//  2. update-account — PUT /Accounts/{sid} with a new name
//  3. get-and-assert-updated — GET /Accounts/{sid} confirms the new name
func TestAccount_PUT(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured")
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-account")
	sid := spClient.ManagedAccount(t, ctx, provision.AccountCreate{
		Name:               provision.Name("acct-put"),
		ServiceProviderSID: cfg.SPSID,
	})
	s.Done()

	s = Step(t, "update-account")
	newName := provision.Name("acct-put-renamed")
	if err := spClient.UpdateAccount(ctx, sid, provision.AccountUpdate{Name: newName}); err != nil {
		s.Fatalf("update account: %v", err)
	}
	s.Done()

	s = Step(t, "get-and-assert-updated")
	after, err := spClient.GetAccount(ctx, sid)
	if err != nil {
		s.Fatalf("get after update: %v", err)
	}
	if after.Name != newName {
		s.Errorf("name not updated: got %q want %q", after.Name, newName)
	}
	s.Done()
}
