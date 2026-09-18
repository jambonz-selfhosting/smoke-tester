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
//	2b      control for the bare "." — same symptom with          Defect2b_BareTerminator…
//	        barge-in disabled and no caller audio
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

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
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

// TestVerb_Agent_Defect1_HistoryTrimmedToSpokenAfterBargeIn — after a CONFIRMED
// barge-in, the assistant's turn must be recorded as what the caller actually
// heard, not as everything the LLM generated. The operator's symptom is the
// consequence of the opposite: the model reads back its own full text on the
// next turn, concludes the caller already heard the pending question, and never
// asks it.
//
// How this is made observable without reaching into the cluster: the agent is
// prompted to count to thirty, and the barge-in lands partway through. Two
// independent witnesses of the same turn are then compared —
//
//	the recording  → the count the CALLER heard (highest number in the audio)
//	turn_end.response → the count jambonz BELIEVES it delivered
//
// turn_end.response is documented in callbacks/agent-turn.schema.json as "may
// be trimmed to what was actually spoken if the turn was interrupted", and it
// is written from the same trimmed text the LLM history gets
// (_confirmInterruption → trimLastAssistantMessage), so it is a faithful proxy
// for history. If the two witnesses disagree by more than TTS buffering can
// explain, history is carrying words the caller never heard.
//
// The interrupt utterance is sent twice if the first produces no transcript
// at all, which happened on roughly one run in two. A failure at
// assert-interruption-confirmed after both attempts is a setup miss, not the
// defect; only assert-response-matches-spoken reports the defect.
//
// Expected to FAIL on any build where the TTS vendor is outside
// TtsAlignmentVendors (deepgram is, in 10.2.1 and 11.1.4 alike): with no
// alignment events getSpokenText() returns null, the trim is skipped entirely,
// and response runs to "thirty" however early the interruption landed.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. request-count
//  8. wait-into-count
//  9. send-interrupt-wav
//
// 10. collect-events
// 11. assert-interruption-confirmed
// 12. assert-response-matches-spoken
func TestVerb_Agent_Defect1_HistoryTrimmedToSpokenAfterBargeIn(t *testing.T) {
	t.Parallel()
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

// TestVerb_Agent_Defect1b_AssistantTurnRetainedWhenInterruptedMidStream — when
// the caller interrupts while the LLM is STILL STREAMING, the assistant's turn
// must still be recorded. On affected builds AgentLlm.prompt() returns on
// userInterrupt without committing anything, so history ends with two
// consecutive user messages and the model has no record of what it just said.
//
// Probe: the agent is told to open with a reference code it invents itself,
// then count slowly to thirty. We interrupt mid-count — early enough that the
// LLM is provably still streaming (thirty numbered sentences take longer to
// generate than the few seconds we wait) — and then ask it to repeat the code.
// The code was spoken (we transcribe it from the recording to learn what it
// was), so a model whose history is intact can repeat it; a model whose turn
// was dropped from history cannot, because it never existed anywhere else.
//
// This is the only black-box probe of history contents available: turn_end and
// llm_response are written from the state machine's own accumulator, not from
// the LLM's conversation array, so neither one witnesses the drop.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs
//  3. script-agent-verb
//  4. place-call
//  5. answer-record-and-silence
//  6. wait-into-count — short, so the LLM is still streaming
//  7. interrupt-mid-stream
//  8. capture-reference-code — from the recording of the interrupted turn
//  9. ask-for-code-again
//
// 10. assert-code-recalled
func TestVerb_Agent_Defect1b_AssistantTurnRetainedWhenInterruptedMidStream(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 240*time.Second)
	uas := claimUAS(t, ctx)

	const recallUtterance = "Wait, what was that reference code you just gave me? Please say the digits again."

	s = Step(t, "ensure-wavs")
	interruptWAV, err := tts.EnsureWAV(ctx, "testdata/agent", interruptUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(interrupt): %v", err)
	}
	recallWAV, err := tts.EnsureWAV(ctx, "testdata/agent", recallUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(recall): %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// The code is spelled digit-by-digit so STT transcribes it as words we
	// can match; a bare "7314" invites SmartFormat-style rewrites.
	prompt := "You are a reference-desk assistant. " +
		"On your very first turn, invent a four digit reference code and open by saying " +
		"'Your reference code is' followed by the four digits spoken one at a time as English " +
		"words, for example 'three seven one four'. " +
		"Immediately after that, count out loud from one to thirty, writing each number as an " +
		"English word followed by a period, and nothing else. " +
		"If the caller later asks what the reference code was, repeat exactly the same four " +
		"digits you gave them, as English words, and say nothing else. " +
		"Never invent a second code."
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: prompt,
		Greeting:     true,
		BargeIn:      true,
	}, nil)
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(180))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	firstRec := filepath.Join(t.TempDir(), "defect1b-first-turn.pcm")
	if err := call.StartRecording(firstRec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-count")
	// Deliberately short. The code plus a handful of numbers have been
	// spoken, while the remaining ~25 numbered sentences are still being
	// generated — that is the mid-stream window this test needs.
	time.Sleep(4 * time.Second)
	s.Done()

	s = Step(t, "interrupt-mid-stream")
	if err := call.SendWAV(interruptWAV); err != nil {
		s.Fatalf("SendWAV(interrupt): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(1 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "capture-reference-code")
	if !stt.HasKey() {
		s.Logf("skipping: %s unset", stt.EnvKey)
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		return
	}
	firstTranscript, err := stt.Transcribe(ctx, firstRec)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", firstRec, err)
	}
	s.Logf("first-turn transcript: %q", truncate(firstTranscript, 400))
	code := referenceCodeFrom(firstTranscript)
	if len(code) < 3 {
		s.Fatalf("could not read a reference code out of the first turn — the agent did not "+
			"follow the prompt, so the recall probe would be meaningless. transcript=%q",
			truncate(firstTranscript, 400))
	}
	s.Logf("reference code the caller heard: %v", code)
	s.Done()

	s = Step(t, "ask-for-code-again")
	secondRec := filepath.Join(t.TempDir(), "defect1b-recall.pcm")
	if err := call.StartRecording(secondRec); err != nil {
		s.Fatalf("StartRecording(recall): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre-recall): %v", err)
	}
	if err := call.SendWAV(recallWAV); err != nil {
		s.Fatalf("SendWAV(recall): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-recall): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-code-recalled")
	// Tolerance: STT on telephony audio drops the occasional digit word, so
	// require most of the code rather than all of it. A model that lost the
	// turn from history cannot produce ANY of the right digits except by
	// chance, so this still discriminates.
	want := len(code) - 1
	if want < 2 {
		want = 2
	}
	AssertTranscriptHasMost(s, ctx, secondRec, want, code...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
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

// --- defect 2 ---------------------------------------------------------------

// TestVerb_Agent_Defect2_FalseInterruptionDoesNotDropTokens — a noise blip that
// is NOT confirmed as a barge-in must leave the assistant's response intact.
//
// The operator heard the opposite: fragments (" ser grabada con fines…") and a
// bare "." read aloud as "punto". Cause, from the code: a VAD speech-started
// moves the state machine to UserSpeaking and arms the minSpeechDuration
// confirmation timer without clearing TTS — correct so far, the barge-in is
// only tentative. But _onTokensReceived discards every token that arrives in
// that state. When the timer then reverts the state as a false interruption,
// the tokens generated during the window are simply gone: never buffered to
// TTS, never added to the response text. The caller hears the response resume
// with a hole in the middle, and when the hole ends mid-sentence the leftover
// terminator is spoken as a word.
//
// Reproduction: drive a noise blip shorter than minSpeechDuration while the
// agent counts. A count makes the hole unmistakable — numbers are monotonic,
// so a missing RUN below the high-water mark cannot be explained by STT noise
// the way a missing word in prose could.
//
// IT HAS NOT BEEN POSSIBLE TO LAND A BLIP IN THE WINDOW. The feature-server
// probe measured first-token to flush at 250-580ms for both deepseek and
// gpt-4o-mini, so the whole response is generated before any barge-in the
// caller could produce. Five attempts — fixed sleeps at 3s and 1.2s, a raised
// maxTokens, a copy-task prompt to lengthen generation, and finally triggering
// the blip off the arrival of the agent's own audio — all recorded ZERO
// discarded tokens. A pass here therefore does not clear the code path; it
// says the window was missed again. The honest conclusion is that the
// exposure is roughly half a second per turn unless the LLM streams slowly,
// which is worth knowing before anyone attributes a customer symptom to it.
//
// Two witnesses are checked, because the defect corrupts both: the audio the
// caller heard, and the response text jambonz reports.
//
// bargeIn.sticky is set to true here. It is a no-op in both 10.2.1 and 11.1.4
// (parsed, never referenced), so it changes nothing today — it is set so that
// if sticky is ever implemented, resumption after a false interruption is
// already under test.
//
// Steps:
//  1. preflight-skips
//  2. ensure-wavs — the count request, plus real speech truncated to a blip
//  3. script-agent-verb — bargeIn on, minSpeechDuration explicit
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. request-count
//  8. wait-into-count
//  9. send-blip
//
// 10. collect-events
// 11. assert-no-interruption-confirmed
// 12. assert-llm-response-has-no-hole
// 13. assert-audio-has-no-hole
func TestVerb_Agent_Defect2_FalseInterruptionDoesNotDropTokens(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	if !cfg.HasOpenAI() {
		s.Done()
		t.Skip("defect2 needs OPENAI_API_KEY — see the vendor note below")
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
	fullWAV, err := tts.EnsureWAV(ctx, "testdata/agent", interruptUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV(blip source): %v", err)
	}
	// 200ms. Two constraints squeeze this from both sides: long enough for
	// Silero to fire speech-started (tens of ms of speech frames), short
	// enough that Deepgram returns no transcript for it. The transcript is
	// the binding one — _onInterimTranscript treats ANY real transcript
	// during AssistantSpeaking as definitive proof of a barge-in and
	// confirms immediately, bypassing the minSpeechDuration timer entirely.
	// At 350ms Deepgram still transcribed the fragment and the blip
	// confirmed as a real interruption, which is not the path under test.
	blipWAV := truncateWAV(t, fullWAV, 200*time.Millisecond)
	s.Logf("blip wav: %s (200ms of %s)", blipWAV, fullWAV)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// openai, not the file's default deepseek: deepseek delivered all thirty
	// numbers 250ms after its first token (measured on the feature-server
	// probe), which makes the discard window unhittable from outside. A
	// hundred numbers from gpt-4o-mini stream over seconds.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: longCountSystemPrompt,
		Greeting:     false,
		BargeIn:      true,
		LLMVendor:    "openai",
		LLMModel:     "gpt-4o-mini",
		LLMApiKey:    cfg.OpenAIAPIKey,
	}, func(verb map[string]any) {
		verb["bargeIn"] = map[string]any{
			"enable": true,
			// Pinned rather than defaulted so the 200ms blip is provably
			// below the confirmation threshold regardless of build.
			"minSpeechDuration": 0.7,
			"sticky":            true,
		}
		// The shared builder caps maxTokens at 128, which truncates the count
		// at thirty — and thirty numbered sentences are generated in about a
		// second, so the stream is closed before any blip can land. Raising
		// the cap is what actually opens the window this test needs; the
		// first two attempts failed here and the probe log proved it by
		// recording zero discarded tokens.
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
	recPath := filepath.Join(t.TempDir(), "defect2-count.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-request): %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-count")
	// The window is narrow and both edges are hard: before the first token
	// the agent is not in AssistantSpeaking and a blip arms nothing; after
	// flush there are no in-flight tokens left to drop. Measured on the
	// feature-server probe, first-token to flush is 250-500ms — far too tight
	// for a fixed sleep, which missed it on three separate attempts. So the
	// blip is triggered off the arrival of the agent's own audio instead.
	if !waitForSpeech(s, recPath, 30*time.Second) {
		s.Fatalf("agent never started speaking within 30s — nothing to interrupt")
	}
	s.Done()

	s = Step(t, "send-blip")
	if err := call.SendWAV(blipWAV); err != nil {
		s.Fatalf("SendWAV(blip): %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-blip): %v", err)
	}
	s.Done()

	s = Step(t, "collect-events")
	// Wait for llm_response, not for playback. llm_response fires at flush,
	// as soon as GENERATION completes, and carries the accumulator — which is
	// exactly what a dropped token never reaches. Waiting out sixty numbers
	// of audio would add a minute and prove nothing extra.
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "llm_response")) > 0
	})
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	time.Sleep(8 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-no-interruption-confirmed")
	// If the blip DID confirm as a real barge-in, the count was legitimately
	// cut short and neither hole assertion means anything. Fail loudly — the
	// blip needs to be shorter, not the assertions weaker.
	if n := len(findAgentEvents(cbs, "user_interruption")); n > 0 {
		s.Fatalf("the 200ms blip confirmed as a real interruption (%d user_interruption events) — "+
			"this test requires a FALSE interruption to exercise the revert path; "+
			"events=%s", n, summarizeEventTypes(cbs))
	}
	s.Done()

	s = Step(t, "assert-llm-response-has-no-hole")
	// The accumulator is the primary witness: a token discarded during the
	// pending-barge-in window never reaches _currentResponseText, so the hole
	// is in llm_response before it is ever in the audio.
	resp := findAgentEvents(cbs, "llm_response")
	if len(resp) == 0 {
		s.Fatalf("no llm_response event; events=%s", summarizeEventTypes(cbs))
	}
	generated := resp[0].String("response")
	s.Logf("llm_response (%d chars): %q", len(generated), truncate(generated, 600))
	if start, run := longestMissingRun(generated); run >= 2 {
		s.Errorf("hole in llm_response: %d consecutive numbers missing starting at %q, below a "+
			"high-water mark of %q. Tokens that arrived while the barge-in was still "+
			"unconfirmed were discarded instead of buffered. response=%q",
			run, numberLabel(start), numberLabel(highestNumber(generated)),
			truncate(generated, 600))
	}
	s.Done()

	s = Step(t, "assert-audio-has-no-hole")
	if !stt.HasKey() {
		s.Logf("skipping audio witness: %s unset", stt.EnvKey)
	} else {
		transcript, err := stt.Transcribe(ctx, recPath)
		if err != nil {
			s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
		}
		s.Logf("count transcript (partial, recording stops mid-count): %q",
			truncate(transcript, 500))
		// Corroboration only. The recording is cut short deliberately, so a
		// missing TAIL is expected; only a gap below the high-water mark counts.
		if start, run := longestMissingRun(transcript); run >= 2 {
			s.Errorf("hole in the spoken count: %d consecutive numbers missing starting at %q. "+
				"transcript=%q", run, numberLabel(start), truncate(transcript, 500))
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Defect2b_BareTerminatorNotSpokenWithoutBargeIn — the control
// for the "punto" half of defect 2: with barge-in DISABLED and no caller audio
// at all during the response, the synthesizer must never voice a bare sentence
// terminator.
//
// This exists because the token-drop explanation does not survive the evidence.
// Defect2's run produced an intact count — every number from one to thirty, no
// hole — and still had the TTS say "dot" twice, at the same two positions on
// repeated runs. A dropped-token hole cannot explain a terminator spoken in the
// middle of a response that is otherwise complete.
//
// THIS IS THE ONE DEFECT STILL OPEN. It fails, and everything that was
// suspected of causing it has now been ruled out:
//
//	feature-server chunking   every chunk handed to the synthesizer is a
//	                          well-formed sentence (" Six.", " Seven."). Both
//	                          send paths were instrumented, including the
//	                          flush path that has no whitespace guard: 80
//	                          chunks on the boundary path, zero on the flush
//	                          path, zero terminator-only.
//	the vendor + mediajam     replaying that exact chunk sequence through
//	                          mediajam's own engine against live Deepgram is
//	                          clean — back-to-back, paced in real time at one
//	                          20ms frame per tick, and spaced 40ms apart to
//	                          mimic token arrival (mediajam
//	                          internal/tts/bare_terminator_live_test.go).
//	the one-shot TTS path     the same text through the `say` verb over a
//	                          live call is clean (Defect2c).
//	the streaming TTS path    the same text through `say` with stream:true is
//	                          clean (Defect2d) — but note it sends ONE 630
//	                          char chunk through the flush path, so it does
//	                          not control for chunking, only for the vendor.
//
// What it DOES track is chunk seams. Coalescing the agent's per-sentence
// chunks into three larger ones moved the artifact: "six dot seven" vanished
// and the only remaining "dot" landed at the new seam, between a chunk ending
// "…Sixteen." and one starting " Seventeen.". Replaying those exact three
// chunks offline through mediajam's engine against the same voice is clean.
//
// So: identical text, identical chunk sequence, identical vendor and engine —
// clean off a live call, artifact on one. That leaves the endpoint's playout
// of separate vendor bursts (the agent verb also runs STT on the same media
// session), which needs audio captured at the mediajam endpoint to pin down.
// Guessing at a fix here would be guessing. The token drop and jambonz'"'"'s
// chunker are both exonerated.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb — bargeIn explicitly disabled
//  4. place-call
//  5. answer-and-silence
//  6. wait-for-stt
//  7. request-count
//  8. let-count-finish
//  9. assert-no-bare-terminator
func TestVerb_Agent_Defect2b_BareTerminatorNotSpokenWithoutBargeIn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	countWAV, err := tts.EnsureWAV(ctx, "testdata/agent", countRequestUtterance, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// bargeIn:false is the whole point — with it off there is no VAD
	// confirmation window, so no token can be discarded for that reason.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: countingSystemPrompt,
		Greeting:     false,
		BargeIn:      false,
	}, nil)
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
		s.Fatalf("SendWAV: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "defect2b-count.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post-request): %v", err)
	}
	s.Done()

	s = Step(t, "let-count-finish")
	time.Sleep(32 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-no-bare-terminator")
	if !stt.HasKey() {
		s.Logf("skipping: %s unset", stt.EnvKey)
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		return
	}
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
	}
	s.Logf("count transcript: %q", truncate(transcript, 500))
	norm := stt.Normalize(transcript)
	for _, spoken := range []string{"dot", "punto", "period", "full stop"} {
		if containsWord(norm, spoken) {
			s.Errorf("TTS voiced a bare sentence terminator (%q) with barge-in DISABLED and no "+
				"caller audio during the response — so the operator's \"punto\" is not caused by "+
				"the barge-in token drop, and fixing that drop will not fix it. transcript=%q",
				spoken, truncate(transcript, 500))
		}
	}
	if start, run := longestMissingRun(transcript); run >= 2 {
		s.Errorf("hole in the count with barge-in disabled: %d numbers missing from %q. "+
			"transcript=%q", run, numberLabel(start), truncate(transcript, 500))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Defect2c_SayVerbHasNoBareTerminator — the second control for
// the "punto" symptom, and the one that localises it.
//
// Same sentence list, same voice, same cluster, same RTP path and the same
// verification STT as Defect2b — but spoken by the NON-streaming `say` verb
// instead of the agent verb's streaming synthesizer. If this passes while
// Defect2b fails, the terminator is produced by the streaming TTS path alone,
// and no amount of work on the agent state machine will remove it.
//
// (Two offline controls point the same way and are cheap to re-run by hand:
// synthesizing the whole list in one REST call, and synthesizing it one chunk
// at a time and concatenating, both transcribe clean.)
//
// Steps:
//  1. preflight-skips
//  2. script-say
//  3. place-call
//  4. answer-and-record
//  5. assert-no-bare-terminator
func TestVerb_Agent_Defect2c_SayVerbHasNoBareTerminator(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Skip("needs the in-jambonz Deepgram credential for TTS + the key for verification STT")
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-say")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say", "text", longCountText()),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(110))
	s.Done()

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "defect2c-say.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	// Long enough for the whole list; the say verb starts immediately, with
	// no STT or LLM ahead of it.
	time.Sleep(55 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-no-bare-terminator")
	if !stt.HasKey() {
		s.Logf("skipping: %s unset", stt.EnvKey)
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		return
	}
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
	}
	s.Logf("say transcript: %q", truncate(transcript, 500))
	norm := stt.Normalize(transcript)
	for _, spoken := range []string{"dot", "punto", "period", "full stop"} {
		if containsWord(norm, spoken) {
			s.Errorf("the non-streaming say verb ALSO voiced a bare terminator (%q), so the "+
				"symptom is not specific to the streaming path after all. transcript=%q",
				spoken, truncate(transcript, 500))
		}
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Defect2d_StreamingSayHasNoBareTerminator — the third control,
// and the one that separates the streaming TTS path from the agent verb.
//
// `say` with stream:true goes through exactly the same streaming synthesizer
// and the same RTP path as the agent verb, but none of the agent's state
// machine, chunker or LLM. If this shows the artifact too, nothing in the
// agent verb can be responsible; if it is clean while Defect2b is not, the
// difference is the agent's chunking cadence.
//
// Requires the WS app: streaming say is rejected over the HTTP paths
// (lib/tasks/say.js).
//
// Steps:
//  1. preflight-skips
//  2. script-streaming-say
//  3. place-ws-call
//  4. answer-and-record
//  5. assert-no-bare-terminator
func TestVerb_Agent_Defect2d_StreamingSayHasNoBareTerminator(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Skip("needs the in-jambonz Deepgram credential and the key for verification STT")
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-streaming-say")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say",
			"text", longCountText(),
			"stream", true,
			"synthesizer", map[string]any{
				"vendor":   "deepgram",
				"label":    deepgramLabel,
				"voice":    deepgramVoice,
				"language": "en-US",
			}),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "defect2d-stream-say.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	time.Sleep(55 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-no-bare-terminator")
	if !stt.HasKey() {
		s.Logf("skipping: %s unset", stt.EnvKey)
		s.Done()
		return
	}
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
	}
	s.Logf("streaming say transcript: %q", truncate(transcript, 500))
	norm := stt.Normalize(transcript)
	for _, spoken := range []string{"dot", "punto", "period", "full stop"} {
		if containsWord(norm, spoken) {
			s.Errorf("the STREAMING say verb voiced a bare terminator (%q) with no agent verb "+
				"involved — the artifact belongs to the streaming synthesis path, not to the "+
				"agent. transcript=%q", spoken, truncate(transcript, 500))
		}
	}
	s.Done()
}

// --- defect 3 ---------------------------------------------------------------

// TestVerb_Agent_Defect3_ToolCallTurnEventsNotDuplicated — on a turn containing
// a tool call, the reported response must be coherent and each piece of text
// must be spoken once.
//
// The operator sees llm_response fire twice and turn_end.response come back as
// the pre-tool text glued to the post-tool text with no separator — sometimes
// the SAME sentence twice ("¡De nada! Hasta mañana.¡De nada! Hasta mañana.").
// Two independent causes in the code: AgentLlm.prompt() emits flush only when
// the stream ends in text, so a stream that ends in tool calls never resets the
// response accumulator and the post-tool tokens append to it; and
// appendAssistantToolCall stores the tool call with empty content, dropping the
// model's pre-tool text from history so it regenerates it after the tool
// result — which means it is SPOKEN twice, not merely reported twice.
//
// The turn is shaped to force text on both sides of the tool call: the model
// must say a fixed acknowledgement first, then call the tool, then say the
// returned word. Three assertions, one per symptom:
//
//	concatenation → turn_end.response contains ".X" (terminator, then a letter)
//	duplication   → the post-tool sentence appears once, not twice
//	spoken twice  → the secret word is heard once in the recording
//
// Observed across four runs, with the feature-server probe confirming all of
// it. Three separate faults, only one of which was predicted:
//
//  1. No flush on the tool path. Probe: "tool call collected, no flush
//     emitted, preToolText='Checking that now.'". The accumulator is never
//     reset, which is the predicted cause of concatenation — and one run
//     produced it verbatim: turn_end.response == "Checking that now.The".
//  2. The pre-tool text is dropped from history. Probe: the appended
//     assistant message has content=” and preToolTextRetained=false. The
//     model therefore re-answers without knowing it already spoke.
//  3. A user_interruption fires on the tool turn although barge-in is
//     DISABLED. Probe: "confirmInterruption entered, via=bargeInConfirmed,
//     bargeInEnabled=false" — the endOfTurn-while-AssistantSpeaking path
//     confirms an interruption without ever checking whether barge-in is
//     enabled. This one was not predicted by the code review and splits the
//     turn, which is why the post-tool text lands in a turn whose turn_end
//     never arrives.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb-with-tool
//  4. place-call
//  5. answer-record-and-silence
//  6. wait-for-stt
//  7. send-prompt-wav
//  8. wait-for-tool-call
//  9. wait-for-turn-end
//
// 10. assert-response-not-concatenated
// 11. assert-response-not-duplicated
// 12. assert-word-spoken-once
func TestVerb_Agent_Defect3_ToolCallTurnEventsNotDuplicated(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	// Distinct, phonetically clean tokens: one for the pre-tool text, one
	// carried by the tool result. Both must survive TTS → SIP → STT.
	const secretWord = "kingfisher"
	const ackWord = "checking"
	const promptText = "What is the secret word?"

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", promptText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-with-tool")
	tool := map[string]any{
		"name":        "get_secret_word",
		"description": "Returns the daily secret word. You MUST call this whenever the user asks for the secret word — you do NOT know the word yourself.",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
	// The "say something first, then call the tool" shape is the whole
	// point: without pre-tool text there is nothing for the post-tool text
	// to be concatenated to, and the defect stays invisible.
	systemPrompt := "You are a voice assistant. You do not know any secret words. " +
		"When the user asks for the secret word, FIRST say exactly 'Checking that now.' " +
		"and THEN call the get_secret_word tool. " +
		"After the tool returns, say exactly 'The secret word is' followed by the returned word, " +
		"and then stop. Never repeat yourself."
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: systemPrompt,
		Tools:        []map[string]any{tool},
	}, nil)
	sess.ScriptActionHookBody("agent-tool", []byte(`{"word":"`+secretWord+`"}`))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(150))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "defect3-tool-turn.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-tool-call")
	toolCtx, tcancel := context.WithTimeout(ctx, 45*time.Second)
	defer tcancel()
	toolCB, earlyCBs, err := WaitCallbackForCollecting(toolCtx, sess, "action/agent-tool")
	if err != nil {
		s.Fatalf("waiting for action/agent-tool: %v", err)
	}
	s.Logf("action/agent-tool body: %s", string(toolCB.Body))
	s.Done()

	s = Step(t, "wait-for-turn-end")
	// Wait for the post-tool text to show up somewhere, not merely for a
	// turn_end naming the tool: the turn_end for the tool call itself can
	// land while the second LLM round-trip is still streaming, and stopping
	// there truncates the recording the audio assertion then reads.
	teCtx, tecancel := context.WithTimeout(ctx, 60*time.Second)
	defer tecancel()
	postToolSeen := func(c []webhook.Callback) bool {
		for _, ev := range append(findAgentEvents(c, "llm_response"), findAgentEvents(c, "turn_end")...) {
			if countOccurrences(ev.String("response"), secretWord) > 0 {
				return true
			}
		}
		return false
	}
	seen := WaitCallbacksUntil(teCtx, sess, func(c []webhook.Callback) bool {
		all := append(earlyCBs, c...)
		return agentTurnEndNamesTool(all, "get_secret_word") && postToolSeen(all)
	})
	cbs := append(earlyCBs, seen...)
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	// Let TTS finish streaming the post-tool sentence into the recording
	// before we read it.
	time.Sleep(5 * time.Second)
	call.StopRecording()
	// Keep draining afterwards: the turn_end that closes the post-tool turn
	// fires at ttsEmpty, which is after the text itself has been reported.
	// Stopping at the text would make a missing turn_end indistinguishable
	// from one that simply had not arrived yet.
	cbs = append(cbs, DrainCallbacks(sess, 12*time.Second)...)
	s.Logf("after drain: %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-tool-roundtrip-completed")
	// Everything below judges HOW the post-tool text was reported and spoken.
	// If it never existed, say so plainly instead of blaming duplication —
	// and note that a missing post-tool reply is itself consistent with the
	// pre-tool text being dropped from history, which leaves the model
	// re-answering from a conversation that no longer makes sense.
	if !postToolSeen(cbs) {
		var bodies []string
		for _, ev := range findAgentEvents(cbs, "llm_response") {
			bodies = append(bodies, truncate(ev.String("response"), 120))
		}
		for _, ev := range findAgentEvents(cbs, "turn_end") {
			bodies = append(bodies, "turn_end:"+truncate(ev.String("response"), 120))
		}
		s.Errorf("the tool returned %q but no reported response ever contained it. The agent "+
			"called get_secret_word and then failed to speak the result. reported=%v",
			secretWord, bodies)
	}
	s.Done()

	s = Step(t, "assert-response-not-concatenated")
	te, ok := lastTurnEnd(cbs)
	if !ok {
		s.Fatalf("no turn_end event; events=%s", summarizeEventTypes(cbs))
	}
	// Concatenate every turn_end response for the join/duplication checks:
	// which turn_end carries the post-tool text is exactly what is in
	// question, so pinning the assertions to one of them would let the
	// defect hide in the other.
	var allResponses []string
	for _, ev := range findAgentEvents(cbs, "turn_end") {
		allResponses = append(allResponses, ev.String("response"))
	}
	s.Logf("turn_end responses: %q", allResponses)
	// The turn summaries must account for the post-tool text somewhere. An
	// llm_response carrying it while no turn_end does means the turn record
	// the app persists is missing what the caller was actually told.
	postToolInTurnEnd := false
	for _, r := range allResponses {
		if countOccurrences(r, secretWord) > 0 {
			postToolInTurnEnd = true
		}
	}
	if !postToolInTurnEnd {
		s.Errorf("no turn_end reports the post-tool text: the tool result was spoken to the "+
			"caller and reported on llm_response, but every turn_end carries only the "+
			"pre-tool text %q. turn_end responses=%q", ackWord, allResponses)
	}
	response := te.String("response")
	s.Logf("turn_end.response: %q", response)
	for _, r := range findAgentEvents(cbs, "llm_response") {
		s.Logf("llm_response: %q", truncate(r.String("response"), 200))
	}
	if joins := unseparatedJoins(response); len(joins) > 0 {
		s.Errorf("turn_end.response glues two response segments together with no separator "+
			"at %v — the pre-tool text was never flushed, so the post-tool tokens appended "+
			"to the same accumulator. response=%q", joins, response)
	}
	s.Done()

	s = Step(t, "assert-response-not-duplicated")
	if n := countOccurrences(response, secretWord); n > 1 {
		s.Errorf("turn_end.response names the secret word %d times — the model regenerated "+
			"its post-tool sentence because the pre-tool text was dropped from history. "+
			"response=%q", n, response)
	}
	if n := countOccurrences(response, ackWord); n > 1 {
		s.Errorf("turn_end.response repeats the pre-tool acknowledgement %d times. response=%q",
			n, response)
	}
	s.Done()

	s = Step(t, "assert-word-spoken-once")
	if !stt.HasKey() {
		s.Logf("skipping audio witness: %s unset", stt.EnvKey)
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		return
	}
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe(%s): %v", recPath, err)
	}
	s.Logf("tool-turn transcript: %q", truncate(transcript, 400))
	switch n := countOccurrences(transcript, secretWord); {
	case n == 0:
		s.Logf("the caller never heard %q; the round-trip check above already reports why. "+
			"transcript=%q", secretWord, truncate(transcript, 400))
	case n > 1:
		s.Errorf("the caller heard the secret word %d times: the post-tool sentence was "+
			"SPOKEN twice, not merely reported twice. transcript=%q",
			n, truncate(transcript, 400))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Defect7_LlmTimeoutEndsVerbWithReason — an LLM request that
// never answers must not hold the turn open, and the application must be able
// to tell that it failed.
//
// The reporter's proxy accepted the connection and never sent response
// headers; the agent waited 30 seconds with the caller hearing nothing,
// because nothing in the LLM path bounds a request. They added a 12s 504 on
// their side, which raised the question this test pins: what does the app
// actually see? Before the fix, an LLM failure completed the verb with
// completion_reason "normal" — indistinguishable from a clean end — and
// nothing was sent to the eventHook at all.
//
// Driven by pointing the verb at a black-hole LLM endpoint (a routable
// address that accepts nothing) with connectOptions.timeout set low, so the timeout
// is the only way the turn can end.
//
// Steps:
//  1. preflight-skips
//  2. script-agent-verb — unreachable LLM base url, connectOptions.timeout 5s
//  3. place-call
//  4. answer-and-silence
//  5. wait-for-action-hook
//  6. assert-completion-reason
func TestVerb_Agent_Defect7_LlmTimeoutEndsVerbWithReason(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// 203.0.113.0/24 is TEST-NET-3: routable-looking, guaranteed to answer
	// nothing, so the request hangs until the timeout fires.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: "You are a brief voice assistant.",
		Greeting:     true,
	}, func(verb map[string]any) {
		llm, _ := verb["llm"].(map[string]any)
		// baseURL goes through connectOptions on purpose: the schema accepts it
		// there, and it used to be forwarded nowhere, so the call went to the
		// vendor default with no error at all. If this test ever passes in
		// under a second with a real answer, that regressed.
		//
		// timeout is the knob that bounds the request. It already existed and
		// reaches the vendor SDK; it was simply undocumented, which is how an
		// operator concluded there was none.
		llm["connectOptions"] = map[string]any{
			"baseURL":    "http://203.0.113.10:9/v1",
			"timeout":    5000,
			"maxRetries": 0,
		}
	})
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

	s = Step(t, "wait-for-action-hook")
	// 5s timeout, one retry without tools, then the verb ends. 45s is several
	// times over; before the fix the request was unbounded.
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/agent-complete")
	if err != nil {
		s.Fatalf("the agent verb never completed after an LLM that never answers — "+
			"the request is unbounded: %v", err)
	}
	s.Logf("action/agent-complete: %s", truncate(string(cb.Body), 300))
	s.Done()

	s = Step(t, "assert-completion-reason")
	if got := cb.String("completion_reason"); got != "llm_failure" {
		s.Errorf("completion_reason = %q, want \"llm_failure\" — the application cannot "+
			"tell an LLM outage from a clean end", got)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// --- defect 4a --------------------------------------------------------------

// TestVerb_Agent_Defect4a_NoResponseTimeoutWithGreetingFalse — with
// greeting:false and a caller who never speaks, noResponseTimeout must still
// fire and re-prompt.
//
// The operator's shape exactly: a fixed `say` greeting before the agent verb,
// greeting:false on the verb itself, noResponseTimeout set. Thirty of their
// calls then sat in silence until the far end hung up, one for 431 seconds.
// Cause: in 10.2.1 the timer is armed only from ttsEmpty, the flush-to-Idle
// path and the awaitingFinal timeout — none of which are reached when the verb
// never prompts the LLM at all. The greeting:true branch of the same exec path
// arms it, which is why the existing TestVerb_Agent_NoResponseTimeout passes.
//
// Fixed in 11.1.2 by the `else { this._startNoResponseTimer(); }` branch, so
// this test passes on 11.1.2+ and fails on everything before it.
//
// The caller sends nothing but silence for the whole test — that is the
// scenario, not an oversight.
//
// Steps:
//  1. preflight-skips
//  2. script-say-then-agent — greeting:false, preceded by a fixed say
//  3. place-call
//  4. answer-and-silence
//  5. wait-for-reprompt
//  6. assert-reprompt-fired
func TestVerb_Agent_Defect4a_NoResponseTimeoutWithGreetingFalse(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s = Step(t, "script-say-then-agent")
	timeout := 5
	// The fixed say in front of the agent verb reproduces the operator's
	// app shape; it is also why greeting:false is set — their greeting is
	// spoken by the say verb, not by the LLM.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt:      "You are a brief voice assistant. Reply with a single short sentence.",
		Greeting:          false,
		NoResponseTimeout: &timeout,
	}, nil)
	// Re-script with the say verb ahead of the agent verb. scriptAgentTweaked
	// owns the call hook, so prepend by rebuilding the script here.
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("say", "text", "Thank you for calling. Your call may be recorded."),
		buildAgentVerb(agentVerbOpts{
			SystemPrompt:      "You are a brief voice assistant. Reply with a single short sentence.",
			Greeting:          false,
			NoResponseTimeout: &timeout,
			ActionURL:         SessionURL(sess, "agent-complete"),
			EventURL:          SessionURL(sess, "agent-turn"),
		}),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "agent-complete", "agent-turn")
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

	s = Step(t, "wait-for-reprompt")
	// Budget: ~4s for the say verb, then the 5s timeout, then the LLM
	// round-trip. 35s is several times over, so a failure here means the
	// timer never armed rather than that it was slow.
	waitCtx, cancel := context.WithTimeout(ctx, 35*time.Second)
	defer cancel()
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "llm_response")) > 0
	})
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-reprompt-fired")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("noResponseTimeout (%ds) never fired with greeting:false: no llm_response in "+
			"%d events after 35s of caller silence. The agent waits forever instead of "+
			"re-prompting, so a silent caller holds the call until the far end hangs up. "+
			"events=%s", timeout, len(cbs), summarizeEventTypes(cbs))
	} else {
		s.Logf("re-prompt fired: %q", truncate(responses[0].String("response"), 150))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// --- defect 4b --------------------------------------------------------------

// TestVerb_Agent_Defect4b_RedirectHangupEndsCall — a `redirect` command
// carrying a bare `hangup` verb, sent while the agent verb is running, must end
// the call.
//
// The operator's end-call tool sent exactly that and got two minutes of dead
// air until the caller gave up; say+hangup worked. Code review could not
// explain it — replaceApplication kills the agent task and the hangup task's
// StableCall precondition only needs a dialog — so this test exists to settle
// it empirically. If it passes, the defect is in their app (a queued command,
// or an actionHook that re-scripted over the hangup) and the logs will say
// which; if it fails, we have a reproduction the code read missed.
//
// Driven over the WebSocket app protocol because that is the operator's
// transport and the only one that carries an unsolicited command mid-verb.
//
// Steps:
//  1. preflight-skips
//  2. script-agent-verb
//  3. place-ws-call
//  4. answer-and-silence
//  5. wait-into-greeting
//  6. send-redirect-hangup
//  7. assert-call-ended
func TestVerb_Agent_Defect4b_RedirectHangupEndsCall(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	// A long greeting keeps the agent verb unambiguously active when the
	// redirect lands — the operator's case was a mid-conversation hangup,
	// not one at a turn boundary.
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: countingSystemPrompt,
		Greeting:     true,
	}, nil)
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-into-greeting")
	time.Sleep(6 * time.Second)
	s.Done()

	s = Step(t, "send-redirect-hangup")
	// Bare hangup, no say in front of it — that is the shape that failed for
	// the operator. queueCommand is deliberately absent: a queued command is
	// appended to the task list and would never run while the agent verb is
	// still executing, which is one of the explanations we are ruling out.
	if err := sess.SendCommand("redirect", []map[string]any{{"verb": "hangup"}}); err != nil {
		s.Fatalf("SendCommand(redirect): %v", err)
	}
	s.Done()

	s = Step(t, "assert-call-ended")
	// The test never calls Hangup(): the redirect must be what tears the
	// call down. 30s is far beyond the sub-second this should take, so a
	// timeout here is the operator's dead air, not cluster latency.
	endCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := call.WaitState(endCtx, jsip.StateEnded); err != nil {
		s.Fatalf("call still up 30s after a `redirect` carrying a bare hangup verb — "+
			"the agent verb kept running and the caller is left in dead air: %v", err)
	}
	if reason := call.EndReason(); reason != "remote-bye" {
		s.Errorf("call ended as %q, want \"remote-bye\" — jambonz did not initiate the "+
			"teardown, so the hangup verb is not what ended it", reason)
	}
	s.Done()
}

// --- defect 4c --------------------------------------------------------------

// TestVerb_Agent_Defect4c_AgentUpdateInjectContextAndGenerateReply — the
// operator asked whether agent:update is supported in 10.2.1. It is
// (_onCommand → _lccAgentUpdate → processAgentUpdate), and this pins the two
// sub-commands they named, end to end.
//
// inject_context appends messages to the LLM's conversation without prompting;
// generate_reply then makes the agent speak. Sending them as a pair is what
// makes the test observable: the injected fact is a word the model could not
// otherwise produce, so hearing it back proves BOTH that the context landed and
// that generate_reply consumed it.
//
// Timing note worth keeping: generate_reply runs immediately only in Idle. It
// is sent here after the greeting has finished for exactly that reason —
// mid-speech it would queue until the agent returns to Idle (or interrupt, with
// interrupt:true), which would make the test time-dependent for no benefit.
//
// Steps:
//  1. preflight-skips
//  2. script-agent-verb
//  3. place-ws-call
//  4. answer-record-and-silence
//  5. wait-for-greeting-to-finish
//  6. send-inject-context
//  7. send-generate-reply
//  8. wait-for-reply
//  9. assert-injected-fact-spoken
func TestVerb_Agent_Defect4c_AgentUpdateInjectContextAndGenerateReply(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	// A word the model has no reason to say unless it was injected.
	const injectedFact = "kingfisher"

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: "You are a brief voice assistant. Keep every reply to one short sentence. " +
			"Greet the caller in one sentence on your first turn.",
		Greeting: true,
	}, nil)
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-record-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting-to-finish")
	// One short sentence: ~3s of LLM + TTS, then Idle. Waiting past it is
	// what lets generate_reply execute immediately rather than queue.
	time.Sleep(10 * time.Second)
	s.Done()

	s = Step(t, "send-inject-context")
	if err := sess.SendCommand("agent:update", map[string]any{
		"type": "inject_context",
		"messages": []map[string]any{
			{"role": "system", "content": "The caller's account codeword is " + injectedFact + "."},
		},
	}); err != nil {
		s.Fatalf("SendCommand(agent:update inject_context): %v", err)
	}
	s.Done()

	s = Step(t, "send-generate-reply")
	recPath := filepath.Join(t.TempDir(), "defect4c-generated-reply.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	// Small gap so inject_context is applied before the reply is generated;
	// both ride the same socket, but they are handled independently.
	time.Sleep(1 * time.Second)
	if err := sess.SendCommand("agent:update", map[string]any{
		"type":         "generate_reply",
		"instructions": "Tell the caller their account codeword, saying the word itself clearly.",
	}); err != nil {
		s.Fatalf("SendCommand(agent:update generate_reply): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-reply")
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "generate_reply")) > 0 &&
			len(findAgentEvents(c, "llm_response")) >= 2
	})
	s.Logf("captured %d agent events: %s", len(cbs), summarizeEventTypes(cbs))
	time.Sleep(3 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-injected-fact-spoken")
	if len(findAgentEvents(cbs, "generate_reply")) == 0 {
		s.Errorf("no generate_reply event — the agent:update command did not reach the verb; "+
			"events=%s", summarizeEventTypes(cbs))
	}
	// Deepgram renders the synthesized word inconsistently over a telephony
	// codec ("…is kingfisher" comes back as "disking fisher"), so match the
	// distinctive second half rather than the whole token. What is being
	// proven is that the injected fact reached the model at all.
	AssertTranscriptHasMost(s, ctx, recPath, 1, "fisher")
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// --- defect 5 ---------------------------------------------------------------

// TestVerb_Agent_Defect5_LargeSystemPromptOverWS — a session:new ack larger
// than the WS client's maxPayload must not kill the call silently.
//
// The operator shipped a ~22 KB system prompt in an agent verb over the
// WebSocket API. JAMBONES_WS_MAX_PAYLOAD defaults to 24 KB, the frame exceeded
// it, and the ws library closed the socket with 1009. What they got: a dead
// call, nothing on the app socket, and an alert that said "timeout" because the
// only thing that eventually failed was the session:new response timer. It
// cost them a day.
//
// Two separate things are under test, and they fail differently:
//
//   - if the cluster's limit accommodates the ack, the call runs normally and
//     this test passes — that is the "raise the env var" remedy working
//   - if it does not, the call dies, and the assertion below is what the
//     operator never got: a statement that the payload was the cause
//
// Either way the value of the test is that the failure is attributable. It is
// NOT asserting a particular limit; it asserts that a large-but-reasonable
// agent prompt does not silently destroy the call.
//
// Steps:
//  1. preflight-skips
//  2. script-agent-verb-with-large-prompt
//  3. place-ws-call
//  4. answer-and-silence
//  5. wait-for-greeting
//  6. assert-agent-ran
func TestVerb_Agent_Defect5_LargeSystemPromptOverWS(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-with-large-prompt")
	// ~30 KB of filler, decisively past the 24 KB default so the test is not
	// sitting on the boundary where envelope overhead decides the outcome.
	// The operative instruction is FIRST so the model still behaves if the
	// prompt survives — the filler is shaped like the policy text real
	// deployments accumulate, not random bytes.
	const filler = "Always remain polite and concise, never speculate about account details, " +
		"and escalate to a human whenever the caller asks for one. "
	var b strings.Builder
	b.WriteString("You are a brief voice assistant. On your first turn say exactly: " +
		"The large prompt arrived intact. Then stop. ")
	for b.Len() < 30*1024 {
		b.WriteString(filler)
	}
	systemPrompt := b.String()
	s.Logf("system prompt is %d bytes; JAMBONES_WS_MAX_PAYLOAD default is %d",
		len(systemPrompt), 24*1024)
	scriptAgentTweaked(sess, agentVerbOpts{
		SystemPrompt: systemPrompt,
		Greeting:     true,
	}, nil)
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "defect5-greeting.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	defer cancel()
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "llm_response")) > 0
	})
	s.Logf("captured %d events: %s", len(cbs), summarizeEventTypes(cbs))
	time.Sleep(3 * time.Second)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-agent-ran")
	if len(findAgentEvents(cbs, "llm_response")) == 0 {
		s.Errorf("the agent never spoke after a session:new ack carrying a %d-byte system "+
			"prompt. If the ack exceeded the WS client's maxPayload the socket was closed "+
			"with 1009 and the call died with nothing reported to the app — raise "+
			"JAMBONES_WS_MAX_PAYLOAD on the feature-server. events=%s ended=%v reason=%q",
			len(systemPrompt), summarizeEventTypes(cbs),
			call.State(), call.EndReason())
		s.Done()
		return
	}
	AssertTranscriptNonEmpty(s, ctx, recPath)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
