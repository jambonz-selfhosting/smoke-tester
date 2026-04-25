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
// webhook.Registry. Phase 2 tests skip cleanly when NGROK_AUTHTOKEN isn't
// set.
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
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/config"
	"github.com/jambonz-selfhosting/smoke-tester/internal/contract"
	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

var (
	cfg    *config.Settings
	client *provision.Client

	// Deepgram speech credential provisioned at TestMain. Verb tests
	// reference this label as `synthesizer.label` / `recognizer.label` so
	// jambonz uses our managed Deepgram key rather than whatever defaults
	// the account happens to have. Empty string when DEEPGRAM_API_KEY is
	// unset — tests fall back to the cluster's default vendor.
	deepgramLabel string
	deepgramSID   string // SpeechCredential SID, used for teardown
	// Default TTS voice when speaking through Deepgram. Aura voices are
	// the only TTS option on Deepgram. Override per-test by passing an
	// explicit synthesizer.voice.
	deepgramVoice = "aura-asteria-en"

	// Webhook-tier globals. Populated only if NGROK_AUTHTOKEN is present;
	// otherwise webhook-dependent tests must t.Skip.
	webhookReg *webhook.Registry
	webhookSrv *webhook.Server
	webhookTun *webhook.Tunnel
	webhookApp string // application_sid of the Application bound to the tunnel
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
	client = provision.New(cfg.APIBaseURL, cfg.APIKey, cfg.AccountSID, v,
		provision.WithLabel("account"))

	// Sweep stale SIP Clients from crashed prior runs before any test runs.
	// claimUAS dynamically provisions a fresh /Clients user per test; without
	// the sweep, escapees from crashed runs would accumulate and eventually
	// collide on the username uniqueness constraint (it's hashed, so collisions
	// are unlikely, but the sweep keeps the account tidy regardless).
	swept, err := (&provision.SIPClientSweeper{C: client}).Sweep(provision.RunID())
	if err != nil {
		log.Printf("tests/verbs: SIP-client sweep failed: %v", err)
	} else if swept > 0 {
		log.Printf("tests/verbs: swept %d stale SIP clients from prior runs", swept)
	}

	// REST credentials are mandatory (claimUAS requires /Clients access).
	if cfg.APIKey == "" || cfg.AccountSID == "" {
		log.Printf("tests/verbs: SKIP (JAMBONZ_API_KEY / JAMBONZ_ACCOUNT_SID unset — needed for /Clients)")
		os.Exit(0)
	}

	// Provision the suite-wide Deepgram credential. Every verb test that
	// emits TTS or runs STT references this label. Skipping with a clear
	// log keeps the suite honest if DEEPGRAM_API_KEY is missing —
	// transcript assertions log-skip via stt.HasKey() and
	// `synthesizer:{vendor:"deepgram", label}` references will fail at
	// jambonz-side credential lookup, which surfaces as test failures
	// (not silent fallback to whatever default vendor the account has).
	if cfg.HasDeepgram() {
		if err := provisionDeepgramCredential(); err != nil {
			log.Fatalf("tests/verbs: Deepgram credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: Deepgram credential provisioned label=%s sid=%s",
			deepgramLabel, deepgramSID)
	} else {
		log.Printf("tests/verbs: DEEPGRAM_API_KEY unset; verb tests using deepgram label will fail")
	}

	// Webhook (Phase 2) — optional. Fails the tests that need it via
	// requireWebhook() rather than aborting the whole run.
	if tok := os.Getenv("NGROK_AUTHTOKEN"); tok != "" {
		if err := setupWebhook(v); err != nil {
			log.Printf("tests/verbs: webhook setup failed (phase-2 tests will skip): %v", err)
		} else {
			webhookOn = true
			log.Printf("tests/verbs: webhook ready app_sid=%s tunnel=%s", webhookApp, webhookTun.URL())
		}
	} else {
		log.Printf("tests/verbs: NGROK_AUTHTOKEN unset; phase-2 (webhook) tests will skip")
	}

	// Heartbeat: prints a one-line status to stderr every 5s so operators
	// running the suite without -v see progress instead of a 60-90 second
	// silence. Quiet enough to not spam, loud enough to prove the suite
	// is alive — and the "now: <test>[step:X]@Ns" suffix instantly
	// surfaces stuck tests.
	stopHeartbeat := StartHeartbeat(5 * time.Second)

	code := m.Run()

	stopHeartbeat()

	// One-line-per-failure summary, written AFTER m.Run() so it lands at
	// the bottom of the output where operators expect to look. Without
	// this, under -parallel and without -v, failure details get buried in
	// interleaved log noise from concurrent tests.
	PrintFailureSummary()

	// Teardown — best-effort; tests are allowed to continue even if cleanup fails.
	teardownWebhook()
	teardownDeepgramCredential()
	os.Exit(code)
}

// provisionDeepgramCredential creates a Deepgram speech credential under
// the test account, labelled `it-deepgram-<runID>`. The label is per-run
// because jambonz enforces uniqueness on (account, vendor, label) — two
// concurrent CI invocations sharing a label would 422.
func provisionDeepgramCredential() error {
	deepgramLabel = "it-deepgram-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, cfg.AccountSID, provision.SpeechCredentialCreate{
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
	if err := client.DeleteAccountSpeechCredential(ctx, cfg.AccountSID, deepgramSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Deepgram credential %s: %v", deepgramSID, err)
	}
}

// setupWebhook starts the local server, opens an ngrok tunnel, and provisions
// the Application that routes verb fetches to the tunnel.
func setupWebhook(v *contract.Validator) error {
	webhookReg = webhook.NewRegistry()
	srv, err := webhook.New(webhookReg, v)
	if err != nil {
		return fmt.Errorf("webhook.New: %w", err)
	}
	webhookSrv = srv
	go func() {
		// Local listener for loopback access (debugging, health checks).
		// Ignored errors are expected on shutdown.
		_ = srv.Serve()
	}()

	tun, err := webhook.StartNgrok(context.Background(), srv)
	if err != nil {
		return fmt.Errorf("ngrok: %w", err)
	}
	webhookTun = tun

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateApplication(ctx, provision.ApplicationCreate{
		Name:       provision.Name("webhook-app"),
		AccountSID: cfg.AccountSID,
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

// requireWebhook skips the test if Phase-2 setup didn't succeed.
func requireWebhook(t *testing.T) {
	t.Helper()
	if !webhookOn {
		t.Skip("webhook not configured (NGROK_AUTHTOKEN missing or setup failed)")
	}
}

// withTimeLimit overrides the default call.timeLimit for a single test.
func withTimeLimit(seconds int) func(*provision.CallCreate) {
	return func(c *provision.CallCreate) { c.TimeLimit = seconds }
}
