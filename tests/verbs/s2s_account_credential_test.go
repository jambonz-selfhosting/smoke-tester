// Tests for the `llm` verb taking its key from the account's LLM Services
// credential instead of an inline `auth` (feature-server feat/s2s-vendors).
// Agent Studio relies on this path: its verbs carry a vendor and a label, never a key.
//
// TestVerb_LLM_OpenAI_AccountCredential — an openai_s2s credential is created
// under the suite account; the verb names only its label. The call must greet,
// take "I'm done" and end through the hangup tool, as the inline-auth test does.
//
// TestVerb_LLM_S2S_NoCredential — for each speech-to-speech vendor added in
// feat/s2s-vendors, a verb whose label matches no credential must end with
// completion_reason "credential not found" and error.code credential_not_found,
// naming the credential vendor and label. No vendor key is needed.
package verbs

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func TestVerb_LLM_OpenAI_AccountCredential(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !hangupPreflightLlm(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "create-openai_s2s-credential")
	label := "it-openai-s2s-" + provision.RunID()
	sid := client.ManagedAccountLlmCredential(t, ctx, suite.AccountSID, map[string]any{
		"vendor": "openai_s2s", "label": label, "api_key": cfg.OpenAIAPIKey,
	})
	s.Logf("llm credential sid=%s label=%s", sid, label)
	s.Done()

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV caller utterance: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-without-auth")
	verb := buildHangupLlmVerb(SessionURL(sess, "llm"))
	delete(verb, "auth")
	verb["label"] = label
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
	SessionAckEmpty(sess, "llm")
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "drive-conversation")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	driveHangupConversation(s, ctx, call, callerWAV)
	s.Done()

	s = Step(t, "assert-server-hangup")
	assertServerHangup(s, call, "")
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "llm")
	s.Done()
}

func TestVerb_LLM_S2S_NoCredential(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	cases := []struct{ vendor, credVendor, model string }{
		{"xai", "xai_s2s", "grok-voice-latest"},
		{"google", "google_s2s", "gemini-live-2.5-flash"},
		{"qwen", "qwen_s2s", "qwen3.5-omni-flash-realtime"},
		{"assemblyai", "assemblyai_s2s", ""},
		{"voicelive", "voicelive_s2s", "gpt-realtime"},
		{"microsoft", "azure_openai_s2s", "gpt-realtime"},
		{"nova", "nova_s2s", "amazon.nova-2-sonic-v1:0"},
	}
	label := "it-missing-" + provision.RunID()
	for _, tc := range cases {
		t.Run(tc.vendor, func(t *testing.T) {
			t.Parallel()
			ctx := WithTimeout(t, 60*time.Second)
			uas := claimUAS(t, ctx)
			_, sess := claimSession(t)

			s := Step(t, "script-llm-without-auth")
			verb := V("llm", "vendor", tc.vendor, "label", label, "actionHook", SessionURL(sess, "llm"),
				"llmOptions", map[string]any{})
			if tc.model != "" {
				verb["model"] = tc.model
			}
			sess.ScriptCallHook(WithWarmupScript(webhook.Script{verb}))
			SessionAckEmpty(sess, "llm")
			s.Done()

			s = Step(t, "place-caller")
			call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(30))
			if err := call.Answer(); err != nil {
				s.Fatalf("Answer: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			s.Done()

			s = Step(t, "assert-credential-not-found")
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cb, err := sess.WaitCallbackFor(waitCtx, "action/llm")
			if err != nil {
				s.Fatalf("WaitCallbackFor action/llm: %v", err)
			}
			s.Logf("action/llm body: %s", string(cb.Body))
			if got := cb.String("completion_reason"); got != "credential not found" {
				s.Errorf("completion_reason: got %q want %q", got, "credential not found")
			}
			code, vendor, gotLabel := cb.NestedString("error.code"), cb.NestedString("error.vendor"), cb.NestedString("error.label")
			if code != "credential_not_found" || vendor != tc.credVendor || gotLabel != label {
				s.Errorf("error: got code %q vendor %q label %q, want credential_not_found %s %s",
					code, vendor, gotLabel, tc.credVendor, label)
			}
			s.Done()
		})
	}
}
