// Regression test for the CES audioMode latch: `welcomeEvent` on model "ces"
// produces the agent's opening turn correctly and then discards every frame of
// caller audio for the rest of the call.
//
// Chain under test:
//
//	dialogflow verb + welcomeEvent  --dialogflow_ces_start … <event>-->
//	@jambonz/mrf --dialogflow.start--> mediajam startCES
//	  -> primed with SessionInput.event, so audioMode is turned OFF
//	  -> SendAudio becomes a no-op and NOTHING ever turns it back on
//	caller speaks ==RTP==> mediajam ==(dropped)==>  CES never hears it
//
// Priming is deliberate — it stops line noise from cutting off the turn the
// event asked for. The defect is that the gate is never reopened. ES and CX
// hide it because they build a fresh stream per turn; CES holds one
// BidiRunSession for the whole call, so the release has to be explicit.
//
// This is why the sibling test in dialogflow_ces_test.go carries the comment
// "No welcomeEvent: a CES session primed with an event/text goes out of audio
// mode, and caller audio would never reach the agent" — it routes around the
// bug. When this test goes green, delete that workaround comment.
//
// # WHAT IS ASSERTED, AND WHY IT IS THE TOOLHOOK
//
// The load-bearing assertion is that a toolHook callback arrives carrying the
// city from our spoken prompt. The agent cannot ask for the weather in Boston
// unless it heard "Boston", so one callback with populated args is direct
// proof that caller audio traversed the full path into CES — no STT, no
// phrasing tolerance, no keyword pooling required. The spoken echo of our tool
// output is kept as a secondary assertion (it proves the round trip closed).
//
// WHAT IS NOT ASSERTED: the greeting itself
//
// Whether the event yields an opening turn depends on the CES app having a
// handler for that event name, which is app configuration rather than jambonz
// behaviour. The bug does not depend on it either way: audioMode is switched
// off from `cfg.Event != ""` alone, before Google has replied at all. So the
// greeting is recorded and logged for diagnosis, never asserted.
//
// That does expose a boundary the fix has to respect: releasing the gate only
// on an end-of-turn signal means an event the app has NO handler for — which
// may produce no turn at all — would leave the caller muted even after the
// fix. Set DIALOGFLOW_CES_WELCOME_EVENT to an event the app actually handles.
//
// Skips when CES is unconfigured (cfg.HasDialogflowCES) or DEEPGRAM_API_KEY is
// missing. Reuses cesPrompt / cesToolOutput / cesToolEchoes from
// dialogflow_ces_test.go (same package).
package verbs

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// cesWelcomeEventName is the event used to drive the opening turn. The right
// value is app-specific — a CES app only reacts to event names it declares —
// so it is overridable. The default is the conventional one.
func cesWelcomeEventName() string {
	if v := os.Getenv("DIALOGFLOW_CES_WELCOME_EVENT"); v != "" {
		return v
	}
	return "WELCOME"
}

// awaitEvent waits for an eventHook callback whose "event" field is want, and
// returns everything it consumed on the way — the caller has to keep those,
// since the later assertions read the same queue.
//
// This replaces a fixed sleep. The verb already publishes start-play/stop-play,
// so "the turn finished speaking" is an observable fact rather than something
// to over-estimate; a budget remains as the ceiling, not the normal path.
func awaitEvent(ctx context.Context, sess *webhook.Session, hook, want string) ([]webhook.Callback, bool) {
	var seen []webhook.Callback
	for {
		cb, skipped, err := WaitCallbackForCollecting(ctx, sess, hook)
		seen = append(seen, skipped...)
		if err != nil {
			return seen, false
		}
		seen = append(seen, cb)
		if cb.String("event") == want {
			return seen, true
		}
	}
}

// TestVerb_Dialogflow_CES_WelcomeEvent — welcomeEvent must not mute the caller.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-ces-verb-with-welcome-event
//  4. place-call / answer-and-record
//  5. greeting window — recorded and logged, NOT asserted
//  6. speak "what is the weather in Boston"
//  7. assert-toolhook-heard-the-caller — STRICT, this is the regression
//  8. assert-tool-output-spoken — the round trip closed
func TestVerb_Dialogflow_CES_WelcomeEvent(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDialogflowCES() {
		s.Done()
		t.Skip("CES welcomeEvent test needs DIALOGFLOW_KEYFILE + DIALOGFLOW_PROJECT + DIALOGFLOW_CES_APP + DIALOGFLOW_CES_LOCATION")
	}
	if !cfg.HasDeepgram() {
		s.Done()
		t.Skip("CES welcomeEvent test needs DEEPGRAM_API_KEY (prompt WAV + STT verification)")
	}
	s.Done()

	ctx := WithTimeout(t, 260*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", cesPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	testID, sess := claimSession(t)

	s = Step(t, "script-ces-verb-with-welcome-event")
	welcomeEvent := cesWelcomeEventName()
	s.Logf("welcomeEvent = %q (override with DIALOGFLOW_CES_WELCOME_EVENT)", welcomeEvent)

	// A known session id, so the conversation this call creates can be reopened
	// afterwards to prove the id travelled the whole chain — verb ->
	// @jambonz/mrf -> media server -> the vendor's session path. Nothing on the
	// box logs that path, so this is the only way to observe it.
	sessionID := fmt.Sprintf("smoke-ces-%d", time.Now().Unix())
	s.Logf("sessionId = %q — reopen it with:", sessionID)
	s.Logf("  CES_PROBE_SESSION_ID=%s go test ./internal/dialogflow/ -run TestCESLiveProbeSession -v", sessionID)
	args := []any{
		"credentials", cfg.DialogflowServiceKey,
		"project", cfg.DialogflowProject,
		"agent", cfg.DialogflowCESApp,
		"region", cfg.DialogflowCESLocation,
		"model", "ces",
		"lang", cfg.DialogflowLang,
		"sessionId", sessionID,
		// The whole point of this test: prime the session with an event.
		"welcomeEvent", welcomeEvent,
		"actionHook", webhookSrv.PublicURL() + "/action/dialogflow-ces-welcome",
		"eventHook", SessionURL(sess, "ces-welcome-event"),
		"toolHook", SessionURL(sess, "ces-welcome-tool"),
		"events", []string{"session-output", "transcription", "tool-calls", "start-play", "stop-play"},
	}
	if cfg.DialogflowCESDeployment != "" {
		args = append(args, "deployment", cfg.DialogflowCESDeployment)
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dialogflow", args...),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dialogflow", "ces-welcome-event")

	if !json.Valid([]byte(cesToolOutput)) {
		s.Fatalf("cesToolOutput is not valid JSON: %s", cesToolOutput)
	}
	toolResultBody, err := json.Marshal(map[string]any{
		"outputParameters": json.RawMessage(cesToolOutput),
	})
	if err != nil {
		s.Fatalf("marshal tool result: %v", err)
	}
	sess.ScriptActionHookBodyFunc("ces-welcome-tool", func(cb webhook.Callback) []byte {
		return toolResultBody
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
	s.Done()

	greetingRec := filepath.Join(t.TempDir(), "ces-welcome-greeting.wav")
	replyRec := filepath.Join(t.TempDir(), "ces-welcome-reply.wav")

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.StartRecording(greetingRec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// Diagnosis only. A greeting here says the event has a handler and the
	// outbound half works; its absence says the event name is wrong for this
	// app, which is worth knowing but is not what this test is about.
	// Two separate questions, and only the second one gates the test.
	//
	//   did the event produce a turn at all?  -> start-play, short budget.
	//     Its absence means the app declares no handler for that event name,
	//     which is their configuration rather than jambonz behaviour, so it is
	//     logged and we move on at once instead of sitting out a budget.
	//   is the agent still speaking?          -> stop-play, longer budget.
	//     This one matters: talking over the agent registers as barge-in and
	//     would muddy the assertion this test exists for.
	s = Step(t, "wait-out-the-opening-turn")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (opening turn): %v", err)
	}
	startCtx, cancelStart := context.WithTimeout(ctx, 15*time.Second)
	collected, spoke := awaitEvent(startCtx, sess, "action/ces-welcome-event", "start-play")
	cancelStart()
	if spoke {
		stopCtx, cancelStop := context.WithTimeout(ctx, 30*time.Second)
		consumed, finished := awaitEvent(stopCtx, sess, "action/ces-welcome-event", "stop-play")
		cancelStop()
		collected = append(collected, consumed...)
		if !finished {
			s.Logf("the opening turn started but never reported stop-play; the prompt below " +
				"may land as barge-in")
		}
	} else {
		s.Logf("NO OPENING TURN: welcomeEvent %q produced no playback. Most likely CES app %s "+
			"declares no handler for that event name — point DIALOGFLOW_CES_WELCOME_EVENT at "+
			"one it does. Not fatal: the gate this test checks closes from the event's presence "+
			"alone, so the caller-audio assertion below still holds.",
			welcomeEvent, cfg.DialogflowCESApp)
	}
	call.StopRecording()
	if stt.HasKey() {
		if txt, err := stt.Transcribe(ctx, greetingRec); err != nil {
			s.Logf("opening-turn transcript unavailable (%v); it is not asserted either way", err)
		} else if strings.TrimSpace(txt) != "" {
			s.Logf("opening turn: %q", txt)
		}
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "speak-prompt")
	if err := call.StartRecording(replyRec); err != nil {
		s.Fatalf("StartRecording reply: %v", err)
	}
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-reply")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	// CES endpointing + the agent's LLM + the toolHook round trip + the
	// continued turn + playback all sit inside this budget.
	replyCtx, cancelReply := context.WithTimeout(ctx, 45*time.Second)
	consumed, gotReply := awaitEvent(replyCtx, sess, "action/ces-welcome-event", "stop-play")
	cancelReply()
	collected = append(collected, consumed...)
	if !gotReply {
		s.Logf("no stop-play for the reply within the budget; the assertions below say why")
	}
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	// Everything the two waits consumed, plus whatever is still queued: the
	// waits read the same queue, so dropping their haul would lose the
	// toolHook POST whenever it arrived before a stop-play.
	cbs := append(collected, DrainCallbacks(sess, 5*time.Second)...)

	// THE REGRESSION ASSERTION. A tool call naming the city we spoke can only
	// exist if CES received our audio. With the latch in place mediajam drops
	// every frame, so the agent hears silence and never asks for anything.
	s = Step(t, "assert-toolhook-heard-the-caller")
	var toolCBs []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == "action/ces-welcome-tool" {
			toolCBs = append(toolCBs, cb)
		}
	}
	if len(toolCBs) == 0 {
		var evTypes []string
		for _, cb := range cbs {
			if cb.Hook == "action/ces-welcome-event" {
				evTypes = append(evTypes, cb.String("event"))
			}
		}
		s.Fatalf("no toolHook callback after the caller spoke %q — CES never heard the caller. "+
			"This is the audioMode latch: priming with welcomeEvent %q turned audio mode off in "+
			"mediajam's startCES and nothing turned it back on, so SendAudio discarded every "+
			"frame. eventHook types seen: %v (of %d callbacks)",
			cesPrompt, welcomeEvent, evTypes, len(cbs))
	}
	first := toolCBs[0]
	var tcBody struct {
		ToolCall struct {
			Args map[string]any `json:"args"`
		} `json:"tool_call"`
	}
	if err := json.Unmarshal(first.Body, &tcBody); err != nil {
		s.Errorf("cannot parse tool_call body: %v", err)
	} else if len(tcBody.ToolCall.Args) == 0 {
		s.Errorf("tool_call.args is empty — the agent fired a tool call with no parameters, so "+
			"it did not actually understand the caller: %s", string(first.Body))
	} else {
		s.Logf("tool_call.args = %v — the caller's words reached CES", tcBody.ToolCall.Args)
	}
	s.Done()

	// Secondary: the agent spoke OUR values back, so the turn completed
	// normally rather than merely starting.
	s = Step(t, "assert-tool-output-spoken")
	echoes := cesToolEchoes(cesToolOutput)
	if len(echoes) == 0 {
		s.Fatalf("cannot derive a verifiable token from cesToolOutput=%s", cesToolOutput)
	}
	minHits := 2
	if len(echoes) < 2 {
		minHits = 1
	}
	s.Logf("requiring >=%d of our own tool-output values in the reply: %v", minHits, echoes)
	AssertTranscriptHasMost(s, ctx, replyRec, minHits, echoes...)
	s.Done()

	s = Step(t, "assert-event-plumbing")
	var types []string
	for _, cb := range cbs {
		if cb.Hook == "action/ces-welcome-event" {
			types = append(types, cb.String("event"))
			if id := cb.NestedString("customer_data.x_test_id"); id != "" && id != testID {
				s.Errorf("eventHook x_test_id=%q want %q", id, testID)
			}
		}
	}
	if len(types) == 0 {
		s.Errorf("no CES eventHook callbacks captured despite the verb subscribing")
	}
	s.Logf("CES event types: %v", types)
	s.Done()
}
