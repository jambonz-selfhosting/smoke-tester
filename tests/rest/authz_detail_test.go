//go:build detail

package rest

import (
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestAuthz_Account_OperatorOnlyFields pins the account-level guards for both
// SP scope (on an account in its SP) and account scope (on itself), on PUT
// and on POST /Accounts.
//
// Steps:
//  1. setup-account — ephemeral account + account-scope key; snapshot it
//  2. sp-scope-rejects-operator-fields — SP PUT each operator field returns 403
//  3. account-scope-rejects-operator-fields — account PUT each one returns 403
//  4. non-canonical-keys-rejected — PLAN_TYPE, accounts.x, SERVICE_PROVIDER_SID, __proto__ return 400
//  5. account-unchanged — operator fields and service_provider_sid read back as before
//  6. allowed-fields-still-save — SP and account PUT name return 204
//  7. sp-post-rejects-operator-fields — POST /Accounts with an operator field returns 403
func TestAuthz_Account_OperatorOnlyFields(t *testing.T) {
	t.Parallel()
	ctx := WithTimeout(t, 90*time.Second)
	requireOperatorColumns(t, ctx)
	acctSchema := "rest/accounts/getAccount.response.200.json"

	s := Step(t, "setup-account")
	sid, acct := accountClient(t, ctx, "authz-acct")
	acctPath := "/Accounts/" + sid
	before, err := getRaw(ctx, spClient, acctPath, acctSchema)
	if err != nil {
		s.Fatalf("get account: %v", err)
	}
	s.Done()

	scopes := []struct {
		step string
		c    *provision.Client
	}{
		{"sp-scope-rejects-operator-fields", spClient},
		{"account-scope-rejects-operator-fields", acct},
	}
	for _, sc := range scopes {
		s = Step(t, sc.step)
		for field, v := range accountOperatorWrites {
			expectRejected(s, put(ctx, sc.c, acctPath, map[string]any{field: v}),
				http.StatusForbidden, sc.c.Label()+" PUT "+field)
		}
		s.Done()
	}

	s = Step(t, "non-canonical-keys-rejected")
	for _, sc := range scopes {
		for _, body := range []map[string]any{
			{"PLAN_TYPE": "paid"},
			{"Trial_End_Date": "2099-01-01 00:00:00"},
			{"accounts.disable_cdrs": 1},
			{"SERVICE_PROVIDER_SID": cfg.SPSID},
			{"name": provision.Name("authz-proto"), "__proto__": map[string]any{"plan_type": "paid", "sip_realm": "it-probe.invalid"}},
		} {
			keys := make([]string, 0, len(body))
			for k := range body {
				keys = append(keys, k)
			}
			expectRejected(s, put(ctx, sc.c, acctPath, body),
				http.StatusBadRequest, fmt.Sprintf("%s PUT %v", sc.c.Label(), keys))
		}
	}
	s.Done()

	s = Step(t, "account-unchanged")
	after, err := getRaw(ctx, spClient, acctPath, acctSchema)
	if err != nil {
		s.Fatalf("get account: %v", err)
	}
	for field := range accountOperatorWrites {
		if !reflect.DeepEqual(before[field], after[field]) {
			s.Errorf("%s changed: %v -> %v", field, before[field], after[field])
		}
	}
	for _, field := range []string{"service_provider_sid", "sip_realm", "name"} {
		if !reflect.DeepEqual(before[field], after[field]) {
			s.Errorf("%s changed: %v -> %v", field, before[field], after[field])
		}
	}
	s.Done()

	s = Step(t, "allowed-fields-still-save")
	if err := put(ctx, spClient, acctPath, map[string]any{"name": provision.Name("authz-acct-sp")}); err != nil {
		s.Errorf("SP PUT name: %v", err)
	}
	if err := put(ctx, acct, acctPath, map[string]any{"name": provision.Name("authz-acct-self")}); err != nil {
		s.Errorf("account PUT name: %v", err)
	}
	s.Done()

	s = Step(t, "sp-post-rejects-operator-fields")
	for _, field := range []string{"plan_type", "disable_cdrs", "device_to_call_ratio", "stripe_customer_id"} {
		body := map[string]any{
			"name":                 provision.Name("authz-post-" + strings.ReplaceAll(field, "_", "-")),
			"service_provider_sid": cfg.SPSID,
			"webhook_secret":       "it-probe",
			field:                  accountOperatorWrites[field],
		}
		raw, err := spClient.Request(ctx, http.MethodPost, "/Accounts", body, "", http.StatusCreated)
		cleanupIfCreated(t, raw, err, spClient.DeleteAccount)
		expectRejected(s, err, http.StatusForbidden, "SP POST /Accounts with "+field)
	}
	raw, err := spClient.Request(ctx, http.MethodPost, "/Accounts", map[string]any{
		"name":                 provision.Name("authz-post-upper"),
		"service_provider_sid": cfg.SPSID,
		"DISABLE_CDRS":         1,
	}, "", http.StatusCreated)
	cleanupIfCreated(t, raw, err, spClient.DeleteAccount)
	expectRejected(s, err, http.StatusBadRequest, "SP POST /Accounts with DISABLE_CDRS")
	s.Done()
}
