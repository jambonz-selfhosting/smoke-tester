// Package verbs exercises individual jambonz verbs end-to-end.
//
// Phase 1 tests drive outbound calls via POST /Calls with inline `app_json`
// and observe the resulting inbound call on the harness UAS — no webhook
// server involved.
//
// Phase 2 tests use a webhook Application whose call_hook / call_status_hook
// point at an ngrok tunnel to our internal/webhook server. jambonz fetches
// verbs from the tunnel, runs them, and (for action-hook verbs like
// `gather`) calls back to our server with payloads the test reads via the
// webhook.Registry.
//
// Per-suite ephemeral account:
// TestMain provisions a fresh account under the SP, mints an account-scope
// API key for it, sets a synthetic sip_realm (`<account-name>.smoke.test`),
// and provisions the Deepgram credential + webhook Application under that
// account. Every verb test creates Clients / places calls under this
// account; the whole tree is deleted at suite end.
//
// Per-test SIP isolation:
// Each test calls claimUAS(t, ctx) (helpers_test.go) to provision its own
// /Clients user and bring up a private sipgo+diago stack registered with
// those credentials. Inbound INVITEs route to a per-test channel — no
// shared singletons — so tests can run in parallel safely.
package verbs

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

var (
	cfg *config.Settings

	// suite holds the ephemeral account + account-scope client + synthetic
	// sip_realm provisioned at TestMain. Every verb test reaches through
	// `client` (which is suite.AccountClient) to provision sub-resources.
	suite  *provision.SuiteAccount
	client *provision.Client // == suite.AccountClient

	// SIP transport's static DNS resolver. Maps the suite's synthetic
	// sip_realm to the SBC public IP so sipgo's transport can reach the
	// cluster without real DNS for the realm. Closed at TestMain teardown.
	sipResolver *jsip.StaticResolver

	// Deepgram speech credential provisioned at TestMain under the suite
	// account. Verb tests reference `synthesizer.label` /
	// `recognizer.label` to use this credential.
	deepgramLabel string
	deepgramSID   string
	// Default TTS voice when speaking through Deepgram.
	deepgramVoice = "aura-asteria-en"

	// Webhook server + ngrok tunnel + Application bound to the suite
	// account. The webhook always runs (NGROK_AUTHTOKEN is mandatory in
	// the new model).
	webhookReg *webhook.Registry
	webhookSrv *webhook.Server
	webhookTun *webhook.Tunnel
	webhookApp string // application_sid of the webhook-bound Application
	webhookOn  bool
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

	sp := provision.New(cfg.APIBaseURL, cfg.SPAPIKey, "", v,
		provision.WithLabel("sp"))

	// Sweep stale ephemeral accounts from previous (crashed) runs. Only
	// accounts whose name starts with `it-` (and not the current run's
	// prefix) are considered. Sweeper has the post-incident hardening:
	// double-checks every account's name before delete and cleans up its
	// clients first to avoid the upstream FK constraint failure.
	swept, err := (&provision.AccountSweeper{C: sp}).Sweep(provision.RunID())
	if err != nil {
		log.Printf("tests/verbs: account sweep failed: %v", err)
	} else if swept > 0 {
		log.Printf("tests/verbs: swept %d stale ephemeral accounts", swept)
	}

	// 1. Provision the per-suite account.
	setupCtx, setupCancel := context.WithTimeout(context.Background(), 90*time.Second)
	suite, err = provision.SetupSuiteAccount(setupCtx, sp, cfg.SPSID, cfg.APIBaseURL,
		"verbs", cfg.SIPRealmZone)
	setupCancel()
	if err != nil {
		log.Fatalf("tests/verbs: suite setup failed: %v", err)
	}
	client = suite.AccountClient
	log.Printf("tests/verbs: suite account=%s sid=%s realm=%s",
		suite.AccountName, suite.AccountSID, suite.SIPRealm)

	// 2. Static DNS resolver pointing the synthetic realm at the SBC IP.
	sipResolver, err = jsip.NewStaticResolver(suite.SBCResolverHosts(cfg.SBCPublicIP))
	if err != nil {
		log.Fatalf("tests/verbs: resolver: %v", err)
	}

	// 3. Deepgram speech credential under the suite account.
	if err := provisionDeepgramCredential(); err != nil {
		log.Fatalf("tests/verbs: Deepgram credential provisioning failed: %v", err)
	}
	log.Printf("tests/verbs: Deepgram credential label=%s sid=%s",
		deepgramLabel, deepgramSID)

	// 4. Webhook server + ngrok tunnel + Application bound to the suite.
	if err := setupWebhook(v); err != nil {
		log.Fatalf("tests/verbs: webhook setup failed: %v", err)
	}
	webhookOn = true
	log.Printf("tests/verbs: webhook ready app_sid=%s tunnel=%s",
		webhookApp, webhookTun.URL())

	// Heartbeat (see helpers_test.go for full rationale).
	stopHeartbeat := StartHeartbeat(5 * time.Second)

	code := m.Run()

	stopHeartbeat()
	PrintFailureSummary()

	// Teardown — best-effort. Order matters: tear down the webhook
	// Application + tunnel BEFORE deleting the account so the account's
	// cascade doesn't have to fight FK constraints.
	teardownWebhook()
	teardownDeepgramCredential()
	if sipResolver != nil {
		_ = sipResolver.Close()
	}
	if suite != nil {
		teardownCtx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		errs := suite.Teardown(teardownCtx)
		cancel()
		for _, e := range errs {
			log.Printf("tests/verbs: teardown: %v", e)
		}
	}
	os.Exit(code)
}

// provisionDeepgramCredential creates a Deepgram speech credential under
// the suite account, labelled `it-deepgram-<runID>`.
func provisionDeepgramCredential() error {
	deepgramLabel = "it-deepgram-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "deepgram",
		Label:     deepgramLabel,
		APIKey:    cfg.DeepgramAPIKey,
		UseForTTS: true,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	deepgramSID = sid
	return nil
}

func teardownDeepgramCredential() {
	if deepgramSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, deepgramSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Deepgram credential %s: %v", deepgramSID, err)
	}
}

// setupWebhook starts the local server, opens an ngrok tunnel, and
// provisions an Application bound to the tunnel under the suite account.
func setupWebhook(v *contract.Validator) error {
	webhookReg = webhook.NewRegistry()
	srv, err := webhook.New(webhookReg, v)
	if err != nil {
		return fmt.Errorf("webhook.New: %w", err)
	}
	webhookSrv = srv
	// Expose tests/verbs/testdata as /static/ on the tunnel so play/dub
	// tests can drive jambonz at a fixture WAV with a pinned transcript
	// (testdata/test_audio.wav → "The sun is shining."). Without this,
	// those tests would have to rely on a third-party-hosted sample
	// whose content they can't verify.
	if abs, err := filepath.Abs("testdata"); err == nil {
		srv.SetStaticDir(abs)
	}
	go func() { _ = srv.Serve() }()

	tun, err := webhook.StartNgrok(context.Background(), srv)
	if err != nil {
		return fmt.Errorf("ngrok: %w", err)
	}
	webhookTun = tun

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateApplication(ctx, provision.ApplicationCreate{
		Name:       provision.Name("webhook-app"),
		AccountSID: suite.AccountSID,
		CallHook: provision.Webhook{
			URL:    tun.URL() + "/hook",
			Method: "POST",
		},
		CallStatusHook: provision.Webhook{
			URL:    tun.URL() + "/status",
			Method: "POST",
		},
		SpeechSynthesisVendor:    "deepgram",
		SpeechSynthesisLabel:     deepgramLabel,
		SpeechSynthesisVoice:     deepgramVoice,
		SpeechRecognizerVendor:   "deepgram",
		SpeechRecognizerLabel:    deepgramLabel,
		SpeechRecognizerLanguage: "en-US",
	})
	if err != nil {
		_ = tun.Close()
		return fmt.Errorf("provision webhook app: %w", err)
	}
	webhookApp = sid
	return nil
}

func teardownWebhook() {
	if webhookApp != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = client.DeleteApplication(ctx, webhookApp)
	}
	if webhookTun != nil {
		_ = webhookTun.Close()
	}
	if webhookSrv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = webhookSrv.Stop(ctx)
	}
}

// requireWebhook is kept for source compatibility with existing tests but
// is now a no-op — the webhook always runs in the new model. (Kept the
// helper so we don't have to edit every Phase-2 test.)
func requireWebhook(t *testing.T) {
	t.Helper()
	if !webhookOn {
		t.Skip("webhook setup failed at TestMain")
	}
}

// withTimeLimit overrides the default call.timeLimit for a single test.
func withTimeLimit(seconds int) func(*provision.CallCreate) {
	return func(c *provision.CallCreate) { c.TimeLimit = seconds }
}
