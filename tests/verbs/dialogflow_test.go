// Tests for the `dialogflow` verb — CX variant with client-side tool calls.
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
// Turn 2 gives a destination + date, which per the playbook triggers a second
// tool call (getFlights) with POPULATED input_parameters; we return a canned
// flight list and the agent reads it out. The playbook is an LLM, so turn 2
// occasionally skips the tool ("a run that doesn't call the tool is not
// necessarily a defect") — turn-2 tool assertions are soft (logged warnings),
// turn-1 assertions are strict.
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

// dialogflowPrompt2 answers the greeting with origin + destination + date.
// Origin is spoken explicitly even though getGeolocation already supplied it:
// the playbook occasionally ignores the tool result and re-asks for the
// departure city, and a self-contained turn keeps the flow moving either way.
const dialogflowPrompt2 = "I want to fly from New York to Paris on December fifth"

// dialogflowPrompt3 picks a flight from the read-out list; the playbook then
// confirms the booking with a generated booking number.
const dialogflowPrompt3 = "Please book the cheapest flight for me"

// Canned client-side tool outputs (the tools have no backend by design).
const (
	dialogflowGeoResult = `{"outputParameters":{"city":"New York","country":"United States","country_code":"us","postcode":"10001"}}`

	dialogflowFlightsResult = `{"outputParameters":{"flights":[` +
		`{"flight_number":"CA101","origin":"JFK","destination":"CDG","departure_time":"08:30","arrival_time":"21:45","price_usd":640},` +
		`{"flight_number":"CA205","origin":"JFK","destination":"CDG","departure_time":"17:10","arrival_time":"06:25","price_usd":545}]}}`
)

// dialogflowReplyKeywords: content words of the post-getGeolocation greeting.
// LLM phrasing varies, so require only ONE.
var dialogflowReplyKeywords = []string{
	"cymbal", "air", "helpdesk", "welcome", "flight", "where", "go", "help", "world",
}

// dialogflowTurn2Keywords: the flight read-out draws on our canned data
// (CA101/CA205, JFK->CDG, $640/$545) plus generic flight words. Require ONE.
var dialogflowTurn2Keywords = []string{
	"flight", "jfk", "cheapest", "leaves", "arrives", "stops", "dollars",
	"book", "option", "forty", "morning", "afternoon", "paris", "december",
}

// dialogflowTurn3Keywords: the booking confirmation ("your booking is
// confirmed, your booking number is X Y Z 1 2 3"). Require ONE.
var dialogflowTurn3Keywords = []string{
	"book", "booked", "booking", "confirm", "confirmed", "reference",
	"number", "success", "successfully", "reservation", "all set", "email",
}

// TestVerb_Dialogflow_CX — two-turn tool-call round trip through jambonz.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wavs (turn 1 + turn 2)
//  3. script-dialogflow-verb — toolHook answers per tool_call.action
//  4. place-call / answer-and-record
//  5. turn 1: "hi, I need a flight" -> getGeolocation -> greeting audio
//  6. turn 2: destination+date -> getFlights (populated params) -> flight list
//  7. turn 3: pick a flight -> booking confirmation audio
//  8. assert-tool-callbacks — getGeolocation strict; getFlights soft
//  9. hangup, event plumbing check
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
		t.Skip("dialogflow test needs DEEPGRAM_API_KEY (prompt WAVs + STT verification)")
	}
	s.Done()

	ctx := WithTimeout(t, 260*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wavs")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", dialogflowPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 1: %v", err)
	}
	prompt2WAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", dialogflowPrompt2, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 2: %v", err)
	}
	prompt3WAV, err := tts.EnsureWAV(ctx, "testdata/dialogflow", dialogflowPrompt3, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV turn 3: %v", err)
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
	// toolHook answers per requested action (raw JSON, not a verb script)
	sess.ScriptActionHookBodyFunc("dialogflow-tool", func(cb webhook.Callback) []byte {
		if cb.NestedString("tool_call.action") == "getFlights" {
			return []byte(dialogflowFlightsResult)
		}
		return []byte(dialogflowGeoResult)
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(170))
	s.Done()

	rec1 := filepath.Join(t.TempDir(), "turn1-greeting.wav")
	rec2 := filepath.Join(t.TempDir(), "turn2-flights.wav")

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.StartRecording(rec1); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-recognizer", RecognizerArmDelayLong)

	s = Step(t, "turn1-speak-prompt")
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "turn1-wait-for-reply")
	// STT endpoint + playbook LLM + toolHook RTT + tool-result turn + TTS +
	// greeting playback
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(16 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "turn1-assert-greeting")
	AssertTranscriptHasMost(s, ctx, rec1, 1, dialogflowReplyKeywords...)
	s.Done()

	s = Step(t, "turn2-speak-destination")
	if err := call.StartRecording(rec2); err != nil {
		s.Fatalf("StartRecording turn 2: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(prompt2WAV); err != nil {
		s.Fatalf("SendWAV turn 2: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "turn2-wait-for-reply")
	// getFlights round trip + the agent reading out two flights — the full
	// readout runs ~25-30s; speaking before it ends is lost (no active
	// listen stream during playback)
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(32 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "turn2-assert-reply")
	AssertTranscriptHasMost(s, ctx, rec2, 1, dialogflowTurn2Keywords...)
	s.Done()

	rec3 := filepath.Join(t.TempDir(), "turn3-booking.wav")

	s = Step(t, "turn3-book-flight")
	if err := call.StartRecording(rec3); err != nil {
		s.Fatalf("StartRecording turn 3: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(prompt3WAV); err != nil {
		s.Fatalf("SendWAV turn 3: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "turn3-wait-for-confirmation")
	// no tool on this turn: playbook LLM confirms + generates a booking number
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (reply window): %v", err)
	}
	time.Sleep(20 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "turn3-assert-booking-confirmed")
	AssertTranscriptHasMost(s, ctx, rec3, 1, dialogflowTurn3Keywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-tool-callbacks")
	cbs := DrainCallbacks(sess, 5*time.Second)
	var toolCBs []webhook.Callback
	for _, cb := range cbs {
		if cb.Hook == "action/dialogflow-tool" {
			toolCBs = append(toolCBs, cb)
		}
	}
	if len(toolCBs) == 0 {
		s.Fatalf("no toolHook callbacks captured in %d callbacks", len(cbs))
	}
	// turn 1 is deterministic: getGeolocation, no input parameters
	if action := toolCBs[0].NestedString("tool_call.action"); action != "getGeolocation" {
		s.Errorf("first tool_call.action = %q, want getGeolocation", action)
	}
	// turn 2 is LLM-dependent: assert getFlights when it fired, warn when not
	sawFlights := false
	for _, cb := range toolCBs[1:] {
		if cb.NestedString("tool_call.action") == "getFlights" {
			sawFlights = true
			// populated params look like {origin_airport_code, destination_airport_code,
			// travel_date, timezone_difference_minutes, flight_duration_minutes, ...}
			if date := cb.NestedString("tool_call.input_parameters.travel_date"); date == "" {
				s.Errorf("getFlights input_parameters not populated: %s", string(cb.Body))
			} else {
				s.Logf("getFlights populated: travel_date=%s origin=%s", date,
					cb.NestedString("tool_call.input_parameters.origin_airport_code"))
			}
		}
	}
	if !sawFlights {
		s.Logf("WARNING: getFlights did not fire this run (playbook LLM nondeterminism; turn-1 round trip still proven)")
	}
	s.Logf("tool callbacks: %d", len(toolCBs))
	s.Done()

	s = Step(t, "assert-event-plumbing")
	var types []string
	for _, cb := range cbs {
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
