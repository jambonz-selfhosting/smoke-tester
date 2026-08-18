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
	"context"
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

// TestVerb_Speechmatics_PassThrough_Gather — stream the Spanish reservation
// clip into `gather input=[speech]` and assert the vendor's transcript and
// per-word structure both survive the round trip.
//
// Steps:
//  1. script-gather-speechmatics
//  2. place-call
//  3. answer-and-silence
//  4. wait-for-recognizer
//  5. send-wav
//  6. post-speech-silence
//  7. wait-action-gather-callback
//  8. assert-vendor-results-reachable
//  9. assert-transcript-passthrough
func TestVerb_Speechmatics_PassThrough_Gather(t *testing.T) {
	if !cfg.HasSpeechmatics() || speechmaticsLabel == "" {
		t.Log("SPEECHMATICS_API_KEY not set — passing without exercising speechmatics STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-gather-speechmatics")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 25,
			"actionHook", actionURL,
			"recognizer", speechmaticsRecognizer()),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "gather")
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
	speechEnd := time.Now()
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 60*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	// The turn boundary is the vendor's EndOfUtterance; it only fires once
	// the caller actually stops, so this lands shortly after the audio ends
	// rather than at any mid-clip pause. Logged because it is the number
	// applications ask about.
	s.Logf("actionHook at +%s after start of speech (+%s after it ended)",
		time.Since(speechStart).Round(time.Millisecond),
		time.Since(speechEnd).Round(time.Millisecond))
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-vendor-results-reachable")
	results, shape, err := speechmaticsResults(cb)
	if err != nil {
		s.Errorf("vendor results[] not reachable in the payload: %v", err)
	} else {
		s.Logf("speech.vendor.evt shape=%s, %d results[] entries", shape, len(results))
		assertResultEntriesComplete(s, results)
	}
	s.Done()

	s = Step(t, "assert-transcript-passthrough")
	got := extractTranscript(cb)
	want, err := speechmaticsVendorTranscript(cb)
	if err != nil {
		s.Fatalf("cannot rebuild the vendor transcript from the payload: %v", err)
	}
	if strings.TrimSpace(want) == "" {
		s.Fatalf("vendor transcript is empty; payload: %s", string(cb.Body))
	}
	s.Logf("vendor metadata.transcript (verbatim): %q", want)
	s.Logf("jambonz returned                     : %q", got)
	if strings.TrimSpace(got) != strings.TrimSpace(want) {
		s.Errorf("jambonz mutated the vendor transcript:\n  vendor : %q\n  jambonz: %q", want, got)
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
	var hooks []webhook.Callback
	deadline := time.Now().Add(30 * time.Second)
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
