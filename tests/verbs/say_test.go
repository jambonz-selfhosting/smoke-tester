// Tests for the `say` verb.
//
// Schema: schemas/verbs/say — `text` (string or array-of-strings) is the
// usual required payload. Optional: `loop` (number or "forever"),
// `synthesizer` override (vendor/voice/language), `earlyMedia` (needs 183).
//
// earlyMedia coverage is deferred — needs diago media-session init plumbing
// we don't yet expose.
//
// maxDur sizing: AudioDuration() counts every inbound RTP sample from answer
// to BYE — including the silence the media server sends before speech starts.
// With WithWarmup the script is [answer, pause 1s, say], and a streaming-TTS
// media server then spends ~0.5s dialing the vendor before first audio, so
// every recording carries ~1.5s of leading silence on top of the spoken
// length (measured on the mediajam cluster: dial ~510ms, vendor ttfb ~190ms,
// + the deliberate 1s warmup pause). The upper bounds below are therefore the
// spoken length plus generous headroom for that fixed startup overhead and
// per-run jitter — they exist to catch runaway/looping playback, not to pin
// exact duration. Lower bounds + transcript assertions are the real content
// guards.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Say_Basic — plain text utterance. Transcript should echo the text.
func TestVerb_Say_Basic(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-basic",
		minDur:     1 * time.Second,
		// ~2s spoken + ~1.5s startup overhead; 9s leaves headroom for jitter.
		maxDur:    9 * time.Second,
		verb:      V("say", "text", "Hello from jambonz integration tests."),
		wantWords: []string{"hello", "jambonz", "integration"},
	})
}

// TestVerb_Say_Stream — `say` with `stream: true` (incremental streaming
// TTS). feature-server rejects streaming say unless the application runs
// over the jambonz WebSocket API (lib/tasks/say.js: "streaming say verb
// requires applications to use the websocket API"), so this test drives the
// call through the WS app (placeWSCallTo → wsApp with a wss:// call_hook)
// rather than the HTTP paths the other say tests use.
//
// The suite's Deepgram credential supports streaming synthesis. Content
// verification is the guard: the streamed audio must transcribe to the
// spoken text, proving the streaming path produced real speech (a broken
// stream yields silence / empty transcript — the failure mode we saw when
// this ran over HTTP).
func TestVerb_Say_Stream(t *testing.T) {
	t.Parallel()
	// Pin the vendor explicitly (Deepgram, which supports streaming
	// synthesis) so a pass proves the streaming path ran through a known
	// vendor rather than whatever the wsApp default happens to be.
	runStreamingSay(t, "say-stream", map[string]any{
		"vendor":   "deepgram",
		"label":    deepgramLabel,
		"voice":    deepgramVoice,
		"language": "en-US",
	})
}

// runStreamingSay drives a `say` with stream:true over the WS app (required
// for streaming synthesis — see TestVerb_Say_Stream doc) using the given
// synthesizer override, then asserts the streamed audio transcribes to the
// spoken text. Shared by the per-vendor streaming say tests.
//
// Steps:
//  1. script-streaming-say — [answer, pause, say stream:true <synth>, hangup]
//  2. place-ws-call — place against wsApp (wss:// call_hook)
//  3. answer-record-and-wait-end — record PCM, send silence, block on end
//  4. assert-audio-duration — non-trivial audio in [1s, 9s]
//  5. assert-transcript — Deepgram transcript contains the spoken words
func runStreamingSay(t *testing.T, tag string, synth map[string]any) {
	t.Helper()
	runStreamingSayMax(t, tag, synth, 9*time.Second)
}

// runStreamingSayMax is runStreamingSay for a vendor whose normal first-audio latency needs more headroom.
func runStreamingSayMax(t *testing.T, tag string, synth map[string]any, maxDur time.Duration) {
	t.Helper()
	ctx := WithTimeout(t, 30*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-streaming-say")
	// closeStreamOnEmpty defaults true, and we send a single plain-text say
	// (no further tokens), so the stream drains and the verb completes;
	// hangup then ends the call deterministically.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say",
			"text", "Streaming synthesis is working correctly.",
			"stream", true,
			"synthesizer", synth),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(30))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	// ~2s spoken + ~1.5s streaming-TTS startup overhead
	AssertAudioDuration(s, call, 1*time.Second, maxDur, tag)
	s.Done()

	if wav != "" {
		s = Step(t, "assert-transcript")
		AssertTranscriptContains(s, ctx, wav, "streaming", "working correctly")
		s.Done()
	}
}

// sayOpts bundles the per-test knobs runSay needs.
type sayOpts struct {
	ctxTimeout       time.Duration
	tag              string
	minDur           time.Duration
	maxDur           time.Duration
	verb             map[string]any
	wantWords        []string // substrings expected anywhere in the transcript
	wantWordsOrdered []string // substrings expected to appear IN ORDER (array tests)
	wantAnyOf        []string // exactly one of these substrings must appear
	wantSilenceMS    int      // SSML break tests: longest silence window must be >= this
	extras           []func(*provision.CallCreate)
}

// runSay places a warmup-paused say call, answers, records, sends silence,
// waits for BYE, asserts audio duration, and if DEEPGRAM_API_KEY is set,
// runs the recording through Deepgram and asserts wantWords appear in the
// transcript.
//
// Steps (shared by all TestVerb_Say_* variants):
//  1. place-call — POST /Calls with [answer, pause, say <opts.verb>]
//  2. answer-record-and-wait-end — record PCM, send silence, block on end
//  3. assert-audio-duration — duration within [minDur, maxDur], RMS non-trivial
//  4. assert-transcript — Deepgram transcript contains wantWords (skipped if
//     DEEPGRAM_API_KEY unset or wantWords empty)
func runSay(t *testing.T, o sayOpts) {
	t.Helper()
	ctx := WithTimeout(t, o.ctxTimeout)
	uas := claimUAS(t, ctx)

	s := Step(t, "place-call")
	call := placeCallTo(ctx, t, uas, WithWarmup([]map[string]any{o.verb}), o.extras...)
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(o.tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, o.minDur, o.maxDur, o.tag)
	s.Done()

	if wav != "" && len(o.wantWords) > 0 {
		s = Step(t, "assert-transcript")
		AssertTranscriptContains(s, ctx, wav, o.wantWords...)
		s.Done()
	}
	if wav != "" && len(o.wantWordsOrdered) > 0 {
		s = Step(t, "assert-transcript-ordered")
		AssertTranscriptContainsInOrder(s, ctx, wav, o.wantWordsOrdered...)
		s.Done()
	}
	if wav != "" && len(o.wantAnyOf) > 0 {
		s = Step(t, "assert-transcript-any-of")
		AssertTranscriptHasAnyOf(s, ctx, wav, o.wantAnyOf...)
		s.Done()
	}
	if wav != "" && o.wantSilenceMS > 0 {
		s = Step(t, "assert-silence-window")
		// SSML <break time="500ms"/> must produce a measurable quiet
		// gap. Threshold 200 (≈ -50 dBFS) marks "true silence" while
		// tolerating mild line noise. We require the longest silence
		// window to be >= wantSilenceMS — a regression that drops the
		// break tag would render "Hello world" with TTS-natural pauses
		// only (~50-100ms) and fail.
		got, err := LongestSilenceMS(wav, 200)
		if err != nil {
			s.Fatalf("LongestSilenceMS: %v", err)
		}
		s.Logf("longest silence window: %dms (want >= %dms)", got, o.wantSilenceMS)
		if got < o.wantSilenceMS {
			s.Errorf("SSML <break> not honored: longest silence %dms < %dms",
				got, o.wantSilenceMS)
		}
		s.Done()
	}
}
