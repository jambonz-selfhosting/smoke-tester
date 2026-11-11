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
	// spClient is the service-provider-scope client, for the few tests that
	// flip an SP-level setting (and restore it).
	spClient *provision.Client

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

	// ElevenLabs TTS credential, provisioned IF ELEVENLABS_API_KEY is set.
	// Tests pick the model per verb via synthesizer options.model_id.
	elevenlabsLabel string
	elevenlabsSID   string
	// George, a premade voice; legacy voices such as Rachel reject audio tags on eleven_v3
	elevenlabsVoice = "JBFqnCBsd6RMkjVDRZzb"
	// separate STT credential so the TTS one keeps its own stt_model_id
	elevenlabsSttLabel string
	elevenlabsSttSID   string

	// KugelAudio TTS credential, provisioned IF KUGELAUDIO_API_KEY is set.
	kugelaudioLabel string
	kugelaudioSID   string
	// Same key, enabled for noise isolation (Clarity) only: labelled, and
	// unlabelled for the default lookup.
	kugelaudioNoiseLabel      string
	kugelaudioNoiseSID        string
	kugelaudioDefaultNoiseSID string
	// STT-only, on kugel-ear-1
	kugelaudioSttLabel string
	kugelaudioSttSID   string
	// Samantha Ferris (en-US); KugelAudio voice ids are numeric.
	kugelaudioVoice = "1071"

	// AssemblyAI TTS credential, provisioned IF ASSEMBLYAI_API_KEY is set.
	assemblyaiLabel string
	assemblyaiSID   string
	assemblyaiVoice = "aai-jane"
	// same key on the EU host (credential api_uri)
	assemblyaiEULabel string
	assemblyaiEUSID   string

	// Speechify TTS credential, provisioned IF SPEECHIFY_API_KEY is set.
	// No model on the credential, so mediajam picks one from the language.
	speechifyLabel string
	speechifySID   string
	speechifyVoice = "geffen_32"

	// Svara TTS credential, provisioned IF SVARA_API_KEY is set.
	svaraLabel string
	svaraSID   string

	// Giggy TTS + STT credential, provisioned IF GIGGY_API_KEY is set.
	giggyLabel string
	giggySID   string

	// xai speech credential (dual-use STT+TTS) provisioned at TestMain IF
	// XAI_API_KEY is set (optional vendor). When unset, xaiLabel stays "" and
	// the xai gather/transcribe/say tests pass without exercising xai.
	xaiLabel string
	xaiSID   string
	// zoom STT-only speech credential, provisioned only when ZOOM_API_KEY is set.
	zoomLabel string
	zoomSID   string

	// gradium STT-only speech credential, provisioned only when GRADIUM_API_KEY is set.
	gradiumLabel string
	gradiumSID   string

	// mistral (Voxtral) STT+TTS speech credential, provisioned only when MISTRAL_API_KEY is set.
	mistralLabel string
	mistralSID   string

	// paxa STT+TTS speech credential, provisioned only when PAXA_API_KEY is set.
	paxaLabel string
	paxaSID   string

	// sarvam STT+TTS speech credential, provisioned only when SARVAM_API_KEY is set.
	sarvamLabel string
	sarvamSID   string

	// smallestai STT+TTS speech credential, provisioned only when SMALLESTAI_API_KEY is set.
	smallestaiLabel string
	smallestaiSID   string

	// azureopenai STT+TTS speech credential, provisioned only when
	// AZUREOPENAI_API_KEY and AZUREOPENAI_API_URI are set.
	azureopenaiLabel string
	azureopenaiSID   string

	// gnani STT+TTS speech credential, provisioned only when GNANI_API_KEY is set.
	gnaniLabel string
	gnaniSID   string

	// meta STT-only speech credential, provisioned only when META_API_KEY is set.
	metaLabel string
	metaSID   string

	// minimax TTS-only speech credential, provisioned only when MINIMAX_API_KEY is set.
	minimaxLabel string
	minimaxSID   string

	// neuphonic TTS-only speech credential, provisioned only when NEUPHONIC_API_KEY is set.
	neuphonicLabel string
	neuphonicSID   string

	// camb TTS-only speech credential, provisioned only when CAMB_API_KEY is set.
	cambLabel string
	cambSID   string

	// asyncai TTS-only speech credential, provisioned only when ASYNCAI_API_KEY is set.
	asyncaiLabel string
	asyncaiSID   string

	// respeecher TTS-only speech credential, provisioned only when RESPEECHER_API_KEY is set.
	respeecherLabel string
	respeecherSID   string

	// bland TTS-only speech credential, provisioned only when BLAND_API_KEY is set.
	blandLabel string
	blandSID   string

	// simplismart TTS-only speech credential, provisioned only when SIMPLISMART_API_KEY is set.
	simplismartLabel string
	simplismartSID   string

	// spitch TTS-only speech credential, provisioned only when SPITCH_API_KEY is set.
	spitchLabel string
	spitchSID   string

	// kokoro TTS-only speech credential, provisioned only when KOKORO_API_URI is set.
	kokoroLabel string
	kokoroSID   string

	// vakyam TTS-only speech credential, provisioned only when VAKYAM_API_KEY is set.
	vakyamLabel string
	vakyamSID   string

	// groq TTS-only speech credential, provisioned only when GROQ_API_KEY is set.
	groqLabel string
	groqSID   string

	// hume TTS-only speech credential, provisioned only when HUME_API_KEY is set.
	humeLabel string
	humeSID   string

	// inworld STT-only speech credential, provisioned only when INWORLD_API_KEY is set.
	inworldLabel string
	inworldSID   string

	// soniox STT+TTS speech credential, provisioned only when SONIOX_API_KEY is set.
	sonioxLabel string
	sonioxSID   string

	// saidwire STT-only speech credential, provisioned only when SAIDWIRE_API_KEY is set.
	saidwireLabel string
	saidwireSID   string

	// Default xai TTS voice.
	xaiVoice    = "eve"
	xaiLlmModel = "grok-4.3" // xAI flagship chat model for the agent-verb LLM test

	// google speech credential whose default STT model is a gemini live
	// model, provisioned at TestMain IF GEMINI_API_KEY is set (optional).
	// When unset, geminiLabel stays "" and the gemini gather/transcribe tests
	// pass without exercising gemini.
	geminiLabel string
	geminiSID   string
	// The live transcription model as Vertex AI publishes it; the
	// recorded-audio sibling is a file API with no place in a call.
	geminiSttModel = "gemini-3.5-transcribe-live-preview"

	// speechmatics speech credential (STT-only) provisioned at TestMain IF
	// SPEECHMATICS_API_KEY is set (optional vendor). When unset,
	// speechmaticsLabel stays "" and the speechmatics gather/transcribe tests
	// pass without exercising speechmatics.
	speechmaticsLabel string
	speechmaticsSID   string

	// speechmatics TTS-only credential (no speechmatics_stt_uri), same
	// SPEECHMATICS_API_KEY; a create failure is kept for the credential test.
	speechmaticsTtsLabel string
	speechmaticsTtsSID   string
	speechmaticsTtsErr   error
	speechmaticsTtsVoice = "sarah"

	// speechmaticsagent (Speechmatics Agent STT) speech credential
	// (STT-only) provisioned at TestMain IF SPEECHMATICS_AGENT_API_KEY is
	// set (optional vendor). When unset, speechmaticsAgentLabel stays ""
	// and the agent-STT gather/transcribe tests pass without exercising it.
	speechmaticsAgentLabel string
	speechmaticsAgentSID   string

	// openai speech credential (STT-only) provisioned at TestMain IF
	// OPENAI_API_KEY is set (optional vendor). When unset, openaiLabel stays
	// "" and the openai gather/transcribe tests pass without exercising
	// openai STT.
	openaiLabel string
	openaiSID   string

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
	spClient = sp

	// Sweep stale ephemeral accounts from previous (crashed) runs. Only
	// accounts whose name starts with `it-` (and not the current run's
	// prefix) are considered. Sweeper has the post-incident hardening:
	// double-checks every account's name before delete and cleans up its
	// clients first to avoid the upstream FK constraint failure.
	swept, err := (&provision.AccountSweeper{C: sp, MinAge: cfg.OrphanTTL}).Sweep(provision.RunID())
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

	// 3b2. ElevenLabs TTS speech credential — optional.
	if cfg.HasElevenlabs() {
		if err := provisionElevenlabsCredential(); err != nil {
			log.Fatalf("tests/verbs: ElevenLabs credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: ElevenLabs credential label=%s sid=%s", elevenlabsLabel, elevenlabsSID)
		if err := provisionElevenlabsSttCredential(); err != nil {
			log.Fatalf("tests/verbs: ElevenLabs STT credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: ElevenLabs STT credential label=%s sid=%s", elevenlabsSttLabel, elevenlabsSttSID)
	} else {
		log.Printf("tests/verbs: ELEVENLABS_API_KEY not set — ElevenLabs tests will skip")
	}

	// 3b3. KugelAudio TTS speech credential — optional.
	if cfg.HasKugelaudio() {
		if err := provisionKugelaudioCredential(); err != nil {
			log.Fatalf("tests/verbs: KugelAudio credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: KugelAudio credentials label=%s sid=%s, noise isolation label=%s sid=%s, stt label=%s sid=%s",
			kugelaudioLabel, kugelaudioSID, kugelaudioNoiseLabel, kugelaudioNoiseSID, kugelaudioSttLabel, kugelaudioSttSID)
	} else {
		log.Printf("tests/verbs: KUGELAUDIO_API_KEY not set — KugelAudio tests will skip")
	}

	// 3b3a. AssemblyAI TTS speech credential — optional.
	if cfg.HasAssemblyai() {
		if err := provisionAssemblyaiCredential(); err != nil {
			log.Fatalf("tests/verbs: AssemblyAI credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: AssemblyAI credential label=%s sid=%s", assemblyaiLabel, assemblyaiSID)
	} else {
		log.Printf("tests/verbs: ASSEMBLYAI_API_KEY not set — AssemblyAI tests will skip")
	}

	// 3b4. Speechify TTS speech credential — optional.
	if cfg.HasSpeechify() {
		if err := provisionSpeechifyCredential(); err != nil {
			log.Fatalf("tests/verbs: Speechify credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: Speechify credential label=%s sid=%s", speechifyLabel, speechifySID)
	} else {
		log.Printf("tests/verbs: SPEECHIFY_API_KEY not set — Speechify tests will skip")
	}

	// 3b5. Svara TTS speech credential — optional.
	if cfg.HasSvara() {
		if err := provisionSvaraCredential(); err != nil {
			log.Fatalf("tests/verbs: Svara credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: Svara credential label=%s sid=%s", svaraLabel, svaraSID)
	} else {
		log.Printf("tests/verbs: SVARA_API_KEY not set — Svara tests will skip")
	}

	// 3b6. Giggy TTS speech credential — optional.
	if cfg.HasGiggy() {
		if err := provisionGiggyCredential(); err != nil {
			log.Fatalf("tests/verbs: Giggy credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: Giggy credential label=%s sid=%s", giggyLabel, giggySID)
	} else {
		log.Printf("tests/verbs: GIGGY_API_KEY not set — Giggy tests will skip")
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

	// 3c'. zoom STT speech credential — optional, same pattern as xai.
	if cfg.HasZoom() {
		if err := provisionZoomCredential(); err != nil {
			log.Fatalf("tests/verbs: zoom credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: zoom credential label=%s sid=%s", zoomLabel, zoomSID)
	} else {
		log.Printf("tests/verbs: ZOOM_API_KEY not set — zoom STT tests will pass without exercising zoom")
	}
	// 3c''. gradium STT speech credential — optional, same pattern as zoom.
	if cfg.HasGradium() {
		if err := provisionGradiumCredential(); err != nil {
			log.Fatalf("tests/verbs: gradium credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: gradium credential label=%s sid=%s", gradiumLabel, gradiumSID)
	} else {
		log.Printf("tests/verbs: GRADIUM_API_KEY not set — gradium STT tests will pass without exercising gradium")
	}
	// 3c'''. mistral STT+TTS speech credential — optional, same pattern as gradium.
	if cfg.HasMistral() {
		if err := provisionMistralCredential(); err != nil {
			log.Fatalf("tests/verbs: mistral credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: mistral credential label=%s sid=%s", mistralLabel, mistralSID)
	} else {
		log.Printf("tests/verbs: MISTRAL_API_KEY not set — mistral tests will pass without exercising mistral")
	}
	// 3c''''. paxa STT+TTS speech credential — optional, same pattern as mistral.
	if cfg.HasPaxa() {
		if err := provisionPaxaCredential(); err != nil {
			log.Fatalf("tests/verbs: paxa credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: paxa credential label=%s sid=%s", paxaLabel, paxaSID)
	} else {
		log.Printf("tests/verbs: PAXA_API_KEY not set — paxa tests will pass without exercising paxa")
	}
	// sarvam STT+TTS speech credential — optional, same pattern as paxa.
	if cfg.HasSarvam() {
		if err := provisionSarvamCredential(); err != nil {
			log.Fatalf("tests/verbs: sarvam credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: sarvam credential label=%s sid=%s", sarvamLabel, sarvamSID)
	} else {
		log.Printf("tests/verbs: SARVAM_API_KEY not set — sarvam tests will pass without exercising sarvam")
	}
	// smallestai STT+TTS speech credential — optional, same pattern as paxa.
	if cfg.HasSmallestai() {
		if err := provisionSmallestaiCredential(); err != nil {
			log.Fatalf("tests/verbs: smallestai credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: smallestai credential label=%s sid=%s", smallestaiLabel, smallestaiSID)
	} else {
		log.Printf("tests/verbs: SMALLESTAI_API_KEY not set — smallestai tests will pass without exercising smallestai")
	}
	// azureopenai STT+TTS speech credential — optional, same pattern as paxa.
	if cfg.HasAzureopenai() {
		if err := provisionAzureopenaiCredential(); err != nil {
			log.Fatalf("tests/verbs: azureopenai credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: azureopenai credential label=%s sid=%s", azureopenaiLabel, azureopenaiSID)
	} else {
		log.Printf("tests/verbs: AZUREOPENAI_API_KEY/AZUREOPENAI_API_URI not set — azureopenai tests will pass without exercising azureopenai")
	}
	// gnani STT+TTS speech credential — optional, same pattern as paxa.
	if cfg.HasGnani() {
		if err := provisionGnaniCredential(); err != nil {
			log.Fatalf("tests/verbs: gnani credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: gnani credential label=%s sid=%s", gnaniLabel, gnaniSID)
	} else {
		log.Printf("tests/verbs: GNANI_API_KEY not set — gnani tests will pass without exercising gnani")
	}
	// meta STT-only speech credential — optional, same pattern as paxa.
	if cfg.HasMeta() {
		if err := provisionMetaCredential(); err != nil {
			log.Fatalf("tests/verbs: meta credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: meta credential label=%s sid=%s", metaLabel, metaSID)
	} else {
		log.Printf("tests/verbs: META_API_KEY not set — meta tests will pass without exercising meta")
	}
	// minimax TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasMinimax() {
		if err := provisionMinimaxCredential(); err != nil {
			log.Fatalf("tests/verbs: minimax credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: minimax credential label=%s sid=%s", minimaxLabel, minimaxSID)
	} else {
		log.Printf("tests/verbs: MINIMAX_API_KEY not set — minimax tests will pass without exercising minimax")
	}
	// neuphonic TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasNeuphonic() {
		if err := provisionNeuphonicCredential(); err != nil {
			log.Fatalf("tests/verbs: neuphonic credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: neuphonic credential label=%s sid=%s", neuphonicLabel, neuphonicSID)
	} else {
		log.Printf("tests/verbs: NEUPHONIC_API_KEY not set — neuphonic tests will pass without exercising neuphonic")
	}
	// camb TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasCamb() {
		if err := provisionCambCredential(); err != nil {
			log.Fatalf("tests/verbs: camb credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: camb credential label=%s sid=%s", cambLabel, cambSID)
	} else {
		log.Printf("tests/verbs: CAMB_API_KEY not set — camb tests will pass without exercising camb")
	}
	// asyncai TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasAsyncai() {
		if err := provisionAsyncaiCredential(); err != nil {
			log.Fatalf("tests/verbs: asyncai credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: asyncai credential label=%s sid=%s", asyncaiLabel, asyncaiSID)
	} else {
		log.Printf("tests/verbs: ASYNCAI_API_KEY not set — asyncai tests will pass without exercising asyncai")
	}
	// respeecher TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasRespeecher() {
		if err := provisionRespeecherCredential(); err != nil {
			log.Fatalf("tests/verbs: respeecher credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: respeecher credential label=%s sid=%s", respeecherLabel, respeecherSID)
	} else {
		log.Printf("tests/verbs: RESPEECHER_API_KEY not set — respeecher tests will pass without exercising respeecher")
	}
	// bland TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasBland() {
		if err := provisionBlandCredential(); err != nil {
			log.Fatalf("tests/verbs: bland credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: bland credential label=%s sid=%s", blandLabel, blandSID)
	} else {
		log.Printf("tests/verbs: BLAND_API_KEY not set — bland tests will pass without exercising bland")
	}
	// simplismart TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasSimplismart() {
		if err := provisionSimplismartCredential(); err != nil {
			log.Fatalf("tests/verbs: simplismart credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: simplismart credential label=%s sid=%s", simplismartLabel, simplismartSID)
	} else {
		log.Printf("tests/verbs: SIMPLISMART_API_KEY not set — simplismart tests will pass without exercising simplismart")
	}
	// spitch TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasSpitch() {
		if err := provisionSpitchCredential(); err != nil {
			log.Fatalf("tests/verbs: spitch credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: spitch credential label=%s sid=%s", spitchLabel, spitchSID)
	} else {
		log.Printf("tests/verbs: SPITCH_API_KEY not set — spitch tests will pass without exercising spitch")
	}
	// kokoro TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasKokoro() {
		if err := provisionKokoroCredential(); err != nil {
			log.Fatalf("tests/verbs: kokoro credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: kokoro credential label=%s sid=%s", kokoroLabel, kokoroSID)
	} else {
		log.Printf("tests/verbs: KOKORO_API_URI not set — kokoro tests will pass without exercising kokoro")
	}
	// vakyam TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasVakyam() {
		if err := provisionVakyamCredential(); err != nil {
			log.Fatalf("tests/verbs: vakyam credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: vakyam credential label=%s sid=%s", vakyamLabel, vakyamSID)
	} else {
		log.Printf("tests/verbs: VAKYAM_API_KEY not set — vakyam tests will pass without exercising vakyam")
	}
	// groq TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasGroq() {
		if err := provisionGroqCredential(); err != nil {
			log.Fatalf("tests/verbs: groq credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: groq credential label=%s sid=%s", groqLabel, groqSID)
	} else {
		log.Printf("tests/verbs: GROQ_API_KEY not set — groq tests will pass without exercising groq")
	}
	// hume TTS-only speech credential — optional, same pattern as paxa.
	if cfg.HasHume() {
		if err := provisionHumeCredential(); err != nil {
			log.Fatalf("tests/verbs: hume credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: hume credential label=%s sid=%s", humeLabel, humeSID)
	} else {
		log.Printf("tests/verbs: HUME_API_KEY not set — hume tests will pass without exercising hume")
	}
	// inworld STT-only speech credential — optional, same pattern as paxa.
	if cfg.HasInworld() {
		if err := provisionInworldCredential(); err != nil {
			log.Fatalf("tests/verbs: inworld credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: inworld credential label=%s sid=%s", inworldLabel, inworldSID)
	} else {
		log.Printf("tests/verbs: INWORLD_API_KEY not set — inworld tests will pass without exercising inworld")
	}
	// soniox STT+TTS speech credential — optional, same pattern as paxa.
	if cfg.HasSoniox() {
		if err := provisionSonioxCredential(); err != nil {
			log.Fatalf("tests/verbs: soniox credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: soniox credential label=%s sid=%s", sonioxLabel, sonioxSID)
	} else {
		log.Printf("tests/verbs: SONIOX_API_KEY not set — soniox tests will pass without exercising soniox")
	}
	// 3c'''''. saidwire STT speech credential — optional, same pattern as gradium.
	if cfg.HasSaidwire() {
		if err := provisionSaidwireCredential(); err != nil {
			log.Fatalf("tests/verbs: saidwire credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: saidwire credential label=%s sid=%s", saidwireLabel, saidwireSID)
	} else {
		log.Printf("tests/verbs: SAIDWIRE_API_KEY not set — saidwire STT tests will pass without exercising saidwire")
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
	if cfg.HasSpeechmatics() {
		// Not fatal: an api-server without TTS-only support must not sink the STT tests.
		if speechmaticsTtsErr = provisionSpeechmaticsTtsCredential(); speechmaticsTtsErr != nil {
			log.Printf("tests/verbs: speechmatics TTS-only credential provisioning failed: %v", speechmaticsTtsErr)
		} else {
			log.Printf("tests/verbs: speechmatics TTS credential label=%s sid=%s", speechmaticsTtsLabel, speechmaticsTtsSID)
		}
	}

	// 3d-bis. speechmaticsagent (Agent STT) speech credential — optional.
	// A separate key from SPEECHMATICS_API_KEY: different vendor, different
	// endpoint, possibly different entitlement.
	if cfg.HasSpeechmaticsAgent() {
		if err := provisionSpeechmaticsAgentCredential(); err != nil {
			log.Fatalf("tests/verbs: speechmaticsagent credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: speechmaticsagent credential label=%s sid=%s",
			speechmaticsAgentLabel, speechmaticsAgentSID)
	} else {
		log.Printf("tests/verbs: SPEECHMATICS_AGENT_API_KEY not set — agent STT tests will pass without exercising it")
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

	// 3f. google/gemini STT speech credential — optional. Only provisioned
	// when GEMINI_API_KEY (plus a service-account key) is set; otherwise the
	// gemini gather/transcribe tests pass without exercising gemini STT.
	if cfg.HasGeminiStt() {
		if err := provisionGeminiCredential(); err != nil {
			log.Fatalf("tests/verbs: gemini credential provisioning failed: %v", err)
		}
		log.Printf("tests/verbs: gemini credential label=%s sid=%s", geminiLabel, geminiSID)
	} else {
		log.Printf("tests/verbs: GEMINI_KEYFILE not set — gemini STT tests will pass without exercising gemini")
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
	teardownElevenlabsCredential()
	teardownElevenlabsSttCredential()
	teardownKugelaudioCredential()
	teardownAssemblyaiCredential()
	teardownSpeechifyCredential()
	teardownSvaraCredential()
	teardownGiggyCredential()
	teardownXaiCredential()
	teardownZoomCredential()
	teardownGradiumCredential()
	teardownMistralCredential()
	teardownPaxaCredential()
	teardownSarvamCredential()
	teardownSmallestaiCredential()
	teardownAzureopenaiCredential()
	teardownGnaniCredential()
	teardownMetaCredential()
	teardownMinimaxCredential()
	teardownNeuphonicCredential()
	teardownCambCredential()
	teardownAsyncaiCredential()
	teardownRespeecherCredential()
	teardownBlandCredential()
	teardownSimplismartCredential()
	teardownSpitchCredential()
	teardownKokoroCredential()
	teardownVakyamCredential()
	teardownGroqCredential()
	teardownHumeCredential()
	teardownInworldCredential()
	teardownSonioxCredential()
	teardownSaidwireCredential()
	teardownSpeechmaticsCredential()
	teardownSpeechmaticsTtsCredential()
	teardownSpeechmaticsAgentCredential()
	teardownOpenaiCredential()
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

// provisionElevenlabsCredential creates a TTS-only ElevenLabs credential,
// labelled `it-elevenlabs-<runID>`, defaulting to a stream-input model.
func provisionElevenlabsCredential() error {
	elevenlabsLabel = "it-elevenlabs-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:  "elevenlabs",
		Label:   elevenlabsLabel,
		APIKey:  cfg.ElevenlabsAPIKey,
		ModelID: "eleven_flash_v2_5",
		// the api-server requires an STT model even on a TTS-only elevenlabs credential
		STTModelID: "scribe_v1",
		UseForTTS:  true,
	})
	if err != nil {
		return err
	}
	elevenlabsSID = sid
	return nil
}

func provisionElevenlabsSttCredential() error {
	elevenlabsSttLabel = "it-elevenlabs-stt-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "elevenlabs",
		Label:      elevenlabsSttLabel,
		APIKey:     cfg.ElevenlabsAPIKey,
		ModelID:    "eleven_flash_v2_5",
		STTModelID: elevenlabsSttModel,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	elevenlabsSttSID = sid
	return nil
}

func teardownElevenlabsSttCredential() {
	if elevenlabsSttSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, elevenlabsSttSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete ElevenLabs STT credential %s: %v", elevenlabsSttSID, err)
	}
}

func teardownElevenlabsCredential() {
	if elevenlabsSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, elevenlabsSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete ElevenLabs credential %s: %v", elevenlabsSID, err)
	}
}

// provisionKugelaudioCredential creates, all with the same key: a TTS-only
// credential labelled `it-kugelaudio-<runID>` on kugel-3, a noise isolation-only
// one labelled `it-<runID>-kugelaudio-ni`, an STT-only one labelled
// `it-<runID>-kugelaudio-stt` on kugel-ear-1, and an unlabelled noise isolation-only
// one for the default lookup. A failed create removes the ones already made.
func provisionKugelaudioCredential() (err error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	defer func() {
		if err != nil {
			teardownKugelaudioCredential()
		}
	}()
	create := func(sid *string, c provision.SpeechCredentialCreate) error {
		c.Vendor, c.APIKey = "kugelaudio", cfg.KugelaudioAPIKey
		var err error
		*sid, err = client.CreateAccountSpeechCredential(ctx, suite.AccountSID, c)
		return err
	}
	kugelaudioLabel = "it-kugelaudio-" + provision.RunID()
	if err = create(&kugelaudioSID, provision.SpeechCredentialCreate{
		Label: kugelaudioLabel, ModelID: "kugel-3", UseForTTS: true,
	}); err != nil {
		return err
	}
	kugelaudioNoiseLabel = provision.Name("kugelaudio-ni")
	if err = create(&kugelaudioNoiseSID, provision.SpeechCredentialCreate{
		Label: kugelaudioNoiseLabel, UseForNoiseIsolation: true,
	}); err != nil {
		return err
	}
	kugelaudioSttLabel = provision.Name("kugelaudio-stt")
	if err = create(&kugelaudioSttSID, provision.SpeechCredentialCreate{
		Label: kugelaudioSttLabel, STTModelID: "kugel-ear-1", UseForSTT: true,
	}); err != nil {
		return err
	}
	return create(&kugelaudioDefaultNoiseSID, provision.SpeechCredentialCreate{UseForNoiseIsolation: true})
}

func teardownKugelaudioCredential() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, sid := range []*string{&kugelaudioSID, &kugelaudioNoiseSID, &kugelaudioSttSID, &kugelaudioDefaultNoiseSID} {
		if *sid == "" {
			continue
		}
		if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, *sid); err != nil {
			log.Printf("tests/verbs: cleanup: delete KugelAudio credential %s: %v", *sid, err)
		}
		*sid = ""
	}
}

// provisionAssemblyaiCredential creates two TTS-only AssemblyAI credentials on
// assemblyai-tts-1 with the same key: `it-assemblyai-<runID>` on the default
// (US) host and `it-assemblyai-eu-<runID>` with the EU api_uri.
func provisionAssemblyaiCredential() error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	create := func(label, apiURI string) (string, error) {
		return client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
			Vendor:    "assemblyai",
			Label:     label,
			APIKey:    cfg.AssemblyaiAPIKey,
			ModelID:   "assemblyai-tts-1",
			APIURI:    apiURI,
			UseForTTS: true,
		})
	}
	var err error
	assemblyaiLabel = "it-assemblyai-" + provision.RunID()
	if assemblyaiSID, err = create(assemblyaiLabel, ""); err != nil {
		return err
	}
	assemblyaiEULabel = "it-assemblyai-eu-" + provision.RunID()
	if assemblyaiEUSID, err = create(assemblyaiEULabel, "tts.eu.assemblyai.com"); err != nil {
		teardownAssemblyaiCredential()
		return err
	}
	return nil
}

func teardownAssemblyaiCredential() {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, sid := range []string{assemblyaiSID, assemblyaiEUSID} {
		if sid == "" {
			continue
		}
		if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, sid); err != nil {
			log.Printf("tests/verbs: cleanup: delete AssemblyAI credential %s: %v", sid, err)
		}
	}
}

// provisionSpeechifyCredential creates a TTS-only Speechify credential,
// labelled `it-speechify-<runID>`.
func provisionSpeechifyCredential() error {
	speechifyLabel = "it-speechify-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "speechify",
		Label:     speechifyLabel,
		APIKey:    cfg.SpeechifyAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	speechifySID = sid
	return nil
}

func teardownSpeechifyCredential() {
	if speechifySID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, speechifySID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Speechify credential %s: %v", speechifySID, err)
	}
}

// provisionSvaraCredential creates a TTS-only Svara credential, labelled
// `it-svara-<runID>`.
func provisionSvaraCredential() error {
	svaraLabel = "it-svara-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "svara",
		Label:     svaraLabel,
		APIKey:    cfg.SvaraAPIKey,
		ModelID:   "svara-tts-turbo",
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	svaraSID = sid
	return nil
}

func teardownSvaraCredential() {
	if svaraSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, svaraSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Svara credential %s: %v", svaraSID, err)
	}
}

// provisionGiggyCredential creates a dual-use (TTS + STT) Giggy credential,
// labelled `it-giggy-<runID>`.
func provisionGiggyCredential() error {
	giggyLabel = "it-giggy-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "giggy",
		Label:     giggyLabel,
		APIKey:    cfg.GiggyAPIKey,
		ModelID:   "giggyspeech",
		UseForTTS: true,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	giggySID = sid
	return nil
}

func teardownGiggyCredential() {
	if giggySID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, giggySID); err != nil {
		log.Printf("tests/verbs: cleanup: delete Giggy credential %s: %v", giggySID, err)
	}
}

// provisionGeminiCredential creates a google speech credential whose default
// STT model is a gemini live model, labelled `it-gemini-<runID>`. The
// service-account JSON satisfies the api-server's google credential contract;
// the api_key is what actually authenticates to the Gemini API, which refuses
// service accounts. STT only — the gemini TTS models are a separate feature.
func provisionGeminiCredential() error {
	geminiLabel = "it-gemini-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "google",
		Label:      geminiLabel,
		ServiceKey: cfg.GeminiServiceKey,
		STTModelID: geminiSttModel,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	// The feature server skips a google credential that has not passed its
	// test, so a provisioned-but-untested one would look like an STT bug.
	if _, err := client.TestAccountSpeechCredential(ctx, suite.AccountSID, sid); err != nil {
		return err
	}
	geminiSID = sid
	return nil
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

// provisionZoomCredential creates an STT-only zoom speech credential under
// the suite account, labelled `it-zoom-<runID>`. Called only when
// ZOOM_API_KEY is set.
func provisionZoomCredential() error {
	zoomLabel = "it-zoom-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "zoom",
		Label:     zoomLabel,
		APIKey:    cfg.ZoomAPIKey,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	zoomSID = sid
	return nil
}

func teardownZoomCredential() {
	if zoomSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, zoomSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete zoom credential %s: %v", zoomSID, err)
	}
}

// provisionGradiumCredential creates an STT-only gradium speech credential under
// the suite account, labelled `it-<runID>-gradium`. Called only when
// GRADIUM_API_KEY is set.
func provisionGradiumCredential() error {
	gradiumLabel = provision.Name("gradium")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "gradium",
		Label:     gradiumLabel,
		APIKey:    cfg.GradiumAPIKey,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	gradiumSID = sid
	return nil
}

func teardownGradiumCredential() {
	if gradiumSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, gradiumSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete gradium credential %s: %v", gradiumSID, err)
	}
}

// provisionMistralCredential creates a mistral speech credential for both STT
// and TTS under the suite account, labelled `it-<runID>-mistral`. Called only
// when MISTRAL_API_KEY is set.
func provisionMistralCredential() error {
	mistralLabel = provision.Name("mistral")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "mistral",
		Label:      mistralLabel,
		APIKey:     cfg.MistralAPIKey,
		ModelID:    mistralTTSModel,
		STTModelID: mistralSTTModel,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	mistralSID = sid
	return nil
}

func teardownMistralCredential() {
	if mistralSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, mistralSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete mistral credential %s: %v", mistralSID, err)
	}
}

// provisionPaxaCredential creates a paxa speech credential for both STT and
// TTS under the suite account, labelled `it-<runID>-paxa`. Called only when
// PAXA_API_KEY is set.
func provisionPaxaCredential() error {
	paxaLabel = provision.Name("paxa")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "paxa",
		Label:      paxaLabel,
		APIKey:     cfg.PaxaAPIKey,
		ModelID:    paxaTTSModel,
		STTModelID: paxaSTTModel,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	paxaSID = sid
	return nil
}

func teardownPaxaCredential() {
	if paxaSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, paxaSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete paxa credential %s: %v", paxaSID, err)
	}
}

// provisionSarvamCredential creates a sarvam speech credential for both STT
// and TTS under the suite account. Called only when SARVAM_API_KEY is set.
func provisionSarvamCredential() error {
	sarvamLabel = provision.Name("sarvam")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "sarvam",
		Label:      sarvamLabel,
		APIKey:     cfg.SarvamAPIKey,
		ModelID:    sarvamTTSModel,
		STTModelID: sarvamSTTModel,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	sarvamSID = sid
	return nil
}

func teardownSarvamCredential() {
	if sarvamSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, sarvamSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete sarvam credential %s: %v", sarvamSID, err)
	}
}

// provisionSmallestaiCredential creates a smallestai speech credential for
// both STT and TTS under the suite account. Called only when SMALLESTAI_API_KEY is set.
func provisionSmallestaiCredential() error {
	smallestaiLabel = provision.Name("smallestai")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "smallestai",
		Label:      smallestaiLabel,
		APIKey:     cfg.SmallestaiAPIKey,
		ModelID:    smallestaiTTSModel,
		STTModelID: smallestaiSTTModel,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	smallestaiSID = sid
	return nil
}

func teardownSmallestaiCredential() {
	if smallestaiSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, smallestaiSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete smallestai credential %s: %v", smallestaiSID, err)
	}
}

// provisionAzureopenaiCredential creates an azureopenai speech credential
// for both STT and TTS under the suite account. Called only when
// AZUREOPENAI_API_KEY and AZUREOPENAI_API_URI are set.
func provisionAzureopenaiCredential() error {
	azureopenaiLabel = provision.Name("azureopenai")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "azureopenai",
		Label:      azureopenaiLabel,
		APIKey:     cfg.AzureopenaiAPIKey,
		APIURI:     cfg.AzureopenaiAPIURI,
		ModelID:    cfg.AzureopenaiTTSDeployment,
		STTModelID: cfg.AzureopenaiSTTDeployment,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	azureopenaiSID = sid
	return nil
}

func teardownAzureopenaiCredential() {
	if azureopenaiSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, azureopenaiSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete azureopenai credential %s: %v", azureopenaiSID, err)
	}
}

// provisionGnaniCredential creates a gnani speech credential for both STT
// and TTS under the suite account. Called only when GNANI_API_KEY is set.
func provisionGnaniCredential() error {
	gnaniLabel = provision.Name("gnani")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "gnani",
		Label:      gnaniLabel,
		APIKey:     cfg.GnaniAPIKey,
		ModelID:    gnaniTTSModel,
		STTModelID: gnaniSTTModel,
		UseForTTS:  true,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	gnaniSID = sid
	return nil
}

func teardownGnaniCredential() {
	if gnaniSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, gnaniSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete gnani credential %s: %v", gnaniSID, err)
	}
}

// provisionMetaCredential creates an STT-only meta speech credential under
// the suite account. Called only when META_API_KEY is set.
func provisionMetaCredential() error {
	metaLabel = provision.Name("meta")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "meta",
		Label:      metaLabel,
		APIKey:     cfg.MetaAPIKey,
		STTModelID: metaSTTModel,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	metaSID = sid
	return nil
}

func teardownMetaCredential() {
	if metaSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, metaSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete meta credential %s: %v", metaSID, err)
	}
}

// provisionMinimaxCredential creates a TTS-only minimax speech credential
// under the suite account. Called only when MINIMAX_API_KEY is set.
func provisionMinimaxCredential() error {
	minimaxLabel = provision.Name("minimax")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "minimax",
		Label:     minimaxLabel,
		APIKey:    cfg.MinimaxAPIKey,
		ModelID:   minimaxTTSModel,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	minimaxSID = sid
	return nil
}

func teardownMinimaxCredential() {
	if minimaxSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, minimaxSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete minimax credential %s: %v", minimaxSID, err)
	}
}

// provisionNeuphonicCredential creates a TTS-only neuphonic speech credential
// under the suite account. Called only when NEUPHONIC_API_KEY is set.
func provisionNeuphonicCredential() error {
	neuphonicLabel = provision.Name("neuphonic")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "neuphonic",
		Label:     neuphonicLabel,
		APIKey:    cfg.NeuphonicAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	neuphonicSID = sid
	return nil
}

func teardownNeuphonicCredential() {
	if neuphonicSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, neuphonicSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete neuphonic credential %s: %v", neuphonicSID, err)
	}
}

// provisionCambCredential creates a TTS-only camb speech credential under the
// suite account. Called only when CAMB_API_KEY is set.
func provisionCambCredential() error {
	cambLabel = provision.Name("camb")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "camb",
		Label:     cambLabel,
		APIKey:    cfg.CambAPIKey,
		ModelID:   "mars-flash",
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	cambSID = sid
	return nil
}

func teardownCambCredential() {
	if cambSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, cambSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete camb credential %s: %v", cambSID, err)
	}
}

// provisionAsyncaiCredential creates a TTS-only asyncai speech credential under
// the suite account. Called only when ASYNCAI_API_KEY is set.
func provisionAsyncaiCredential() error {
	asyncaiLabel = provision.Name("asyncai")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "asyncai",
		Label:     asyncaiLabel,
		APIKey:    cfg.AsyncaiAPIKey,
		ModelID:   "async_flash_v1.5",
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	asyncaiSID = sid
	return nil
}

func teardownAsyncaiCredential() {
	if asyncaiSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, asyncaiSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete asyncai credential %s: %v", asyncaiSID, err)
	}
}

// provisionRespeecherCredential creates a TTS-only respeecher speech credential
// under the suite account. No model_id, so the model follows the language.
func provisionRespeecherCredential() error {
	respeecherLabel = provision.Name("respeecher")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "respeecher",
		Label:     respeecherLabel,
		APIKey:    cfg.RespeecherAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	respeecherSID = sid
	return nil
}

func teardownRespeecherCredential() {
	if respeecherSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, respeecherSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete respeecher credential %s: %v", respeecherSID, err)
	}
}

// provisionBlandCredential creates a TTS-only bland speech credential under
// the suite account. No model: the voice UUID selects it.
func provisionBlandCredential() error {
	blandLabel = provision.Name("bland")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "bland",
		Label:     blandLabel,
		APIKey:    cfg.BlandAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	blandSID = sid
	return nil
}

func teardownBlandCredential() {
	if blandSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, blandSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete bland credential %s: %v", blandSID, err)
	}
}

// provisionSimplismartCredential creates a TTS-only simplismart speech
// credential under the suite account. No model: mediajam picks it by language.
func provisionSimplismartCredential() error {
	simplismartLabel = provision.Name("simplismart")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "simplismart",
		Label:     simplismartLabel,
		APIKey:    cfg.SimplismartAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	simplismartSID = sid
	return nil
}

func teardownSimplismartCredential() {
	if simplismartSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, simplismartSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete simplismart credential %s: %v", simplismartSID, err)
	}
}

// provisionSpitchCredential creates a TTS-only spitch speech credential
// under the suite account (default host: no api_uri).
func provisionSpitchCredential() error {
	spitchLabel = provision.Name("spitch")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "spitch",
		Label:     spitchLabel,
		APIKey:    cfg.SpitchAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	spitchSID = sid
	return nil
}

func teardownSpitchCredential() {
	if spitchSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, spitchSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete spitch credential %s: %v", spitchSID, err)
	}
}

// provisionKokoroCredential creates a TTS-only kokoro speech credential
// under the suite account; api_uri is required, api_key only when set.
func provisionKokoroCredential() error {
	kokoroLabel = provision.Name("kokoro")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "kokoro",
		Label:     kokoroLabel,
		APIURI:    cfg.KokoroAPIURI,
		APIKey:    cfg.KokoroAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	kokoroSID = sid
	return nil
}

func teardownKokoroCredential() {
	if kokoroSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, kokoroSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete kokoro credential %s: %v", kokoroSID, err)
	}
}

// provisionVakyamCredential creates a TTS-only vakyam speech credential
// under the suite account (default host: no api_uri).
func provisionVakyamCredential() error {
	vakyamLabel = provision.Name("vakyam")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "vakyam",
		Label:     vakyamLabel,
		APIKey:    cfg.VakyamAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	vakyamSID = sid
	return nil
}

func teardownVakyamCredential() {
	if vakyamSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, vakyamSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete vakyam credential %s: %v", vakyamSID, err)
	}
}

// provisionGroqCredential creates a TTS-only groq speech credential
// under the suite account (default host: no api_uri).
func provisionGroqCredential() error {
	groqLabel = provision.Name("groq")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "groq",
		Label:     groqLabel,
		APIKey:    cfg.GroqAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	groqSID = sid
	return nil
}

func teardownGroqCredential() {
	if groqSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, groqSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete groq credential %s: %v", groqSID, err)
	}
}

// provisionHumeCredential creates a TTS-only hume speech credential
// under the suite account (default host, no model_id: Hume auto-routes).
func provisionHumeCredential() error {
	humeLabel = provision.Name("hume")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "hume",
		Label:     humeLabel,
		APIKey:    cfg.HumeAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		return err
	}
	humeSID = sid
	return nil
}

func teardownHumeCredential() {
	if humeSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, humeSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete hume credential %s: %v", humeSID, err)
	}
}

// provisionInworldCredential creates an STT-only inworld speech credential
// with no model_id, which the api-server must accept when use_for_tts is off.
func provisionInworldCredential() error {
	inworldLabel = provision.Name("inworld")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:     "inworld",
		Label:      inworldLabel,
		APIKey:     cfg.InworldAPIKey,
		STTModelID: inworldSTTModel,
		UseForSTT:  true,
	})
	if err != nil {
		return err
	}
	inworldSID = sid
	return nil
}

func teardownInworldCredential() {
	if inworldSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, inworldSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete inworld credential %s: %v", inworldSID, err)
	}
}

// provisionSonioxCredential creates a soniox speech credential for both STT
// and TTS under the suite account. Called only when SONIOX_API_KEY is set.
func provisionSonioxCredential() error {
	sonioxLabel = provision.Name("soniox")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "soniox",
		Label:     sonioxLabel,
		APIKey:    cfg.SonioxAPIKey,
		ModelID:   sonioxTTSModel,
		UseForTTS: true,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	sonioxSID = sid
	return nil
}

func teardownSonioxCredential() {
	if sonioxSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, sonioxSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete soniox credential %s: %v", sonioxSID, err)
	}
}

// provisionSaidwireCredential creates an STT-only saidwire speech credential
// under the suite account, labelled `it-<runID>-saidwire`. Called only when
// SAIDWIRE_API_KEY is set.
func provisionSaidwireCredential() error {
	saidwireLabel = provision.Name("saidwire")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "saidwire",
		Label:     saidwireLabel,
		APIKey:    cfg.SaidwireAPIKey,
		UseForSTT: true,
	})
	if err != nil {
		return err
	}
	saidwireSID = sid
	return nil
}

func teardownSaidwireCredential() {
	if saidwireSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, saidwireSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete saidwire credential %s: %v", saidwireSID, err)
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

// provisionSpeechmaticsTtsCredential creates a TTS-only speechmatics
// credential without speechmatics_stt_uri (default TTS host).
func provisionSpeechmaticsTtsCredential() error {
	speechmaticsTtsLabel = "it-speechmatics-tts-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor:    "speechmatics",
		Label:     speechmaticsTtsLabel,
		APIKey:    cfg.SpeechmaticsAPIKey,
		UseForTTS: true,
	})
	if err != nil {
		speechmaticsTtsLabel = ""
		return err
	}
	speechmaticsTtsSID = sid
	return nil
}

func teardownSpeechmaticsTtsCredential() {
	if speechmaticsTtsSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, speechmaticsTtsSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete speechmatics TTS credential %s: %v", speechmaticsTtsSID, err)
	}
}

// provisionSpeechmaticsAgentCredential creates a speechmaticsagent
// (Speechmatics Agent STT) speech credential under the suite account,
// labelled `it-speechmaticsagent-<runID>`. STT-only. speechmatics_stt_uri is
// optional for this vendor — unset means the global endpoint — and a verb can
// still override it per call via recognizer.speechmaticsOptions.host.
// Called only when SPEECHMATICS_AGENT_API_KEY is set.
func provisionSpeechmaticsAgentCredential() error {
	speechmaticsAgentLabel = "it-speechmaticsagent-" + provision.RunID()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	sid, err := client.CreateAccountSpeechCredential(ctx, suite.AccountSID, provision.SpeechCredentialCreate{
		Vendor: "speechmaticsagent",
		Label:  speechmaticsAgentLabel,
		APIKey: cfg.SpeechmaticsAgentAPIKey,
		// optional here, unlike the classic vendor: unset means the global
		// endpoint, which routes to the nearest region
		SpeechmaticsSTTURI: cfg.SpeechmaticsAgentHost,
		UseForSTT:          true,
	})
	if err != nil {
		return err
	}
	speechmaticsAgentSID = sid
	return nil
}

func teardownSpeechmaticsAgentCredential() {
	if speechmaticsAgentSID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := client.DeleteAccountSpeechCredential(ctx, suite.AccountSID, speechmaticsAgentSID); err != nil {
		log.Printf("tests/verbs: cleanup: delete speechmaticsagent credential %s: %v", speechmaticsAgentSID, err)
	}
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
