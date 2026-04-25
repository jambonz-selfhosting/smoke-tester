package rest

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestError_403_AccountScopeMutation verifies that an account-scoped token
// cannot perform admin-only mutations — specifically, creating a new
// ServiceProvider. This is the negative-auth half of the scope-coverage
// matrix.
//
// Note: account-scoped *reads* of /ServiceProviders are permitted (return an
// empty list or the SP the account belongs to). Only mutations are denied.
//
// Steps:
//  1. post-service-provider-as-account — direct Request; expected to fail
//  2. assert-denied-401-or-403 — err is *APIError with status 401 or 403
func TestError_403_AccountScopeMutation(t *testing.T) {
	ctx := WithTimeout(t, 10*time.Second)

	s := Step(t, "post-service-provider-as-account")
	_, err := client.Request(ctx, http.MethodPost, "/ServiceProviders",
		map[string]any{"name": provision.Name("sp-403-probe")},
		"", http.StatusCreated)
	if err == nil {
		s.Fatal("expected account-scope to be denied on POST /ServiceProviders, got success (would have created an SP!)")
	}
	s.Done()

	s = Step(t, "assert-denied-401-or-403")
	var apiErr *provision.APIError
	if !errors.As(err, &apiErr) {
		s.Fatalf("expected *APIError, got %T: %v", err, err)
	}
	switch apiErr.Status {
	case http.StatusUnauthorized, http.StatusForbidden:
		s.Logf("account→POST /ServiceProviders denied with %d (%s) — expected", apiErr.Status, apiErr.Msg)
	default:
		s.Errorf("unexpected status on unauthorised mutation: got %d want 401/403", apiErr.Status)
	}
	s.Done()
}

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

// TestError_422_InUse verifies the "in use" / FK-violation class of error.
// Target: delete an Account that still has Applications attached.
//
// Note: on this cluster, deleting a VoipCarrier with attached SipGateways
// succeeds (cascade); the policy differs by resource.
//
// Steps:
//  1. create-account-sp — SP client creates a fresh account (cleanup on defer)
//  2. create-application-in-account — SP client creates an Application under it
//  3. delete-account-with-child — DELETE /Accounts/{sid}; observe 422 or cascade
//  4. assert-child-gone-if-cascade — on cascade path, Application must also be gone
func TestError_422_InUse(t *testing.T) {
	if spClient == nil {
		t.Skip("SP scope not configured — need SP to create/delete Accounts")
	}
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-account-sp")
	acctSID, err := spClient.CreateAccount(ctx, provision.AccountCreate{
		Name:               provision.Name("acct-422"),
		ServiceProviderSID: cfg.SPSID,
	})
	if err != nil {
		s.Fatalf("create account: %v", err)
	}
	t.Cleanup(func() {
		// Best-effort — the point of this test is that the first delete
		// attempt fails, but we still need to clean up afterwards.
		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		_ = spClient.DeleteAccount(ctx2, acctSID)
	})
	s.Done()

	s = Step(t, "create-application-in-account")
	// Use the SP key to create an Application under the new account —
	// ApiKeys scoped to the account would work too but add moving parts.
	// SP key has cross-account power.
	appSID, err := spClient.CreateApplication(ctx, provision.ApplicationCreate{
		Name:           provision.Name("app-for-422"),
		AccountSID:     acctSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
	})
	if err != nil {
		s.Fatalf("create application: %v", err)
	}
	t.Cleanup(func() {
		ctx2, cancel2 := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel2()
		_ = spClient.DeleteApplication(ctx2, appSID)
	})
	s.Done()

	s = Step(t, "delete-account-with-child")
	// Observe the cluster's behaviour — some clusters enforce FK and return
	// 422, others cascade-delete silently. Both are valid implementations;
	// this test records which. If 2xx, also verify the dependent application
	// is gone (confirming cascade rather than orphan).
	err = spClient.DeleteAccount(ctx, acctSID)
	if err != nil {
		var apiErr *provision.APIError
		if !errors.As(err, &apiErr) {
			s.Fatalf("expected *APIError, got %T: %v", err, err)
		}
		s.Logf("account-delete-with-app: status=%d msg=%q (strict FK)", apiErr.Status, apiErr.Msg)
		s.Done()
		return
	}
	s.Done()

	s = Step(t, "assert-child-gone-if-cascade")
	_, getErr := spClient.GetApplication(ctx, appSID)
	if getErr == nil {
		s.Errorf("account delete succeeded but child application still exists (orphan)")
	} else {
		s.Logf("account-delete-with-app: cascade delete (account + app both gone)")
	}
	s.Done()
}
