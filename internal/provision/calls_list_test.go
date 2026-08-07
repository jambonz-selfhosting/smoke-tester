package provision

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
)

// newAccountClient builds a *Client pointed at a local httptest server, with
// a real contract.Validator rooted at the repo's schemas/ dir (mirrors the
// pattern in internal/contract/contract_test.go). accountSID may be "" to
// exercise the SP-scope (no account) case.
func newAccountClient(t *testing.T, handler http.HandlerFunc, accountSID string) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	root, err := contract.ResolveSchemasRoot()
	if err != nil {
		t.Fatalf("resolve schemas root: %v", err)
	}
	v, err := contract.New(root)
	if err != nil {
		t.Fatalf("new validator: %v", err)
	}
	c := New(srv.URL, "test-api-key", accountSID, v)
	return c, srv
}

// TestListLiveCalls_RequestShape pins behaviour 1: GET on the account-scoped
// /Accounts/{sid}/Calls path with a Bearer auth header. The handler captures
// the request regardless of how the response body is decoded/validated
// downstream, so this test is independent of the (possibly still-pending)
// listCalls schema.
func TestListLiveCalls_RequestShape(t *testing.T) {
	const accountSID = "acct-shape-1"

	var gotMethod, gotPath, gotAuth string
	handler := func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}

	c, _ := newAccountClient(t, handler, accountSID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	if _, err := c.ListLiveCalls(ctx); err != nil {
		// The body-decode/contract-validation outcome is asserted by other
		// tests; here we only care that the request was actually sent as
		// specified. Log so a genuinely broken transport is still visible.
		t.Logf("ListLiveCalls returned err (checked elsewhere): %v", err)
	}

	if gotMethod != http.MethodGet {
		t.Errorf("method = %q, want GET", gotMethod)
	}
	wantPath := "/Accounts/" + accountSID + "/Calls"
	if gotPath != wantPath {
		t.Errorf("path = %q, want %q", gotPath, wantPath)
	}
	if gotAuth != "Bearer test-api-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer test-api-key")
	}
}

// TestListLiveCalls_DecodesArray pins behaviour 2 and the edge cases around
// it: multiple elements decoded in order, an element missing an optional
// field ("to") zero-valuing rather than erroring, and an unrecognized extra
// property on an element not causing an error (behaviour 4).
func TestListLiveCalls_DecodesArray(t *testing.T) {
	body := `[
		{"call_sid":"CA1","call_status":"in-progress","direction":"inbound","from":"+15551110000","to":"+15552220000","carrier_extra_field":"ignore me"},
		{"call_sid":"CA2","call_status":"ringing","direction":"outbound","from":"+15553330000"}
	]`

	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(body))
	}
	c, _ := newAccountClient(t, handler, "acct-decode-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := c.ListLiveCalls(ctx)
	if err != nil {
		if errors.Is(err, contract.ErrNoSchema) {
			t.Fatalf("pending T-06 schema: %v", err)
		}
		t.Fatalf("ListLiveCalls: %v", err)
	}

	if len(got) != 2 {
		t.Fatalf("len(got) = %d, want 2 (got %+v)", len(got), got)
	}

	first := got[0]
	if first.CallSID != "CA1" || first.CallStatus != "in-progress" ||
		first.Direction != "inbound" || first.From != "+15551110000" || first.To != "+15552220000" {
		t.Errorf("got[0] = %+v, want CA1/in-progress/inbound/+15551110000/+15552220000", first)
	}

	second := got[1]
	if second.CallSID != "CA2" || second.CallStatus != "ringing" ||
		second.Direction != "outbound" || second.From != "+15553330000" {
		t.Errorf("got[1] = %+v, want CA2/ringing/outbound/+15553330000/<any>", second)
	}
	if second.To != "" {
		t.Errorf("got[1].To = %q, want zero value for a missing optional field", second.To)
	}
}

// TestListLiveCalls_EmptyArray pins behaviour 3: a bare 200 `[]` yields an
// empty, non-nil-error slice.
func TestListLiveCalls_EmptyArray(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}
	c, _ := newAccountClient(t, handler, "acct-empty-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := c.ListLiveCalls(ctx)
	if err != nil {
		if errors.Is(err, contract.ErrNoSchema) {
			t.Fatalf("pending T-06 schema: %v", err)
		}
		t.Fatalf("ListLiveCalls on empty array: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("len(got) = %d, want 0", len(got))
	}
}

// TestListLiveCalls_ObjectBodyRejected pins the single most valuable
// negative case: the api-server contract is a BARE array. If a caller
// (either upstream jambonz, or a regression in this client) ever returns/
// decodes an enveloped object shape instead, that must surface as an error
// — never silently as an empty slice. This is exactly the shape drift the
// JSON Schema (and Go's own array-typed decode) exist to catch.
func TestListLiveCalls_ObjectBodyRejected(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"total":0,"data":[]}`))
	}
	c, _ := newAccountClient(t, handler, "acct-object-body-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := c.ListLiveCalls(ctx)
	if err == nil {
		t.Fatalf("expected error for an enveloped object body, got nil error and %d calls", len(got))
	}
}

// TestListLiveCalls_MalformedJSON pins edge case: a 200 with truncated/
// malformed JSON must error, not panic.
func TestListLiveCalls_MalformedJSON(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[{"call_sid":"CA1","call_stat`))
	}
	c, _ := newAccountClient(t, handler, "acct-malformed-1")
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("ListLiveCalls panicked on malformed JSON: %v", r)
		}
	}()
	if _, err := c.ListLiveCalls(ctx); err == nil {
		t.Fatalf("expected error for malformed JSON body, got nil error")
	}
}

// TestListLiveCalls_NonOKStatus pins behaviour 5: non-200 responses surface
// as a *provision.APIError with the right status, accessible only via
// AsAPIError/StatusOf (never string-matched).
func TestListLiveCalls_NonOKStatus(t *testing.T) {
	cases := []struct {
		name   string
		status int
	}{
		{"not-found", http.StatusNotFound},
		{"server-error", http.StatusInternalServerError},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			handler := func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"msg":"boom"}`))
			}
			c, _ := newAccountClient(t, handler, "acct-nonok-1")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()

			got, err := c.ListLiveCalls(ctx)
			if err == nil {
				t.Fatalf("expected error for status %d, got nil error and %d calls", tc.status, len(got))
			}
			apiErr, ok := AsAPIError(err)
			if !ok {
				t.Fatalf("AsAPIError(err) ok = false, want true (err: %v)", err)
			}
			if apiErr.Status != tc.status {
				t.Errorf("apiErr.Status = %d, want %d", apiErr.Status, tc.status)
			}
			if StatusOf(err) != tc.status {
				t.Errorf("StatusOf(err) = %d, want %d", StatusOf(err), tc.status)
			}
		})
	}
}

// TestListLiveCalls_RequiresAccountScope pins behaviour 6: a *Client with no
// account SID must refuse to call ListLiveCalls with a non-nil error, and
// crucially must never hit the network at all.
func TestListLiveCalls_RequiresAccountScope(t *testing.T) {
	var requests atomic.Int32
	handler := func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}
	c, _ := newAccountClient(t, handler, "") // no account SID: SP-scoped client
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	got, err := c.ListLiveCalls(ctx)
	if err == nil {
		t.Fatalf("expected error for a non-account-scoped client, got nil error and %d calls", len(got))
	}
	if n := requests.Load(); n != 0 {
		t.Errorf("requests observed by server = %d, want 0 (no network call should be made)", n)
	}
}

// TestListLiveCalls_ContextCancelled pins behaviour 7: a cancelled context
// must fail fast with a non-nil error, never hang.
func TestListLiveCalls_ContextCancelled(t *testing.T) {
	handler := func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`[]`))
	}
	c, _ := newAccountClient(t, handler, "acct-ctx-1")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before the call

	done := make(chan error, 1)
	go func() {
		_, err := c.ListLiveCalls(ctx)
		done <- err
	}()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("expected error for a cancelled context, got nil error")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListLiveCalls hung on a cancelled context instead of returning promptly")
	}
}
