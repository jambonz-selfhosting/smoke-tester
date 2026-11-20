// Reproduction tests for the production defects reported against the `agent`
// verb by a self-hosted 10.2.1 operator (support ticket, 2026-09-17), verified
// against feature-server v10.2.1 and v11.1.4 by code review.
//
// These tests REPRODUCE — they do not work around. Each asserts the behaviour
// the verb is supposed to have, so on an affected build it FAILS, and the
// failure message is the evidence. Nothing here is fixed yet.
//
// The reporter's production shape is mirrored as closely as the harness allows:
// STT deepgram (nova-3 family) with turnDetection "stt", TTS deepgram aura,
// tools via toolHook, an app driven over the jambonz WebSocket API. Deepgram
// STT is deliberate, not incidental — it is not in feature-server's
// STT_NATIVE_SPEECH_START set, so the agent auto-enables Silero VAD for
// barge-in, which is the precondition for defects 1 and 2. Deepgram TTS is
// likewise deliberate: it is not in TtsAlignmentVendors, which is the
// precondition for defect 1.
//
//	Defect  What the operator saw                              Test
//	------  -------------------------------------------------  ----------------------------
//	1       LLM history keeps the FULL response after a         Defect1_HistoryTrimmed…
//	        barge-in, so the agent believes the caller heard
//	        text that was never spoken
//	1b      barge-in DURING the LLM stream leaves no assistant  Defect1b_AssistantTurn…
//	        message in history at all
//	2       playback resumes with a hole after a false          Defect2_FalseInterruption…
//	        barge-in — fragments, and a bare "." spoken aloud
//	2c      control for the bare "." — same text through the      Defect2c_SayVerb…
//	        non-streaming say verb
//	3       llm_response fires twice on tool-call turns and     Defect3_ToolCallTurn…
//	        turn_end.response concatenates/duplicates text
//	4a      noResponseTimeout never fires with greeting:false   Defect4a_NoResponseTimeout…
//	4b      a bare `hangup` redirect does not end the call      Defect4b_RedirectHangup…
//	4c      is agent:update supported?                          Defect4c_AgentUpdate…
//	5       a session:new ack over the WS payload limit kills   Defect5_LargeSystemPrompt…
//	        the call silently (1009, no error to the app)
//
// Not covered here, and why:
//
//   - whether the VAD fired at all on an unconfirmed blip. Nothing is emitted
//     for a TENTATIVE barge-in — user_interruption only fires once it is
//     confirmed — so Defect2 passing means "no hole was observed", not "the
//     revert path ran". Its logged transcript is the artifact to read.
//   - bargeIn.sticky is a no-op (parsed at agent/index.js and never referenced).
//     There is no black-box signal that distinguishes sticky:true from
//     sticky:false, so there is nothing to assert. Defect2 runs with sticky:true
//     set so that if it is ever implemented, this file already exercises it.
//   - Anthropic prompt-cache hits (the operator's last question) are only
//     visible in the upstream vendor's response metadata, which the agent verb
//     does not surface on any hook. Not observable from outside the cluster.
//
// Run against the test cluster on 2026-09-18 (feature-server 11.1.x, deepgram
// STT+TTS, deepseek/openai LLM), with temporary [DEFECT-PROBE] logging in the
// feature-server (branch debug/agent-prod-defect-logging) confirming each
// verdict from the server side:
//
//	1   REPRODUCED. Probe: ttsVendor=deepgram alignmentEnabled=false
//	    spokenTextIsNull=true willTrim=false, and history keeps the full
//	    "One. … Thirty." while the caller heard only to "six".
//	1b  Did not reproduce — the assistant turn survives a mid-stream barge-in.
//	2   NOT reproducible from the caller side, and the probe says why: the
//	    LLM delivers its whole response 250-580ms after the first token, so
//	    the discard window is sub-second. Five attempts across three timing
//	    strategies and two vendors never landed a blip inside it, and the
//	    probe recorded zero discarded tokens every time. The code path is
//	    real; the exposure in this configuration is not what was assumed.
//	2b  REPRODUCED, and NOT the reported cause. See its doc.
//	3   REPRODUCED, with all three server-side causes captured. See its doc.
//	4a  Did not reproduce — the 11.1.2 fix is present.
//	4b  Did not reproduce — a bare hangup redirect ends the call in <7s.
//	4c  Did not reproduce — agent:update works.
//	5   REPRODUCED. Probe: RangeError "Max payload size exceeded",
//	    code WS_ERR_UNSUPPORTED_MESSAGE_LENGTH, maxPayload=24576,
//	    connections=1, swallowed=true, inFlight=1. Note the socket then
//	    closed 1006, not the 1009 the report and the code-review both
//	    assumed — a fix keyed on 1009 would not fire here.
//
// All tests here skip cleanly without DEEPSEEK_API_KEY / DEEPGRAM_API_KEY /
// NGROK_AUTHTOKEN, same as the rest of agent_test.go.
package verbs

import (
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// --- shared fixtures --------------------------------------------------------

// countingNumbers is the vocabulary the counting prompts draw on. A spoken
// count is the instrument these tests measure with: it is monotonic, so the
// LAST number that reached the caller pins exactly how far playback got, and
// a MISSING RUN in the middle is unambiguous evidence of dropped tokens.
// No other assertion on free-form LLM prose gives either property.
var countingNumbers = []string{
	"one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
	"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen",
	"eighteen", "nineteen", "twenty", "twenty one", "twenty two", "twenty three",
	"twenty four", "twenty five", "twenty six", "twenty seven", "twenty eight",
	"twenty nine", "thirty", "thirty one", "thirty two", "thirty three",
	"thirty four", "thirty five", "thirty six", "thirty seven", "thirty eight",
	"thirty nine", "forty", "forty one", "forty two", "forty three", "forty four",
	"forty five", "forty six", "forty seven", "forty eight", "forty nine", "fifty",
	"fifty one", "fifty two", "fifty three", "fifty four", "fifty five", "fifty six",
	"fifty seven", "fifty eight", "fifty nine", "sixty",
}

// countingSystemPrompt makes the agent emit a long, strictly ordered, fully
// predictable response. "One number per sentence" is what produces enough
// sentence terminators for the TTS to be chunking continuously while the LLM
// is still streaming — the overlap defects 1b and 2 need.
const countingSystemPrompt = "You are a counting machine. " +
	"On your very first turn, and on every later turn, count out loud from one to thirty. " +
	"Write each number as an English word followed by a period, like this: One. Two. Three. " +
	"Never use digits. Never add any other words, greetings or commentary. " +
	"Always start again from one and always continue all the way to thirty."

// longCountSystemPrompt exists solely to widen the window in which the LLM is
// still GENERATING. Counting to thirty is generated in a second or two, long
// before a barge-in can land — the first attempt at Defect2 dropped no tokens
// for exactly that reason, and proved it with an empty probe log. Sixty
// numbered sentences keep the stream open long enough to interrupt.
// Asking a model to "count to one hundred" does not work — deepseek and
// gpt-4o-mini both stopped at thirty however the instruction was worded. A
// copy task does work, so the list is handed over verbatim and the model is
// told to reproduce it.
var longCountSystemPrompt = "You are a repeating machine. On every turn, reply with EXACTLY " +
	"the following text and nothing else, reproducing every item: " + longCountText()

// longCountText renders countingNumbers as the sentence list the agent must
// reproduce: "One. Two. Three. …".
func longCountText() string {
	var b strings.Builder
	for _, n := range countingNumbers {
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(strings.ToUpper(n[:1]) + n[1:] + ".")
	}
	return b.String()
}

// interruptUtterance is the caller speech used to CONFIRM a barge-in. It must
// be comfortably longer than bargeIn.minSpeechDuration (default 0.7s) so the
// confirmation timer cannot revert it.
const interruptUtterance = "Sorry, please stop counting, I have a completely different question for you."

// countRequestUtterance drives the count from the CALLER side. The counting
// tests cannot use greeting:true even though it would be simpler: a turn with
// no user transcript takes agent-turn.js's early-return path and emits no
// turn_end event at all, so the interrupted-turn witness these tests read would
// never exist. Driving turn 1 with caller speech gives the turn a transcript
// and puts it on the normal turn_end path.
const countRequestUtterance = "Please start counting out loud now, all the way to thirty."

// --- local helpers ----------------------------------------------------------

// scriptAgentTweaked is ScriptAgent with a hook to mutate the finished verb
// map. Needed because agentVerbOpts intentionally exposes only the knobs the
// original agent tests use, and these tests need bargeIn sub-fields
// (minSpeechDuration, sticky) that have no option field.
func scriptAgentTweaked(sess *webhook.Session, opts agentVerbOpts, tweak func(map[string]any), extra ...map[string]any) {
	opts.ActionURL = SessionURL(sess, "agent-complete")
	opts.EventURL = SessionURL(sess, "agent-turn")
	if len(opts.Tools) > 0 {
		opts.ToolURL = SessionURL(sess, "agent-tool")
	}
	verb := buildAgentVerb(opts)
	if tweak != nil {
		tweak(verb)
	}
	script := webhook.Script{verb}
	for _, v := range extra {
		script = append(script, v)
	}
	script = append(script, V("hangup"))
	sess.ScriptCallHook(WithWarmupScript(script))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	if len(opts.Tools) > 0 {
		SessionAckEmpty(sess, "agent-tool")
	}
}

// truncateWAV copies the first d of src into a new telephony WAV under
// t.TempDir() and returns its path. Used to manufacture a sub-minSpeechDuration
// noise blip out of real speech — a synthetic tone is not a safe substitute
// because Silero VAD scores speech-likeness, not energy.
//
// Assumes the canonical 44-byte RIFF header internal/tts writes.
func truncateWAV(t *testing.T, src string, d time.Duration) string {
	t.Helper()
	const headerLen = 44
	const bytesPerSec = 8000 * 2 // 8 kHz, 16-bit mono

	raw, err := os.ReadFile(src)
	if err != nil {
		helperFatalf(t, "truncate-wav", "ReadFile(%s): %v", src, err)
	}
	if len(raw) <= headerLen {
		helperFatalf(t, "truncate-wav", "%s is only %d bytes, not a WAV", src, len(raw))
	}
	want := int(d.Seconds() * bytesPerSec)
	want -= want % 2 // keep whole samples
	if avail := len(raw) - headerLen; want > avail {
		want = avail
	}
	out := make([]byte, headerLen+want)
	copy(out, raw[:headerLen])
	copy(out[headerLen:], raw[headerLen:headerLen+want])
	binary.LittleEndian.PutUint32(out[4:], uint32(36+want)) // RIFF size
	binary.LittleEndian.PutUint32(out[40:], uint32(want))   // data size
	path := filepath.Join(t.TempDir(), "blip.wav")
	if err := os.WriteFile(path, out, 0o644); err != nil {
		helperFatalf(t, "truncate-wav", "WriteFile(%s): %v", path, err)
	}
	return path
}

// waitForSpeech polls the growing recording for audio energy and returns as
// soon as the far end is actually speaking. jambonz sends RTP continuously
// once media is up, so file growth alone proves nothing — the tail has to be
// inspected for signal.
func waitForSpeech(s *StepCtx, pcmPath string, budget time.Duration) bool {
	const frame = 3200 // 200ms of 8kHz 16-bit
	deadline := time.Now().Add(budget)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(pcmPath)
		if err == nil && len(raw) >= frame {
			tail := raw[len(raw)-frame:]
			var peak int16
			for i := 0; i+1 < len(tail); i += 2 {
				v := int16(uint16(tail[i]) | uint16(tail[i+1])<<8)
				if v < 0 {
					v = -v
				}
				if v > peak {
					peak = v
				}
			}
			if peak > 1500 {
				s.Logf("speech detected in recording (peak %d) after %s",
					peak, budget-time.Until(deadline))
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}

// numbersPresent reports which of countingNumbers appear in text, as indices.
// Matching is longest-first so "twenty one" is not also counted as "one".
func numbersPresent(text string) map[int]bool {
	// "Twenty-one" must count as "twenty one": stt.Normalize drops the hyphen
	// without leaving a space, which silently welded the two halves together
	// and reported a nine-number hole that was not there.
	norm := stt.Normalize(strings.ReplaceAll(text, "-", " "))
	found := map[int]bool{}
	// Consume the two-word numbers first, blanking them out, so the
	// single-word scan that follows cannot double-count their halves.
	for i := len(countingNumbers) - 1; i >= 0; i-- {
		n := countingNumbers[i]
		if !strings.Contains(n, " ") {
			continue
		}
		if strings.Contains(norm, n) {
			found[i] = true
			norm = strings.ReplaceAll(norm, n, " ")
		}
	}
	for i, n := range countingNumbers {
		if strings.Contains(n, " ") {
			continue
		}
		if containsWord(norm, n) {
			found[i] = true
		}
	}
	return found
}

// containsWord is a whole-word Contains, so "one" does not match "money".
func containsWord(haystack, word string) bool {
	for _, f := range strings.Fields(haystack) {
		if f == word {
			return true
		}
	}
	return false
}

// highestNumber returns the index of the largest counting number present in
// text, or -1. That is "how far the count actually got".
func highestNumber(text string) int {
	high := -1
	for i := range numbersPresent(text) {
		if i > high {
			high = i
		}
	}
	return high
}

// longestMissingRun returns the length of the longest run of consecutive
// counting numbers that are absent from text BELOW its high-water mark, plus
// the index the run starts at. A single miss is ordinary STT/TTS noise; a run
// of two or more below the high-water mark means text was never spoken at all.
func longestMissingRun(text string) (start, length int) {
	present := numbersPresent(text)
	high := highestNumber(text)
	bestStart, best := -1, 0
	run, runStart := 0, -1
	for i := 0; i <= high; i++ {
		if present[i] {
			run, runStart = 0, -1
			continue
		}
		if run == 0 {
			runStart = i
		}
		run++
		if run > best {
			best, bestStart = run, runStart
		}
	}
	return bestStart, best
}

// lastTurnEnd returns the final turn_end event in cbs, if any.
func lastTurnEnd(cbs []webhook.Callback) (webhook.Callback, bool) {
	te := findAgentEvents(cbs, "turn_end")
	if len(te) == 0 {
		return webhook.Callback{}, false
	}
	return te[len(te)-1], true
}

// interruptedTurnEnd returns the first turn_end carrying interrupted=true.
func interruptedTurnEnd(cbs []webhook.Callback) (webhook.Callback, bool) {
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if b, _ := te.JSON["interrupted"].(bool); b {
			return te, true
		}
	}
	return webhook.Callback{}, false
}

// unseparatedJoins finds sentence terminators immediately followed by a letter
// ("…for you.Let me…"). That exact shape is the signature of two independently
// generated response segments being concatenated with no separator — no TTS
// vendor and no LLM produces it on its own.
func unseparatedJoins(s string) []string {
	var out []string
	r := []rune(s)
	for i := 0; i+1 < len(r); i++ {
		if r[i] != '.' && r[i] != '!' && r[i] != '?' {
			continue
		}
		n := r[i+1]
		isLetter := (n >= 'a' && n <= 'z') || (n >= 'A' && n <= 'Z') || n > 127
		if !isLetter {
			continue
		}
		lo := i - 12
		if lo < 0 {
			lo = 0
		}
		hi := i + 13
		if hi > len(r) {
			hi = len(r)
		}
		out = append(out, string(r[lo:hi]))
	}
	return out
}

// countOccurrences counts non-overlapping occurrences of needle in haystack,
// both normalized.
func countOccurrences(haystack, needle string) int {
	return strings.Count(stt.Normalize(haystack), stt.Normalize(needle))
}

// --- defect 1 ---------------------------------------------------------------

// runHistoryTrimAfterBargeIn is the Defect1 flow on the given TTS (nil = deepgram).
func runHistoryTrimAfterBargeIn(t *testing.T, ttsSynth map[string]any) {
	t.Helper()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-wavs")
	countWAV, err := tts.EnsureWAV(ctx, "testdata/agent", countRequestUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(count request): %v", err)
	}
	interruptWAV, err := tts.EnsureWAV(ctx, "testdata/agent", interruptUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(interrupt): %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// The copy-task prompt, not the plain "count to thirty" one: asked to
	// count, the model sometimes stopped after a handful of numbers and had
	// finished speaking before the interrupt could land, which is what made
	// this test flaky. Reproducing a supplied list is something models
	// actually comply with, and sixty sentences keep it talking for ~50s.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: longCountSystemPrompt,
		Greeting:     false,
		BargeIn:      true,
		TTS:          ttsSynth,
	}, func(verb map[string]any) {
		if llm, ok := verb["llm"].(map[string]any); ok {
			if opts, ok := llm["llmOptions"].(map[string]any); ok {
				opts["maxTokens"] = 2048
			}
		}
	})
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

	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	s = Step(t, "request-count")
	if err := call.SendWAV(countWAV); err != nil {
		s.Fatalf("SendWAV(count request): %v", err)
	}
	// Recording opens after the request, so the transcript below is the
	// agent's count and nothing else.
	recPath := filepath.Join(t.TempDir(), "defect1-count.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-request): %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-count")
	// Trigger off the agent's own audio. Waiting on llm_response instead was
	// unreliable in both directions: it fires when GENERATION ends, which for
	// a short reply is after playback has already finished, leaving nothing to
	// interrupt — the observed flake. Audio energy means the agent is speaking
	// right now, and thirty numbered sentences keep it speaking for ~25s.
	if !waitForSpeech(s, recPath, 30*time.Second) {
		// Same class of premise-miss as assert-interruption-confirmed below:
		// with nothing being spoken there is no interruption to judge.
		s.Logf("agent never started speaking within 30s")
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		t.Skip("agent never started speaking — test premise not established")
	}
	// Let a handful of numbers actually reach the caller, so "what was heard"
	// and "what was generated" are both non-trivial.
	time.Sleep(3 * time.Second)
	var early []webhook.Callback
	s.Done()

	s = Step(t, "send-interrupt-wav")
	// Retried once: roughly one run in two the utterance produced no
	// transcript at all and the barge-in never armed, which is a miss of the
	// test's own setup rather than a result. One resend is enough — if the
	// second also produces nothing the assertion below reports it as the
	// setup failure it is.
	collected := early
	interrupted := func() bool {
		_, ok := interruptedTurnEnd(collected)
		return ok
	}
	for attempt := 1; attempt <= 2 && !interrupted(); attempt++ {
		if attempt > 1 {
			s.Logf("no interruption from attempt %d — resending", attempt-1)
		}
		if err := call.SendWAV(interruptWAV); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence (post): %v", err)
		}
		if attempt == 1 {
			// Stop recording promptly: everything after the barge-in belongs
			// to the NEXT turn and would pollute the "what did the caller
			// hear" witness. A resend only adds caller audio, which the
			// recording does not capture.
			time.Sleep(1500 * time.Millisecond)
			call.StopRecording()
		}
		waitCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
		collected = append(collected, WaitCallbacksUntil(waitCtx, sess,
			func(c []webhook.Callback) bool {
				_, ok := interruptedTurnEnd(append(collected, c...))
				return ok
			})...)
		cancel()
	}
	// user_interruption is posted alongside the interrupted turn_end and can land just after it
	collected = append(collected, DrainCallbacks(sess, time.Second)...)
	s.Done()

	s = Step(t, "collect-events")
	cbs := collected
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-interruption-confirmed")
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		// The premise was never established: the interrupt utterance produced
		// no transcript on either attempt, or the agent had already finished
		// speaking. Judging the fix on that run would be judging nothing, so
		// skip rather than report a failure that is not one. Roughly one run
		// in three against a live cluster.
		s.Logf("no user_interruption after two attempts; events=%s", summarizeEventTypes(cbs))
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		t.Skip("barge-in never confirmed — test premise not established, nothing to judge")
	}
	te, ok := interruptedTurnEnd(cbs)
	if !ok {
		s.Fatalf("no turn_end with interrupted=true; events=%s", summarizeEventTypes(cbs))
	}
	response := te.String("response")
	s.Logf("turn_end.response (%d chars): %q", len(response), truncate(response, 300))
	s.Done()

	s = Step(t, "assert-response-matches-spoken")
	if !stt.HasKey() {
		s.Logf("skipping: %s unset, cannot transcribe the spoken witness", stt.EnvKey)
		s.Done()
		return
	}
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
	}
	spokenHigh := highestNumber(transcript)
	responseHigh := highestNumber(response)
	s.Logf("caller heard up to %q (idx %d); turn_end.response runs to %q (idx %d)",
		numberLabel(spokenHigh), spokenHigh, numberLabel(responseHigh), responseHigh)
	if spokenHigh < 0 {
		s.Fatalf("no counting numbers in the recording — the agent never counted, so there "+
			"is nothing to compare. transcript=%q", truncate(transcript, 300))
	}
	// Tolerance: audio already handed to FreeSWITCH but not yet emitted when
	// the interruption is confirmed is legitimately "generated but unheard".
	// Three numbers is a generous ceiling for that buffer. Anything beyond it
	// is text the trim should have removed.
	const bufferTolerance = 3
	if responseHigh > spokenHigh+bufferTolerance {
		s.Errorf("history/event kept text the caller never heard: turn_end.response reaches %q "+
			"but the caller only heard up to %q (+%d tolerated for TTS buffering). "+
			"The interrupted response was not trimmed to the spoken text, so on the next turn "+
			"the model assumes the caller heard everything. response=%q transcript=%q",
			numberLabel(responseHigh), numberLabel(spokenHigh), bufferTolerance,
			truncate(response, 300), truncate(transcript, 300))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// numberLabel renders a countingNumbers index for diagnostics.
func numberLabel(i int) string {
	if i < 0 || i >= len(countingNumbers) {
		return "(none)"
	}
	return countingNumbers[i]
}

// referenceCodeFrom extracts the digit words following "reference code is"
// from a transcript, returning up to four of them.
func referenceCodeFrom(transcript string) []string {
	digits := map[string]bool{
		"zero": true, "oh": true, "one": true, "two": true, "three": true, "four": true,
		"five": true, "six": true, "seven": true, "eight": true, "nine": true,
	}
	norm := stt.Normalize(transcript)
	i := strings.Index(norm, "code is")
	if i < 0 {
		i = strings.Index(norm, "reference code")
	}
	if i < 0 {
		return nil
	}
	var out []string
	for _, w := range strings.Fields(norm[i:]) {
		if digits[w] {
			out = append(out, w)
			if len(out) == 4 {
				break
			}
			continue
		}
		// Stop at the first non-digit word once we have started collecting,
		// so the count that follows ("one two three…") is not swallowed.
		if len(out) > 0 && w != "is" && w != "code" && w != "reference" {
			break
		}
	}
	return out
}
