// Tests for the `dialogflow` verb — CX variant with a client-side tool call.
//
// Chain under test:
//
//	caller speaks (our WAV)  ==RTP==>
//	feature-server dialogflow task  --dialogflow_cx_start-->
//	@jambonz/mrf  --dialogflow.start--> mediajam --StreamingDetectIntent--> CX
//	CX returns a tool_call --> mediajam tool_calls event --> task POSTs toolHook
//	our webhook returns {outputParameters} --> task --dialogflow_cx_tool_result-->
//	mediajam primes the next turn --> CX speaks --> audio to caller (recorded)
//
// The agent (Airline Support, 99e7b4c8) is a Playbook scripted to call
// getGeolocation before saying anything. "hi, I need a flight" triggered that
// tool call 6/6 in manual runs; after the tool result it greets ("welcome to
// the Cymbal Air helpdesk ... Where would you like to go?").
//
// Assertions: toolHook fired with action=getGeolocation, and the post-tool
// greeting audio lands on the caller leg (independent Deepgram STT).
//
// Skips when Dialogflow is unconfigured (cfg.HasDialogflow) or DEEPGRAM_API_KEY
// is missing.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// dialogflowPrompt reliably drives the playbook into its getGeolocation call.
const dialogflowPrompt = "hi, I need a flight"

// dialogflowToolResult is the canned getGeolocation output (client-side tool:
// we control both sides; the data is fictional by design).
const dialogflowToolResult = `{"outputParameters":{"city":"New York","country":"United States","country_code":"us","postcode":"10001"}}`

// dialogflowReplyKeywords: content words of the post-tool greeting. LLM
// phrasing varies, so require only ONE — the contract is "the agent spoke
// after the tool result", not exact wording.
var dialogflowReplyKeywords = []string{
	"cymbal", "air", "helpdesk", "welcome", "flight", "where", "go", "help", "world",
}

// TestVerb_Dialogflow_CX — full tool-call round trip through jambonz.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-dialogflow-verb — toolHook answers with canned geolocation
//  4. place-call / answer-and-record
//  5. speak-prompt, wait-for-reply (tool RTT + LLM + TTS)
//  6. assert-toolhook — tool-call callback captured, action=getGeolocation
//  7. assert-reply-audio — greeting words in the caller-leg recording
//  8. hangup, drain, event plumbing check
func TestVerb_Dialogflow_CX(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDialogflow() {
		s.Done()
		t.Skip("dialogflow test needs DIALOGFLOW_KEYFILE + DIALOGFLOW_PROJECT + DIALOGFLOW_AGENT + DIALOGFLOW_REGION")
	}
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("dialogflow test needs DEEPGRAM_API_KEY (prompt WAV + STT verification)")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", dialogflowPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	testID, sess := claimSession(t)

	s = Step(t, "script-dialogflow-verb")
	dfVerb := V("dialogflow",
		"credentials", cfg.DialogflowServiceKey,
		"project", cfg.DialogflowProject,
		"agent", cfg.DialogflowAgent,
		"region", cfg.DialogflowRegion,
		"model", "cx",
		"lang", cfg.DialogflowLang,
		"actionHook", webhookSrv.PublicURL()+"/action/dialogflow",
		"eventHook", SessionURL(sess, "dialogflow-event"),
		"toolHook", SessionURL(sess, "dialogflow-tool"),
		"events", []string{"intent", "transcription", "tool-calls", "start-play", "stop-play"},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		dfVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dialogflow", "dialogflow-event")
	// toolHook answers with the canned geolocation result (raw JSON object,
	// not a verb script)
	sess.ScriptActionHookBody("dialogflow-tool", []byte(dialogflowToolResult))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	recPath := filepath.Join(t.TempDir(), "dialogflow-reply.wav")

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "speak-prompt")
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-reply")
	// STT endpoint + playbook LLM + toolHook RTT + tool-result turn + TTS +
	// playback of the greeting (~12s speech)
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(16 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-toolhook")
	cbs := DrainCallbacks(sess, 2*time.Second)
	var toolCB *webhook.Callback
	for i := range cbs {
		if cbs[i].Hook == "action/dialogflow-tool" {
			toolCB = &cbs[i]
			break
		}
	}
	if toolCB == nil {
		s.Errorf("no toolHook callback captured in %d callbacks", len(cbs))
	} else {
		s.Logf("toolHook body: %s", string(toolCB.Body))
		if action := toolCB.NestedString("tool_call.action"); action != "getGeolocation" {
			s.Errorf("tool_call.action = %q, want getGeolocation", action)
		}
	}
	s.Done()

	s = Step(t, "assert-reply-audio")
	AssertTranscriptHasMost(s, ctx, recPath, 1, dialogflowReplyKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-event-plumbing")
	var types []string
	for _, cb := range append(cbs, DrainCallbacks(sess, 3*time.Second)...) {
		if cb.Hook == "action/dialogflow-event" {
			types = append(types, cb.String("event"))
			if id := cb.NestedString("customer_data.x_test_id"); id != "" && id != testID {
				s.Errorf("eventHook x_test_id=%q want %q", id, testID)
			}
		}
	}
	if len(types) == 0 {
		s.Logf("WARNING: no dialogflow eventHook callbacks captured")
	} else {
		s.Logf("dialogflow event types: %v", types)
	}
	s.Done()
}
