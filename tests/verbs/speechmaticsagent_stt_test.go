// Tests for the `gather` and `transcribe` verbs with STT vendor
// "speechmaticsagent" — the Speechmatics Agent STT API
// (wss://<host>/v2/agent/<profile>, the Linden-1 endpoint), which is a
// different vendor from "speechmatics" in every layer: its own speech
// credential (api_key only, no speechmatics_stt_uri), its own media-server
// dialect, and its own turn-taking, signalled by the vendor rather than
// inferred from a silence trigger.
//
// What these cover that the classic speechmatics tests do not:
//
//   - the speechmaticsagent credential provisions and authenticates
//   - speechmaticsOptions.profile reaches the media server and picks the
//     agent endpoint's path segment
//   - transcription_config still applies on the agent endpoint (the gather
//     test proves it end to end with a word-replacement rule)
//   - a gather turn finalizes on the vendor's own EndOfTurn, with no
//     conversation_config.end_of_utterance_silence_trigger configured
//
// speechmaticsagent is an OPTIONAL vendor, keyed separately from classic
// speechmatics because the endpoint and the entitlement differ: TestMain
// provisions the credential only when SPEECHMATICS_AGENT_API_KEY is set.
// When it is unset these tests pass immediately after a log — a plain
// `return`, never t.Skip, never a failure — so the suite stays green with
// or without the key.
package verbs

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// speechmaticsAgentOptions builds the recognizer.speechmaticsOptions the
// agent-STT tests send: the turn-taking profile always, and the host only
// when one is configured, so an unset SPEECHMATICS_AGENT_HOST leaves the
// media server on its own default rather than pinning the tests to a
// hostname that moves at GA.
func speechmaticsAgentOptions(extra map[string]any) map[string]any {
	opts := map[string]any{"profile": cfg.SpeechmaticsAgentProfile}
	if cfg.SpeechmaticsAgentHost != "" {
		opts["host"] = cfg.SpeechmaticsAgentHost
	}
	for k, v := range extra {
		opts[k] = v
	}
	return opts
}

// TestVerb_Gather_Speech_SpeechmaticsAgent — stream a WAV into `gather
// input=[speech]` using recognizer vendor "speechmaticsagent" with a
// transcript_filtering_config replacement rule, then assert the replacement
// shows up in the returned transcript. Seeing "glowing" proves the whole
// path held: webhook JSON -> feature-server channel vars
// (SPEECHMATICS_AGENT_*) -> the media server's agent dialect ->
// StartRecognition on /v2/agent/<profile>. That the action hook fires at
// all proves the turn finalized on the vendor's EndOfTurn, since nothing
// here configures a silence trigger.
func TestVerb_Gather_Speech_SpeechmaticsAgent(t *testing.T) {
	if !cfg.HasSpeechmaticsAgent() || speechmaticsAgentLabel == "" {
		t.Log("SPEECHMATICS_AGENT_API_KEY not set — passing without exercising speechmatics agent STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "load-ground-truth")
	wavPath, truthPath := resolveFixture(t, speechWAV), resolveFixture(t, speechTranscriptTxt)
	truthBytes, err := os.ReadFile(truthPath)
	if err != nil {
		s.Fatalf("read truth transcript: %v", err)
	}
	truth := strings.ToLower(strings.TrimSpace(string(truthBytes)))
	s.Logf("ground truth: %q", truth)
	s.Done()

	s = Step(t, "script-gather-speech-speechmaticsagent")
	actionURL := SessionURL(sess, "gather")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("gather",
			"input", []any{"speech"},
			"timeout", 15,
			"actionHook", actionURL,
			"recognizer", map[string]any{
				"vendor":   "speechmaticsagent",
				"label":    speechmaticsAgentLabel,
				"language": "en-US",
				"speechmaticsOptions": speechmaticsAgentOptions(map[string]any{
					"transcription_config": map[string]any{
						"transcript_filtering_config": map[string]any{
							"replacements": []any{
								map[string]any{"from": "/[Ss]hining/", "to": "glowing"},
							},
						},
					},
				}),
			}),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "gather")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
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
	// same LONG pad the classic speechmatics gather uses: a streaming vendor
	// plus its network round-trip has to be armed before the WAV starts
	time.Sleep(RecognizerArmDelayLong)
	s.Done()

	s = Step(t, "send-wav")
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV(%s): %v", wavPath, err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	s.Done()

	s = Step(t, "wait-action-gather-callback")
	waitCtx, wcancel := context.WithTimeout(ctx, 45*time.Second)
	defer wcancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/gather")
	if err != nil {
		s.Fatalf("WaitCallbackFor action/gather: %v", err)
	}
	s.Logf("action/gather body: %s", string(cb.Body))
	s.Done()

	s = Step(t, "assert-word-replacement-applied")
	transcript := extractTranscript(cb)
	if transcript == "" {
		s.Fatalf("no transcript in action/gather payload: %s", string(cb.Body))
	}
	s.Logf("recognized: %q", transcript)
	normalized := strings.ToLower(transcript)
	// "sun" proves the agent endpoint heard the fixture at all; "glowing"
	// proves the /[Ss]hining/ -> glowing replacement was applied.
	if !strings.Contains(normalized, "sun") && !strings.Contains(normalized, "glowing") {
		s.Fatalf("transcript %q matched neither sun nor glowing (truth=%q)", transcript, truth)
	}
	if strings.Contains(normalized, "shining") {
		s.Errorf("transcript %q still contains %q — replacement rule not applied", transcript, "shining")
	}
	if !strings.Contains(normalized, "glowing") {
		s.Errorf("transcript %q missing replacement %q (truth=%q)", transcript, "glowing", truth)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}

// TestVerb_Transcribe_SpeechmaticsAgent — `transcribe` runs continuous
// STT via recognizer vendor "speechmaticsagent". The agent API keeps the
// session open across turns (continuesAfterFinal), so this asserts the
// longer weather fixture arrives across one or more transcription hooks.
func TestVerb_Transcribe_SpeechmaticsAgent(t *testing.T) {
	if !cfg.HasSpeechmaticsAgent() || speechmaticsAgentLabel == "" {
		t.Log("SPEECHMATICS_AGENT_API_KEY not set — passing without exercising speechmatics agent STT")
		return
	}

	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)

	_, sess := claimSession(t)

	s := Step(t, "script-transcribe-pause-hangup-speechmaticsagent")
	transcriptionURL := SessionURL(sess, "transcription")
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("transcribe",
			"transcriptionHook", transcriptionURL,
			"recognizer", map[string]any{
				"vendor":   "speechmaticsagent",
				"label":    speechmaticsAgentLabel,
				"language": "en-US",
				"speechmaticsOptions": speechmaticsAgentOptions(map[string]any{
					"transcription_config": map[string]any{
						"transcript_filtering_config": map[string]any{
							"remove_disfluencies": true,
						},
					},
				}),
			}),
		V("pause", "length", 15),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "transcription")
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
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
	wavPath, err := tts.EnsureWAV(ctx, "testdata/transcribe", transcribeText, tts.PromptOptions{
		Model: "aura-asteria-en",
	})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	s.Done()

	s = Step(t, "post-speech-silence")
	if err := call.SendSilence(); err != nil {
		s.Fatalf("post-SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "collect-transcription-hook")
	deadline := time.Now().Add(30 * time.Second)
	var parts []string
	transcript := ""
	for time.Now().Before(deadline) {
		for _, sess := range sessionsToDrain(sess) {
			if cb, err := tryPop(sess); err == nil {
				if cb.Hook != "action/transcription" {
					continue
				}
				s.Logf("action/transcription body: %s", string(cb.Body))
				if seg := strings.ToLower(extractTranscript(cb)); seg != "" {
					parts = append(parts, seg)
					transcript = strings.Join(parts, " ")
				}
			}
		}
		if transcript != "" && transcriptHits(transcript) >= 2 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	s.Done()

	s = Step(t, "assert-transcript-weather-words")
	if transcript == "" {
		s.Fatalf("no transcript received within timeout")
	}
	if hits := transcriptHits(transcript); hits < 2 {
		s.Errorf("transcript %q matched only %d of %v; want >= 2",
			transcript, hits, transcribeWords)
	}
	s.Done()

	s = Step(t, "hangup")
	_ = call.Hangup()
	s.Done()
}
