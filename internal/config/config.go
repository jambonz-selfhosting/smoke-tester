// Package config loads harness settings from .env + process environment into a
// typed Settings struct. Loaded once per process in TestMain — see ADR-0009.
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
	APIKey     string
	AccountSID string
	SIPDomain  string
	SIPProxy   string // falls back to SIPDomain if empty

	// Optional — Service-provider-scoped credentials. When set, tests that
	// require SP scope (ServiceProviders, Accounts create/delete, SP-scoped
	// carriers) run; when unset, they skip with a clear reason.
	SPAPIKey string
	SPSID    string

	// Optional — Deepgram API key. Used for two distinct purposes:
	//   1. Provisioning the in-jambonz Deepgram SpeechCredential at TestMain
	//      (label: "it-deepgram"). Verb tests reference this label as
	//      `synthesizer.label` / `recognizer.label`.
	//   2. Offline transcript verification — internal/stt/ uploads PCM
	//      recordings to Deepgram and asserts the transcript matches.
	// When unset, verb tests fall back to the cluster's default speech
	// vendor (whatever the account has provisioned out-of-band) and
	// transcript assertions log-skip.
	DeepgramAPIKey string

	// Optional — Deepseek LLM API key. Passed inline as `agent.llm.auth.apiKey`
	// in the agent verb test, bypassing jambonz's database credential lookup
	// (feature-server/lib/tasks/agent/index.js:446 honors inline auth). When
	// unset, the agent test skips.
	DeepseekAPIKey string

	// Environment capability (ADR-0007, ADR-0014)
	BehindNAT bool
	PublicIP  net.IP // empty unless set; only required for Carrier/Inbound modes

	// Tier 3+ — ngrok
	NgrokAuthToken string
	NgrokDomain    string

	// Test-run knobs
	RunID          string
	LogLevel       string // "info" | "debug"
	OrphanTTL      time.Duration
	ContractStrict bool // ADR-0015: schema violations fail tests
}

// HasSPScope reports whether SP-scoped tests can run.
func (s *Settings) HasSPScope() bool { return s.SPAPIKey != "" && s.SPSID != "" }

// HasDeepgram reports whether Deepgram-backed flows can run (provisioned
// SpeechCredential at TestMain, plus offline transcript verification).
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
		APIKey:         os.Getenv("JAMBONZ_API_KEY"),
		AccountSID:     os.Getenv("JAMBONZ_ACCOUNT_SID"),
		SPAPIKey:       os.Getenv("JAMBONZ_SP_API_KEY"),
		SPSID:          os.Getenv("JAMBONZ_SP_SID"),
		SIPDomain:      os.Getenv("JAMBONZ_SIP_DOMAIN"),
		SIPProxy:       os.Getenv("JAMBONZ_SIP_PROXY"),
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
	if s.APIKey == "" {
		missing = append(missing, "JAMBONZ_API_KEY")
	}
	if s.AccountSID == "" {
		missing = append(missing, "JAMBONZ_ACCOUNT_SID")
	}
	if s.SIPDomain == "" {
		missing = append(missing, "JAMBONZ_SIP_DOMAIN")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("required env vars missing: %s", strings.Join(missing, ", "))
	}
	if s.SIPProxy == "" {
		s.SIPProxy = s.SIPDomain
	}

	// capability
	if v := os.Getenv("BEHIND_NAT"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return nil, fmt.Errorf("BEHIND_NAT must be true/false: %w", err)
		}
		s.BehindNAT = b
	} else {
		// default to true (laptop) — safest, gates Inbound mode off unless opted in
		s.BehindNAT = true
	}
	if v := os.Getenv("PUBLIC_IP"); v != "" {
		ip := net.ParseIP(v)
		if ip == nil {
			return nil, fmt.Errorf("PUBLIC_IP is not a valid IP: %q", v)
		}
		s.PublicIP = ip
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
