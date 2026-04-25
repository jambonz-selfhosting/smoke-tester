package rest

import (
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
	client   *provision.Client // account-scope
	spClient *provision.Client // service-provider-scope; nil if SP creds unset
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

	client = provision.New(cfg.APIBaseURL, cfg.APIKey, cfg.AccountSID, valid,
		provision.WithLabel("account"))
	log.Printf("tests/rest: runID=%s schemas=%s", provision.RunID(), schemasRoot)
	log.Printf("  account-scope: account_sid=%s", cfg.AccountSID)

	if cfg.HasSPScope() {
		spClient = provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", valid,
			provision.WithLabel("sp"))
		log.Printf("  sp-scope: service_provider_sid=%s", cfg.SPSID)
	} else {
		log.Printf("  sp-scope: DISABLED (set JAMBONZ_SP_API_KEY + JAMBONZ_SP_SID to enable)")
	}

	orphanSweep()

	// Heartbeat: prints a status line every 5s to /dev/tty so operators
	// see progress even without -v. See helpers_test.go for the full
	// rationale.
	stopHeartbeat := StartHeartbeat(5 * time.Second)

	code := m.Run()

	stopHeartbeat()

	// One-line-per-failure summary. Without this, under -parallel and
	// without -v, failure details get buried in interleaved log noise.
	PrintFailureSummary()
	os.Exit(code)
}

// orphanSweep runs every registered Sweeper and logs results. Failures are
// best-effort — a crashed prior run shouldn't block new runs.
func orphanSweep() {
	sweepers := []provision.Sweeper{
		&provision.ApplicationSweeper{C: client},
		&provision.VoipCarrierSweeper{C: client},
		// SIP Clients are created per-test by the verbs suite (see
		// provision.ManagedSIPClient). If a verbs run crashed mid-test,
		// the it-<oldRunID>-* row leaks. Sweep here so any account-scope
		// run cleans up before the next verbs run tries to recreate the
		// same usernames.
		&provision.SIPClientSweeper{C: client},
	}
	if spClient != nil {
		// Accounts can only be swept with SP scope.
		sweepers = append(sweepers, &provision.AccountSweeper{C: spClient})
	}
	for _, s := range sweepers {
		n, err := s.Sweep(provision.RunID())
		if err != nil {
			log.Printf("orphan sweep %s: %v", s.Name(), err)
			continue
		}
		if n > 0 {
			log.Printf("orphan sweep: deleted %d stale %s", n, s.Name())
		}
	}
}
