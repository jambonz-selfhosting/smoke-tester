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
}

// HasDeepgram reports whether Deepgram-backed flows can run. With the
// new self-provisioning model both keys are mandatory at TestMain, so
// these helpers are kept only for the rare callers that still want to
// guard for diagnostic logs.
func (s *Settings) HasDeepgram() bool { return s.DeepgramAPIKey != "" }

// HasDeepseek reports whether the Deepseek-backed agent verb test can run.
func (s *Settings) HasDeepseek() bool { return s.DeepseekAPIKey != "" }

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
		APIBaseURL:     strings.TrimRight(os.Getenv("JAMBONZ_API_URL"), "/"),
		SPAPIKey:       os.Getenv("JAMBONZ_SP_API_KEY"),
		SPSID:          os.Getenv("JAMBONZ_SP_SID"),
		SIPRealmZone:   firstNonEmpty(os.Getenv("JAMBONZ_SIP_REALM_ZONE"), "smoke.test"),
		DeepgramAPIKey: os.Getenv("DEEPGRAM_API_KEY"),
		DeepseekAPIKey: os.Getenv("DEEPSEEK_API_KEY"),
		NgrokAuthToken: os.Getenv("NGROK_AUTHTOKEN"),
		NgrokDomain:    os.Getenv("NGROK_DOMAIN"),
		RunID:          os.Getenv("RUN_ID"),
		LogLevel:       strings.ToLower(firstNonEmpty(os.Getenv("LOG_LEVEL"), "info")),
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

	// contract strictness defaults on per ADR-0015
	s.ContractStrict = true
	if v := os.Getenv("CONTRACT_STRICT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("CONTRACT_STRICT must be true/false: %w", err)
		}
		s.ContractStrict = b
	}

	return s, nil
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
