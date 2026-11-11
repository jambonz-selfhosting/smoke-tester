// Package config loads harness settings from .env + process environment into a
// typed Settings struct. Loaded once per process in TestMain — see ADR-0009.
//
// The harness is fully self-provisioning: TestMain creates an ephemeral
// account under the configured Service Provider, mints an API key for it,
// and provisions a synthetic sip_realm whose DNS is mapped locally to the
// SBC public IP via a custom *net.Resolver. There are no long-lived
// account-scope credentials in env any more.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Settings struct {
	// Required — jambonz target
	APIBaseURL string

	// Required — Service-provider scope. Every test resource is created
	// under an ephemeral account beneath this SP. Both fields are
	// mandatory; the long-lived JAMBONZ_API_KEY / JAMBONZ_ACCOUNT_SID env
	// vars from older revisions of the harness are gone.
	SPAPIKey string
	SPSID    string

	// AllowSPPolicyChanges lets tests change the SP's support audio capture
	// policy. allow_all overrides every account's opt-out, so leave it off on
	// a shared cluster with customer accounts under the SP.
	AllowSPPolicyChanges bool

	// Required — SBC public IP. Verb tests dial this IP for all SIP
	// traffic regardless of the Request-URI domain. The harness installs
	// a custom DNS resolver in the SIP transport so the synthetic
	// `<account>.<SIPRealmZone>` domain resolves to this IP locally
	// without needing real DNS records.
	SBCPublicIP net.IP

	// Optional — SIP realm zone suffix appended to the per-suite account
	// name to form the account's sip_realm. Default "smoke.test" — must
	// have at least one dot so the upstream `(.*)\.(.*\..*)$` regex on
	// POST /SipRealms accepts the full realm.
	SIPRealmZone string

	// Required — Deepgram API key. Used for two purposes:
	//   1. Provisioning a SpeechCredential under the test's ephemeral
	//      account at TestMain (label `it-deepgram-<runID>`). Verb tests
	//      reference this label as `synthesizer.label` / `recognizer.label`.
	//   2. Offline transcript verification — internal/stt/ uploads PCM
	//      recordings to Deepgram and asserts the transcript matches.
	DeepgramAPIKey string

	// Required — Deepseek LLM API key. Passed inline as
	// `agent.llm.auth.apiKey` in the agent verb test, bypassing
	// /LlmCredentials provisioning (feature-server honors inline auth via
	// lib/tasks/agent/index.js:446).
	DeepseekAPIKey string

	// Optional — OpenAI API key. Passed inline as `llm.auth.apiKey` /
	// `agent.llm.auth.apiKey` in the transfer-handoff tests. Optional: tests
	// that need it skip when unset (see HasOpenAI), so the broader suite still
	// runs without it.
	OpenAIAPIKey string

	// Optional — Murf.ai TTS API key. When set, TestMain provisions a Murf
	// SpeechCredential under the ephemeral account and the Murf say test
	// exercises it. When unset the Murf test skips (passes) with a
	// credential-missing log — see HasMurf.
	MurfAPIKey string

	// Optional — ElevenLabs API key for the eleven_v3 (Text to Dialogue)
	// streaming tests. Unset = those tests skip — see HasElevenlabs.
	ElevenlabsAPIKey string

	// Optional — KugelAudio TTS API key. Unset = the kugelaudio tests skip.
	KugelaudioAPIKey string
	// Optional — set when the media server has an ai-coustics license
	// (AIC_SDK_LICENSE on mediajam). Unset = aicoustics tests skip.
	AicousticsEnabled bool

	// Optional — AssemblyAI API key for the TTS tests. Unset = they skip.
	AssemblyaiAPIKey string

	// Optional — Speechify TTS API key. Unset = the speechify tests skip.
	SpeechifyAPIKey string

	// Optional — Svara TTS API key. Unset = the svara tests skip.
	SvaraAPIKey string
	// Svara voice ids; default to an American English and a Hindi library voice.
	SvaraVoiceID      string
	SvaraHindiVoiceID string

	// Optional — Giggy API key (TTS + STT). Unset = the giggy tests skip.
	GiggyAPIKey string
	// Giggy voice UUID; defaults to an English (US General) public voice.
	GiggyVoiceID string

	// Optional — xAI STT API key. When set, TestMain provisions an xai
	// SpeechCredential under the ephemeral account and the xai gather/
	// transcribe tests exercise it. When unset those tests pass without
	// exercising xai STT — see HasXai.
	XaiAPIKey string

	// Optional — Zoom Build platform API key (Scribe Live mode STT). When set,
	// TestMain provisions a zoom SpeechCredential and the zoom gather/transcribe
	// tests exercise it; unset, they pass without exercising zoom — see HasZoom.
	ZoomAPIKey string

	// Optional — Gradium API key (streaming STT). When set, TestMain provisions
	// a gradium SpeechCredential and the gradium gather/transcribe/agent tests
	// exercise it; unset, they pass without exercising gradium — see HasGradium.
	GradiumAPIKey string

	// Optional — Mistral API key (Voxtral 3 realtime STT and TTS). When set,
	// TestMain provisions a mistral SpeechCredential for both; unset, the
	// mistral tests pass without exercising it — see HasMistral.
	MistralAPIKey string

	// Optional — Paxa Labs API key (TTS and realtime STT). When set, TestMain
	// provisions a paxa SpeechCredential for both; unset, the paxa tests pass
	// without exercising it — see HasPaxa.
	PaxaAPIKey string

	// Optional — Sarvam AI API key (TTS and realtime STT). Unset = the sarvam
	// tests pass without exercising it — see HasSarvam.
	SarvamAPIKey string

	// Optional — Smallest AI API key (Lightning TTS and Pulse STT). Unset =
	// the smallestai tests pass without exercising it — see HasSmallestai.
	SmallestaiAPIKey string

	// Optional — Azure OpenAI resource key and endpoint (realtime STT and
	// /audio/speech TTS). Both are needed — see HasAzureopenai. The
	// deployment names default to the model names.
	AzureopenaiAPIKey        string
	AzureopenaiAPIURI        string
	AzureopenaiSTTDeployment string
	AzureopenaiTTSDeployment string

	// Optional — Gnani (Vachana) API key (STT and TTS). Unset = the gnani
	// tests pass without exercising it — see HasGnani.
	GnaniAPIKey string

	// Optional — Meta Model API key (Muse Voice Transcribe, STT only). Unset =
	// the meta tests pass without exercising it — see HasMeta.
	MetaAPIKey string

	// Optional — MiniMax TTS API key. Unset = the minimax tests pass without
	// exercising it — see HasMinimax.
	MinimaxAPIKey string
	// MiniMax system voice id; defaults to the en-US default voice.
	MinimaxVoiceID string

	// Optional — Neuphonic TTS API key. Unset = the neuphonic tests pass
	// without exercising it — see HasNeuphonic.
	NeuphonicAPIKey string
	// Neuphonic voice id; defaults to the en-US stock voice (Hannah).
	NeuphonicVoiceID string

	// Optional — Camb.ai TTS API key. Unset = the camb tests pass without
	// exercising it — see HasCamb.
	CambAPIKey string
	// Camb numeric voice id; defaults to the documented default voice.
	CambVoiceID string

	// Optional — Async (asyncai) TTS API key. Unset = the asyncai tests pass
	// without exercising it — see HasAsyncai.
	AsyncaiAPIKey string
	// Async voice id; defaults to the only documented voice.
	AsyncaiVoiceID string

	// Optional — Respeecher TTS API key. Unset = the respeecher tests pass
	// without exercising it — see HasRespeecher.
	RespeecherAPIKey string
	// Respeecher voice id for the en-rt model; defaults to samantha.
	RespeecherVoiceID string

	// Optional — Bland AI TTS API key. Unset = the bland tests pass
	// without exercising it — see HasBland.
	BlandAPIKey string
	// Bland voice UUID; defaults to Karen.
	BlandVoiceID string

	// Optional — Simplismart TTS API key. Unset = the simplismart tests pass
	// without exercising it — see HasSimplismart.
	SimplismartAPIKey string
	// Simplismart Orpheus voice; defaults to tara.
	SimplismartVoiceID string

	// Optional — Spitch TTS API key. Unset = the spitch tests pass without
	// exercising it — see HasSpitch.
	SpitchAPIKey string
	// Spitch voice; defaults to lina (English).
	SpitchVoiceID string

	// Optional — self-hosted Kokoro-FastAPI base URL (required by the vendor).
	// Unset = the kokoro tests pass without exercising it — see HasKokoro.
	KokoroAPIURI string
	// Optional — only when the Kokoro server sits behind a bearer token.
	KokoroAPIKey string
	// Kokoro voice; defaults to af_heart (en-US).
	KokoroVoiceID string

	// Optional — Vakyam TTS API key. Unset = the vakyam tests pass without
	// exercising it — see HasVakyam.
	VakyamAPIKey string
	// Vakyam voice; defaults to Archana.
	VakyamVoiceID string

	// Optional — Groq TTS (Orpheus) API key. Unset = the groq tests pass
	// without exercising it — see HasGroq.
	GroqAPIKey string
	// Groq voice; defaults to autumn (en-US).
	GroqVoiceID string

	// Optional — Hume TTS (Octave) API key. Unset = the hume tests pass
	// without exercising it — see HasHume.
	HumeAPIKey string
	// Hume voice name or UUID; defaults to Ava Song.
	HumeVoiceID string

	// Optional — Inworld Portal Base64 Basic credential (realtime STT). Unset =
	// the inworld tests pass without exercising it — see HasInworld.
	InworldAPIKey string

	// Optional — Soniox API key (realtime TTS; the same key serves STT). Unset =
	// the soniox tests pass without exercising it — see HasSoniox.
	SonioxAPIKey string

	// Optional — Saidwire API key (realtime STT). Unset = the saidwire tests
	// pass without exercising it — see HasSaidwire.
	SaidwireAPIKey string

	// Optional — service-account JSON (read from GEMINI_KEYFILE) for the gemini
	// live transcription models, which authenticate as a service account on
	// Vertex AI. Needs roles/aiplatform.user, which the dialogflow key does not
	// carry, so it is a separate variable.
	GeminiServiceKey string

	// Optional — Gemini Developer API key ("AIza..."), for the google s2s
	// (Gemini Live) tests. NOT interchangeable with GeminiServiceKey: Google
	// refuses service accounts on the Developer API ("Access to Gemini API is
	// restricted with service accounts"), and Vertex refuses API keys. Hence a
	// separate key; the Vertex s2s test uses GeminiServiceKey instead.
	GeminiAPIKey string

	// Optional — OpenAI GPT-Live API key, kept separate from OpenAIAPIKey so
	// the gptlive tests run only when explicitly asked for. When unset they
	// pass without exercising GPT-Live — see HasGptLive.
	GptLiveAPIKey string

	// Optional — Agent Studio for tests/studio (make test-studio). The workspace's
	// session cookie comes from StudioSIDFile, or one sign-in with the email and
	// password. StudioAccountID empty: the login's first owner/admin workspace.
	StudioAPIURL    string
	StudioAccountID string
	StudioSIDFile   string
	StudioEmail     string
	StudioPassword  string

	// Optional — GPT-Live model/host/path overrides. Empty host/path means the
	// feature-server default (api.openai.com, v1/live/sessions).
	GptLiveModel string
	GptLiveHost  string
	GptLivePath  string

	// Optional — the Responses-side model a `responses` delegation runs its
	// turns on (delegation.responses.model, required by the server). Distinct
	// from GptLiveModel, which is the GPT-Live voice model.
	GptLiveDelegationModel string

	// Optional — Azure Voice Live. The host names a Microsoft Foundry or Azure
	// Speech resource and has no default, so BOTH the key and the host must be
	// set for the voicelive tests to run — see HasVoiceLive. VoiceLiveVoice is
	// an Azure TTS voice name, not an OpenAI one.
	VoiceLiveAPIKey string
	VoiceLiveHost   string
	VoiceLiveModel  string
	VoiceLiveVoice  string

	// Optional — Amazon Nova Sonic on Bedrock. Static IAM credentials (a
	// session token too for STS creds) — see HasNova. NovaRegion must be a
	// region serving Nova Sonic.
	NovaAccessKeyID     string
	NovaSecretAccessKey string
	NovaSessionToken    string
	NovaRegion          string
	NovaModel           string
	NovaVoice           string

	// Optional — Speechmatics STT API key. When set, TestMain provisions a
	// speechmatics SpeechCredential under the ephemeral account and the
	// speechmatics gather/transcribe tests exercise it (model selection +
	// transcript_filtering_config). When unset those tests pass without
	// exercising speechmatics STT — see HasSpeechmatics.
	SpeechmaticsAPIKey string

	// Optional — Speechmatics realtime host for the provisioned credential
	// (the API requires speechmatics_stt_uri). Defaults to
	// "eu2.rt.speechmatics.com" when SPEECHMATICS_STT_URI is unset.
	SpeechmaticsSTTURI string

	// Optional — Speechmatics Agent STT ("speechmaticsagent") API key. A
	// separate credential from SpeechmaticsAPIKey: a different vendor, a
	// different endpoint (/v2/agent/<profile>), and the key may be entitled
	// to one and not the other. When set, TestMain provisions a
	// speechmaticsagent SpeechCredential and the agent-STT gather/
	// transcribe tests exercise it — see HasSpeechmaticsAgent.
	SpeechmaticsAgentAPIKey string

	// Optional — the Agent STT turn-taking profile the tests request
	// (recognizer.speechmaticsOptions.profile). Defaults to "smart".
	SpeechmaticsAgentProfile string

	// Optional — Agent STT host override (recognizer.speechmaticsOptions.host).
	// Empty leaves the media server on its own default, which is where the
	// Agent API currently lives; set it to move the tests onto the production
	// endpoints without a rebuild.
	SpeechmaticsAgentHost string

	// Optional — Google Dialogflow. When DialogflowServiceKey is set (the
	// full service-account JSON, read from DIALOGFLOW_KEYFILE), the
	// dialogflow verb test provisions nothing via /SpeechCredentials —
	// jambonz's dialogflow verb takes the credentials inline — and drives a
	// real CX agent. When unset the test skips (passes) with a
	// credential-missing log. See HasDialogflow.
	//
	// DialogflowServiceKey is the raw JSON string (not a path); the loader
	// reads it from the file named by DIALOGFLOW_KEYFILE.
	DialogflowServiceKey string
	DialogflowProject    string // GCP project id (e.g. "drachtio-cpaas")
	DialogflowAgent      string // CX agent id (uuid)
	DialogflowRegion     string // GCP region (e.g. "us-central1")
	DialogflowLang       string // language code; default "en-US"

	// Optional — Conversational Agents (CES, model "ces") coordinates. A CES
	// app is a DIFFERENT resource from a CX agent, so it cannot be derived
	// from DialogflowAgent: the CES test needs its own app id and reuses
	// DialogflowServiceKey / DialogflowProject / DialogflowRegion. The app must
	// expose a client-side (Function) tool WITH A NON-EMPTY DESCRIPTION — a
	// tool imported with an empty description is never offered to the model,
	// which presents as a silent stall rather than an error. See HasDialogflowCES.
	DialogflowCESApp        string // CES app id
	DialogflowCESDeployment string // optional CES deployment id; "" => none

	// CES location. NOT the same as DialogflowRegion: a CES app commonly lives
	// in a MULTI-REGION ("us") while a CX agent lives in a region
	// ("us-central1"), so the two cannot share one value. Defaults to "us".
	DialogflowCESLocation string

	// Required — ngrok auth token. Phase-2 verb tests + Phase-1 status
	// callbacks both need a public URL forwarded to the local webhook
	// server. The whole verb suite gates on this.
	NgrokAuthToken string

	// Optional — reserved ngrok subdomain (paid tier). When set the
	// public URL is stable across runs.
	NgrokDomain string

	// Test-run knobs
	RunID          string
	LogLevel       string // "info" | "debug"
	OrphanTTL      time.Duration
	ContractStrict bool // ADR-0015: schema violations fail tests

	// Per-leg call recording (ADR-0016). When RecordLegs is true, every
	// recorded call leg is additionally archived as a playable WAV under
	// RecordDir/<test-name>/<role>.wav so a developer can hear each leg
	// after a run. Off by default (release-gate posture, ADR-0010).
	RecordLegs bool
	RecordDir  string // base dir for WAV archives; default "./recordings"
}

// HasDeepgram reports whether Deepgram-backed flows can run. With the
// new self-provisioning model both keys are mandatory at TestMain, so
// these helpers are kept only for the rare callers that still want to
// guard for diagnostic logs.
func (s *Settings) HasDeepgram() bool { return s.DeepgramAPIKey != "" }

// HasDeepseek reports whether the Deepseek-backed agent verb test can run.
func (s *Settings) HasDeepseek() bool { return s.DeepseekAPIKey != "" }

func (s *Settings) HasOpenAI() bool { return s.OpenAIAPIKey != "" }

// HasMurf reports whether the Murf.ai TTS say test can run. Optional: when
// the key is unset the test skips (passes) with a credential-missing log.
func (s *Settings) HasMurf() bool { return s.MurfAPIKey != "" }

// HasElevenlabs reports whether the ElevenLabs TTS tests can run.
func (s *Settings) HasElevenlabs() bool { return s.ElevenlabsAPIKey != "" }

// HasKugelaudio reports whether the KugelAudio TTS tests can run.
func (s *Settings) HasKugelaudio() bool { return s.KugelaudioAPIKey != "" }

// HasAicoustics reports whether the media server can run ai-coustics noise isolation.
func (s *Settings) HasAicoustics() bool { return s.AicousticsEnabled }

// HasAssemblyai reports whether the AssemblyAI TTS tests can run.
func (s *Settings) HasAssemblyai() bool { return s.AssemblyaiAPIKey != "" }

// HasSpeechify reports whether the Speechify TTS tests can run.
func (s *Settings) HasSpeechify() bool { return s.SpeechifyAPIKey != "" }

// HasSvara reports whether the Svara TTS tests can run.
func (s *Settings) HasSvara() bool { return s.SvaraAPIKey != "" }

// HasGiggy reports whether the Giggy TTS and STT tests can run.
func (s *Settings) HasGiggy() bool { return s.GiggyAPIKey != "" }

// HasXai reports whether the xai STT gather/transcribe tests can run.
// Optional: when the key is unset those tests pass without exercising xai.
func (s *Settings) HasXai() bool { return s.XaiAPIKey != "" }

// HasZoom reports whether the zoom STT gather/transcribe tests can run.
// Optional: when the key is unset those tests pass without exercising zoom.
func (s *Settings) HasZoom() bool { return s.ZoomAPIKey != "" }

// HasGradium reports whether the gradium STT gather/transcribe tests can run.
// Optional: when the key is unset those tests pass without exercising gradium.
func (s *Settings) HasGradium() bool { return s.GradiumAPIKey != "" }

// HasMistral reports whether the mistral (Voxtral) STT and TTS tests can run.
// Optional: when the key is unset those tests pass without exercising mistral.
func (s *Settings) HasMistral() bool { return s.MistralAPIKey != "" }

// HasPaxa reports whether the paxa STT and TTS tests can run.
// Optional: when the key is unset those tests pass without exercising paxa.
func (s *Settings) HasPaxa() bool { return s.PaxaAPIKey != "" }

// HasSarvam reports whether the sarvam STT and TTS tests can run.
// Optional: when the key is unset those tests pass without exercising sarvam.
func (s *Settings) HasSarvam() bool { return s.SarvamAPIKey != "" }

// HasSmallestai reports whether the smallestai STT and TTS tests can run.
// Optional: when the key is unset those tests pass without exercising smallestai.
func (s *Settings) HasSmallestai() bool { return s.SmallestaiAPIKey != "" }

// HasAzureopenai reports whether the azureopenai STT and TTS tests can run.
// Optional: unless both key and endpoint are set those tests pass without exercising it.
func (s *Settings) HasAzureopenai() bool {
	return s.AzureopenaiAPIKey != "" && s.AzureopenaiAPIURI != ""
}

// HasGnani reports whether the gnani STT and TTS tests can run.
// Optional: when the key is unset those tests pass without exercising gnani.
func (s *Settings) HasGnani() bool { return s.GnaniAPIKey != "" }

// HasMeta reports whether the meta STT tests can run.
// Optional: when the key is unset those tests pass without exercising meta.
func (s *Settings) HasMeta() bool { return s.MetaAPIKey != "" }

// HasMinimax reports whether the minimax TTS tests can run.
// Optional: when the key is unset those tests pass without exercising minimax.
func (s *Settings) HasMinimax() bool { return s.MinimaxAPIKey != "" }

// HasNeuphonic reports whether the neuphonic TTS tests can run.
// Optional: when the key is unset those tests pass without exercising neuphonic.
func (s *Settings) HasNeuphonic() bool { return s.NeuphonicAPIKey != "" }

// HasCamb reports whether the camb TTS tests can run.
// Optional: when the key is unset those tests pass without exercising camb.
func (s *Settings) HasCamb() bool { return s.CambAPIKey != "" }

// HasAsyncai reports whether the asyncai TTS tests can run.
// Optional: when the key is unset those tests pass without exercising asyncai.
func (s *Settings) HasAsyncai() bool { return s.AsyncaiAPIKey != "" }

// HasRespeecher reports whether the respeecher TTS tests can run.
// Optional: when the key is unset those tests pass without exercising respeecher.
func (s *Settings) HasRespeecher() bool { return s.RespeecherAPIKey != "" }

// HasBland reports whether the bland TTS tests can run.
// Optional: when the key is unset those tests pass without exercising bland.
func (s *Settings) HasBland() bool { return s.BlandAPIKey != "" }

// HasSimplismart reports whether the simplismart TTS tests can run.
// Optional: when the key is unset those tests pass without exercising simplismart.
func (s *Settings) HasSimplismart() bool { return s.SimplismartAPIKey != "" }

// HasSpitch reports whether the spitch TTS tests can run.
// Optional: when the key is unset those tests pass without exercising spitch.
func (s *Settings) HasSpitch() bool { return s.SpitchAPIKey != "" }

// HasKokoro reports whether the kokoro TTS tests can run. Gated on the
// server URL: Kokoro is self-hosted and the api key is optional.
func (s *Settings) HasKokoro() bool { return s.KokoroAPIURI != "" }

// HasVakyam reports whether the vakyam TTS tests can run.
// Optional: when the key is unset those tests pass without exercising vakyam.
func (s *Settings) HasVakyam() bool { return s.VakyamAPIKey != "" }

// HasGroq reports whether the groq TTS tests can run.
// Optional: when the key is unset those tests pass without exercising groq.
func (s *Settings) HasGroq() bool { return s.GroqAPIKey != "" }

// HasHume reports whether the hume TTS tests can run.
// Optional: when the key is unset those tests pass without exercising hume.
func (s *Settings) HasHume() bool { return s.HumeAPIKey != "" }

// HasInworld reports whether the inworld STT tests can run.
// Optional: when the key is unset those tests pass without exercising inworld.
func (s *Settings) HasInworld() bool { return s.InworldAPIKey != "" }

// HasSoniox reports whether the soniox TTS tests can run.
// Optional: when the key is unset those tests pass without exercising soniox.
func (s *Settings) HasSoniox() bool { return s.SonioxAPIKey != "" }

// HasSaidwire reports whether the saidwire STT tests can run.
// Optional: when the key is unset those tests pass without exercising saidwire.
func (s *Settings) HasSaidwire() bool { return s.SaidwireAPIKey != "" }

// HasGeminiStt reports whether the google/gemini STT gather/transcribe tests
// can run. Optional: when the key is unset those tests pass without
// exercising gemini.
// HasGeminiStt reports whether the gemini gather/transcribe tests can run.
// Optional: when GEMINI_KEYFILE is unset those tests pass without exercising
// gemini. The dialogflow key is NOT a fallback — it lacks aiplatform access.
func (s *Settings) HasGeminiStt() bool { return s.GeminiServiceKey != "" }

// HasGeminiS2S reports whether the google (Gemini Live) S2S tests can run.
// Optional: when GEMINI_API_KEY is unset those tests pass without exercising
// Gemini Live. GeminiServiceKey is deliberately NOT a fallback — see the
// GeminiAPIKey field comment.
func (s *Settings) HasGeminiS2S() bool { return s.GeminiAPIKey != "" }

// HasVoiceLive reports whether the Azure Voice Live S2S tests can run.
// Optional, and needs BOTH values: unlike every other s2s vendor the endpoint
// is resource-specific, so there is no host to fall back on.
func (s *Settings) HasVoiceLive() bool { return s.VoiceLiveAPIKey != "" && s.VoiceLiveHost != "" }

// HasNova reports whether the Amazon Nova Sonic S2S tests can run.
// Optional: when the IAM keys are unset those tests pass without exercising nova.
func (s *Settings) HasNova() bool { return s.NovaAccessKeyID != "" && s.NovaSecretAccessKey != "" }

// HasGptLive reports whether the OpenAI GPT-Live S2S tests can run.
// Optional: when the key is unset those tests pass without exercising gptlive.
func (s *Settings) HasGptLive() bool { return s.GptLiveAPIKey != "" }

// HasStudio reports whether tests/studio can reach Agent Studio: its API and a way to sign in.
func (s *Settings) HasStudio() bool {
	return s.StudioAPIURL != "" && (s.StudioSIDFile != "" || (s.StudioEmail != "" && s.StudioPassword != ""))
}

// HasSpeechmatics reports whether the speechmatics STT gather/transcribe
// tests can run. Optional: when the key is unset those tests pass without
// exercising speechmatics.
func (s *Settings) HasSpeechmatics() bool { return s.SpeechmaticsAPIKey != "" }

// HasSpeechmaticsAgent reports whether the Speechmatics Agent STT
// gather/transcribe tests can run. Optional: when the key is unset those
// tests pass without exercising the agent API.
func (s *Settings) HasSpeechmaticsAgent() bool { return s.SpeechmaticsAgentAPIKey != "" }

// HasDialogflow reports whether the Google Dialogflow verb test can run. It
// needs the service-account JSON plus the CX agent coordinates (project +
// agent + region). Optional: when any is unset the test skips (passes) with
// a credential-missing log.
func (s *Settings) HasDialogflow() bool {
	return s.DialogflowServiceKey != "" && s.DialogflowProject != "" &&
		s.DialogflowAgent != "" && s.DialogflowRegion != ""
}

// HasDialogflowCES reports whether the Conversational Agents (CES) verb test
// can run. It needs the service-account JSON plus the CES app coordinates
// (project + app + region); DIALOGFLOW_AGENT is a CX agent and is NOT a
// substitute. Optional: when any is unset the test skips (passes) with a
// credential-missing log.
func (s *Settings) HasDialogflowCES() bool {
	return s.DialogflowServiceKey != "" && s.DialogflowProject != "" &&
		s.DialogflowCESApp != "" && s.DialogflowCESLocation != ""
}

var (
	loadOnce sync.Once
	loaded   *Settings
	loadErr  error
)

// Load reads .env then process env, returning the parsed Settings. Safe to call
// many times; subsequent calls return the first result.
func Load() (*Settings, error) {
	loadOnce.Do(func() {
		if p, ok := findDotEnv(); ok {
			_ = loadDotEnv(p)
		}
		loaded, loadErr = parse()
	})
	return loaded, loadErr
}

// findDotEnv walks up from CWD looking for a .env file. Returns the first
// match or (_, false). This makes `go test ./tests/rest/...` pick up the
// repo-root .env without the test package knowing about it.
func findDotEnv() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(dir, ".env")
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// MustLoad panics on error — intended for TestMain where failure is fatal.
func MustLoad() *Settings {
	s, err := Load()
	if err != nil {
		panic(fmt.Sprintf("config: %v", err))
	}
	return s
}

func parse() (*Settings, error) {
	s := &Settings{
		APIBaseURL:               strings.TrimRight(os.Getenv("JAMBONZ_API_URL"), "/"),
		SPAPIKey:                 os.Getenv("JAMBONZ_SP_API_KEY"),
		SPSID:                    os.Getenv("JAMBONZ_SP_SID"),
		SIPRealmZone:             firstNonEmpty(os.Getenv("JAMBONZ_SIP_REALM_ZONE"), "smoke.test"),
		DeepgramAPIKey:           os.Getenv("DEEPGRAM_API_KEY"),
		DeepseekAPIKey:           os.Getenv("DEEPSEEK_API_KEY"),
		OpenAIAPIKey:             os.Getenv("OPENAI_API_KEY"),
		MurfAPIKey:               os.Getenv("MURF_API_KEY"),
		ElevenlabsAPIKey:         os.Getenv("ELEVENLABS_API_KEY"),
		KugelaudioAPIKey:         os.Getenv("KUGELAUDIO_API_KEY"),
		AssemblyaiAPIKey:         os.Getenv("ASSEMBLYAI_API_KEY"),
		SpeechifyAPIKey:          os.Getenv("SPEECHIFY_API_KEY"),
		SvaraAPIKey:              os.Getenv("SVARA_API_KEY"),
		SvaraVoiceID:             firstNonEmpty(os.Getenv("SVARA_VOICE_ID"), "sv_a5thzx8v"),
		SvaraHindiVoiceID:        firstNonEmpty(os.Getenv("SVARA_HINDI_VOICE_ID"), "sv_fhn6tfve"),
		GiggyAPIKey:              os.Getenv("GIGGY_API_KEY"),
		GiggyVoiceID:             firstNonEmpty(os.Getenv("GIGGY_VOICE_ID"), "8f5aee7d-dd96-41c4-b04f-19bff046b02a"),
		XaiAPIKey:                os.Getenv("XAI_API_KEY"),
		ZoomAPIKey:               os.Getenv("ZOOM_API_KEY"),
		GradiumAPIKey:            os.Getenv("GRADIUM_API_KEY"),
		MistralAPIKey:            os.Getenv("MISTRAL_API_KEY"),
		PaxaAPIKey:               os.Getenv("PAXA_API_KEY"),
		SarvamAPIKey:             os.Getenv("SARVAM_API_KEY"),
		SmallestaiAPIKey:         os.Getenv("SMALLESTAI_API_KEY"),
		AzureopenaiAPIKey:        os.Getenv("AZUREOPENAI_API_KEY"),
		AzureopenaiAPIURI:        os.Getenv("AZUREOPENAI_API_URI"),
		AzureopenaiSTTDeployment: firstNonEmpty(os.Getenv("AZUREOPENAI_STT_DEPLOYMENT"), "gpt-live-transcribe"),
		AzureopenaiTTSDeployment: firstNonEmpty(os.Getenv("AZUREOPENAI_TTS_DEPLOYMENT"), "gpt-4o-mini-tts"),
		GnaniAPIKey:              os.Getenv("GNANI_API_KEY"),
		MetaAPIKey:               os.Getenv("META_API_KEY"),
		MinimaxAPIKey:            os.Getenv("MINIMAX_API_KEY"),
		MinimaxVoiceID:           firstNonEmpty(os.Getenv("MINIMAX_VOICE_ID"), "English_expressive_narrator"),
		NeuphonicAPIKey:          os.Getenv("NEUPHONIC_API_KEY"),
		NeuphonicVoiceID:         firstNonEmpty(os.Getenv("NEUPHONIC_VOICE_ID"), "eeb488b8-d532-4235-aa57-f070cfc44750"),
		CambAPIKey:               os.Getenv("CAMB_API_KEY"),
		CambVoiceID:              firstNonEmpty(os.Getenv("CAMB_VOICE_ID"), "147320"),
		AsyncaiAPIKey:            os.Getenv("ASYNCAI_API_KEY"),
		AsyncaiVoiceID:           firstNonEmpty(os.Getenv("ASYNCAI_VOICE_ID"), "e0f39dc4-f691-4e78-bba5-5c636692cc04"),
		RespeecherAPIKey:         os.Getenv("RESPEECHER_API_KEY"),
		RespeecherVoiceID:        firstNonEmpty(os.Getenv("RESPEECHER_VOICE_ID"), "samantha"),
		BlandAPIKey:              os.Getenv("BLAND_API_KEY"),
		BlandVoiceID:             firstNonEmpty(os.Getenv("BLAND_VOICE_ID"), "29158307-9893-4149-8a75-bc9ce313d64e"),
		SimplismartAPIKey:        os.Getenv("SIMPLISMART_API_KEY"),
		SimplismartVoiceID:       firstNonEmpty(os.Getenv("SIMPLISMART_VOICE_ID"), "tara"),
		SpitchAPIKey:             os.Getenv("SPITCH_API_KEY"),
		SpitchVoiceID:            firstNonEmpty(os.Getenv("SPITCH_VOICE_ID"), "lina"),
		KokoroAPIURI:             os.Getenv("KOKORO_API_URI"),
		KokoroAPIKey:             os.Getenv("KOKORO_API_KEY"),
		KokoroVoiceID:            firstNonEmpty(os.Getenv("KOKORO_VOICE_ID"), "af_heart"),
		VakyamAPIKey:             os.Getenv("VAKYAM_API_KEY"),
		VakyamVoiceID:            firstNonEmpty(os.Getenv("VAKYAM_VOICE_ID"), "Archana"),
		GroqAPIKey:               os.Getenv("GROQ_API_KEY"),
		GroqVoiceID:              firstNonEmpty(os.Getenv("GROQ_VOICE_ID"), "autumn"),
		HumeAPIKey:               os.Getenv("HUME_API_KEY"),
		HumeVoiceID:              firstNonEmpty(os.Getenv("HUME_VOICE_ID"), "Ava Song"),
		InworldAPIKey:            os.Getenv("INWORLD_API_KEY"),
		SonioxAPIKey:             os.Getenv("SONIOX_API_KEY"),
		SaidwireAPIKey:           os.Getenv("SAIDWIRE_API_KEY"),
		GptLiveAPIKey:            os.Getenv("GPTLIVE_API_KEY"),
		StudioAPIURL:             strings.TrimRight(os.Getenv("STUDIO_API_URL"), "/"),
		StudioAccountID:          os.Getenv("STUDIO_ACCOUNT_ID"),
		StudioSIDFile:            os.Getenv("STUDIO_SID_FILE"),
		StudioEmail:              os.Getenv("STUDIO_EMAIL"),
		StudioPassword:           os.Getenv("STUDIO_PASSWORD"),
		GeminiAPIKey:             os.Getenv("GEMINI_API_KEY"),
		GptLiveModel:             firstNonEmpty(os.Getenv("GPTLIVE_MODEL"), "gpt-live-1"),
		GptLiveHost:              os.Getenv("GPTLIVE_HOST"),
		GptLivePath:              os.Getenv("GPTLIVE_PATH"),
		GptLiveDelegationModel:   firstNonEmpty(os.Getenv("GPTLIVE_DELEGATION_MODEL"), "gpt-5.5"),
		VoiceLiveAPIKey:          os.Getenv("VOICELIVE_API_KEY"),
		VoiceLiveHost:            os.Getenv("VOICELIVE_HOST"),
		VoiceLiveModel:           firstNonEmpty(os.Getenv("VOICELIVE_MODEL"), "gpt-realtime"),
		VoiceLiveVoice:           firstNonEmpty(os.Getenv("VOICELIVE_VOICE"), "en-US-AvaNeural"),
		NovaAccessKeyID:          os.Getenv("NOVA_ACCESS_KEY_ID"),
		NovaSecretAccessKey:      os.Getenv("NOVA_SECRET_ACCESS_KEY"),
		NovaSessionToken:         os.Getenv("NOVA_SESSION_TOKEN"),
		NovaRegion:               firstNonEmpty(os.Getenv("NOVA_REGION"), "us-east-1"),
		NovaModel:                firstNonEmpty(os.Getenv("NOVA_MODEL"), "amazon.nova-2-5-sonic"),
		NovaVoice:                firstNonEmpty(os.Getenv("NOVA_VOICE"), "tiffany"),
		SpeechmaticsAPIKey:       os.Getenv("SPEECHMATICS_API_KEY"),
		SpeechmaticsSTTURI:       firstNonEmpty(os.Getenv("SPEECHMATICS_STT_URI"), "eu2.rt.speechmatics.com"),
		SpeechmaticsAgentAPIKey:  os.Getenv("SPEECHMATICS_AGENT_API_KEY"),
		SpeechmaticsAgentProfile: firstNonEmpty(os.Getenv("SPEECHMATICS_AGENT_PROFILE"), "smart"),
		SpeechmaticsAgentHost:    os.Getenv("SPEECHMATICS_AGENT_HOST"),
		DialogflowProject:        os.Getenv("DIALOGFLOW_PROJECT"),
		DialogflowCESApp:         os.Getenv("DIALOGFLOW_CES_APP"),
		DialogflowCESDeployment:  os.Getenv("DIALOGFLOW_CES_DEPLOYMENT"),
		DialogflowCESLocation:    firstNonEmpty(os.Getenv("DIALOGFLOW_CES_LOCATION"), "us"),
		DialogflowAgent:          os.Getenv("DIALOGFLOW_AGENT"),
		DialogflowRegion:         firstNonEmpty(os.Getenv("DIALOGFLOW_REGION"), "us-central1"),
		DialogflowLang:           firstNonEmpty(os.Getenv("DIALOGFLOW_LANG"), "en-US"),
		NgrokAuthToken:           os.Getenv("NGROK_AUTHTOKEN"),
		NgrokDomain:              os.Getenv("NGROK_DOMAIN"),
		RunID:                    os.Getenv("RUN_ID"),
		LogLevel:                 strings.ToLower(firstNonEmpty(os.Getenv("LOG_LEVEL"), "info")),
	}

	// required
	var missing []string
	if s.APIBaseURL == "" {
		missing = append(missing, "JAMBONZ_API_URL")
	} else if _, err := url.Parse(s.APIBaseURL); err != nil {
		return nil, fmt.Errorf("JAMBONZ_API_URL is not a valid URL: %w", err)
	}
	if s.SPAPIKey == "" {
		missing = append(missing, "JAMBONZ_SP_API_KEY")
	}
	if s.SPSID == "" {
		missing = append(missing, "JAMBONZ_SP_SID")
	}
	if v := os.Getenv("JAMBONZ_SBC_PUBLIC_IP"); v == "" {
		missing = append(missing, "JAMBONZ_SBC_PUBLIC_IP")
	} else {
		ip := net.ParseIP(v)
		if ip == nil {
			return nil, fmt.Errorf("JAMBONZ_SBC_PUBLIC_IP is not a valid IP: %q", v)
		}
		s.SBCPublicIP = ip
	}
	if s.DeepgramAPIKey == "" {
		missing = append(missing, "DEEPGRAM_API_KEY")
	}
	if s.DeepseekAPIKey == "" {
		missing = append(missing, "DEEPSEEK_API_KEY")
	}
	if s.NgrokAuthToken == "" {
		missing = append(missing, "NGROK_AUTHTOKEN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required env vars missing: %s", strings.Join(missing, ", "))
	}
	// SIPRealmZone must be a multi-label domain so the upstream
	// `(.*)\.(.*\..*)$` regex on POST /SipRealms accepts our full
	// `<account>.<zone>` realm.
	if !strings.Contains(s.SIPRealmZone, ".") {
		return nil, fmt.Errorf("JAMBONZ_SIP_REALM_ZONE must contain at least one dot (got %q)", s.SIPRealmZone)
	}

	// Optional — Google Dialogflow service-account key. DIALOGFLOW_KEYFILE
	// names a file whose contents (service-account JSON) are read verbatim
	// and passed inline to the dialogflow verb. Unset => the dialogflow test
	// skips cleanly. A set-but-unreadable file is a hard error so a
	// misconfigured path doesn't silently disable the test.
	if kf := os.Getenv("GEMINI_KEYFILE"); kf != "" {
		b, err := os.ReadFile(kf)
		if err != nil {
			return nil, fmt.Errorf("GEMINI_KEYFILE=%q: %w", kf, err)
		}
		if !json.Valid(b) {
			return nil, fmt.Errorf("GEMINI_KEYFILE=%q is not valid JSON", kf)
		}
		s.GeminiServiceKey = string(b)
	}

	if kf := os.Getenv("DIALOGFLOW_KEYFILE"); kf != "" {
		raw, err := os.ReadFile(kf)
		if err != nil {
			return nil, fmt.Errorf("DIALOGFLOW_KEYFILE=%q: %w", kf, err)
		}
		if !json.Valid(raw) {
			return nil, fmt.Errorf("DIALOGFLOW_KEYFILE=%q is not valid JSON", kf)
		}
		s.DialogflowServiceKey = string(raw)
	}

	// ttl
	ttl := 2 * time.Hour
	if v := os.Getenv("ORPHAN_TTL_HOURS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("ORPHAN_TTL_HOURS must be a non-negative integer: %q", v)
		}
		ttl = time.Duration(n) * time.Hour
	}
	s.OrphanTTL = ttl

	if v := os.Getenv("AICOUSTICS_ENABLED"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("AICOUSTICS_ENABLED must be true/false: %w", err)
		}
		s.AicousticsEnabled = b
	}

	if v := os.Getenv("JAMBONZ_ALLOW_SP_POLICY_CHANGES"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("JAMBONZ_ALLOW_SP_POLICY_CHANGES must be true/false: %w", err)
		}
		s.AllowSPPolicyChanges = b
	}

	// contract strictness defaults on per ADR-0015
	s.ContractStrict = true
	if v := os.Getenv("CONTRACT_STRICT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CONTRACT_STRICT must be true/false: %w", err)
		}
		s.ContractStrict = b
	}

	// per-leg recording (ADR-0016): off by default
	if v := os.Getenv("RECORD_LEGS"); v != "" {
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "on", "yes":
			s.RecordLegs = true
		case "off", "no":
			s.RecordLegs = false
		default:
			b, err := strconv.ParseBool(v)
			if err != nil {
				return nil, fmt.Errorf("RECORD_LEGS must be true/false (or 1/0, on/off, yes/no): %w", err)
			}
			s.RecordLegs = b
		}
	}
	// A relative RECORD_DIR is anchored at the repo root, not the CWD: `go
	// test` runs each test binary with CWD = the package dir (tests/verbs),
	// which would scatter recordings/ trees across packages.
	s.RecordDir = firstNonEmpty(os.Getenv("RECORD_DIR"), "recordings")
	if !filepath.IsAbs(s.RecordDir) {
		if root, ok := findRepoRoot(); ok {
			s.RecordDir = filepath.Join(root, s.RecordDir)
		}
	}

	return s, nil
}

// findRepoRoot walks up from CWD looking for go.mod — the module root. Same
// walk as findDotEnv, but keyed on a file that always exists in a checkout
// (a .env may be absent when config comes from process env alone).
func findRepoRoot() (string, bool) {
	dir, err := os.Getwd()
	if err != nil {
		return "", false
	}
	for {
		if fi, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil && !fi.IsDir() {
			return dir, true
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", false
		}
		dir = parent
	}
}

// loadDotEnv reads KEY=VALUE lines from path into os env, without overriding
// variables that are already set in the process env.
func loadDotEnv(path string) error {
	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			continue
		}
		k := strings.TrimSpace(line[:eq])
		v := strings.TrimSpace(line[eq+1:])
		v = strings.Trim(v, `"'`)
		if _, exists := os.LookupEnv(k); !exists {
			_ = os.Setenv(k, v)
		}
	}
	return sc.Err()
}

func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
