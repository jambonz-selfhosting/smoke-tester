package rest

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// Authorization-level guards on writes (api-server #134 and the carrier
// ownership fix). Columns that only the operator may set are rejected for
// SP- and account-scope callers, non-canonical keys (PLAN_TYPE,
// accounts.x, __proto__) are a 400 because MySQL matches column names
// case-insensitively, and carriers stay inside the caller's tenant.
//
// The operator columns exist only on commercial clusters, so every test
// here skips on an open-source cluster (see requireOperatorColumns).

const generalErrorSchema = "rest/common/general_error.json"

// spOperatorColumns are the service_providers columns an SP may never write.
var spOperatorColumns = []string{
	"account_limit", "managed_number_limit", "carrier_kyc_status",
	"is_enterprise", "stripe_customer_id", "user_limit",
}

// accountOperatorWrites are probe values for the operator-only accounts columns.
var accountOperatorWrites = map[string]any{
	"plan_type":            "paid",
	"trial_end_date":       "2099-01-01 00:00:00",
	"is_active":            0,
	"deactivated_reason":   "it-probe",
	"device_to_call_ratio": 500,
	"managed_number_limit": 1000,
	"disable_cdrs":         1,
	"carrier_kyc_status":   "completed",
	"beta_portal_invited":  1,
	"stripe_customer_id":   "cus_it_probe",
}

// getRaw GETs a resource as a generic map, contract-validated against schema.
func getRaw(ctx context.Context, c *provision.Client, path, schema string) (map[string]any, error) {
	raw, err := c.Request(ctx, http.MethodGet, path, nil, schema, http.StatusOK)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	return out, nil
}

// requireOperatorColumns skips the test on clusters without the commercial
// operator columns (open-source jambonz has none of these guards).
func requireOperatorColumns(t *testing.T, ctx context.Context) {
	t.Helper()
	if spClient == nil {
		t.Skip("SP scope not configured (JAMBONZ_SP_API_KEY / JAMBONZ_SP_SID)")
	}
	acct, err := getRaw(ctx, spClient, "/Accounts/"+suite.AccountSID, "rest/accounts/getAccount.response.200.json")
	if err != nil {
		recordFailure(t, "detect-operator-columns", err.Error())
		t.Fatalf("detect operator columns: %v", err)
	}
	if _, ok := acct["carrier_kyc_status"]; !ok {
		t.Skip("no operator columns on this cluster (open-source build); authz guards are commercial-only")
	}
}

// expectRejected asserts the write failed with want, and that the error body
// honours the GeneralError contract.
func expectRejected(s *StepCtx, err error, want int, label string) {
	if err == nil {
		s.Errorf("%s: succeeded, want %d", label, want)
		return
	}
	apiErr, ok := provision.AsAPIError(err)
	if !ok {
		s.Errorf("%s: %v, want HTTP %d", label, err, want)
		return
	}
	if apiErr.Status != want {
		s.Errorf("%s: status %d (%s), want %d", label, apiErr.Status, apiErr.Msg, want)
		return
	}
	if verr := valid.ValidateResponse(generalErrorSchema, apiErr.Body); verr != nil {
		s.Errorf("%s: error body violates GeneralError: %v", label, verr)
	}
}

func put(ctx context.Context, c *provision.Client, path string, body any) error {
	_, err := c.Request(ctx, http.MethodPut, path, body, "", http.StatusNoContent)
	return err
}

// accountClient provisions an ephemeral account under the SP plus an
// account-scope API key for it.
func accountClient(t *testing.T, ctx context.Context, suffix string) (string, *provision.Client) {
	t.Helper()
	sid := spClient.ManagedAccount(t, ctx, provision.AccountCreate{
		Name:               provision.Name(suffix),
		ServiceProviderSID: cfg.SPSID,
	})
	_, token := spClient.ManagedApiKey(t, ctx, provision.ApiKeyCreate{AccountSID: sid})
	return sid, provision.New(cfg.APIBaseURL, token, sid, valid, provision.WithLabel("acct-"+suffix))
}

// cleanupIfCreated deletes a resource a rejected POST created anyway, so an
// unguarded cluster fails the test without leaking it.
func cleanupIfCreated(t *testing.T, raw []byte, err error, del func(context.Context, string) error) {
	if err != nil {
		return
	}
	var ok struct {
		SID string `json:"sid"`
	}
	if json.Unmarshal(raw, &ok) != nil || ok.SID == "" {
		return
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = del(cctx, ok.SID)
	})
}

// TestAuthz_ServiceProvider_OperatorOnlyFields pins that an SP-scope key
// cannot write the operator columns on its own service provider. Every probe
// re-sends the column's current value, so an unguarded cluster stays intact.
//
// Steps:
//  1. snapshot-sp — GET /ServiceProviders/{sid}; operator columns present
//  2. reject-operator-columns — PUT each column (current value) returns 403
//  3. reject-unknown-key — PUT a key that is not an SP column returns 403
//  4. sp-unchanged — operator columns read back as before
//  5. allowed-field-still-saves — PUT name (current value) returns 204
func TestAuthz_ServiceProvider_OperatorOnlyFields(t *testing.T) {
	ctx := WithTimeout(t, 60*time.Second)
	requireOperatorColumns(t, ctx)
	spPath := "/ServiceProviders/" + cfg.SPSID
	spSchema := "rest/service_providers/getServiceProvider.response.200.json"

	s := Step(t, "snapshot-sp")
	before, err := getRaw(ctx, spClient, spPath, spSchema)
	if err != nil {
		s.Fatalf("get service provider: %v", err)
	}
	var present []string
	for _, col := range spOperatorColumns {
		if _, ok := before[col]; ok {
			present = append(present, col)
		}
	}
	if len(present) == 0 {
		s.Fatalf("GET %s returned none of %v", spPath, spOperatorColumns)
	}
	s.Done()

	s = Step(t, "reject-operator-columns")
	for _, col := range present {
		expectRejected(s, put(ctx, spClient, spPath, map[string]any{col: before[col]}),
			http.StatusForbidden, "SP PUT "+col)
	}
	s.Done()

	s = Step(t, "reject-unknown-key")
	expectRejected(s, put(ctx, spClient, spPath, map[string]any{"it_not_a_column": 1}),
		http.StatusForbidden, "SP PUT unknown key")
	s.Done()

	s = Step(t, "sp-unchanged")
	after, err := getRaw(ctx, spClient, spPath, spSchema)
	if err != nil {
		s.Fatalf("get service provider: %v", err)
	}
	for _, col := range present {
		if !reflect.DeepEqual(before[col], after[col]) {
			s.Errorf("%s changed: %v -> %v", col, before[col], after[col])
		}
	}
	s.Done()

	s = Step(t, "allowed-field-still-saves")
	if err := put(ctx, spClient, spPath, map[string]any{"name": before["name"]}); err != nil {
		s.Errorf("SP PUT name: %v", err)
	}
	s.Done()
}

// TestAuthz_VoipCarrier_Tenancy pins carrier ownership across the flat and
// nested carrier routes: an account cannot write another account's carrier
// or an SP-level one, and nobody but the operator writes the managed-carrier
// columns. Cross-SP writes need a second SP and are covered by api-server's
// own tests.
//
// Steps:
//  1. setup-carriers — second account + key, a carrier on each tier
//  2. other-account-cannot-write — account B PUT/DELETE on account A's carrier is refused
//  3. account-cannot-write-sp-carrier — account PUT on an SP-level carrier is refused
//  4. account-cannot-bind-other-account — account POST with another account_sid returns 403
//  5. managed-columns-rejected — is_managed / supplier_sid return 403, IS_MANAGED returns 400
//  6. allowed-writes — SP and account PUT on their own carriers return 204
func TestAuthz_VoipCarrier_Tenancy(t *testing.T) {
	t.Parallel()
	ctx := WithTimeout(t, 90*time.Second)
	requireOperatorColumns(t, ctx)

	s := Step(t, "setup-carriers")
	sidB, clientB := accountClient(t, ctx, "authz-carrier-b")
	cA := client.ManagedVoipCarrier(t, ctx, provision.VoipCarrierCreate{
		Name:        provision.Name("authz-carrier-a"),
		Description: "owned by A",
		AccountSID:  suite.AccountSID,
	})
	cSP, err := spClient.CreateVoipCarrierUnderSP(ctx, cfg.SPSID, provision.VoipCarrierCreate{
		Name:        provision.Name("authz-carrier-sp"),
		Description: "owned by SP",
	})
	if err != nil {
		s.Fatalf("create SP carrier: %v", err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = spClient.DeleteVoipCarrier(cctx, cSP)
	})
	s.Done()

	s = Step(t, "other-account-cannot-write")
	desc := map[string]any{"description": "hijacked"}
	expectRejected(s, put(ctx, clientB, "/VoipCarriers/"+cA, desc),
		http.StatusUnprocessableEntity, "account B PUT /VoipCarriers/{A's}")
	expectRejected(s, put(ctx, clientB, "/Accounts/"+sidB+"/VoipCarriers/"+cA, desc),
		http.StatusUnprocessableEntity, "account B PUT /Accounts/{B}/VoipCarriers/{A's}")
	_, err = clientB.Request(ctx, http.MethodDelete, "/VoipCarriers/"+cA, nil, "", http.StatusNoContent)
	expectRejected(s, err, http.StatusUnprocessableEntity, "account B DELETE /VoipCarriers/{A's}")
	got, err := client.GetVoipCarrier(ctx, cA)
	if err != nil {
		s.Fatalf("account A carrier gone after refused writes: %v", err)
	}
	if got.Description != "owned by A" {
		s.Errorf("account A carrier description = %q, want unchanged", got.Description)
	}
	s.Done()

	s = Step(t, "account-cannot-write-sp-carrier")
	expectRejected(s, put(ctx, client, "/Accounts/"+suite.AccountSID+"/VoipCarriers/"+cSP, desc),
		http.StatusUnprocessableEntity, "account PUT /Accounts/{own}/VoipCarriers/{SP carrier}")
	s.Done()

	s = Step(t, "account-cannot-bind-other-account")
	raw, err := client.Request(ctx, http.MethodPost, "/Accounts/"+suite.AccountSID+"/VoipCarriers",
		map[string]any{"name": provision.Name("authz-carrier-x"), "account_sid": sidB}, "", http.StatusCreated)
	cleanupIfCreated(t, raw, err, spClient.DeleteVoipCarrier)
	expectRejected(s, err, http.StatusForbidden, "account POST carrier with account B's account_sid")
	s.Done()

	s = Step(t, "managed-columns-rejected")
	carrier, err := getRaw(ctx, spClient, "/VoipCarriers/"+cSP, "rest/voip_carriers/getVoipCarrier.response.200.json")
	if err != nil {
		s.Fatalf("get SP carrier: %v", err)
	}
	if _, ok := carrier["is_managed"]; !ok {
		s.Fatalf("carrier has no is_managed column on a commercial cluster")
	}
	// current values only, so an unguarded cluster is left untouched
	for _, body := range []map[string]any{
		{"is_managed": carrier["is_managed"]},
		{"supplier_sid": carrier["supplier_sid"]},
	} {
		for _, path := range []string{"/VoipCarriers/" + cSP, "/ServiceProviders/" + cfg.SPSID + "/VoipCarriers/" + cSP} {
			expectRejected(s, put(ctx, spClient, path, body), http.StatusForbidden,
				fmt.Sprintf("SP PUT %s %v", path, body))
		}
	}
	expectRejected(s, put(ctx, client, "/Accounts/"+suite.AccountSID+"/VoipCarriers/"+cA,
		map[string]any{"is_managed": 0}), http.StatusForbidden, "account PUT is_managed on its own carrier")
	for _, body := range []map[string]any{{"IS_MANAGED": 0}, {"voip_carriers.supplier_sid": nil}} {
		expectRejected(s, put(ctx, spClient, "/ServiceProviders/"+cfg.SPSID+"/VoipCarriers/"+cSP, body),
			http.StatusBadRequest, fmt.Sprintf("SP PUT %v", body))
	}
	s.Done()

	s = Step(t, "allowed-writes")
	if err := put(ctx, spClient, "/ServiceProviders/"+cfg.SPSID+"/VoipCarriers/"+cSP,
		map[string]any{"description": "SP edit", "service_provider_sid": cfg.SPSID}); err != nil {
		s.Errorf("SP PUT own carrier: %v", err)
	}
	if err := put(ctx, client, "/Accounts/"+suite.AccountSID+"/VoipCarriers/"+cA,
		map[string]any{"description": "A edit"}); err != nil {
		s.Errorf("account PUT own carrier: %v", err)
	}
	s.Done()
}
