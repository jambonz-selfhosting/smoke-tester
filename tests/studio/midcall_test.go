//go:build studio

package studiotests

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"testing"

	"github.com/jambonz-selfhosting/smoke-tester/internal/studio"
)

// validSignature checks X-Studio-Signature (t=<unix s>,v1=<hex hmac-sha256 of "t.body">) as a customer would.
func validSignature(secret string, raw []byte, header string) bool {
	var t, v1 string
	for _, p := range strings.Split(header, ",") {
		if k, v, ok := strings.Cut(p, "="); ok {
			switch k {
			case "t":
				t = v
			case "v1":
				v1 = v
			}
		}
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(t + "." + string(raw)))
	return t != "" && hmac.Equal([]byte(hex.EncodeToString(mac.Sum(nil))), []byte(v1))
}

// TestStudio_MidCallControl: one call where the customer's server steers the agent two ways. Its call-event webhook
// answers a question with context the model can't know, and its "payment system" pushes a situation into the live
// call with a Studio API key.
//
// Steps:
//   - setup: an API key, an agent with a server-only situation and a call-event webhook, its signing secret
//   - call: ask for the account manager, say the bill is paid, goodbye
//   - check-webhook: every event was signed with the agent's secret; call_start came first and call_end arrived
//   - check-push: Studio accepted the push (202)
func TestStudio_MidCallControl(t *testing.T) {
	ctx := WithTimeout(t, 5*time.Minute)
	t.Parallel()
	cs := customer(t)
	ns := "midcall"

	s := Step(t, "setup")
	var key struct {
		APIKey struct {
			ID string `json:"id"`
		} `json:"api_key"`
		Secret string `json:"secret"`
	}
	if err := ws.Do(ctx, http.MethodPost, "/api-keys", map[string]any{"name": itName(t, "key")}, &key); err != nil {
		s.Fatalf("api key: %v", err)
	}
	cleanupLog(t, "api key", func(c context.Context) error {
		return ws.Do(c, http.MethodDelete, "/api-keys/"+key.APIKey.ID, nil, nil)
	})
	prompt := `You are the customer care line for Acme Bank, on the phone. Keep every reply to one or two short sentences.
Greet the caller and ask how you can help. If you don't know something about the caller's account, say you'll check.
When the caller says goodbye, say goodbye and end the call with the hangup tool.`
	id, issues := newSitAgent(t, ctx, studio.Pipeline{PromptID: newPrompt(t, ctx, prompt), Hangup: true, Voice: voice(t, ctx),
		Situations: []studio.Situation{{Name: "payment_received", Who: "code",
			Instructions: "The caller's payment has just arrived. Tell them it was received and end that reply with the words: all settled."}},
		Edit: func(c map[string]any) {
			c["events"] = map[string]any{"url": cs.URL + "/" + ns + "/events",
				"events": []string{"call_start", "user_transcript", "situation_entered", "call_end"}}
		}})
	if slices.ContainsFunc(issues, func(i studio.Issue) bool { return i.Severity == "error" }) {
		s.Fatalf("draft errors: %+v", issues)
	}
	var sec struct {
		Secret string `json:"secret"`
	}
	if err := ws.Do(ctx, http.MethodGet, "/agents/"+id+"/webhook-secret", nil, &sec); err != nil || sec.Secret == "" {
		s.Fatalf("webhook secret: %v", err)
	}

	var mu sync.Mutex
	var events, badSig []string
	pushStatus := 0
	cs.onEvents(ns, func(raw []byte, sig string) any {
		var ev struct {
			Event   string         `json:"event"`
			CallSID string         `json:"call_sid"`
			Data    map[string]any `json:"data"`
		}
		_ = json.Unmarshal(raw, &ev)
		mu.Lock()
		events = append(events, ev.Event)
		if !validSignature(sec.Secret, raw, sig) {
			badSig = append(badSig, ev.Event)
		}
		mu.Unlock()
		said := strings.ToLower(func() string { x, _ := ev.Data["transcript"].(string); return x }())
		switch {
		case ev.Event == "user_transcript" && strings.Contains(said, "account manager"):
			return map[string]any{"actions": map[string]any{
				"inject_context": "The caller's account manager is Morgan Reyes.",
				"generate_reply": "Tell the caller the name of their account manager.",
			}}
		case ev.Event == "user_transcript" && strings.Contains(said, "paid"):
			// The payment system hears about it on its own and pushes into the call.
			go func() {
				code, err := ws.PushActions(ctx, key.Secret, ev.CallSID,
					map[string]any{"situation": "payment_received", "generate_reply": true})
				if err != nil {
					t.Logf("push: %v", err)
				}
				mu.Lock()
				pushStatus = code
				mu.Unlock()
			}()
		}
		return map[string]any{}
	})
	s.Done()

	r := call(t, ctx, studio.Scenario{
		Name: "mid-call-control", AgentID: id, Direction: "inbound", Language: "en", Voice: "aura-2-thalia-en",
		Turns: []studio.Turn{
			{Say: "Hi, who is my account manager?"},
			{Say: "Thanks. Also, I just paid my bill online.", PauseSec: 6},
			{Say: "Great, that's all. Goodbye.", PauseSec: 8},
		},
		MaxSec: 150, TailSec: 15,
		Expect: &studio.Expect{
			Events: []string{
				`Context added by your webhook \(user_transcript\)`,
				"Started payment_received by a push from your server",
				"Agent asked to reply by a push from your server",
			},
			Heard: []string{`(?i)morgan`, `(?i)all settled`},
		},
	})

	s = Step(t, "check-webhook")
	deadline := time.Now().Add(20 * time.Second)
	for {
		mu.Lock()
		got := slices.Clone(events)
		mu.Unlock()
		if slices.Contains(got, "call_end") || time.Now().After(deadline) {
			if len(got) == 0 || got[0] != "call_start" || !slices.Contains(got, "call_end") ||
				!slices.Contains(got, "situation_entered") {
				s.Errorf("call %s: events %v, want call_start first, then situation_entered and call_end", r.CallSID, got)
			}
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	mu.Lock()
	if len(badSig) > 0 {
		s.Errorf("events with a signature that doesn't verify against the agent's secret: %v", badSig)
	}
	mu.Unlock()
	s.Done()

	s = Step(t, "check-push")
	mu.Lock()
	if pushStatus != http.StatusAccepted {
		s.Errorf("push answered %d, want 202", pushStatus)
	}
	mu.Unlock()
	s.Done()
}
