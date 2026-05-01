package rest

import (
	"context"
	"log"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

var (
	cfg      *config.Settings
	valid    *contract.Validator
	suite    *provision.SuiteAccount
	client   *provision.Client // == suite.AccountClient
	spClient *provision.Client // service-provider scope, used by SP-only tests
)

func TestMain(m *testing.M) {
	cfg = config.MustLoad()

	schemasRoot, err := contract.ResolveSchemasRoot()
	if err != nil {
		log.Fatalf("contract: %v", err)
	}
	v, err := contract.New(schemasRoot)
	if err != nil {
		log.Fatalf("contract new: %v", err)
	}
	valid = v

	spClient = provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", valid,
		provision.WithLabel("sp"))

	// Sweep stale ephemeral accounts from previous runs (only `it-` prefix
	// AND not the current run's prefix). Hardened: re-checks each account
	// name client-side and deletes its clients first to dodge the upstream
	// FK constraint.
	swept, err := (&provision.AccountSweeper{C: spClient}).Sweep(provision.RunID())
	if err != nil {
		log.Printf("tests/rest: account sweep failed: %v", err)
	} else if swept > 0 {
		log.Printf("tests/rest: swept %d stale ephemeral accounts", swept)
	}

	// Per-suite ephemeral account so account-scope rest tests have a
	// place to provision sub-resources without touching any pre-existing
	// account on the cluster. The account is deleted at suite end.
	setupCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	suite, err = provision.SetupSuiteAccount(setupCtx, spClient, cfg.SPSID, cfg.APIBaseURL,
		"rest", cfg.SIPRealmZone)
	cancel()
	if err != nil {
		log.Fatalf("tests/rest: suite setup failed: %v", err)
	}
	client = suite.AccountClient
	log.Printf("tests/rest: runID=%s schemas=%s", provision.RunID(), schemasRoot)
	log.Printf("  suite account=%s sid=%s realm=%s",
		suite.AccountName, suite.AccountSID, suite.SIPRealm)
	log.Printf("  sp-scope: service_provider_sid=%s", cfg.SPSID)

	// Heartbeat: prints a status line every 5s to /dev/tty so operators
	// see progress even without -v. See helpers_test.go for the full
	// rationale.
	stopHeartbeat := StartHeartbeat(5 * time.Second)

	code := m.Run()

	stopHeartbeat()

	// One-line-per-failure summary.
	PrintFailureSummary()

	if suite != nil {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		errs := suite.Teardown(teardownCtx)
		cancel()
		for _, e := range errs {
			log.Printf("tests/rest: teardown: %v", e)
		}
	}
	os.Exit(code)
}
