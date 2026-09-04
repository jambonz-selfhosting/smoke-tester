// Tests for how jambonz surfaces Speechmatics results to an application,
// driven by a 14.8s Spanish restaurant-reservation clip at the harness's
// mono/8kHz/16-bit telephony format.
//
// What these pin, and why the shape assertion looks the way it does:
//
// Speechmatics streams a turn as many `AddTranscript` messages, and
// jambonz consolidates them into one transcript per turn. That leaves
// `speech.vendor.evt` with TWO shapes, and both are current, documented
// behaviour that applications must handle:
//
//	single AddTranscript  -> the raw vendor object; results at
//	                         speech.vendor.evt.results
//	many AddTranscripts   -> an array of jambonz-normalized objects; the raw
//	                         vendor payload sits one level deeper, at
//	                         speech.vendor.evt[i].vendor.evt.results
//
// With the max_delay jambonz sends by default the engine finalizes roughly
// per word, so the array shape is what an application sees essentially
// always — a 1.3s "the sun is shining" already arrives as 4 segments.
//
// These tests therefore assert what actually matters: no vendor data is
// lost in either shape. Every `results[]` entry the vendor sent must reach
// the application with its start_time / end_time / confidence / type
// (and attaches_to / is_eos on punctuation), and the transcript jambonz
// returns must be the vendor's own `metadata.transcript` strings
// concatenated verbatim — no inserted punctuation, no re-casing.
//
// They do NOT assert that `speech.vendor.evt` is the raw object: flattening
// the consolidated shape would break every application reading the array
// today.
//
// speechmatics is an OPTIONAL vendor: without SPEECHMATICS_API_KEY these
// tests log and return (never t.Skip, never fail), matching the convention
// in speechmatics_stt_test.go.
package verbs

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const spanishWAV = "testdata/es_reservation.wav"

// speechmaticsRecognizer is the recognizer block these tests use. Deliberately
// minimal — no speechmaticsOptions — so we exercise the defaults jambonz
// forces onto StartRecognition rather than a tuned configuration.
func speechmaticsRecognizer() map[string]any {
	return map[string]any{
		"vendor":   "speechmatics",
		"label":    speechmaticsLabel,
		"language": "es",
	}
}

// TestVerb_Speechmatics_PassThrough_Verbatim — stream the Spanish reservation
// clip through `transcribe` and assert jambonz hands back the vendor's own
// transcript, character for character.
//
// This was a gather test until the fixture outgrew the verb. gather ends its
// turn at the first EndOfUtterance — that is what endpointing is — and this
// clip carries a 0.60s mid-utterance gap against a 0.5s default trigger, so
// the turn resolved mid-clip, the script's hangup ran, and SendWAV died
// writing to a closed socket ~8s of audio short. transcribe keeps the session
// open across turns, which is what a 14.8s multi-sentence clip needs.
//
// Steps:
//  1. script-transcribe-speechmatics
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-recognizer
//  5. send-wav
//  6. post-speech-silence
//  7. collect-transcription-hooks
//  8. assert-transcript-passthrough
func TestVerb_Speechmatics_PassThrough_Verbatim(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-speechmatics")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", SessionURL(sess, "transcription"),
			"recognizer", speechmaticsRecognizer()),
		V("pause", "length", 30),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	wavPath := resolveFixture(t, spanishWAV)
	speechStart := time.Now()
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hooks")
	hooks := collectTranscriptionHooks(s, sess, speechStart, 30*time.Second)
	s.Done()

	s = Step(t, "assert-transcript-passthrough")
	if len(hooks) == 0 {
		s.Fatalf("no transcriptionHook payloads received within 30s")
	}
	var compared int
	for i, cb := range hooks {
		want, err := speechmaticsVendorTranscript(cb)
		if err != nil || strings.TrimSpace(want) == "" {
			continue // an event-only hook carries no vendor transcript to compare
		}
		got := extractTranscript(cb)
		compared++
		s.Logf("hook[%d] vendor %q / jambonz %q", i, want, got)
		if strings.TrimSpace(got) != strings.TrimSpace(want) {
			s.Errorf("jambonz mutated the vendor transcript:\n  vendor : %q\n  jambonz: %q", want, got)
		}
	}
	if compared == 0 {
		s.Fatalf("no hook carried a vendor transcript to compare against")
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Speechmatics_PassThrough_Transcribe — same clip through the
// `transcribe` verb: every transcriptionHook payload must expose the
// vendor's results[], in whichever of the two shapes it arrives.
//
// Steps:
//  1. script-transcribe-speechmatics
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-recognizer
//  5. send-wav
//  6. post-speech-silence
//  7. collect-transcription-hooks
//  8. assert-every-hook-exposes-results
func TestVerb_Speechmatics_PassThrough_Transcribe(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-speechmatics")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", speechmaticsRecognizer()),
		V("pause", "length", 30),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	wavPath := resolveFixture(t, spanishWAV)
	speechStart := time.Now()
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hooks")
	hooks := collectTranscriptionHooks(s, sess, speechStart, 30*time.Second)
	s.Done()

	s = Step(t, "assert-every-hook-exposes-results")
	if len(hooks) == 0 {
		s.Fatalf("no transcriptionHook payloads received within 30s")
	}
	for i, cb := range hooks {
		results, shape, err := speechmaticsResults(cb)
		if err != nil {
			s.Errorf("hook[%d] exposes no vendor results[]: %v", i, err)
			continue
		}
		s.Logf("hook[%d] shape=%s, %d results[] entries", i, shape, len(results))
		assertResultEntriesComplete(s, results)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// assertResultEntriesComplete checks the fields applications consume off
// each results[] entry. attaches_to / is_eos are only present on
// punctuation entries, so they are asserted there and not on words.
func assertResultEntriesComplete(s *StepCtx, results []map[string]any) {
	if len(results) == 0 {
		s.Errorf("vendor results[] is empty")
		return
	}
	var sawWord, sawPunctuation bool
	for i, r := range results {
		for _, want := range []string{"start_time", "end_time", "type", "alternatives"} {
			if _, ok := r[want]; !ok {
				s.Errorf("results[%d] is missing %q — got keys %v", i, want, keysOf(r))
			}
		}
		switch r["type"] {
		case "word":
			sawWord = true
			if alts, ok := r["alternatives"].([]any); ok && len(alts) > 0 {
				if alt, ok := alts[0].(map[string]any); ok {
					if _, ok := alt["confidence"]; !ok {
						s.Errorf("results[%d].alternatives[0] has no confidence — got keys %v", i, keysOf(alt))
					}
				}
			}
		case "punctuation":
			sawPunctuation = true
			for _, want := range []string{"attaches_to", "is_eos"} {
				if _, ok := r[want]; !ok {
					s.Errorf("punctuation results[%d] is missing %q — got keys %v", i, want, keysOf(r))
				}
			}
		}
	}
	if !sawWord {
		s.Errorf("vendor results[] carried no word entries at all (%d entries)", len(results))
	}
	// The clip ends in "Hasta luego." so the engine must have emitted at
	// least one punctuation entry; its absence means punctuation entries are
	// being dropped somewhere in the chain.
	if !sawPunctuation {
		s.Errorf("vendor results[] carried no punctuation entries; the fixture is a punctuated sentence")
	}
}

// speechmaticsResults collects every raw Speechmatics results[] entry
// reachable under speech.vendor.evt, in order, and names the shape it
// found. Handles both shapes jambonz emits (see the file header).
func speechmaticsResults(cb webhook.Callback) ([]map[string]any, string, error) {
	evt := cb.NestedAny("speech.vendor.evt")
	if evt == nil {
		return nil, "", fmt.Errorf("speech.vendor.evt absent")
	}
	shape := "raw-object"
	if _, isArray := evt.([]any); isArray {
		shape = "consolidated-array"
	}
	var out []map[string]any
	forEachVendorEvent(evt, func(raw map[string]any) {
		arr, _ := raw["results"].([]any)
		for _, e := range arr {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
	})
	if out == nil {
		body, _ := json.Marshal(evt)
		return nil, shape, fmt.Errorf("no results[] found under speech.vendor.evt (shape=%s): %s", shape, body)
	}
	return out, shape, nil
}

// speechmaticsVendorTranscript rebuilds what Speechmatics actually said by
// concatenating every raw `metadata.transcript` in order. Those strings
// carry their own leading/trailing spaces and are designed to be joined
// with no separator, so this is the vendor's text verbatim.
func speechmaticsVendorTranscript(cb webhook.Callback) (string, error) {
	evt := cb.NestedAny("speech.vendor.evt")
	if evt == nil {
		return "", fmt.Errorf("speech.vendor.evt absent")
	}
	var sb strings.Builder
	forEachVendorEvent(evt, func(raw map[string]any) {
		if md, ok := raw["metadata"].(map[string]any); ok {
			if s, ok := md["transcript"].(string); ok {
				sb.WriteString(s)
			}
		}
	})
	if sb.Len() == 0 {
		body, _ := json.Marshal(evt)
		return "", fmt.Errorf("no metadata.transcript found under speech.vendor.evt: %s", body)
	}
	return sb.String(), nil
}

// forEachVendorEvent walks speech.vendor.evt and calls fn for each raw
// Speechmatics message it finds, in order. A raw message is recognised by
// its `metadata` key; the consolidated shape nests one under each element's
// own `vendor.evt`.
func forEachVendorEvent(v any, fn func(map[string]any)) {
	switch n := v.(type) {
	case []any:
		for _, e := range n {
			forEachVendorEvent(e, fn)
		}
	case map[string]any:
		if _, ok := n["metadata"]; ok {
			fn(n)
			return
		}
		if inner, ok := n["vendor"].(map[string]any); ok {
			forEachVendorEvent(inner["evt"], fn)
		}
	}
}

func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestVerb_Speechmatics_PermittedMarks — punctuation_overrides.permitted_marks
// must reach the vendor intact, including a mark that is itself a comma.
//
// The list travels as one channel variable, so a comma-separated encoding
// cannot carry it: [",", "?", "!"] left feature-server as ",,?,!" and the media
// server split it back to ["?", "!"]. Losing the comma is not cosmetic — the
// engine substitutes full stops and marks them is_eos, so one sentence arrives
// as two. JSON is the encoding now.
//
// transcribe rather than gather: the fixture's 0.60s mid-utterance gap ends a
// gather turn mid-clip, and the marks worth counting are spread across the
// whole 14.8s, so the counts are aggregated over every hook.
//
// Steps:
//  1. script-transcribe-permitted-marks
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-recognizer
//  5. send-wav
//  6. post-speech-silence
//  7. collect-transcription-hooks
//  8. assert-permitted-marks-honoured
func TestVerb_Speechmatics_PermittedMarks(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-permitted-marks")
	rec := speechmaticsRecognizer()
	rec["speechmaticsOptions"] = map[string]any{
		"transcription_config": map[string]any{
			"punctuation_overrides": map[string]any{
				"permitted_marks": []any{",", "?", "!"},
			},
		},
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", SessionURL(sess, "transcription"),
			"recognizer", rec),
		V("pause", "length", 30),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	speechStart := time.Now()
	if err := call.SendWAV(resolveFixture(t, spanishWAV)); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hooks")
	hooks := collectTranscriptionHooks(s, sess, speechStart, 30*time.Second)
	s.Done()

	s = Step(t, "assert-permitted-marks-honoured")
	if len(hooks) == 0 {
		s.Fatalf("no transcriptionHook payloads received within 30s")
	}
	marks := map[string]int{}
	eosOnComma := 0
	var sawResults bool
	for _, cb := range hooks {
		results, _, err := speechmaticsResults(cb)
		if err != nil {
			continue // event-only hook
		}
		sawResults = true
		for _, r := range results {
			if r["type"] != "punctuation" {
				continue
			}
			alts, _ := r["alternatives"].([]any)
			if len(alts) == 0 {
				continue
			}
			alt, _ := alts[0].(map[string]any)
			content, _ := alt["content"].(string)
			marks[content]++
			if content == "," && r["is_eos"] == true {
				eosOnComma++
			}
		}
	}
	if !sawResults {
		s.Fatalf("no hook exposed vendor results[]")
	}
	s.Logf("punctuation seen across %d hook(s): %v", len(hooks), marks)

	if marks[","] == 0 {
		s.Errorf(`permitted_marks included "," but the engine emitted none: %v — `+
			`the mark list is being mangled between the verb and the vendor`, marks)
	}
	if marks["."] > 0 {
		s.Errorf(`permitted_marks excluded "." but the engine emitted %d — `+
			`the mark list is not reaching the vendor at all`, marks["."])
	}
	// commas are mid-sentence, so they must not carry is_eos; an application
	// splitting sentences on is_eos depends on this.
	if eosOnComma > 0 {
		s.Errorf("%d comma entries carry is_eos=true", eosOnComma)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// collectTranscriptionHooks drains action/transcription payloads for d, logging
// each with its offset from the start of speech.
func collectTranscriptionHooks(s *StepCtx, sess *webhook.Session, speechStart time.Time, d time.Duration) []webhook.Callback {
	var hooks []webhook.Callback
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		for _, drain := range sessionsToDrain(sess) {
			if cb, err := tryPop(drain); err == nil && cb.Hook == "action/transcription" {
				s.Logf("+%s action/transcription: %s",
					time.Since(speechStart).Round(time.Millisecond), string(cb.Body))
				hooks = append(hooks, cb)
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Logf("collected %d transcriptionHook payload(s)", len(hooks))
	return hooks
}

// TestVerb_Speechmatics_EndOfUtterance — the vendor's EndOfUtterance must reach
// the application, and it must arrive as a recognisable event.
//
// transcribe used to have no handler for it at all: stt-task's event map points
// EndOfUtterance at this._onEndOfUtterance, only TaskGather implemented it, so
// for transcribe the listener registered as undefined and every event was
// dropped. The turn could then only end on the asr silence timer — measured at
// +21.5s after speech with asrTimeout 20s, against +1.5s once the handler
// consumed the event.
//
// Two things are pinned here, and each has already been broken once:
//
//   - at least one speech_event with type EndOfUtterance reaches the hook. This
//     needs recognizer.interim; without it the event is consumed internally as
//     the turn boundary and never posted.
//   - it is named. _resolve keys speech_event off `type`, which the raw
//     Speechmatics message does not carry, so an unnamed event produced a
//     webhook body with neither speech nor speech_event in it — delivered, and
//     invisible.
//
// Transcript payloads must keep flowing alongside, so the extra hook is additive
// rather than displacing them.
//
// The trigger is pinned at 0.4s: on this fixture the mid-utterance gap measures
// 0.60s on word timings, so 0.4 has margin while the 0.5 default sits 0.1s from
// the edge and fires only sometimes.
//
// Steps:
//  1. script-transcribe-interim
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-recognizer
//  5. send-wav-async
//  6. collect-hooks
//  7. assert-end-of-utterance-surfaced
func TestVerb_Speechmatics_EndOfUtterance(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-interim")
	rec := speechmaticsRecognizer()
	rec["interim"] = true
	rec["speechmaticsOptions"] = map[string]any{
		"transcription_config": map[string]any{
			"conversation_config": map[string]any{"end_of_utterance_silence_trigger": 0.4},
		},
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe", "transcriptionHook", SessionURL(sess, "transcription"), "recognizer", rec),
		V("pause", "length", 40),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-recognizer")
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	// Streamed in the background: an end-of-utterance can land mid-clip, and
	// blocking until the WAV finishes would hide it.
	s = Step(t, "send-wav-async")
	go func() {
		if err := call.SendWAV(resolveFixture(t, spanishWAV)); err != nil {
			GoroutineFailf(t, "wav-sender", "SendWAV: %v", err)
		}
		_ = call.SendSilence()
	}()
	s.Done()

	s = Step(t, "collect-hooks")
	var (
		events     []map[string]any
		transcript int
		neither    int
	)
	deadline := time.Now().Add(32 * time.Second)
	for time.Now().Before(deadline) {
		for _, drain := range sessionsToDrain(sess) {
			cb, err := tryPop(drain)
			if err != nil || cb.Hook != "action/transcription" {
				continue
			}
			var body map[string]any
			if err := json.Unmarshal(cb.Body, &body); err != nil {
				continue
			}
			switch {
			case body["speech_event"] != nil:
				ev, _ := body["speech_event"].(map[string]any)
				events = append(events, ev)
				b, _ := json.Marshal(ev)
				s.Logf("speech_event: %s", string(b))
			case body["speech"] != nil:
				transcript++
			default:
				neither++
				s.Logf("hook with no speech and no speech_event: %s", string(cb.Body))
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	s.Logf("collected %d speech_event, %d speech, %d neither", len(events), transcript, neither)
	s.Done()

	s = Step(t, "assert-end-of-utterance-surfaced")
	if len(events) == 0 {
		s.Errorf("no speech_event reached the transcriptionHook in 32s — the vendor's " +
			"EndOfUtterance is not being surfaced to the application")
	}
	for i, ev := range events {
		if ev["type"] != "EndOfUtterance" {
			s.Errorf("speech_event[%d].type = %v, want EndOfUtterance", i, ev["type"])
		}
		if _, ok := ev["metadata"].(map[string]any); !ok {
			s.Errorf("speech_event[%d] carries no metadata: %v", i, ev)
		}
	}
	// A hook with neither key is the failure mode of an unnamed event: posted,
	// but with nothing in it the application can act on.
	if neither > 0 {
		s.Errorf("%d hook(s) arrived carrying neither speech nor speech_event", neither)
	}
	if transcript == 0 {
		s.Errorf("no transcript payloads arrived; the event hook displaced them")
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}
