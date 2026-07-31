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
	"github.com/jambonz-selfhosting/smoke-tester/internal/recording"
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

	// Deepgram Flux TTS credential — a distinct vendor ("deepgramflux",
	// wss://api.deepgram.com/v2/speak) that reuses DEEPGRAM_API_KEY.
	// Provisioned unconditionally at TestMain (the key is already required).
	deepgramFluxLabel string
	deepgramFluxSID   string
	// Default TTS voice/model when speaking through Deepgram Flux.
	deepgramFluxVoice = "flux-alexis-en"

	// Murf TTS speech credential provisioned at TestMain IF MURF_API_KEY is
	// set (optional vendor). When unset, murfLabel stays "" and the Murf say
	// test skips (passes) with a credential-missing log.
	murfLabel string
	murfSID   string
	// Default Murf voice (verified live against api.murf.ai/v1/speech/voices).
	murfVoice = "en-US-alina"

	// xai speech credential (dual-use STT+TTS) provisioned at TestMain IF
	// XAI_API_KEY is set (optional vendor). When unset, xaiLabel stays "" and
	// the xai gather/transcribe/say tests pass without exercising xai.
	xaiLabel string
	xaiSID   string
	// Default xai TTS voice.
	xaiVoice    = "eve"
	xaiLlmModel = "grok-4.3" // xAI flagship chat model for the agent-verb LLM test

	// speechmatics speech credential (STT-only) provisioned at TestMain IF
	// SPEECHMATICS_API_KEY is set (optional vendor). When unset,
	// speechmaticsLabel stays "" and the speechmatics gather/transcribe tests
	// pass without exercising speechmatics.
	speechmaticsLabel string
	speechmaticsSID   string

	// openai speech credential (STT-only) provisioned at TestMain IF
	// OPENAI_API_KEY is set (optional vendor). When unset, openaiLabel stays
	// "" and the openai gather/transcribe tests pass without exercising
	// openai STT.
	openaiLabel string
	openaiSID   string

	// google speech credential (STT-only) provisioned at TestMain IF
	// GOOGLE_STT_KEYFILE is set (optional vendor). When unset, googleLabel
	// stays "" and the google STT v2 tests pass without exercising google.
	googleLabel string
	googleSID   string

	// Webhook server + ngrok tunnel + Application bound to the suite
	// account. The webhook always runs (NGROK_AUTHTOKEN is mandatory in
	// the new model).
	webhookReg *webhook.Registry
	webhookSrv *webhook.Server
	webhookTun *webhook.Tunnel
	webhookApp string // application_sid of the webhook-bound Application
	webhookOn  bool

	// wsApp is a second Application whose call_hook is a wss:// URL pointing
	// at the webhook server's /appws/ endpoint. Calls placed against it run
	// the whole verb script over the jambonz WebSocket API, which turns on
	// feature-server's appIsUsingWebsockets — required by streaming-only
	// verbs (e.g. `say` with stream:true). Provisioned at TestMain.
	wsApp string
)

// wsSharedPathID mirrors webhook.wsSharedPathID — the sentinel path segment
// of the shared WS Application's call_hook. The real test is resolved from
// the frame's x_test_id (the POST /Calls tag).
const wsSharedPathID = "shared"

func TestMain(m *testing.M) {
	cfg = config.MustLoad()

	// Per-leg call recording (ADR-0016). When RECORD_LEGS is set, install an
	// archiver so each recorded leg is also written as a playable WAV under
	// <RECORD_DIR>/<test>/<leg>.wav. The test comes from the per-test stack's
	// Owner (claimUAS) and the leg name from the recording file's basename —
	// no per-test wiring. Off by default (nil hook = no-op).
	if cfg.RecordLegs {
		arch := recording.New(cfg.RecordDir)
		jsip.SetArchiveHook(arch.Hook)
		log.Printf("tests/verbs: RECORD_LEGS on — archiving call legs to %s/<test>/<leg>.wav", cfg.RecordDir)
	}

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

	// 3a2. Deepgram Flux TTS credential — reuses DEEPGRAM_API_KEY, so it is
	// always provisioned (no separate env gate).
	if err := provisionDeepgramFluxCredential(); err != nil {
		log.Fatalf("tests/verbs: Deepgram Flux credential provisioning failed: %v", err)
	}
	log.Printf("tests/verbs: Deepgram Flux credential label=%s sid=%s",
		deepgramFluxLabel, deepgramFluxSID)

	// 3b. Murf TTS speech credential — optional. Only provisioned when
	// MURF_API_KEY is set; otherwise the Murf say test skips with a log.
	if cfg.HasMurf() {
		if err := provisionMurfCredential(); err != nil {
			log.Fatalf("tests/verbs: Murf credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: Murf credential label=%s sid=%s", murfLabel, murfSID)
	} else {
		log.Printf("tests/verbs: MURF_API_KEY not set — Murf say test will skip")
	}

	// 3c. xai STT speech credential — optional. Only provisioned when
	// XAI_API_KEY is set; otherwise the xai gather/transcribe tests pass
	// without exercising xai STT.
	if cfg.HasXai() {
		if err := provisionXaiCredential(); err != nil {
			log.Fatalf("tests/verbs: xai credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: xai credential label=%s sid=%s", xaiLabel, xaiSID)
	} else {
		log.Printf("tests/verbs: XAI_API_KEY not set — xai STT tests will pass without exercising xai")
	}

	// 3d. speechmatics STT speech credential — optional. Only provisioned
	// when SPEECHMATICS_API_KEY is set; otherwise the speechmatics gather/
	// transcribe tests pass without exercising speechmatics STT.
	if cfg.HasSpeechmatics() {
		if err := provisionSpeechmaticsCredential(); err != nil {
			log.Fatalf("tests/verbs: speechmatics credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: speechmatics credential label=%s sid=%s", speechmaticsLabel, speechmaticsSID)
	} else {
		log.Printf("tests/verbs: SPEECHMATICS_API_KEY not set — speechmatics STT tests will pass without exercising speechmatics")
	}

	// 3e. openai STT speech credential — optional. Only provisioned when
	// OPENAI_API_KEY is set; otherwise the openai gather/transcribe tests
	// pass without exercising openai STT.
	if cfg.HasOpenAI() {
		if err := provisionOpenaiCredential(); err != nil {
			log.Fatalf("tests/verbs: openai credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: openai credential label=%s sid=%s", openaiLabel, openaiSID)
	} else {
		log.Printf("tests/verbs: OPENAI_API_KEY not set — openai STT tests will pass without exercising openai")
	}

	// 3f. google STT speech credential — optional. Only provisioned when
	// GOOGLE_STT_KEYFILE is set; otherwise the google STT v2 tests pass
	// without exercising google STT.
	if cfg.HasGoogleSTT() {
		if err := provisionGoogleCredential(); err != nil {
			log.Fatalf("tests/verbs: google credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: google credential label=%s sid=%s", googleLabel, googleSID)
	} else {
		log.Printf("tests/verbs: GOOGLE_STT_KEYFILE not set — google STT v2 tests will pass without exercising google")
	}

	// 4. Webhook server + ngrok tunnel + Application bound to the suite.
	if err := setupWebhook(v); err != nil {
		log.Fatalf("tests/verbs: webhook setup failed: %v", err)
	}
	webhookOn = true
	log.Printf("tests/verbs: webhook ready app_sid=%s ws_app_sid=%s tunnel=%s",
		webhookApp, wsApp, webhookTun.URL())

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
	teardownDeepgramFluxCredential()
	teardownMurfCredential()
	teardownXaiCredential()
	teardownSpeechmaticsCredential()
	teardownOpenaiCredential()
	teardownGoogleCredential()
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

// provisionDeepgramFluxCredential creates a Deepgram Flux TTS speech
// credential under the suite account, labelled `it-deepgramflux-<runID>`.
// TTS-only here (Flux STT has its own recognizer path); reuses the required
// DEEPGRAM_API_KEY, so it is always provisioned.
func provisionDeepgramFluxCredential() error {
	deepgramFluxLabel = "it-deepgramflux-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "deepgramflux",
		Label:     deepgramFluxLabel,
		APIKey:    cfg.DeepgramAPIKey,
		UseForTTS: true,
		UseForSTT: false,
	})
	if err != nil {
		return err
	}
	deepgramFluxSID = sid
	return nil
}

func teardownDeepgramFluxCredential() {
	if deepgramFluxSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, deepgramFluxSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Deepgram Flux credential %s: %v", deepgramFluxSID, err)
	}
}

// provisionMurfCredential creates a Murf TTS speech credential under the
// suite account, labelled `it-murf-<runID>`. TTS-only (Murf is a synthesis
// vendor). Called only when MURF_API_KEY is set.
func provisionMurfCredential() error {
	murfLabel = "it-murf-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "murf",
		Label:     murfLabel,
		APIKey:    cfg.MurfAPIKey,
		UseForTTS: true,
		UseForSTT: false,
	})
	if err != nil {
		return err
	}
	murfSID = sid
	return nil
}

func teardownMurfCredential() {
	if murfSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, murfSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Murf credential %s: %v", murfSID, err)
	}
}

// provisionXaiCredential creates an xai speech credential under the suite
// account, labelled `it-xai-<runID>`. Dual-use (xai supports both TTS and
// STT); the xai TTS say tests reuse this same credential. Called only when
// XAI_API_KEY is set.
func provisionXaiCredential() error {
	xaiLabel = "it-xai-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "xai",
		Label:     xaiLabel,
		APIKey:    cfg.XaiAPIKey,
		UseForTTS: true,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	xaiSID = sid
	return nil
}

func teardownXaiCredential() {
	if xaiSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, xaiSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete xai credential %s: %v", xaiSID, err)
	}
}

// provisionSpeechmaticsCredential creates a speechmatics speech credential
// under the suite account, labelled `it-speechmatics-<runID>`. STT-only.
// Called only when SPEECHMATICS_API_KEY is set.
func provisionSpeechmaticsCredential() error {
	speechmaticsLabel = "it-speechmatics-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:             "speechmatics",
		Label:              speechmaticsLabel,
		APIKey:             cfg.SpeechmaticsAPIKey,
		SpeechmaticsSTTURI: cfg.SpeechmaticsSTTURI,
		UseForSTT:          true,
	})
	if err != nil {
		return err
	}
	speechmaticsSID = sid
	return nil
}

func teardownSpeechmaticsCredential() {
	if speechmaticsSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, speechmaticsSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete speechmatics credential %s: %v", speechmaticsSID, err)
	}
}

// provisionOpenaiCredential creates an openai speech credential under the
// suite account, labelled `it-openai-<runID>`. STT-only — the model is chosen
// per-test via recognizer.openaiOptions.model, not on the credential.
// Called only when OPENAI_API_KEY is set.
func provisionOpenaiCredential() error {
	openaiLabel = "it-openai-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor: "openai",
		Label:  openaiLabel,
		APIKey: cfg.OpenAIAPIKey,
		// the API requires a model on an openai credential; each test overrides
		// it via recognizer.openaiOptions.model
		ModelID:   "gpt-live-transcribe",
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	openaiSID = sid
	return nil
}

func teardownOpenaiCredential() {
	if openaiSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, openaiSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete openai credential %s: %v", openaiSID, err)
	}
}

// provisionGoogleCredential creates a google speech credential under the
// suite account, labelled `it-google-<runID>`. STT-only; the service-account
// JSON goes in `service_key`. Called only when GOOGLE_STT_KEYFILE is set.
func provisionGoogleCredential() error {
	googleLabel = "it-google-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "google",
		Label:      googleLabel,
		ServiceKey: cfg.GoogleSTTServiceKey,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	googleSID = sid

	// google-only, and mandatory: feature-server ignores a google credential
	// until stt_tested_ok is set on the row, so an untested credential makes
	// every google gather fail with "creds not supplied". See
	// provision.TestAccountSpeechCredential.
	res, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, sid)
	if err != nil {
		return fmt.Errorf("test google credential %s: %w", sid, err)
	}
	if res.STT.Status != "ok" {
		return fmt.Errorf("google credential %s failed its STT test: status=%q reason=%q",
			sid, res.STT.Status, res.STT.Reason)
	}
	return nil
}

func teardownGoogleCredential() {
	if googleSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, googleSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete google credential %s: %v", googleSID, err)
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

	// Second Application driven over the jambonz WebSocket API. Its
	// call_hook is a wss:// URL (method WS); feature-server builds a
	// WsRequestor for it and fetches the verb script over the socket, which
	// is what streaming-only verbs (say stream:true) require. The path
	// carries the shared sentinel id; the real test is resolved from the
	// per-call x_test_id tag inside session:new.
	wsHookURL := wssURL(tun.URL(), "/appws/"+wsSharedPathID)
	wsSID, err := client.CreateApplication(ctx, provision.ApplicationCreate{
		Name:       provision.Name("ws-app"),
		AccountSID: suite.AccountSID,
		// No Method: the DB's webhook.method column only accepts GET/POST.
		// feature-server keys WS off the URL scheme (lib/middleware.js:
		// url.startsWith('wss://')), so a wss:// call_hook alone builds a
		// WsRequestor and sets appIsUsingWebsockets.
		CallHook: provision.Webhook{
			URL: wsHookURL,
		},
		// call_status over WS rides the same socket; give a status hook
		// anyway so the Application validates identically to the HTTP one.
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
		return fmt.Errorf("provision ws app: %w", err)
	}
	wsApp = wsSID
	return nil
}

func teardownWebhook() {
	if wsApp != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = client.DeleteApplication(ctx, wsApp)
	}
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
