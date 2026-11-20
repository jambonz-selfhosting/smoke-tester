//go:build detail

package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_LLM_VoiceLive_Hangup covers the built-in hangup tool on voicelive.
//
// Worth its own test because the hangup tool is declared with no parameters, so
// the model calls it with an empty `arguments` string — a path that is easy to
// drop on the floor while still passing the get_weather test, which always
// carries an argument.
func TestVerb_LLM_VoiceLive_Hangup(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasVoiceLive() {
		t.Log("VOICELIVE_API_KEY/VOICELIVE_HOST not set — passing without exercising voicelive hangup")
		return
	}

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising voicelive hangup")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "ensure-caller-wav")
	callerWAV, err := tts.EnsureWAV(ctx, "testdata/hangup", hangupCallerUtterance,
		tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV caller utterance: %v", err)
	}
	s.Done()

	s = Step(t, "script-llm-hangup")
	verb := V("llm",
		"vendor", "voicelive",
		"model", cfg.VoiceLiveModel,
		"auth", map[string]any{"apiKey": cfg.VoiceLiveAPIKey},
		"connectOptions", map[string]any{"host": cfg.VoiceLiveHost},
		"actionHook", SessionURL(sess, "llm"),
		"llmOptions", map[string]any{
			"session_update": map[string]any{
				"modalities":   []string{"text", "audio"},
				"instructions": hangupRealtimePrompt,
				"voice": map[string]any{
					"name": cfg.VoiceLiveVoice,
					"type": "azure-standard",
				},
				"turn_detection": map[string]any{
					"type":                "azure_semantic_vad",
					"silence_duration_ms": 500,
				},
			},
			"response_create": map[string]any{
				"instructions": "Greet the caller in one short sentence and ask how you can help.",
			},
		},
		"hangup", map[string]any{"reason": hangupAppReason},
	)
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
	// "" → the model supplies its own free-form reason, which by design wins
	// over hangup.reason; assert only that SOME reason propagated.
	assertServerHangup(s, call, "")
	s.Done()

	s = Step(t, "assert-completion-hangup")
	assertCompletionHangup(s, ctx, sess, "llm")
	s.Done()
}
