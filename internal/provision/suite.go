package provision

import (
	"context"
	"fmt"
	"net"
	"strings"
	"time"
)

// SuiteAccount is a fully-provisioned ephemeral account ready for tests.
// TestMain calls SetupSuiteAccount once per process and tears it down at
// the end via SuiteAccount.Teardown.
type SuiteAccount struct {
	// AccountSID is the new account's SID.
	AccountSID string

	// AccountName is the human-readable name (e.g. `it-<runID>-verbs`).
	AccountName string

	// APIKeySID + Token are the account-scope credentials minted off this
	// account. Pass `Token` to provision.New as the bearer for any
	// account-scope client.
	APIKeySID string
	Token     string

	// SIPRealm is the synthetic realm assigned to this account
	// (`<AccountName>.<SIPRealmZone>`). Use as the SIP domain when dialing
	// users in this account.
	SIPRealm string

	// AccountClient is a *Client wired with the account-scope token and
	// AccountSID. Use this to provision sub-resources (Applications,
	// Clients, SpeechCredentials, …) under this suite account.
	AccountClient *Client

	// SPClient is the parent SP-scope client passed in. Held here so
	// Teardown can DELETE the account at the end.
	SPClient *Client
}

// SetupSuiteAccount provisions an ephemeral account under the configured
// service provider and returns a SuiteAccount handle. Call Teardown when
// done — it deletes the account's clients (cascade workaround) then the
// account itself.
//
// nameSuffix is appended to the runID-prefixed name (e.g. nameSuffix="verbs"
// → "it-<runID>-verbs"). Two parallel suites in the same process should
// pass different suffixes.
//
// realmZone is the trailing portion of the synthetic sip_realm (e.g.
// "smoke.test"). Combined with the account name we get a unique multi-label
// domain that satisfies the upstream `(.*)\.(.*\..*)$` regex.
//
// baseURL is the API base url (e.g. `https://jambonz.me/api/v1`); the
// returned AccountClient is wired with this URL + the per-suite token.
func SetupSuiteAccount(ctx context.Context, sp *Client, spSID, baseURL, nameSuffix, realmZone string) (*SuiteAccount, error) {
	name := Name(nameSuffix)

	// 1. Account
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	accountSID, err := sp.CreateAccount(cctx, AccountCreate{
		Name:               name,
		ServiceProviderSID: spSID,
	})
	cancel()
	if err != nil {
		return nil, fmt.Errorf("setup-suite: create account: %w", err)
	}

	// 2. ApiKey for the account
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	keySID, token, err := sp.CreateApiKey(cctx, ApiKeyCreate{AccountSID: accountSID})
	cancel()
	if err != nil {
		_ = bestEffortDeleteAccount(sp, accountSID)
		return nil, fmt.Errorf("setup-suite: create api key: %w", err)
	}

	// Build account-scope client — same validator as the parent SP client.
	acct := New(baseURL, token, accountSID, sp.validator,
		WithLabel("acct-"+nameSuffix))

	// 3. SipRealm
	realm := strings.ToLower(name) + "." + realmZone
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	if err := acct.SetSipRealm(cctx, accountSID, realm); err != nil {
		cancel()
		_ = bestEffortDeleteAccount(sp, accountSID)
		return nil, fmt.Errorf("setup-suite: set sip_realm: %w", err)
	}
	cancel()

	// Verify the realm landed in the DB regardless of HTTP-level DNS errors.
	cctx, cancel = context.WithTimeout(ctx, 15*time.Second)
	a, err := sp.GetAccount(cctx, accountSID)
	cancel()
	if err != nil {
		_ = bestEffortDeleteAccount(sp, accountSID)
		return nil, fmt.Errorf("setup-suite: verify sip_realm: %w", err)
	}
	if !strings.EqualFold(a.SIPRealm, realm) {
		_ = bestEffortDeleteAccount(sp, accountSID)
		return nil, fmt.Errorf("setup-suite: sip_realm not persisted (got %q want %q)", a.SIPRealm, realm)
	}

	return &SuiteAccount{
		AccountSID:    accountSID,
		AccountName:   name,
		APIKeySID:     keySID,
		Token:         token,
		SIPRealm:      realm,
		AccountClient: acct,
		SPClient:      sp,
	}, nil
}

// Teardown deletes the account's clients then the account itself. Always
// returns nil — failures are logged in HANDOFF style as best-effort.
func (s *SuiteAccount) Teardown(ctx context.Context) []error {
	cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	var errs []error

	// Enumerate clients via SP scope (it sees all clients under the SP);
	// filter client-side to match THIS account.
	clients, err := s.SPClient.ListSIPClientsForAccount(cctx, s.AccountSID)
	if err != nil {
		errs = append(errs, fmt.Errorf("list clients: %w", err))
	}
	for _, cl := range clients {
		if cl.AccountSID != s.AccountSID {
			continue // belt + braces against drift
		}
		if err := s.SPClient.DeleteSIPClient(cctx, cl.ClientSID); err != nil {
			errs = append(errs, fmt.Errorf("delete client %s: %w", cl.ClientSID, err))
		}
	}

	// Delete the API key we minted (cascading delete on the account would
	// remove it but this keeps the api_keys table tidy on the way out).
	if s.APIKeySID != "" {
		_ = s.SPClient.DeleteApiKey(cctx, s.APIKeySID)
	}

	if err := s.SPClient.DeleteAccount(cctx, s.AccountSID); err != nil {
		errs = append(errs, fmt.Errorf("delete account: %w", err))
	}
	return errs
}

func bestEffortDeleteAccount(sp *Client, sid string) error {
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	clients, _ := sp.ListSIPClientsForAccount(cctx, sid)
	for _, cl := range clients {
		_ = sp.DeleteSIPClient(cctx, cl.ClientSID)
	}
	return sp.DeleteAccount(cctx, sid)
}

// SBCResolverHosts returns the host→IP map to feed into
// internal/sip.NewStaticResolver so synthetic realms route to the SBC.
func (s *SuiteAccount) SBCResolverHosts(sbcIP net.IP) map[string]string {
	return map[string]string{
		s.SIPRealm: sbcIP.String(),
	}
}
