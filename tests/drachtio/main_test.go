//go:build drachtio

package drachtio

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

var (
	cfg *config.Settings

	// suite holds the ephemeral account + account-scope client + synthetic
	// sip_realm provisioned at TestMain. helpers_test.go reaches through
	// `client` (== suite.AccountClient) to provision sub-resources.
	suite  *provision.SuiteAccount
	client *provision.Client

	// SIP transport's static DNS resolver. Maps the suite's synthetic
	// sip_realm to the SBC public IP so sipgo's transport can reach the
	// cluster without real DNS for the realm. Closed at TestMain teardown.
	sipResolver *jsip.StaticResolver

	// appSID is the application_sid of the single inline-app_json
	// Application provisioned at TestMain. inviteApp dials
	// sip:app-<appSID>@<suite.SIPRealm> to reach it.
	appSID string
)

func TestMain(m *testing.M) {
	// config.MustLoad still requires NGROK_AUTHTOKEN (and DEEPGRAM_API_KEY /
	// DEEPSEEK_API_KEY) even though this suite opens no tunnel and uses no
	// speech vendor — those envs are shared with tests/verbs via the same
	// .env. A "missing NGROK_AUTHTOKEN" fatal here does NOT mean this suite
	// uses ngrok; it means the shared .env is incomplete.
	cfg = config.MustLoad()

	schemasRoot, err := contract.ResolveSchemasRoot()
	if err != nil {
		log.Fatalf("tests/drachtio: contract schemas root: %v", err)
	}
	v, err := contract.New(schemasRoot)
	if err != nil {
		log.Fatalf("tests/drachtio: contract validator: %v", err)
	}

	sp := provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", v,
		provision.WithLabel("sp"))

	// Sweep stale ephemeral accounts from previous (crashed) runs.
	swept, err := (&provision.AccountSweeper{C: sp}).Sweep(provision.RunID())
	if err != nil {
		log.Printf("tests/drachtio: account sweep failed: %v", err)
	} else if swept > 0 {
		log.Printf("tests/drachtio: swept %d stale ephemeral accounts", swept)
	}

	// 1. Provision the per-suite account.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 90*time.Second)
	suite, err = provision.SetupSuiteAccount(setupCtx, sp, cfg.SPSID, cfg.APIBaseURL,
		"drachtio", cfg.SIPRealmZone)
	setupCancel()
	if err != nil {
		log.Fatalf("tests/drachtio: suite account provisioning failed: %v", err)
	}
	client = suite.AccountClient
	log.Printf("tests/drachtio: suite account=%s sid=%s realm=%s",
		suite.AccountName, suite.AccountSID, suite.SIPRealm)

	// 2. Static DNS resolver pointing the synthetic realm at the SBC IP.
	sipResolver, err = jsip.NewStaticResolver(suite.SBCResolverHosts(cfg.SBCPublicIP))
	if err != nil {
		log.Fatalf("tests/drachtio: static DNS resolver setup failed: %v", err)
	}

	// 3. Provision a single Application with an inline app_json script.
	// call_hook / call_status_hook are set to a syntactically-valid but
	// non-resolving placeholder URL: api-server only validates the hook URL
	// syntactically (never checks reachability) at Application creation, and
	// feature-server short-circuits on app_json and never fetches the hook
	// URL at call time, so no webhook server is needed here. answer, then a
	// long pause so session-timer refreshes/expiries have room to occur
	// mid-call. The trailing hangup makes the app's self-termination
	// explicit rather than relying on feature-server end-of-application
	// behaviour (_clearResources already sends a BYE once the app's verb
	// list runs out, so the call would end at the pause's expiry either
	// way) — it does not change when the call ends. The pause length (150s)
	// must stay comfortably above the session-timer tests' longest required
	// in-call window: TestDrachtio_SessionTimer_UACRefresherKeepalive
	// (tests/drachtio/session_timer_test.go) sends two refreshes at
	// delta/2 apart (2*(delta/2) == sessionInterval, since the SBC's
	// Min-SE=90 floor equals the offered sessionInterval) and then asserts
	// the call is STILL UP 30s after that — a required in-call floor of
	// sessionInterval + 30s == 120s. 150s clears that by a 30s margin, so
	// do not shorten it.
	appCtx, appCancel := context.WithTimeout(context.Background(), 30*time.Second)
	appSID, err = client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("drachtio-app"),
		AccountSID:     suite.AccountSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
		AppJSON:        `[{"verb":"answer"},{"verb":"pause","length":150},{"verb":"hangup"}]`,
	})
	appCancel()
	if err != nil {
		log.Fatalf("tests/drachtio: inline app_json Application creation failed: %v", err)
	}
	log.Printf("tests/drachtio: application ready sid=%s", appSID)

	code := m.Run()

	// Teardown — best-effort. Order matters: delete the Application before
	// the account so the account's cascade doesn't have to fight FK
	// constraints.
	if appSID != "" {
		delCtx, delCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := client.DeleteApplication(delCtx, appSID); err != nil {
			log.Printf("tests/drachtio: cleanup: delete application %s: %v", appSID, err)
		}
		delCancel()
	}
	if sipResolver != nil {
		_ = sipResolver.Close()
	}
	if suite != nil {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		errs := suite.Teardown(teardownCtx)
		cancel()
		for _, e := range errs {
			log.Printf("tests/drachtio: teardown: %v", e)
		}
	}
	os.Exit(code)
}
