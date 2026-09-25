package rest

import (
	"net/http"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestMediaCapture_Account_REST pins the disable_media_capture contract on
// Accounts: default opted in, writable by SP and account scope, 0/1 and
// booleans only. The SP-level flag is exercised by the verbs test
// (TestMediaCapture_NoRecordHeader), which runs alone in its package; this
// package runs concurrently with it, so it never writes the SP flag.
//
// Steps:
//  1. create-account-default — POST /Accounts; GET shows 0 plus the derived field
//  2. create-account-opted-out — POST /Accounts with disable_media_capture=true
//  3. sp-scope-toggle — SP PUT true then false on the default account
//  4. account-scope-toggle — account-scope PUT on its own (suite) account
//  5. reject-non-boolean — PUT with "yes"/2/null on account and SP returns 400
func TestMediaCapture_Account_REST(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured (JAMBONZ_SP_API_KEY / JAMBONZ_SP_SID)")
	}
	t.Parallel()
	ctx := WithTimeout(t, 60*time.Second)
	on, off := true, false

	s := Step(t, "create-account-default")
	sid := spClient.ManagedAccount(t, ctx, provision.AccountCreate{
		Name:               provision.Name("media-capture"),
		ServiceProviderSID: cfg.SPSID,
	})
	acct, err := spClient.GetAccount(ctx, sid)
	if err != nil {
		s.Fatalf("get account: %v", err)
	}
	if acct.DisableMediaCapture {
		s.Errorf("new account should be opted in, got disable_media_capture=%v", acct.DisableMediaCapture)
	}
	sp, err := spClient.GetServiceProvider(ctx, cfg.SPSID)
	if err != nil {
		s.Fatalf("get service provider: %v", err)
	}
	s.Logf("service provider disable_media_capture=%v, account derived=%v",
		sp.DisableMediaCapture, acct.ServiceProviderDisableMediaCapture)
	s.Done()

	s = Step(t, "create-account-opted-out")
	sid2 := spClient.ManagedAccount(t, ctx, provision.AccountCreate{
		Name:                provision.Name("media-capture-off"),
		ServiceProviderSID:  cfg.SPSID,
		DisableMediaCapture: &on,
	})
	acct, err = spClient.GetAccount(ctx, sid2)
	if err != nil {
		s.Fatalf("get account: %v", err)
	}
	if !acct.DisableMediaCapture {
		s.Errorf("account created with disable_media_capture=true reads back %v", acct.DisableMediaCapture)
	}
	s.Done()

	s = Step(t, "sp-scope-toggle")
	for _, v := range []bool{true, false} {
		v := v
		if err := spClient.UpdateAccount(ctx, sid, provision.AccountUpdate{DisableMediaCapture: &v}); err != nil {
			s.Fatalf("SP PUT disable_media_capture=%v: %v", v, err)
		}
		acct, err := spClient.GetAccount(ctx, sid)
		if err != nil {
			s.Fatalf("get account: %v", err)
		}
		if bool(acct.DisableMediaCapture) != v {
			s.Errorf("after SP PUT %v: got %v", v, acct.DisableMediaCapture)
		}
	}
	s.Done()

	s = Step(t, "account-scope-toggle")
	t.Cleanup(func() {
		_ = client.UpdateAccount(ctx, suite.AccountSID, provision.AccountUpdate{DisableMediaCapture: &off})
	})
	for _, v := range []bool{true, false} {
		v := v
		if err := client.UpdateAccount(ctx, suite.AccountSID, provision.AccountUpdate{DisableMediaCapture: &v}); err != nil {
			s.Fatalf("account-scope PUT disable_media_capture=%v: %v", v, err)
		}
		acct, err := client.GetAccount(ctx, suite.AccountSID)
		if err != nil {
			s.Fatalf("get account: %v", err)
		}
		if bool(acct.DisableMediaCapture) != v {
			s.Errorf("after account-scope PUT %v: got %v", v, acct.DisableMediaCapture)
		}
	}
	s.Done()

	s = Step(t, "reject-non-boolean")
	for _, bad := range []any{"yes", 2, nil} {
		body := map[string]any{"disable_media_capture": bad}
		_, err := spClient.Request(ctx, http.MethodPut, "/Accounts/"+sid, body, "", http.StatusNoContent)
		if got := provision.StatusOf(err); got != http.StatusBadRequest {
			s.Errorf("account PUT disable_media_capture=%#v: status %d, want 400 (err=%v)", bad, got, err)
		}
		_, err = spClient.Request(ctx, http.MethodPut, "/ServiceProviders/"+cfg.SPSID, body, "", http.StatusNoContent)
		if got := provision.StatusOf(err); got != http.StatusBadRequest {
			s.Errorf("SP PUT disable_media_capture=%#v: status %d, want 400 (err=%v)", bad, got, err)
		}
	}
	s.Done()
}
