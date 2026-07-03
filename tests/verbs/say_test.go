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
		maxDur: 9 * time.Second,
		verb:      V("say", "text", "Hello from jambonz integration tests."),
		wantWords: []string{"hello", "jambonz", "integration"},
	})
}

// TestVerb_Say_SSML — SSML markup renders without error; both sides of
// the <break> land in the transcript AND the recording carries a
// measurable silence window matching the break tag. Without the
// silence-window assertion, a regression that strips SSML and renders
// the text as plain prose would still pass on word content (since
// "hello" + "world" are present either way).
func TestVerb_Say_SSML(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-ssml",
		// "Hello" + 500ms break + "world" → observed ~900ms on this cluster;
		// TTS voices compress short utterances. Upper bound covers the ~1.5s
		// streaming-TTS startup overhead on top of the short utterance.
		minDur: 500 * time.Millisecond,
		maxDur: 9 * time.Second,
		verb:      V("say", "text", "<speak>Hello <break time='500ms'/> world.</speak>"),
		wantWords: []string{"hello", "world"},
		// We asked for 500ms of silence in the middle. TTS engines often
		// shorten the pause slightly under prosody compression; require
		// at least 250ms — well above natural inter-word pauses (~50-
		// 100ms) so a regression that drops the break tag fails.
		wantSilenceMS: 250,
	})
}

// TestVerb_Say_LongText — multi-sentence text; transcript should include a
// representative phrase from the middle.
//
// Skipped under `go test -short` because this test pays ~15s of real TTS
// wall-clock and the shorter `say` variants already cover the code path.
// Full release gate runs it; inner-loop `-short` skips it.
func TestVerb_Say_LongText(t *testing.T) {
	t.Parallel()
	if testing.Short() {
		t.Skip("skipping in -short mode: 15s TTS wall-clock; shorter say tests cover the code path")
	}
	long := "The quick brown fox jumps over the lazy dog. " +
		"The five boxing wizards jump quickly. " +
		"Pack my box with five dozen liquor jugs. " +
		"How vexingly quick daft zebras jump."
	runSay(t, sayOpts{
		ctxTimeout: 60 * time.Second,
		tag:        "say-long",
		minDur:     8 * time.Second,
		maxDur:     30 * time.Second,
		verb:       V("say", "text", long),
		extras:     []func(*provision.CallCreate){withTimeLimit(45)},
		wantWords:  []string{"quick brown fox", "boxing wizards"},
	})
}

// TestVerb_Say_ArrayRandom — text as array-of-strings. The schema
// documents "one entry is selected at random", but the current cluster
// plays ALL entries sequentially. Assert all three markers land so a
// regression that drops one of the entries (or drops the array path
// entirely) fails. Each phrase carries a unique phonetic-alphabet
// marker (alpha/bravo/charlie) so STT can distinguish them.
//
// maxDur sized for three concatenated phrases.
func TestVerb_Say_ArrayRandom(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-array",
		minDur:     1 * time.Second,
		// Three concatenated phrases (~10s spoken) + ~1.5s startup overhead.
		maxDur: 16 * time.Second,
		verb: V("say", "text", []any{
			"Number one apple.",
			"Number two banana.",
			"Number three cherry.",
		}),
		// All three markers must appear in order (cluster plays the
		// whole list sequentially). Fruits + ordinals chosen because
		// Deepgram nova-3 transcribes them reliably at telephony quality
		// (alpha/bravo/charlie occasionally drift to "alphet"/"brevo").
		wantWordsOrdered: []string{"apple", "banana", "cherry"},
		extras:           []func(*provision.CallCreate){withTimeLimit(30)},
	})
}

// TestVerb_Say_Loop2 — loop=2 produces roughly double the audio of loop=1.
// Transcript should contain the phrase and then repeat it (at least
// partially — first-word clipping is possible on the first pass).
func TestVerb_Say_Loop2(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 45 * time.Second,
		tag:        "say-loop2",
		// "one two three" ≈ 1s → loop=2 ≈ 2s + gap. Wide window for codec +
		// network variance, plus ~1.5s streaming-TTS startup overhead.
		minDur: 1500 * time.Millisecond,
		maxDur: 11 * time.Second,
		verb:   V("say", "text", "one two three.", "loop", 2),
		// Asserting the second pass's phrase is distinctive enough; if the
		// loop didn't run twice, we'd only see one copy and miss this.
		wantWords: []string{"two three one two three"},
	})
}

// TestVerb_Say_SynthesizerOverride — explicit vendor + voice. Transcript
// verifies the override didn't break content; voice identity isn't
// checkable via STT.
func TestVerb_Say_SynthesizerOverride(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-override",
		minDur:     1 * time.Second,
		// ~1.5s spoken + ~1.5s startup overhead; 8s leaves headroom.
		maxDur: 8 * time.Second,
		verb: V("say", "text", "Override voice test.",
			"synthesizer", map[string]any{
				"vendor":   "deepgram",
				"label":    deepgramLabel,
				"voice":    "aura-orion-en",
				"language": "en-US",
			}),
		wantWords: []string{"override voice test"},
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

// TestVerb_Say_Stream_Murf — the same streaming say path as
// TestVerb_Say_Stream, but pinned to the Murf.ai vendor (which has its own
// WebSocket streaming module). Optional vendor: skips (passes) with a
// credential-missing log when MURF_API_KEY is unset, exactly like
// TestVerb_Say_Murf. When the key is set it drives real Murf streaming over
// the WS app and asserts the transcript.
func TestVerb_Say_Stream_Murf(t *testing.T) {
	t.Parallel()
	if !cfg.HasMurf() || murfLabel == "" {
		t.Skip("Murf streaming say test needs MURF_API_KEY (credential missing — skipping, not a failure)")
	}
	runStreamingSay(t, "say-stream-murf", map[string]any{
		"vendor":   "murf",
		"label":    murfLabel,
		"voice":    murfVoice,
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
	// ~2s spoken + ~1.5s streaming-TTS startup overhead; 9s headroom.
	AssertAudioDuration(s, call, 1*time.Second, 9*time.Second, tag)
	s.Done()

	if wav != "" {
		s = Step(t, "assert-transcript")
		AssertTranscriptContains(s, ctx, wav, "streaming", "working correctly")
		s.Done()
	}
}

// TestVerb_Say_Murf — speak through the Murf.ai TTS vendor via a
// synthesizer override, using the Murf speech credential provisioned at
// TestMain. Murf is an optional vendor: when MURF_API_KEY is not set no
// credential is provisioned, and this test skips (which counts as a pass)
// with a credential-missing log rather than failing.
//
// Transcript verifies the override didn't break content; Murf voice
// identity isn't checkable via STT. Startup overhead / duration bounds
// mirror the Deepgram override test.
func TestVerb_Say_Murf(t *testing.T) {
	t.Parallel()
	if !cfg.HasMurf() || murfLabel == "" {
		t.Skip("Murf say test needs MURF_API_KEY (credential missing — skipping, not a failure)")
	}
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-murf",
		minDur:     1 * time.Second,
		// ~1.5s spoken + ~1.5s startup overhead; 9s leaves headroom.
		maxDur: 9 * time.Second,
		// Plain prose only — no "Murf" brand word: telephony-quality STT
		// mangles the coined name ("mers"), which is a transcription
		// artifact, not a synthesis failure. The words below transcribe
		// reliably and still prove the Murf voice spoke the text.
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", map[string]any{
				"vendor":   "murf",
				"label":    murfLabel,
				"voice":    murfVoice,
				"language": "en-US",
			}),
		wantWords: []string{"voice test", "working correctly"},
	})
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
