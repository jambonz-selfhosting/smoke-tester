// Tests for the `say` verb.
//
// Schema: schemas/verbs/say — `text` (string or array-of-strings) is the
// usual required payload. Optional: `loop` (number or "forever"),
// `synthesizer` override (vendor/voice/language), `earlyMedia` (needs 183).
//
// earlyMedia coverage is deferred — needs diago media-session init plumbing
// we don't yet expose.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestVerb_Say_Basic — plain text utterance. Transcript should echo the text.
func TestVerb_Say_Basic(t *testing.T) {
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-basic",
		minDur:     1 * time.Second,
		maxDur:     6 * time.Second,
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
		// TTS voices compress short utterances.
		minDur:    500 * time.Millisecond,
		maxDur:    6 * time.Second,
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
		maxDur:     12 * time.Second,
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
		// network variance.
		minDur: 1500 * time.Millisecond,
		maxDur: 8 * time.Second,
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
		maxDur:     5 * time.Second,
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
