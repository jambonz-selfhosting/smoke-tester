// Tests for the `agent` verb with LLM vendor "xai" (Grok).
//
// xai is an OPTIONAL vendor (see config.HasXai in internal/config/config.go):
// the SAME XAI_API_KEY used for the xai STT/TTS smoke tests also works
// against xAI's chat-completions endpoint, so no new config or credential
// provisioning is needed — the key is supplied inline via
// agent.llm.auth.apiKey (feature-server skips the DB credential lookup when
// auth is set on the verb, same as the Deepseek default — see
// feature-server/lib/tasks/agent/index.js:446). When XAI_API_KEY is unset
// the test passes immediately without exercising the xai agent LLM — a
// plain `return` after a log, never t.Skip, never a failure, matching
// xai_stt_test.go / xai_tts_test.go.
//
// Clone of TestVerb_Agent_EventHook (agent_test.go) with the agent verb's
// LLM vendor/model/apiKey swapped to xai via agentVerbOpts.LLMVendor/
// LLMModel/LLMApiKey. STT + TTS stay on Deepgram, as in every other agent
// test.
package verbs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

// TestVerb_Agent_Xai — eventHook fires user_transcript, llm_response and
// turn_end events around each conversational turn, with the agent's LLM
// running on xai/Grok instead of the Deepseek default. We assert each event
// type appears at least once, with a content-bearing payload — same
// contract as TestVerb_Agent_EventHook.
//
// Steps:
//  1. preflight-skips (agentSkipPreflight, plus the xai key guard)
//  2. ensure-prompt-wav
//  3. register-webhook-session
//  4. script-agent-verb-xai (eventHook → /action/agent-turn — schema-validated)
//  5. place-call
//  6. answer-and-silence
//  7. wait-for-stt
//  8. send-prompt-wav
//  9. wait-for-events — silence while LLM thinks + TTS streams
//
// 10. drain-anon-events
// 11. assert-user-transcript-xai — find a user_transcript event with our keywords
// 12. assert-llm-response-xai — find an llm_response event with non-empty body
// 13. assert-turn-end-xai — find a turn_end with transcript + response + latency
// 14. hangup
func TestVerb_Agent_Xai(t *testing.T) {
	t.Parallel()
	requireWebhook(t)

	if !cfg.HasXai() {
		t.Log("XAI_API_KEY not set — passing without exercising xai agent LLM")
		return
	}

	// NOTE: deliberately does NOT use agentSkipPreflight — that helper
	// requires DEEPSEEK_API_KEY (the default agent-verb LLM), which this
	// test does not use (the LLM is xai, supplied inline below). We only
	// need Deepgram, for the agent's STT/TTS and the reply-verify STT.
	// Deepgram missing → pass-and-return (never t.Skip / fail), same
	// posture as the xai-key guard above.
	s := Step(t, "preflight-skips")
	if !cfg.HasDeepgram() || deepgramLabel == "" {
		s.Done()
		t.Log("Deepgram STT/TTS not available — passing without exercising xai agent LLM")
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb-xai")
	// Append ?X-Test-Id=<testID> to per-callback URLs so the webhook server
	// routes eventHook callbacks to THIS session (the payload itself has no
	// customerData and would otherwise land in shared `_anon`, racing with
	// parallel agent tests).
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: agentEchoSystemPrompt,
		LLMVendor:    "xai",
		LLMModel:     xaiLlmModel,
		LLMApiKey:    cfg.XaiAPIKey,
	})
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	s = Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	// Record the caller leg (inbound = the agent's xai-driven TTS reply) so
	// RECORD_LEGS=1 archives a playable WAV under
	// <RecordDir>/TestVerb_Agent_Xai/agent-xai-reply.wav a developer can
	// listen to. Started after Answer (StartRecording requires
	// StateAnswered); we stop it explicitly once the reply window closes.
	// The eventHook assertions below are the functional check — this
	// recording is purely for human listening (ADR-0016 / RECORD_LEGS).
	recPath := filepath.Join(t.TempDir(), "agent-xai-reply.pcm")
	if err := call.StartRecording(recPath); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "send-prompt-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-events")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	// Drain THIS session while events stream in (eventHook URL was
	// minted via SessionURL so it carries our X-Test-Id query param —
	// no `_anon` contention). Each event type fires at a different
	// moment (user_transcript ~when STT finalizes, llm_response ~when
	// LLM stream completes, turn_end ~when TTS empties).
	cbs := DrainCallbacks(sess, LLMReplyWindow)
	call.StopRecording()
	s.Logf("captured %d agent events", len(cbs))
	s.Done()

	s = Step(t, "assert-user-transcript-xai")
	transcripts := findAgentEvents(cbs, "user_transcript")
	if len(transcripts) == 0 {
		s.Fatalf("no user_transcript event in %d agent events: %s",
			len(cbs), summarizeEventTypes(cbs))
	}
	// The transcript should contain at least one of our injected keywords —
	// proves it was OUR call's event, not a parallel test's.
	matched := ""
	for _, cb := range transcripts {
		txt := strings.ToLower(cb.String("transcript"))
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(txt, kw) {
				matched = txt
				break
			}
		}
		if matched != "" {
			break
		}
	}
	if matched == "" {
		s.Errorf("no user_transcript matched our prompt; got %d transcripts: %v",
			len(transcripts), summarizeTranscripts(transcripts))
	} else {
		s.Logf("user_transcript matched: %q", matched)
	}
	s.Done()

	s = Step(t, "assert-llm-response-xai")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("no llm_response event in %d agent events", len(cbs))
	} else {
		// The echo system prompt forces the LLM to repeat the prompt's
		// words. Concatenate every llm_response and require at least
		// one of our 4 keywords lands in the text — proves the LLM
		// actually responded to OUR prompt's content (a regression
		// that emits hallucinated/empty/generic text would fail).
		var all string
		for _, r := range responses {
			all += " " + strings.ToLower(r.String("response"))
		}
		if strings.TrimSpace(all) == "" {
			s.Errorf("all llm_response events have empty response field")
		}
		hits := 0
		for _, kw := range []string{"alpha", "bravo", "charlie", "delta"} {
			if strings.Contains(all, kw) {
				hits++
			}
		}
		if hits == 0 {
			s.Errorf("llm_response %q contains none of the prompt's keywords (alpha/bravo/charlie/delta)",
				truncate(all, 200))
		} else {
			s.Logf("llm_response keyword hits=%d: %q", hits, truncate(all, 100))
		}
	}
	s.Done()

	s = Step(t, "assert-turn-end-xai")
	turnEnds := findAgentEvents(cbs, "turn_end")
	if len(turnEnds) == 0 {
		s.Errorf("no turn_end event in %d agent events", len(cbs))
	} else {
		// Schema requires transcript + response + interrupted + latency.
		// validateInbound ran already; we just confirm fields are present.
		te := turnEnds[0].JSON
		for _, want := range []string{"transcript", "response", "interrupted", "latency"} {
			if _, ok := te[want]; !ok {
				s.Errorf("turn_end missing required field %q: %s",
					want, string(turnEnds[0].Body))
			}
		}
		// Latency block should at least include voice_latency or model_latency.
		if lat, ok := te["latency"].(map[string]any); ok {
			s.Logf("turn_end latency: %v", lat)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}
