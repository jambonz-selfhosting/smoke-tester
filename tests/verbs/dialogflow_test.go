// Tests for the `dialogflow` verb — connects the caller to a Google
// Dialogflow agent (ES / CX / CES). We exercise the CX path against a real
// CX agent because that's the model the mediajam media server most recently
// gained support for; the mrf adapter translates the feature-server task's
// FreeSWITCH-style `dialogflow_cx_start` api command into mediajam's
// `dialogflow.start` control message, and mediajam streams the caller's
// audio to Dialogflow and plays the agent's synthesized reply back.
//
// What this proves end-to-end (the whole chain that broke with
// "mediajam: api command not supported: dialogflow_cx_start"):
//
//	caller speaks (our WAV)  ==RTP==>
//	feature-server dialogflow task  --ep.api(dialogflow_cx_start)-->
//	@jambonz/mrf endpoint shim      --dialogflow.start / audio-->
//	mediajam dialogflow session     --StreamingDetectIntent--> Google CX
//	Google CX  --audio_provided--> mediajam --play--> caller (our recording)
//
// This CX agent's start page replies to the caller's FIRST turn of speech
// with a greeting (verified out-of-band: a REST detectIntent with
// {"text":"hello"} returns "Hi! How are you doing?"). It does NOT define a
// named welcome/WELCOME event handler — sending one gets a CX
// "No handler is defined for the event" error. So the realistic drive is
// exactly what a human caller does: speak first, then assert the agent's
// spoken reply comes back over the SIP leg.
//
// We record the caller-side audio, speak a "hello" prompt, wait for the
// agent's TTS reply, and assert via Deepgram STT (independent of
// Dialogflow's own recognition) that the greeting words land. That proves
// the full media round-trip, not a circular read of Dialogflow's events.
//
// Skips cleanly (passes) when Dialogflow is not configured — see
// cfg.HasDialogflow(): needs DIALOGFLOW_KEYFILE + DIALOGFLOW_PROJECT +
// DIALOGFLOW_AGENT + DIALOGFLOW_REGION. Also needs DEEPGRAM_API_KEY (to
// pre-gen the prompt WAV and STT-verify the reply) and NGROK_AUTHTOKEN.
package verbs

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// dialogflowPrompt is what the caller says to open the conversation. The CX
// agent's start page greets on the first turn regardless of the exact words,
// so a plain greeting is enough to trigger a reply.
const dialogflowPrompt = "Hello there."

// dialogflowReplyKeywords are content words the CX agent's start-page
// greeting uses. The agent varies its phrasing turn to turn — observed live:
// "Hi! How are you doing?", "Good day! What can I do for you today?",
// "Greetings, how can I assist?" — so this pools the content words across
// those variants. Deepgram STT on a telephony clip can drop a word, so we
// require only ONE via AssertTranscriptHasMost(..., 1, ...): the contract
// under test is "the agent spoke a reply back over the SIP leg", not its
// exact wording.
var dialogflowReplyKeywords = []string{
	"hi", "how", "doing", "day", "good", "help", "today",
	"greetings", "assist", "can", "you",
}

// TestVerb_Dialogflow_CX — drive a real Dialogflow CX agent through the
// dialogflow verb: speak a greeting and assert the agent speaks a reply back
// over the caller leg.
//
// Steps:
//  1. preflight-skips — skip unless Dialogflow + Deepgram are configured
//  2. ensure-prompt-wav — Deepgram-TTS the "hello" prompt (cached on disk)
//  3. register-webhook-session
//  4. script-dialogflow-verb — call_hook=[answer, pause, dialogflow(cx), hangup]
//  5. place-call
//  6. answer-and-record — 200 OK, start recording, prime with silence
//  7. wait-for-recognizer — let the CX audio stream arm
//  8. speak-prompt — stream the "hello" WAV, then trailing silence
//  9. wait-for-reply — let CX detect the turn + stream its reply audio back
// 10. assert-reply-audio — Deepgram STT the recording, assert greeting words
// 11. hangup-and-wait-ended
// 12. drain-callbacks — best-effort eventHook/actionHook capture
// 13. assert-event-plumbing — dialogflow eventHook fired (soft check)
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
		t.Skip("dialogflow test needs DEEPGRAM_API_KEY (pre-gen the prompt WAV + STT-verify the reply)")
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
	s.Logf("prompt wav: %s", promptWAV)
	s.Done()

	testID, sess := claimSession(t)

	s = Step(t, "script-dialogflow-verb")
	// No welcomeEvent: this CX agent has no named welcome-event handler and
	// greets on the caller's first spoken turn instead. eventHook carries
	// the X-Test-Id query param so per-event callbacks (which don't include
	// callInfo) correlate back to this session. actionHook fires when the
	// dialogflow session ends; ack it empty so jambonz doesn't chain
	// follow-up verbs.
	dfVerb := V("dialogflow",
		"credentials", cfg.DialogflowServiceKey,
		"project", cfg.DialogflowProject,
		"agent", cfg.DialogflowAgent,
		"region", cfg.DialogflowRegion,
		"model", "cx",
		"lang", cfg.DialogflowLang,
		"actionHook", webhookSrv.PublicURL()+"/action/dialogflow",
		"eventHook", SessionURL(sess, "dialogflow-event"),
		"events", []string{"intent", "transcription", "start-play", "stop-play"},
	)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		dfVerb,
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dialogflow", "dialogflow-event")
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
	// Prime the RTP path with silence so the recording is open and the media
	// leg has latched before we speak.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	// Let the CX audio-input stream arm before the first syllable, or the
	// leading word gets clipped and CX may not endpoint the turn.
	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "speak-prompt")
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	// Trailing silence so CX's single-utterance endpointer fires and the
	// agent is triggered to reply.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-reply")
	// CX detect-intent + fulfillment audio + mediajam file + jambonz play
	// round-trip. Keep sending silence so the recording stays open while the
	// reply streams back.
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(6 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-reply-audio")
	// Independent verification: Deepgram STT on the caller-side recording
	// must hear the CX agent's spoken reply. The agent greets with one of a
	// few phrasings, so require at least 1 greeting content word.
	AssertTranscriptHasMost(s, ctx, recPath, 1, dialogflowReplyKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "drain-callbacks")
	cbs := DrainCallbacks(sess, 5*time.Second)
	s.Logf("captured %d hook callbacks", len(cbs))
	s.Done()

	s = Step(t, "assert-event-plumbing")
	// Soft check: confirm the dialogflow eventHook fired at all (start-play
	// when the reply audio begins, or intent/transcription). Proves the
	// dialogflow_cx::* event → verb:hook plumbing works; orthogonal to the
	// reply-audio assertion above, so a miss is a warning, not a failure.
	var events []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == "action/dialogflow-event" {
			events = append(events, cb)
		}
	}
	if len(events) == 0 {
		s.Logf("WARNING: no dialogflow eventHook callbacks captured (media round-trip still asserted above)")
	} else {
		var types []string
		for _, cb := range events {
			types = append(types, cb.String("event"))
			if id := cb.NestedString("customer_data.x_test_id"); id != "" && id != testID {
				s.Errorf("eventHook customer_data.x_test_id=%q want %q", id, testID)
			}
		}
		s.Logf("dialogflow event types: %v", types)
	}
	s.Done()
}
