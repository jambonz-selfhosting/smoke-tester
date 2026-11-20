//go:build detail

package verbs

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func TestVerb_LLM_GptLive_AgentSpeaksFirst(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S")
		return
	}
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 300*time.Second)

	var allCBs []webhook.Callback

	// one attempt: place a call, send only silence, measure what the caller heard
	attempt := func(n int) (peak int, recPath string, sawSessionStarted bool, types []string) {
		uas := claimUAS(t, ctx)
		_, sess := claimSession(t)

		st := Step(t, fmt.Sprintf("attempt-%d-script-and-call", n))
		llmVerb := gptLiveVerb(map[string]any{
			"instructions": gptLiveGreetPrompt,
			"audio":        map[string]any{"output": map[string]any{"voice": gptLiveVoice}},
			"delegation":   map[string]any{"type": "client"},
		}, "eventHook", SessionURL(sess, "llm-gptlive-event"))
		sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
		SessionAckEmpty(sess, "llm")
		SessionAckEmpty(sess, "llm-gptlive-event")

		// WS transport: session.commentary.append is only reachable as an llm:update
		// COMMAND over the application WebSocket — the REST updateCall API does
		// not accept llm_update — so a webhook-transport call cannot ask the
		// agent to open the conversation at all.
		call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(90))
		if err := call.Answer(); err != nil {
			st.Fatalf("Answer: %v", err)
		}
		// outbound RTP FIRST: jambonz uses symmetric RTP and cannot return audio
		// until our silence opens the path, so recording before this loses the
		// greeting outright.
		if err := call.SendSilence(); err != nil {
			st.Fatalf("SendSilence: %v", err)
		}
		recPath = filepath.Join(t.TempDir(), fmt.Sprintf("greeting-%d.pcm", n))
		if err := call.StartRecording(recPath); err != nil {
			st.Fatalf("StartRecording: %v", err)
		}
		st.Done()

		st = Step(t, fmt.Sprintf("attempt-%d-listen", n))
		// A FIXED window, deliberately not gated on a playback event: that event
		// fires for silent deltas, so gating on it stopped the recorder before the
		// real greeting and made this flaky in both directions.
		var drained []webhook.Callback
		requested := false
		deadline := time.Now().Add(GptLiveGreetingWindow)
		for time.Now().Before(deadline) {
			batch := DrainCallbacks(sess, 2*time.Second)
			drained = append(drained, batch...)
			for _, cb := range batch {
				if cb.Hook != gptLiveEventHook {
					continue
				}
				ty := cb.String("type")
				if ty != "" {
					types = append(types, ty)
				}
				if ty != "session.started" {
					continue
				}
				sawSessionStarted = true
				if requested {
					continue
				}
				requested = true
				// Ask the agent to open the call, the moment the session is up.
				if err := sess.SendCommand("llm:update", map[string]any{
					"type":          "session.commentary.append",
					"delegation_id": nil,
					"content":       gptLiveGreetRequest,
				}); err != nil {
					st.Fatalf("SendCommand(llm:update session.commentary.append): %v", err)
				}
			}
		}
		allCBs = append(allCBs, drained...)
		if !requested && sawSessionStarted {
			st.Errorf("session.started arrived but the greeting request was never sent")
		}
		call.StopRecording()
		HangupAndWaitEnded(t, ctx, call)

		peak = gptLivePeak(t, recPath)
		st.Logf("attempt %d: peak=%d sessionStarted=%v", n, peak, sawSessionStarted)
		st.Done()
		return peak, recPath, sawSessionStarted, types
	}

	var bestPeak int
	var bestPath string
	var lastStarted bool
	var lastTypes []string
	for n := 1; n <= gptLiveGreetAttempts; n++ {
		peak, path, started, types := attempt(n)
		lastStarted, lastTypes = started, types
		if peak > bestPeak {
			bestPeak, bestPath = peak, path
		}
		if peak >= gptLiveAudibledB {
			break
		}
	}

	s = Step(t, "assert-greeting-heard")
	if bestPeak < gptLiveAudibledB {
		if !lastStarted {
			s.Errorf("no session.started in %d attempts, so the media server's input gate never "+
				"lifted and no caller audio reached the vendor — with no audio the model never "+
				"generates. events=%v", gptLiveGreetAttempts, lastTypes)
		} else {
			s.Errorf("the agent never opened the conversation in %d attempts (best peak=%d, "+
				"i.e. silence) even though a session.commentary.append greeting was sent on "+
				"each. Appended content GUIDES the model and it may stay silent, but %d "+
				"consecutive declines means either the request is not reaching the vendor "+
				"(check for a session.commentary.appended ack) or the vendor's behavior "+
				"changed. events=%v",
				gptLiveGreetAttempts, bestPeak, gptLiveGreetAttempts, lastTypes)
		}
		s.Done()
		return
	}
	// The passphrase can only be present if the agent spoke unprompted AND its
	// 24kHz audio survived resampling and playout to the caller.
	AssertTranscriptHasMost(s, ctx, bestPath, 1, gptLiveGreetPassphrase, gptLiveGreetPassphrase2)
	s.Done()

	s = Step(t, "assert-contract")
	gptLiveAssertContract(s, allCBs)
	s.Done()
}

// TestVerb_LLM_GptLive_BargeIn proves the caller can interrupt the agent.
// GPT-Live has no interruption event, so mediajam infers barge-in from the
// transcript timeline and drains its playout; the proof is the recording:
// the agent goes quiet soon after the caller starts talking, the story's last
// sentence is never heard, and the interrupting question gets answered.
//
// Steps:
//  1. preflight-skips — gptlive key guard, then deepgram guard (plain return)
//  2. ensure-prompt-wav — the interrupting question
//  3. script-and-call — gptlive verb + eventHook, WS transport (llm:update)
//  4. answer-and-record
//  5. start-monologue — on session.started, session.commentary.append
//  6. wait-for-agent-speech — first session.output_transcript.delta, then 2s
//  7. interrupt — send the question WAV while the agent is talking
//  8. wait-for-answer
//  9. hangup-and-wait-ended
//  10. assert-interruption-heard — input transcript mentions two / 2
//  11. assert-playout-cut — mediajam reports an interrupted playout, and the
//     recording goes quiet within gptLiveCutMaxMS of the interruption
//  12. assert-answer-not-story — "four" heard, marker word not heard
//  13. assert-contract — every eventHook payload matches its schema
func TestVerb_LLM_GptLive_BargeIn(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasGptLive() {
		t.Log("GPTLIVE_API_KEY not set — passing without exercising gptlive S2S barge-in")
		return
	}
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram not available — passing without exercising gptlive S2S barge-in")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 180*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	promptWAV, err := tts.EnsureWAV(ctx, "testdata/llm", gptLiveInterruptPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-and-call")
	llmVerb := gptLiveVerb(map[string]any{
		"instructions": "You are a voice assistant on a phone call. Always speak English. " +
			"If the caller interrupts you, stop and answer their question in one short sentence.",
		"audio":      map[string]any{"output": map[string]any{"voice": gptLiveVoice}},
		"delegation": map[string]any{"type": "client"},
	}, "eventHook", SessionURL(sess, "llm-gptlive-event"))
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{llmVerb, V("hangup")}))
	SessionAckEmpty(sess, "llm")
	SessionAckEmpty(sess, "llm-gptlive-event")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-record")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	recPath := filepath.Join(t.TempDir(), "gptlive-bargein.pcm")
	// the recorder writes only received RTP, so file offsets come from bytes written
	recBase := call.PCMBytesIn()
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	s.Done()

	var drained []webhook.Callback

	s = Step(t, "start-monologue")
	started := false
	for deadline := time.Now().Add(20 * time.Second); !started && time.Now().Before(deadline); {
		batch := DrainCallbacks(sess, time.Second)
		drained = append(drained, batch...)
		started = len(gptLiveEvents(batch, "session.started")) > 0
	}
	if !started {
		s.Fatalf("no session.started within 20s; completionReason=%q", gptLiveCompletionReason(drained))
	}
	if err := sess.SendCommand("llm:update", map[string]any{
		"type":          "session.commentary.append",
		"delegation_id": nil,
		"content":       gptLiveMonologue,
	}); err != nil {
		s.Fatalf("SendCommand(llm:update session.commentary.append): %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-agent-speech")
	speaking := false
	for deadline := time.Now().Add(20 * time.Second); !speaking && time.Now().Before(deadline); {
		batch := DrainCallbacks(sess, 500*time.Millisecond)
		drained = append(drained, batch...)
		speaking = len(gptLiveEvents(batch, "session.output_transcript.delta")) > 0
	}
	if !speaking {
		s.Fatalf("the agent never started the monologue (no session.output_transcript.delta in 20s)")
	}
	// transcripts trail the audio; this puts the caller well inside the story
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "interrupt")
	interruptMS := int((call.PCMBytesIn() - recBase) / 16) // 16 bytes/ms at 8kHz PCM16
	if err := call.SendWAV(promptWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Logf("interrupted at %dms into the recording", interruptMS)
	s.Done()

	s = Step(t, "wait-for-answer")
	drained = append(drained, DrainCallbacks(sess, 12*time.Second)...)
	call.StopRecording()
	s.Done()

	HangupAndWaitEnded(t, ctx, call)

	s = Step(t, "assert-interruption-heard")
	var heard strings.Builder
	for _, cb := range gptLiveEvents(drained, "session.input_transcript.delta") {
		heard.WriteString(cb.String("delta"))
	}
	s.Logf("caller transcript seen by GPT-Live: %q", heard.String())
	if words := strings.Fields(stt.Normalize(heard.String())); !slices.Contains(words, "two") && !slices.Contains(words, "2") {
		s.Errorf("GPT-Live never transcribed the interruption (%q); without it mediajam has no "+
			"barge-in signal", heard.String())
	}
	s.Done()

	s = Step(t, "assert-playout-cut")
	// only mediajam's drain emits this, so it separates the drain from the vendor pausing
	drainedByMediajam := false
	for _, cb := range gptLiveEvents(drained, "output_audio.playback_stopped") {
		if cb.String("completion_reason") == "interrupted" {
			drainedByMediajam = true
		}
	}
	if !drainedByMediajam {
		s.Errorf("no output_audio.playback_stopped{completion_reason:interrupted} — mediajam " +
			"never took a barge-in from the transcript timeline")
	}
	gapAt, gapLen, err := SilenceAfterMS(recPath, interruptMS, gptLiveCutGapMS, gptLiveAudibledB)
	if err != nil {
		s.Fatalf("SilenceAfterMS: %v", err)
	}
	switch {
	case gapAt < 0:
		s.Errorf("the agent never went quiet for %dms after the caller interrupted at %dms — "+
			"mediajam did not drain the playout", gptLiveCutGapMS, interruptMS)
	case gapAt-interruptMS > gptLiveCutMaxMS:
		s.Errorf("the agent kept talking for %dms after the caller interrupted (limit %dms) — "+
			"mediajam did not drain the playout", gapAt-interruptMS, gptLiveCutMaxMS)
	default:
		s.Logf("agent audio went quiet %dms after the interruption began, for %dms", gapAt-interruptMS, gapLen)
	}
	s.Done()

	s = Step(t, "assert-answer-not-story")
	transcript, err := stt.Transcribe(ctx, recPath)
	if err != nil {
		s.Fatalf("stt.Transcribe: %v", err)
	}
	s.Logf("transcript: %q", transcript)
	words := strings.Fields(transcript)
	if !slices.Contains(words, "pineapple") {
		s.Errorf("the story's opening was never heard, so nothing was interrupted: %q", transcript)
	}
	if slices.Contains(words, gptLiveMonologueMarker) {
		s.Errorf("the story's last sentence (%q) was heard — the interruption did not cut it off: %q",
			gptLiveMonologueMarker, transcript)
	}
	if !slices.Contains(words, "four") && !slices.Contains(words, "4") {
		s.Errorf("the interrupting question was not answered (no \"four\"): %q", transcript)
	}
	s.Done()

	s = Step(t, "assert-contract")
	gptLiveAssertContract(s, drained)
	s.Done()
}
