// Agent emotion: the LLM's <voice …/> directive is rendered as the TTS
// vendor's own tag (soniox: empathetic → "[warm]") and never spoken, hooks
// carry the text without directives, and a barge-in trims the reply to what
// was heard without tag characters. Every test passes without exercising
// soniox when SONIOX_API_KEY is unset.
package verbs

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const emotionEchoSystemPrompt = "Start every reply with exactly this directive: <voice emotion=\"empathetic\"/> " +
	"and then echo back whatever you hear from the user, word for word."

// words that would only be heard if a tag or directive were read aloud
var emotionTagWords = []string{"warm", "voice", "emotion", "empathetic"}

func emotionStyles(cb webhook.Callback) []map[string]any {
	raw, _ := cb.JSON["styles"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		if m, ok := r.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestVerb_Agent_Emotion_TagNotSpoken — the directive reaches soniox as a
// tag in front of the sentence; the caller hears the sentence only.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (soniox tts-rt-v2, emotion on)
//  4. place-call
//  5. answer-and-silence
//  6. record-and-speak
//  7. assert-echo-without-tags (independent STT of the reply audio)
//  8. assert-llm-response-clean (no directive, styles[0].emotion = empathetic)
//  9. assert-turn-end-styles
//  10. hangup-and-wait-ended
func TestVerb_Agent_Emotion_TagNotSpoken(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wav, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt:      emotionEchoSystemPrompt,
		NoResponseTimeout: &noReprompt,
		TTS:               sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
		Emotion:           map[string]any{"listen": "llm"},
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

	WaitFor(t, "wait-for-stt", RecognizerArmDelay)

	s = Step(t, "record-and-speak")
	rec := filepath.Join(t.TempDir(), "emotion-echo.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (pre): %v", err)
	}
	if err := call.SendWAV(wav); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence (post): %v", err)
	}
	time.Sleep(LLMReplyWindow)
	call.StopRecording()
	s.Done()

	s = Step(t, "assert-echo-without-tags")
	AssertTranscriptHasMost(s, ctx, rec, 2, "alpha", "bravo", "charlie", "delta")
	if stt.HasKey() {
		transcript, err := stt.Transcribe(ctx, rec)
		if err != nil {
			s.Fatalf("stt.Transcribe: %v", err)
		}
		for _, w := range emotionTagWords {
			if strings.Contains(transcript, w) {
				s.Errorf("tag or directive read aloud: %q contains %q", transcript, w)
			}
		}
	}
	s.Done()

	cbs := DrainCallbacks(sess, 3*time.Second)

	s = Step(t, "assert-llm-response-clean")
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Fatalf("no llm_response: %s", summarizeEventTypes(cbs))
	}
	resp := responses[0]
	if text := resp.String("response"); strings.Contains(text, "<voice") || text == "" {
		s.Errorf("llm_response.response not clean: %q", text)
	}
	styles := emotionStyles(resp)
	if len(styles) == 0 || styles[0]["emotion"] != "empathetic" {
		s.Errorf("llm_response.styles = %v, want a leading empathetic style", resp.JSON["styles"])
	} else {
		s.Logf("styles: %v", styles)
	}
	s.Done()

	s = Step(t, "assert-turn-end-styles")
	found := false
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if _, ok := te.JSON["agent_styles"].([]any); !ok {
			continue
		}
		found = true
		if strings.Contains(te.String("response"), "<voice") {
			s.Errorf("turn_end response holds a directive: %q", te.String("response"))
		}
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			s.Errorf("styled reply marked interrupted: %v", te.JSON)
		}
	}
	if !found {
		s.Errorf("no turn_end with agent_styles: %s", summarizeEventTypes(cbs))
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Emotion_BargeInTrim — a styled reply interrupted mid-way is
// trimmed to what the caller heard; soniox counts tag characters in its
// alignment, and the trimmed text must hold none of them.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (barge-in on, long styled reply)
//  4. place-call
//  5. answer-and-silence
//  6. ask-for-long-reply
//  7. wait-into-reply
//  8. barge-in
//  9. assert-reply-trimmed-without-tags
//  10. hangup-and-wait-ended
func TestVerb_Agent_Emotion_BargeInTrim(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		s.Fatalf("EnsureWAV: %v", err)
	}
	askWAV := resolveFixture(t, speechWAV)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "You are a friendly voice assistant. " +
			"If the user says a list of code words, repeat them back verbatim. " +
			"Otherwise answer with a long, slow reply of at least six full sentences about what the user said, " +
			"and start every sentence with a voice directive, alternating <voice emotion=\"calm\"/> and " +
			"<voice emotion=\"happy\"/>.",
		BargeIn:           true,
		NoResponseTimeout: &noReprompt,
		TTS:               sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
		Emotion:           map[string]any{"listen": "llm"},
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

	WaitFor(t, "settle", 2*time.Second)

	s = Step(t, "ask-for-long-reply")
	if err := call.SendWAV(askWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	WaitFor(t, "wait-into-reply", 8*time.Second)

	s = Step(t, "barge-in")
	if err := call.SendWAV(bargeWAV); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	cbs := DrainCallbacks(sess, LLMReplyWindow)

	s = Step(t, "assert-reply-trimmed-without-tags")
	if len(findAgentEvents(cbs, "user_interruption")) == 0 {
		s.Errorf("no user_interruption event: %s", summarizeEventTypes(cbs))
	}
	trimmed := ""
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			trimmed = te.String("response")
			break
		}
	}
	switch {
	case !strings.HasSuffix(trimmed, " -"):
		s.Errorf("interrupted turn_end response not trimmed: %q (%s)", trimmed, summarizeEventTypes(cbs))
	case strings.ContainsAny(trimmed, "[<"):
		s.Errorf("trimmed reply holds tag characters: %q", trimmed)
	default:
		s.Logf("reply heard before barge-in: %q", trimmed)
	}
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}

// TestVerb_Agent_Emotion_AudibleContrast — the style is heard, not only
// signalled: the same sentence is greeted once sad and once happy on
// ElevenLabs eleven_v3, and an independent acoustic emotion model
// (smallestai pulse-2) must score the sad take sadder and the happy take
// happier than each other. A relative check, because a single label from the
// model on synthetic speech is not stable.
//
// Steps (per take):
//  1. script-agent-verb (greeting with a directive)
//  2. place-call
//  3. record-greeting
//  4. score-emotion
//
// then assert-contrast.
func TestVerb_Agent_Emotion_AudibleContrast(t *testing.T) {
	requireElevenlabs(t)
	if !cfg.HasSmallestai() {
		t.Log("SMALLESTAI_API_KEY not set — passing without scoring emotion")
		return
	}
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	const sentence = "The meeting has moved to Thursday afternoon at three o'clock."
	take := func(emotion string) map[string]float64 {
		ctx := WithTimeout(t, 90*time.Second)
		uas := claimUAS(t, ctx)
		_, sess := claimSession(t)

		s := Step(t, "script-agent-verb-"+emotion)
		noReprompt := 0
		ScriptAgent(sess, agentVerbOpts{
			SystemPrompt: "Your whole first reply is exactly this, with nothing before or after it: " +
				"<voice emotion=\"" + emotion + "\"/>" + sentence,
			Greeting:          true,
			NoResponseTimeout: &noReprompt,
			TTS:               elevenlabsSynth("eleven_v3"),
			Emotion:           map[string]any{"listen": "off", "emotions": []string{"sad", "happy"}},
		})
		s.Done()

		s = Step(t, "place-call-"+emotion)
		call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
		s.Done()

		s = Step(t, "record-greeting-"+emotion)
		rec := filepath.Join(t.TempDir(), "emotion-"+emotion+".pcm")
		if err := call.Answer(); err != nil {
			s.Fatalf("Answer: %v", err)
		}
		if err := call.StartRecording(rec); err != nil {
			s.Fatalf("StartRecording: %v", err)
		}
		if err := call.SendSilence(); err != nil {
			s.Fatalf("SendSilence: %v", err)
		}
		time.Sleep(LLMReplyWindow)
		call.StopRecording()
		s.Done()

		s = Step(t, "score-emotion-"+emotion)
		AssertTranscriptHasMost(s, ctx, rec, 2, "meeting", "thursday", "afternoon", "three")
		scores, err := stt.SmallestaiEmotion(ctx, rec, cfg.SmallestaiAPIKey)
		if err != nil {
			s.Fatalf("SmallestaiEmotion: %v", err)
		}
		s.Logf("%s take scores: %v", emotion, scores)
		s.Done()
		HangupAndWaitEnded(t, ctx, call)
		return scores
	}

	sad := take("sad")
	happy := take("happy")

	s = Step(t, "assert-contrast")
	if sad["sad"] <= happy["sad"] {
		s.Errorf("sad take not sadder: sad=%.3f vs happy take sad=%.3f", sad["sad"], happy["sad"])
	}
	if happy["happy"] <= sad["happy"] {
		s.Errorf("happy take not happier: happy=%.3f vs sad take happy=%.3f", happy["happy"], sad["happy"])
	}
	s.Done()
}
