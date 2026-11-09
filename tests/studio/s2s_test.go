//go:build studio

package studiotests

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

// s2sVendor is one realtime voice API vendor: its credential as jambonz stores it, and the agent's model.
type s2sVendor struct {
	key          string // scenarios/studio-s2s/<key>.json and the subtest name
	credVendor   string
	cred         func() map[string]any // nil: no key in .env, the vendor is not called
	model, voice string                // empty: Studio's starting value for the vendor
}

func ifSet(ok bool, body map[string]any) func() map[string]any {
	if !ok {
		return nil
	}
	return func() map[string]any { return body }
}

func s2sVendors() []s2sVendor {
	env := os.Getenv
	return []s2sVendor{
		{key: "openai", credVendor: "openai_s2s",
			cred: ifSet(cfg.HasOpenAI(), map[string]any{"api_key": cfg.OpenAIAPIKey})},
		{key: "xai", credVendor: "xai_s2s",
			cred: ifSet(cfg.HasXai(), map[string]any{"api_key": cfg.XaiAPIKey})},
		{key: "google-key", credVendor: "google_s2s",
			cred: ifSet(cfg.HasGeminiS2S(), map[string]any{"api_key": cfg.GeminiAPIKey})},
		{key: "google-vertex", credVendor: "google_s2s",
			cred: ifSet(cfg.GeminiServiceKey != "", map[string]any{"service_key": cfg.GeminiServiceKey,
				"region": firstSet(env("VERTEX_REGION"), "us-central1")})},
		{key: "assemblyai", credVendor: "assemblyai_s2s",
			cred: ifSet(cfg.HasAssemblyai(), map[string]any{"api_key": cfg.AssemblyaiAPIKey})},
		{key: "voicelive", credVendor: "voicelive_s2s", model: cfg.VoiceLiveModel,
			cred: ifSet(cfg.HasVoiceLive(), map[string]any{"api_key": cfg.VoiceLiveAPIKey, "host": cfg.VoiceLiveHost})},
		{key: "nova", credVendor: "nova_s2s", model: cfg.NovaModel,
			cred: ifSet(cfg.HasNova(), map[string]any{"access_key_id": cfg.NovaAccessKeyID,
				"secret_access_key": cfg.NovaSecretAccessKey, "region": cfg.NovaRegion})},
		{key: "qwen", credVendor: "qwen_s2s", model: env("QWEN_MODEL"),
			cred: ifSet(env("QWEN_API_KEY") != "" && env("QWEN_HOST") != "",
				map[string]any{"api_key": env("QWEN_API_KEY"), "host": env("QWEN_HOST")})},
		// The model of an Azure OpenAI agent is the deployment name.
		{key: "azure-openai", credVendor: "azure_openai_s2s", model: env("AZURE_OPENAI_REALTIME_DEPLOYMENT"),
			cred: ifSet(env("AZURE_OPENAI_REALTIME_API_KEY") != "" && env("AZURE_OPENAI_REALTIME_HOST") != "",
				map[string]any{"api_key": env("AZURE_OPENAI_REALTIME_API_KEY"), "host": env("AZURE_OPENAI_REALTIME_HOST")})},
	}
}

func firstSet(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}

// newAgent adds the vendor's credential under a run-unique label and builds an order-desk agent on it.
// Both are deleted when the test ends, the agent first; dropCred deletes the credential sooner.
func newAgent(t *testing.T, ctx context.Context, v s2sVendor) (agentID string, dropCred func() error) {
	t.Helper()
	label := "smoke-" + provision.RunID() + "-" + v.key
	body := map[string]any{"vendor": v.credVendor, "label": label}
	for k, val := range v.cred() {
		body[k] = val
	}
	credSID, err := jz.CreateAccountLlmCredential(ctx, conn.JambonzAccountSID, body)
	if err != nil {
		t.Fatalf("add %s credential: %v", v.credVendor, err)
	}
	// api-server answers 500, not 404, for a credential already deleted, so it is deleted once.
	var once sync.Once
	var dropErr error
	dropCred = func() error {
		once.Do(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			dropErr = jz.DeleteAccountLlmCredential(cctx, conn.JambonzAccountSID, credSID)
		})
		return dropErr
	}
	t.Cleanup(func() {
		if err := dropCred(); err != nil {
			t.Logf("delete credential %s: %v", credSID, err)
		}
	})
	name := "smoke " + provision.RunID() + " " + v.key
	agentID, err = ws.CreateS2sAgent(ctx, name, studio.S2s{Vendor: v.credVendor, CredentialSID: credSID, Label: label,
		Model: v.model, Voice: v.voice}, desk)
	if agentID != "" {
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := ws.DeleteAgent(cctx, agentID, name); err != nil {
				t.Logf("delete agent %s: %v", agentID, err)
			}
		})
	}
	if err != nil {
		t.Fatalf("build %s agent: %v", v.key, err)
	}
	t.Logf("agent %s on credential %s (%s, label %s)", agentID, credSID, v.credVendor, label)
	return agentID, dropCred
}

func scenario(t *testing.T, key, agentID string) studio.Scenario {
	t.Helper()
	sc, err := studio.LoadScenario(filepath.Join("..", "..", "scenarios", "studio-s2s", key+".json"))
	if err != nil {
		t.Fatalf("scenario: %v", err)
	}
	sc.AgentID = agentID
	return sc
}

func play(t *testing.T, ctx context.Context, sc studio.Scenario) *studio.Result {
	t.Helper()
	r, err := studio.Run(ctx, ws, sc, studio.RunOptions{OutDir: outDir, SBC: sbc, Logf: t.Logf})
	if err != nil {
		t.Fatalf("%s: %v (artifacts in %s)", sc.Name, err, r.Dir)
	}
	t.Logf("call_sid=%s ended_by_agent=%v, artifacts in %s\n%s", r.CallSID, r.EndedByAgent, r.Dir, r.Transcript)
	if len(r.Failures) > 0 {
		t.Errorf("%s: Studio's record of call %s misses:\n  %s", sc.Name, r.CallSID, strings.Join(r.Failures, "\n  "))
	}
	return r
}

// TestStudio_S2S calls an order-desk agent on each vendor with a key in .env: a greeting, the order tool,
// a billing transfer that comes back, a barge-in and a goodbye the agent ends with its hangup tool.
func TestStudio_S2S(t *testing.T) {
	for _, v := range s2sVendors() {
		t.Run(v.key, func(t *testing.T) {
			t.Parallel()
			if v.cred == nil {
				t.Logf("no %s key in .env — passing without calling it", v.key)
				return
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			defer cancel()
			agentID, _ := newAgent(t, ctx, v)
			play(t, ctx, scenario(t, v.key, agentID))
		})
	}
}

// TestStudio_S2S_CredentialDeleted: once an agent's credential is deleted in the portal, Studio refuses to test
// the agent and names the missing credential (CRED_NOT_FOUND), instead of placing a call that cannot run.
func TestStudio_S2S_CredentialDeleted(t *testing.T) {
	var v *s2sVendor
	for _, c := range s2sVendors() {
		if c.cred != nil {
			v = &c
			break
		}
	}
	if v == nil {
		t.Log("no realtime voice API key in .env — passing without an agent")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	agentID, dropCred := newAgent(t, ctx, *v)
	if err := dropCred(); err != nil {
		t.Fatalf("delete credential: %v", err)
	}
	err := ws.Do(ctx, "POST", "/agents/"+agentID+"/test-sessions", map[string]any{"record": false}, nil)
	var ae *studio.APIError
	if !errors.As(err, &ae) || ae.Status != 422 || !strings.Contains(ae.Body, "CRED_NOT_FOUND") {
		t.Fatalf("test session for an agent without its credential: got %v, want 422 naming CRED_NOT_FOUND", err)
	}
	t.Logf("refused as expected: %s", ae.Body)
}
