//go:build failover

package failover

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
	cfg         *config.Settings
	suite       *provision.SuiteAccount
	client      *provision.Client
	sipResolver *jsip.StaticResolver
	appSID      string
	declineSID  string
	busySID     string
	box         *boxSSH
)

func TestMain(m *testing.M) {
	cfg = config.MustLoad()

	var err error
	box, err = newBoxSSH()
	if err != nil {
		log.Fatalf("tests/failover: %v", err)
	}

	schemasRoot, err := contract.ResolveSchemasRoot()
	if err != nil {
		log.Fatalf("tests/failover: contract schemas root: %v", err)
	}
	v, err := contract.New(schemasRoot)
	if err != nil {
		log.Fatalf("tests/failover: contract validator: %v", err)
	}
	sp := provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", v, provision.WithLabel("sp"))

	if swept, err := (&provision.AccountSweeper{C: sp, MinAge: cfg.OrphanTTL}).Sweep(provision.RunID()); err != nil {
		log.Printf("tests/failover: account sweep failed: %v", err)
	} else if swept > 0 {
		log.Printf("tests/failover: swept %d stale ephemeral accounts", swept)
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 90*time.Second)
	suite, err = provision.SetupSuiteAccount(setupCtx, sp, cfg.SPSID, cfg.APIBaseURL, "failover", cfg.SIPRealmZone)
	setupCancel()
	if err != nil {
		log.Fatalf("tests/failover: suite account provisioning failed: %v", err)
	}
	client = suite.AccountClient

	sipResolver, err = jsip.NewStaticResolver(suite.SBCResolverHosts(cfg.SBCPublicIP))
	if err != nil {
		log.Fatalf("tests/failover: static DNS resolver setup failed: %v", err)
	}

	// Inline app_json, so the hook URLs are never fetched; the long pause keeps
	// the call up while a test kills its feature server.
	appCtx, appCancel := context.WithTimeout(context.Background(), 30*time.Second)
	appSID, err = client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("failover-app"),
		AccountSID:     suite.AccountSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
		AppJSON:        `[{"verb":"answer"},{"verb":"pause","length":150},{"verb":"hangup"}]`,
	})
	appCancel()
	if err != nil {
		log.Fatalf("tests/failover: application creation failed: %v", err)
	}

	appCtx, appCancel = context.WithTimeout(context.Background(), 30*time.Second)
	declineSID, err = client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("failover-decline"),
		AccountSID:     suite.AccountSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
		AppJSON:        `[{"verb":"sip:decline","status":486,"reason":"Busy Here","headers":{"X-Reason":"declined by app"}}]`,
	})
	appCancel()
	if err != nil {
		log.Fatalf("tests/failover: decline application creation failed: %v", err)
	}

	appCtx, appCancel = context.WithTimeout(context.Background(), 30*time.Second)
	busySID, err = client.CreateApplication(appCtx, provision.ApplicationCreate{
		Name:           provision.Name("failover-busy"),
		AccountSID:     suite.AccountSID,
		CallHook:       provision.Webhook{URL: "https://example.invalid/hook", Method: "POST"},
		CallStatusHook: provision.Webhook{URL: "https://example.invalid/status", Method: "POST"},
		AppJSON:        `[{"verb":"sip:decline","status":503,"reason":"Service Unavailable"}]`,
	})
	appCancel()
	if err != nil {
		log.Fatalf("tests/failover: 503 application creation failed: %v", err)
	}

	code := m.Run()

	for _, sid := range []string{appSID, declineSID, busySID} {
		if sid == "" {
			continue
		}
		delCtx, delCancel := context.WithTimeout(context.Background(), 15*time.Second)
		if err := client.DeleteApplication(delCtx, sid); err != nil {
			log.Printf("tests/failover: cleanup: delete application %s: %v", sid, err)
		}
		delCancel()
	}
	if sipResolver != nil {
		_ = sipResolver.Close()
	}
	if suite != nil {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		for _, e := range suite.Teardown(teardownCtx) {
			log.Printf("tests/failover: teardown: %v", e)
		}
		cancel()
	}
	os.Exit(code)
}
