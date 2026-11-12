// Agent emotion across vendors: every TTS vendor renders the LLM's
// <voice …/> directive as its own tag without reading it aloud, the
// emotion-capable STTs report the caller's tone, and the LLM picks a fitting
// style on its own. Each case passes without exercising a vendor whose key is
// unset.
package verbs

import (
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/stt"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/wav"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const (
	cartesiaTTSModel  = "sonic-3"
	cartesiaVoice     = "f786b574-daa5-4673-aa0c-cbe3e8534c02" // Katie - Friendly Fixer
	fishaudioTTSModel = "s2.1-pro"
	fishaudioVoice    = "933563129e564b19a115bedd57b7406a" // "Sarah", public
	inworldVoice      = "Ashley"
	modulateSTTModel  = "velma-2-stt-streaming"

	emotionSentence = "The meeting has moved to Thursday afternoon at three o'clock."
	angryLine       = "I have called three times and nobody fixed it, this is unacceptable!"
	// Aria sounds angry to the STT emotion models; George's "angry" reads as unknown or fearful
	angryVoice = "9BWtsMINqrJLrRacOk9x"
)

var emotionSentenceWords = []string{"meeting", "moved", "thursday", "afternoon", "three"}

// emotionTTSVendor is one row of the TTS matrix.
type emotionTTSVendor struct {
	name   string
	ready  func() bool
	synth  func() map[string]any
	sounds bool // sound tags only, no emotions
	// tag words a listener would hear if the rendered tag for style were read aloud
	tagWords func(style string) []string
}

func emotionTTSVendors() []emotionTTSVendor {
	same := func(style string) []string { return []string{style} }
	adverb := map[string]string{"sad": "sadly", "happy": "happily", "calm": "calmly"}
	return []emotionTTSVendor{
		{name: "elevenlabs", ready: func() bool { return cfg.HasElevenlabs() && elevenlabsLabel != "" },
			synth: func() map[string]any { return elevenlabsSynth("eleven_v3") }, tagWords: same},
		{name: "soniox", ready: func() bool { return cfg.HasSoniox() && sonioxLabel != "" },
			synth: func() map[string]any {
				return sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel})
			}, tagWords: same},
		{name: "paxa", ready: func() bool { return cfg.HasPaxa() && paxaLabel != "" },
			synth:    func() map[string]any { return paxaSynth(paxaVoice, "en-US", nil) },
			tagWords: func(st string) []string { return []string{st, "neutral"} }},
		{name: "cartesia", ready: func() bool { return cfg.HasCartesia() && cartesiaLabel != "" },
			synth: func() map[string]any {
				return emotionSynth("cartesia", cartesiaLabel, cartesiaVoice, map[string]any{"model_id": cartesiaTTSModel})
			},
			tagWords: func(st string) []string { return []string{st, "value", "ratio"} }},
		{name: "fishaudio", ready: func() bool { return cfg.HasFishaudio() && fishaudioLabel != "" },
			synth: func() map[string]any {
				return emotionSynth("fishaudio", fishaudioLabel, fishaudioVoice, map[string]any{"model_id": fishaudioTTSModel})
			}, tagWords: same},
		// fish's own voice: "default" must not become the application's default voice
		{name: "fishaudio-default", ready: func() bool { return cfg.HasFishaudio() && fishaudioLabel != "" },
			synth: func() map[string]any {
				return emotionSynth("fishaudio", fishaudioLabel, "default", map[string]any{"model_id": fishaudioTTSModel})
			}, tagWords: same},
		{name: "inworld", ready: func() bool { return cfg.HasInworld() && inworldLabel != "" },
			synth: func() map[string]any {
				return emotionSynth("inworld", inworldLabel, inworldVoice, map[string]any{"model_id": inworldTTSModel})
			},
			tagWords: func(st string) []string { return []string{"say", adverb[st]} }},
		{name: "minimax", sounds: true, ready: func() bool { return cfg.HasMinimax() && minimaxLabel != "" },
			synth:    func() map[string]any { return minimaxSynth(cfg.MinimaxVoiceID, "en-US", nil) },
			tagWords: func(string) []string { return []string{"laughs"} }},
		{name: "xai", sounds: true, ready: func() bool { return cfg.HasXai() && xaiLabel != "" },
			synth:    func() map[string]any { return emotionSynth("xai", xaiLabel, xaiVoice, nil) },
			tagWords: func(string) []string { return []string{"laugh"} }},
	}
}

func emotionTTSVendorByName(name string) emotionTTSVendor {
	for _, v := range emotionTTSVendors() {
		if v.name == name {
			return v
		}
	}
	panic("unknown emotion vendor " + name)
}

func emotionSynth(vendor, label, voice string, options map[string]any) map[string]any {
	s := map[string]any{"vendor": vendor, "label": label, "voice": voice, "language": "en-US"}
	if options != nil {
		s["options"] = options
	}
	return s
}

// --- shared call drivers and assertions ------------------------------------

func hasEvent(cbs []webhook.Callback, typ string) bool { return len(findAgentEvents(cbs, typ)) > 0 }

// emotionGreeting runs a greeting-only agent call and returns the recording of
// the greeting and every callback, once the greeting's turn has ended.
// scriptedGreeting retries once when the LLM did not follow the scripted reply (no directive reached
// llm_response.styles); the LLM's compliance is not what these tests check.
func scriptedGreeting(t *testing.T, opts agentVerbOpts) (string, []webhook.Callback) {
	t.Helper()
	rec, cbs := emotionGreeting(t, opts)
	if r := findAgentEvents(cbs, "llm_response"); len(r) > 0 && len(emotionStyles(r[0])) == 0 {
		t.Logf("LLM did not follow the scripted reply (%q), retrying once", r[0].String("response"))
		rec, cbs = emotionGreeting(t, opts)
	}
	return rec, cbs
}

func emotionGreeting(t *testing.T, opts agentVerbOpts) (string, []webhook.Callback) {
	t.Helper()
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)
	opts.Greeting = true
	noReprompt := 0
	opts.NoResponseTimeout = &noReprompt
	ScriptAgent(sess, opts)

	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s := Step(t, "record-greeting")
	rec := filepath.Join(t.TempDir(), "greeting.pcm")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "turn_end") })
	cancel()
	time.Sleep(time.Second)
	call.StopRecording()
	cbs = append(cbs, DrainCallbacks(sess, 2*time.Second)...)
	s.Logf("events: %s", summarizeEventTypes(cbs))
	s.Done()
	HangupAndWaitEnded(t, ctx, call)
	return rec, cbs
}

// emotionCallerTurn answers, plays wavPath as the caller and returns the
// recording of the agent's reply plus every callback, once that turn has ended.
func emotionCallerTurn(t *testing.T, opts agentVerbOpts, wavPath string) (string, []webhook.Callback) {
	t.Helper()
	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)
	noReprompt := 0
	opts.NoResponseTimeout = &noReprompt
	ScriptAgent(sess, opts)

	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s := Step(t, "answer-and-silence")
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()
	WaitFor(t, "wait-for-stt", RecognizerArmDelayLong)

	s = Step(t, "speak-and-record-reply")
	rec := filepath.Join(t.TempDir(), "reply.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	if err := call.SendWAV(wavPath); err != nil {
		s.Fatalf("SendWAV: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "turn_end") })
	cancel()
	time.Sleep(time.Second)
	call.StopRecording()
	cbs = append(cbs, DrainCallbacks(sess, 2*time.Second)...)
	s.Logf("events: %s", summarizeEventTypes(cbs))
	s.Done()
	HangupAndWaitEnded(t, ctx, call)
	return rec, cbs
}

func transcribeReply(s *StepCtx, ctx context.Context, rec string) string {
	transcript, err := stt.Transcribe(ctx, rec)
	if err != nil {
		s.Fatalf("stt.Transcribe: %v", err)
	}
	transcript = stt.Normalize(transcript)
	s.Logf("transcript: %q", transcript)
	return transcript
}

func wordsOf(s string) []string { return strings.Fields(stt.Normalize(s)) }

// assertWordsHeard needs at least minHits of wants in the transcript.
func assertWordsHeard(s *StepCtx, transcript string, minHits int, wants ...string) {
	heard := wordsOf(transcript)
	var missing []string
	for _, w := range wants {
		if !slices.Contains(heard, w) {
			missing = append(missing, w)
		}
	}
	if len(wants)-len(missing) < minHits {
		s.Errorf("reply not heard: %q missing %v", transcript, missing)
	}
}

// assertNoTagWords flags a tag word that was heard although the clean reply
// text does not hold it.
func assertNoTagWords(s *StepCtx, transcript, clean string, tagWords []string) {
	heard, said := wordsOf(transcript), wordsOf(clean)
	for _, w := range append([]string{"voice", "emotion"}, tagWords...) {
		if w != "" && slices.Contains(heard, w) && !slices.Contains(said, w) {
			s.Errorf("tag read aloud: %q holds %q, the reply text %q does not", transcript, w, clean)
		}
	}
}

// firstCleanResponse asserts the first llm_response has text and no directive.
func firstCleanResponse(s *StepCtx, cbs []webhook.Callback) (webhook.Callback, bool) {
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("no llm_response: %s", summarizeEventTypes(cbs))
		return webhook.Callback{}, false
	}
	resp := responses[0]
	if text := resp.String("response"); text == "" || strings.Contains(text, "<voice") {
		s.Errorf("llm_response.response not clean: %q", text)
	}
	s.Logf("llm_response: %q styles=%v", resp.String("response"), resp.JSON["styles"])
	return resp, true
}

func assertNoTTSError(s *StepCtx, cbs []webhook.Callback) {
	for _, e := range findAgentEvents(cbs, "tts_error") {
		s.Errorf("tts_error: %v", e.JSON)
	}
}

// --- a. TTS matrix -----------------------------------------------------------

// TestVerb_Agent_Emotion_Matrix_TTS — per TTS vendor, a greeting opened by a
// directive: the sentence is heard, the vendor's tag is not, the hooks carry
// the clean text with styles, no tts_error. Emotion vendors also take a sad and
// a happy greeting that an acoustic model (smallestai) must tell apart.
//
// Steps (per vendor, per take):
//  1. record-greeting
//  2. assert-take
//
// then assert-contrast for emotion vendors.
func TestVerb_Agent_Emotion_Matrix_TTS(t *testing.T) {
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	for _, v := range emotionTTSVendors() {
		t.Run(v.name, func(t *testing.T) {
			if !v.ready() {
				t.Logf("%s key not set — passing without exercising %s", v.name, v.name)
				return
			}
			take := func(style string) string {
				directive := `<voice emotion="` + style + `"/>`
				emotion := map[string]any{"listen": "off", "emotions": []string{"sad", "happy"}}
				if v.sounds {
					directive = `<voice sound="` + style + `"/>`
					emotion = map[string]any{"listen": "off", "sounds": true}
				}
				opts := agentVerbOpts{
					SystemPrompt: "Your whole first reply is exactly this, with nothing before or after it: " +
						directive + emotionSentence,
					TTS:     v.synth(),
					Emotion: emotion,
				}
				rec, cbs := scriptedGreeting(t, opts)
				s := Step(t, "assert-take-"+style)
				defer s.Done()
				ctx := WithTimeout(t, 60*time.Second)
				transcript := transcribeReply(s, ctx, rec)
				assertWordsHeard(s, transcript, 3, emotionSentenceWords...)
				if resp, ok := firstCleanResponse(s, cbs); ok {
					if len(emotionStyles(resp)) == 0 {
						s.Errorf("llm_response.styles empty")
					}
					assertNoTagWords(s, transcript, resp.String("response"), v.tagWords(style))
				}
				assertNoTTSError(s, cbs)
				return rec
			}

			if v.sounds {
				take("laugh")
				return
			}
			sadRec := take("sad")
			happyRec := take("happy")
			if !cfg.HasSmallestai() {
				t.Log("SMALLESTAI_API_KEY not set — no audible contrast check")
				return
			}
			s := Step(t, "assert-contrast")
			defer s.Done()
			ctx := WithTimeout(t, 90*time.Second)
			sad, err := stt.SmallestaiEmotion(ctx, sadRec, cfg.SmallestaiAPIKey)
			if err != nil {
				s.Logf("WARN SmallestaiEmotion sad: %v", err)
				return
			}
			happy, err := stt.SmallestaiEmotion(ctx, happyRec, cfg.SmallestaiAPIKey)
			if err != nil {
				s.Logf("WARN SmallestaiEmotion happy: %v", err)
				return
			}
			s.Logf("sad take %v | happy take %v", sad, happy)
			// a classifier's view of how expressive a voice is; the hard check is TestVerb_Agent_Emotion_AudibleContrast
			if sad["sad"] <= happy["sad"] {
				s.Logf("WARN sad take not sadder: %.3f vs %.3f", sad["sad"], happy["sad"])
			}
			if happy["happy"] <= sad["happy"] {
				s.Logf("WARN happy take not happier: %.3f vs %.3f", happy["happy"], sad["happy"])
			}
		})
	}
}

// --- b. STT matrix -----------------------------------------------------------

// ensureAngryWAV synthesizes angryLine shouted on ElevenLabs eleven_v3 as an
// 8 kHz telephony WAV, cached like tts.EnsureWAV.
func ensureAngryWAV(ctx context.Context) (string, error) {
	const model = "eleven_v3"
	text := "[angry] [shouting] " + angryLine
	sum := sha1.Sum([]byte("elevenlabs|" + model + "|" + angryVoice + "|" + text))
	out := filepath.Join("testdata/agent", hex.EncodeToString(sum[:8])+".wav")
	if fi, err := os.Stat(out); err == nil && fi.Size() > 44 {
		return out, nil
	}
	body, _ := json.Marshal(map[string]string{"text": text, "model_id": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		"https://api.elevenlabs.io/v1/text-to-speech/"+angryVoice+"?output_format=pcm_8000", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("xi-api-key", cfg.ElevenlabsAPIKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	pcm, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("elevenlabs %d: %s", resp.StatusCode, truncate(string(pcm), 300))
	}
	if len(pcm) < 1600 {
		return "", fmt.Errorf("elevenlabs returned only %d bytes", len(pcm))
	}
	return out, wav.Write(out, pcm)
}

func requireAngryWAV(t *testing.T) string {
	t.Helper()
	requireElevenlabs(t)
	path, err := ensureAngryWAV(WithTimeout(t, 90*time.Second))
	if err != nil {
		t.Fatalf("angry fixture: %v", err)
	}
	return path
}

// TestVerb_Agent_Emotion_Matrix_STT — an angry caller on each emotion-capable
// STT: user_transcript carries an acoustic, not-happy tone, and the agent
// replies (the end-of-turn path works for STTs without a turn signal).
//
// Steps (per vendor):
//  1. speak-and-record-reply
//  2. assert-caller-tone
func TestVerb_Agent_Emotion_Matrix_STT(t *testing.T) {
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()
	angry := requireAngryWAV(t)

	vendors := []struct {
		name  string
		ready func() bool
		stt   func() map[string]any
	}{
		{"modulate", func() bool { return cfg.HasModulate() && modulateLabel != "" }, func() map[string]any {
			return map[string]any{"vendor": "modulate", "label": modulateLabel, "language": "en-US"}
		}},
		{"smallestai", func() bool { return cfg.HasSmallestai() && smallestaiLabel != "" }, func() map[string]any {
			r := smallestaiRecognizer("en-US", nil)
			r["model"] = smallestaiSTTModelV2
			return r
		}},
		{"inworld", func() bool { return cfg.HasInworld() && inworldLabel != "" }, func() map[string]any {
			return inworldRecognizer("en-US", nil)
		}},
	}
	for _, v := range vendors {
		t.Run(v.name, func(t *testing.T) {
			if !v.ready() {
				t.Logf("%s key not set — passing without exercising %s", v.name, v.name)
				return
			}
			_, cbs := emotionCallerTurn(t, agentVerbOpts{
				SystemPrompt: "You are a customer support agent. Reply in one short sentence.",
				BargeIn:      true,
				STT:          v.stt(),
				Emotion:      map[string]any{"listen": "auto"},
			}, angry)

			s := Step(t, "assert-caller-tone")
			defer s.Done()
			var tone map[string]any
			for _, ut := range findAgentEvents(cbs, "user_transcript") {
				s.Logf("user_transcript: %q emotion=%v", ut.String("transcript"), ut.JSON["emotion"])
				if e, ok := ut.JSON["emotion"].(map[string]any); ok && tone == nil {
					tone = e
				}
			}
			switch {
			case tone == nil:
				s.Errorf("no user_transcript.emotion: %s", summarizeEventTypes(cbs))
			case tone["source"] != "acoustic":
				s.Errorf("emotion.source = %v, want acoustic", tone["source"])
			case !slices.Contains([]string{"angry", "other", "fearful", "sad"}, fmt.Sprint(tone["label"])):
				s.Errorf("angry caller labelled %v (detail %v, confidence %v)", tone["label"], tone["detail"], tone["confidence"])
			}
			if !hasEvent(cbs, "llm_response") {
				s.Errorf("agent did not reply: %s", summarizeEventTypes(cbs))
			}
		})
	}
}

// --- c. LLM decides ----------------------------------------------------------

// TestVerb_Agent_Emotion_LLMDecides — an ordinary prompt with emotion on: the
// LLM answers an angry caller with an empathetic or calm style by itself.
//
// Steps:
//  1. speak-and-record-reply
//  2. assert-first-style
func TestVerb_Agent_Emotion_LLMDecides(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()
	angry := requireAngryWAV(t)

	rec, cbs := emotionCallerTurn(t, agentVerbOpts{
		SystemPrompt: "You are a customer support agent for an internet provider. Keep replies to one or two sentences.",
		TTS:          sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
		Emotion:      map[string]any{},
	}, angry)

	s = Step(t, "assert-first-style")
	defer s.Done()
	resp, ok := firstCleanResponse(s, cbs)
	if !ok {
		return
	}
	styles := emotionStyles(resp)
	if len(styles) == 0 {
		s.Fatalf("llm_response.styles empty")
	}
	if e := styles[0]["emotion"]; e != "empathetic" && e != "calm" {
		s.Errorf("first style %v, want empathetic or calm", styles[0])
	}
	transcript := transcribeReply(s, WithTimeout(t, 60*time.Second), rec)
	assertNoTagWords(s, transcript, resp.String("response"), []string{"warm", "calm"})
	assertNoTTSError(s, cbs)
}

// --- 1. barge-in trim per vendor ----------------------------------------------

// TestVerb_Agent_Emotion_Matrix_BargeIn — a long greeting with a directive on
// every sentence, interrupted after ~8 s: the trimmed reply holds only words
// that were in the clean reply, in order, and no tag characters or words.
//
// Steps (per vendor):
//  1. record-greeting-and-barge-in
//  2. assert-trim
func TestVerb_Agent_Emotion_Matrix_BargeIn(t *testing.T) {
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	for _, name := range []string{"elevenlabs", "soniox", "paxa", "xai", "inworld", "cartesia"} {
		v := emotionTTSVendorByName(name)
		t.Run(name, func(t *testing.T) {
			if !v.ready() {
				t.Logf("%s key not set — passing without exercising %s", name, name)
				return
			}
			a, b := `<voice emotion="calm"/>`, `<voice emotion="happy"/>`
			emotion := map[string]any{"listen": "off", "emotions": []string{"calm", "happy"}}
			tagWords := append(v.tagWords("calm"), v.tagWords("happy")...)
			if v.sounds {
				a, b = `<voice sound="laugh"/>`, `<voice sound="sigh"/>`
				emotion = map[string]any{"listen": "off", "sounds": true}
				tagWords = []string{"laugh", "sigh"}
			}
			ctx := WithTimeout(t, 120*time.Second)
			uas := claimUAS(t, ctx)
			bargeWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
			if err != nil {
				t.Fatalf("EnsureWAV: %v", err)
			}
			_, sess := claimSession(t)
			noReprompt := 0
			ScriptAgent(sess, agentVerbOpts{
				SystemPrompt: "On your first turn tell a long, slow story of at least eight full sentences about a trip " +
					"to the mountains. Start every sentence with a voice directive, alternating " + a + " and " + b + ". " +
					"If the user says a list of code words, repeat them back verbatim.",
				Greeting:          true,
				BargeIn:           true,
				NoResponseTimeout: &noReprompt,
				TTS:               v.synth(),
				Emotion:           emotion,
			})
			call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))

			s := Step(t, "record-greeting-and-barge-in")
			if err := call.Answer(); err != nil {
				s.Fatalf("Answer: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "llm_response") })
			cancel()
			time.Sleep(8 * time.Second)
			if err := call.SendWAV(bargeWAV); err != nil {
				s.Fatalf("SendWAV: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			cbs = append(cbs, DrainCallbacks(sess, LLMReplyWindow)...)
			s.Logf("events: %s", summarizeEventTypes(cbs))
			s.Done()

			s = Step(t, "assert-trim")
			assertBargeInTrim(s, cbs, tagWords)
			assertNoTTSError(s, cbs)
			s.Done()
			HangupAndWaitEnded(t, ctx, call)
		})
	}
}

func assertBargeInTrim(s *StepCtx, cbs []webhook.Callback, tagWords []string) {
	if !hasEvent(cbs, "user_interruption") {
		s.Errorf("no user_interruption: %s", summarizeEventTypes(cbs))
	}
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) == 0 {
		s.Errorf("no llm_response for the greeting")
		return
	}
	clean := responses[0].String("response")
	trimmed := ""
	for _, te := range findAgentEvents(cbs, "turn_end") {
		if interrupted, _ := te.JSON["interrupted"].(bool); interrupted {
			trimmed = te.String("response")
			break
		}
	}
	s.Logf("clean reply: %q", clean)
	s.Logf("heard before barge-in: %q", trimmed)
	if !strings.HasSuffix(trimmed, " -") {
		s.Errorf("interrupted turn_end response not trimmed: %q", trimmed)
		return
	}
	if strings.ContainsAny(trimmed, "[<") {
		s.Errorf("trimmed reply holds tag characters: %q", trimmed)
	}
	got, full := wordsOf(strings.TrimSuffix(trimmed, " -")), wordsOf(clean)
	for _, w := range tagWords {
		if slices.Contains(got, w) && !slices.Contains(full, w) {
			s.Errorf("trimmed reply holds tag word %q", w)
		}
	}
	// the trim is a prefix of the clean reply; its last word may be cut short
	for i, w := range got {
		last := i == len(got)-1
		if i >= len(full) || (w != full[i] && !(last && strings.HasPrefix(full[i], w))) {
			s.Errorf("trimmed reply is not a prefix of the clean reply at word %d (%q)", i, w)
			return
		}
	}
	if len(got) == len(full) {
		s.Errorf("trimmed reply is the whole reply: nothing was cut")
	}
}

// --- 2. mid-reply style change ------------------------------------------------

// TestVerb_Agent_Emotion_MidReplyStyle — one reply, two directives: both
// sentences are heard in order, no tts_error, one turn_end. On cartesia the
// second style opens a second context.
//
// Steps (per vendor):
//  1. record-greeting
//  2. assert-both-sentences
func TestVerb_Agent_Emotion_MidReplyStyle(t *testing.T) {
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	for _, name := range []string{"soniox", "cartesia"} {
		v := emotionTTSVendorByName(name)
		t.Run(name, func(t *testing.T) {
			if !v.ready() {
				t.Logf("%s key not set — passing without exercising %s", name, name)
				return
			}
			rec, cbs := scriptedGreeting(t, agentVerbOpts{
				SystemPrompt: "Your whole first reply is exactly this, with nothing before or after it: " +
					`<voice emotion="empathetic"/>I am so sorry about the delay with your order. ` +
					`<voice emotion="neutral"/>Your package will arrive on Friday morning.`,
				TTS:     v.synth(),
				Emotion: map[string]any{"listen": "off"},
			})
			s := Step(t, "assert-both-sentences")
			defer s.Done()
			transcript := transcribeReply(s, WithTimeout(t, 60*time.Second), rec)
			words := wordsOf(transcript)
			sorry, friday := slices.Index(words, "sorry"), slices.Index(words, "friday")
			if sorry < 0 || friday < 0 || sorry > friday {
				s.Errorf("sentences not heard in order: %q", transcript)
			}
			if resp, ok := firstCleanResponse(s, cbs); ok {
				if n := len(emotionStyles(resp)); n != 2 {
					s.Errorf("llm_response.styles has %d entries, want 2", n)
				}
				assertNoTagWords(s, transcript, resp.String("response"), []string{"warm", "sympathetic", "neutral", "value"})
			}
			if n := len(findAgentEvents(cbs, "turn_end")); n != 1 {
				s.Errorf("%d turn_end events for one reply", n)
			}
			assertNoTTSError(s, cbs)
		})
	}
}

// --- 3. a sound keeps the emotion ----------------------------------------------

// TestVerb_Agent_Emotion_SoundKeepsEmotion — a sound in the middle of a happy
// reply on eleven_v3: neither tag is spoken and the hook lists both styles.
//
// Steps:
//  1. record-greeting
//  2. assert-sound-and-emotion
func TestVerb_Agent_Emotion_SoundKeepsEmotion(t *testing.T) {
	requireElevenlabs(t)
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	rec, cbs := scriptedGreeting(t, agentVerbOpts{
		SystemPrompt: "Your whole first reply is exactly this, with nothing before or after it: " +
			`<voice emotion="happy"/>Great news, your refund has been approved. <voice sound="laugh"/>That works out nicely.`,
		TTS:     elevenlabsSynth("eleven_v3"),
		Emotion: map[string]any{"listen": "off", "sounds": true},
	})
	s = Step(t, "assert-sound-and-emotion")
	defer s.Done()
	transcript := transcribeReply(s, WithTimeout(t, 60*time.Second), rec)
	assertWordsHeard(s, transcript, 3, "refund", "approved", "works", "nicely")
	resp, ok := firstCleanResponse(s, cbs)
	if !ok {
		return
	}
	assertNoTagWords(s, transcript, resp.String("response"), []string{"happy", "laughs", "laugh"})
	var emotion, sound bool
	for _, st := range emotionStyles(resp) {
		emotion = emotion || st["emotion"] == "happy"
		sound = sound || st["sound"] == "laugh"
	}
	if !emotion || !sound {
		s.Errorf("styles %v, want happy and laugh", resp.JSON["styles"])
	}
	assertNoTTSError(s, cbs)
}

// --- 4. update_emotion mid-call ------------------------------------------------

// TestVerb_Agent_Emotion_UpdateEmotion — agent:update update_emotion
// {speak:false} over the app websocket is applied; a directive the LLM still
// writes afterwards is neither spoken nor rendered as a style.
//
// Steps:
//  1. place-ws-call
//  2. wait-for-greeting
//  3. send-update-emotion
//  4. send-generate-reply
//  5. assert-unstyled-reply
func TestVerb_Agent_Emotion_UpdateEmotion(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 150*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)
	noReprompt := 0
	ScriptAgent(sess, agentVerbOpts{
		SystemPrompt: "Start every reply with exactly this directive: <voice emotion=\"sad\"/> . " +
			"Your first reply is: Hello, how can I help you today?",
		Greeting:          true,
		NoResponseTimeout: &noReprompt,
		TTS:               sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
		Emotion:           map[string]any{"listen": "off", "speak": true},
	})

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(120))
	if err := call.Answer(); err != nil {
		s.Fatalf("Answer: %v", err)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-for-greeting")
	waitCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
	cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "turn_end") })
	cancel()
	time.Sleep(2 * time.Second)
	s.Done()

	s = Step(t, "send-update-emotion")
	if err := sess.SendCommand("agent:update", map[string]any{
		"type": "update_emotion", "emotion": map[string]any{"speak": false},
	}); err != nil {
		s.Fatalf("SendCommand(update_emotion): %v", err)
	}
	s.Done()

	s = Step(t, "send-generate-reply")
	rec := filepath.Join(t.TempDir(), "after-update.pcm")
	if err := call.StartRecording(rec); err != nil {
		s.Fatalf("StartRecording: %v", err)
	}
	time.Sleep(time.Second)
	if err := sess.SendCommand("agent:update", map[string]any{
		"type":         "generate_reply",
		"instructions": "Write the directive <voice emotion=\"sad\"/> and then say: " + emotionSentence,
	}); err != nil {
		s.Fatalf("SendCommand(generate_reply): %v", err)
	}
	waitCtx, cancel = context.WithTimeout(ctx, 40*time.Second)
	cbs = append(cbs, WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool {
		return len(findAgentEvents(c, "turn_end")) > 0 && hasEvent(c, "agent_update")
	})...)
	cancel()
	time.Sleep(time.Second)
	call.StopRecording()
	cbs = append(cbs, DrainCallbacks(sess, 2*time.Second)...)
	s.Logf("events: %s", summarizeEventTypes(cbs))
	s.Done()

	s = Step(t, "assert-unstyled-reply")
	updates := findAgentEvents(cbs, "agent_update")
	if len(updates) == 0 {
		s.Errorf("no agent_update event")
	} else if applied, _ := updates[0].JSON["applied"].(bool); !applied {
		s.Errorf("update_emotion not applied: %v", updates[0].JSON)
	} else {
		s.Logf("agent_update: %v", updates[0].JSON)
	}
	responses := findAgentEvents(cbs, "llm_response")
	if len(responses) < 2 {
		s.Fatalf("no reply after the update: %s", summarizeEventTypes(cbs))
	}
	reply := responses[len(responses)-1]
	s.Logf("reply after update: %q styles=%v", reply.String("response"), reply.JSON["styles"])
	turnEnds := findAgentEvents(cbs, "turn_end")
	if last := turnEnds[len(turnEnds)-1]; last.JSON["agent_styles"] != nil {
		s.Errorf("styles still rendered after speak:false: %v", last.JSON["agent_styles"])
	}
	transcript := transcribeReply(s, WithTimeout(t, 60*time.Second), rec)
	assertWordsHeard(s, transcript, 3, emotionSentenceWords...)
	assertNoTagWords(s, transcript, reply.String("response"), []string{"sad"})
	assertNoTTSError(s, cbs)
	s.Done()
	HangupAndWaitEnded(t, ctx, call)
}

// --- 5. real-world caller lines ------------------------------------------------

// TestVerb_Agent_Emotion_RealWorld — an ordinary receptionist prompt with
// emotion and sounds on: the LLM's first style fits what the caller said, and
// no tag is heard.
//
// Steps (per line):
//  1. speak-and-record-reply
//  2. assert-style
func TestVerb_Agent_Emotion_RealWorld(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	cases := []struct {
		name, line string
		check      func(styles []map[string]any) string // "" = ok
	}{
		{"slower", "Could you please speak a bit slower?", func(st []map[string]any) string {
			if len(st) == 0 || st[0]["speed"] != "slow" {
				return "want first style speed slow"
			}
			return ""
		}},
		{"louder", "I can barely hear you, can you speak louder?", func(st []map[string]any) string {
			if len(st) == 0 || st[0]["volume"] != "loud" {
				return "want first style volume loud"
			}
			return ""
		}},
		{"bereavement", "My mother passed away last week, I need to cancel her appointment.", func(st []map[string]any) string {
			if len(st) == 0 || (st[0]["emotion"] != "empathetic" && st[0]["emotion"] != "calm") {
				return "want first emotion empathetic or calm"
			}
			for _, x := range st {
				if x["emotion"] == "happy" || x["sound"] == "laugh" {
					return "happy or laugh on a bereavement"
				}
			}
			return ""
		}},
		{"joke", "Haha, my dog ate the appointment card.", func(st []map[string]any) string {
			if len(st) == 0 || st[0]["emotion"] != "happy" {
				return "want first emotion happy"
			}
			return ""
		}},
		{"neutral", "What time do you open on Saturday?", func(st []map[string]any) string {
			for _, x := range st {
				if e, ok := x["emotion"]; ok && e != "neutral" {
					return "want no style or neutral"
				}
				if x["sound"] != nil || x["speed"] != nil || x["volume"] != nil {
					return "want no style or neutral"
				}
			}
			return ""
		}},
	}
	sonioxTags := []string{"warm", "slowly", "quickly", "whispering", "loudly", "laughs", "sighs", "exhales"}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := WithTimeout(t, 60*time.Second)
			wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", c.line, tts.PromptOptions{Model: "aura-asteria-en"})
			if err != nil {
				t.Fatalf("EnsureWAV: %v", err)
			}
			rec, cbs := emotionCallerTurn(t, agentVerbOpts{
				SystemPrompt: "You are a friendly receptionist for a dental clinic, open Monday to Saturday from nine to five. " +
					"Keep replies to one or two short sentences.",
				TTS:     sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
				Emotion: map[string]any{"sounds": true},
			}, wavPath)
			s := Step(t, "assert-style")
			defer s.Done()
			resp, ok := firstCleanResponse(s, cbs)
			if !ok {
				return
			}
			if msg := c.check(emotionStyles(resp)); msg != "" {
				s.Errorf("%s: styles=%v reply=%q", msg, resp.JSON["styles"], resp.String("response"))
			}
			transcript := transcribeReply(s, ctx, rec)
			assertNoTagWords(s, transcript, resp.String("response"), sonioxTags)
			assertNoTTSError(s, cbs)
		})
	}
}

// --- 6. emotion off regression --------------------------------------------------

// TestVerb_Agent_Emotion_Off — without `emotion` nothing changes: no styles
// in llm_response, no agent_styles/user_emotion in turn_end, greeting and an
// echo turn both heard.
//
// Steps (per vendor):
//  1. record-greeting
//  2. speak-and-record-echo
//  3. assert-no-emotion-fields
func TestVerb_Agent_Emotion_Off(t *testing.T) {
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	for _, name := range []string{"soniox", "elevenlabs"} {
		v := emotionTTSVendorByName(name)
		t.Run(name, func(t *testing.T) {
			if !v.ready() {
				t.Logf("%s key not set — passing without exercising %s", name, name)
				return
			}
			ctx := WithTimeout(t, 150*time.Second)
			uas := claimUAS(t, ctx)
			echoWAV, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
			if err != nil {
				t.Fatalf("EnsureWAV: %v", err)
			}
			_, sess := claimSession(t)
			noReprompt := 0
			ScriptAgent(sess, agentVerbOpts{
				SystemPrompt: "Your first reply is exactly: " + emotionSentence +
					" After that, echo back whatever you hear from the user, word for word.",
				Greeting:          true,
				NoResponseTimeout: &noReprompt,
				TTS:               v.synth(),
			})
			call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(120))

			s := Step(t, "record-greeting")
			greetRec := filepath.Join(t.TempDir(), "greeting.pcm")
			if err := call.Answer(); err != nil {
				s.Fatalf("Answer: %v", err)
			}
			if err := call.StartRecording(greetRec); err != nil {
				s.Fatalf("StartRecording: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			waitCtx, cancel := context.WithTimeout(ctx, 40*time.Second)
			cbs := WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "turn_end") })
			cancel()
			time.Sleep(time.Second)
			call.StopRecording()
			s.Done()

			s = Step(t, "speak-and-record-echo")
			echoRec := filepath.Join(t.TempDir(), "echo.pcm")
			if err := call.StartRecording(echoRec); err != nil {
				s.Fatalf("StartRecording: %v", err)
			}
			if err := call.SendWAV(echoWAV); err != nil {
				s.Fatalf("SendWAV: %v", err)
			}
			if err := call.SendSilence(); err != nil {
				s.Fatalf("SendSilence: %v", err)
			}
			waitCtx, cancel = context.WithTimeout(ctx, 40*time.Second)
			cbs = append(cbs, WaitCallbacksUntil(waitCtx, sess, func(c []webhook.Callback) bool { return hasEvent(c, "turn_end") })...)
			cancel()
			time.Sleep(time.Second)
			call.StopRecording()
			cbs = append(cbs, DrainCallbacks(sess, 2*time.Second)...)
			s.Logf("events: %s", summarizeEventTypes(cbs))
			s.Done()

			s = Step(t, "assert-no-emotion-fields")
			sctx := WithTimeout(t, 60*time.Second)
			assertWordsHeard(s, transcribeReply(s, sctx, greetRec), 3, emotionSentenceWords...)
			assertWordsHeard(s, transcribeReply(s, sctx, echoRec), 2, "alpha", "bravo", "charlie", "delta")
			if n := len(findAgentEvents(cbs, "llm_response")); n < 2 {
				s.Errorf("%d llm_response events, want 2", n)
			}
			for _, r := range findAgentEvents(cbs, "llm_response") {
				if _, ok := r.JSON["styles"]; ok {
					s.Errorf("llm_response has styles without emotion: %v", r.JSON)
				}
			}
			for _, te := range findAgentEvents(cbs, "turn_end") {
				for _, k := range []string{"agent_styles", "user_emotion"} {
					if _, ok := te.JSON[k]; ok {
						s.Errorf("turn_end has %s without emotion: %v", k, te.JSON)
					}
				}
			}
			for _, ut := range findAgentEvents(cbs, "user_transcript") {
				if _, ok := ut.JSON["emotion"]; ok {
					s.Errorf("user_transcript has emotion without emotion: %v", ut.JSON)
				}
			}
			assertNoTTSError(s, cbs)
			s.Done()
			HangupAndWaitEnded(t, ctx, call)
		})
	}
}

// --- 7. preflight -----------------------------------------------------------------

// TestVerb_Agent_Emotion_Preflight — earlyGeneration on Flux with emotion on:
// the turn completes with styles and exactly one reply. Whether preflight
// actually fired is read from the agent's summary and logged.
//
// Steps:
//  1. speak-and-record-reply
//  2. assert-one-styled-reply
func TestVerb_Agent_Emotion_Preflight(t *testing.T) {
	if !requireSoniox(t) {
		return
	}
	requireWebhook(t)
	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()
	ctx := WithTimeout(t, 60*time.Second)
	wavPath, err := tts.EnsureWAV(ctx, "testdata/agent", agentEchoPrompt, tts.PromptOptions{Model: "aura-asteria-en"})
	if err != nil {
		t.Fatalf("EnsureWAV: %v", err)
	}
	rec, cbs := emotionCallerTurn(t, agentVerbOpts{
		SystemPrompt: emotionEchoSystemPrompt,
		STT: map[string]any{
			"vendor": "deepgramflux", "label": deepgramFluxLabel, "language": "en-US",
			"deepgramOptions": map[string]any{"eagerEotThreshold": 0.5},
		},
		EarlyGeneration: true,
		TTS:             sonioxSynth(sonioxVoice, "en-US", map[string]any{"model_id": sonioxTTSModel}),
		Emotion:         map[string]any{"listen": "llm"},
	}, wavPath)

	s = Step(t, "assert-one-styled-reply")
	defer s.Done()
	for _, cb := range cbs {
		if p, ok := cb.JSON["preflight"]; ok {
			s.Logf("agent summary preflight: %v", p)
		}
	}
	if n := len(findAgentEvents(cbs, "llm_response")); n != 1 {
		s.Errorf("%d llm_response events for one turn", n)
	}
	if n := len(findAgentEvents(cbs, "turn_end")); n != 1 {
		s.Errorf("%d turn_end events for one turn", n)
	}
	resp, ok := firstCleanResponse(s, cbs)
	if !ok {
		return
	}
	if len(emotionStyles(resp)) == 0 {
		s.Errorf("llm_response.styles empty")
	}
	for _, te := range findAgentEvents(cbs, "turn_end") {
		s.Logf("turn_end: preflight_result=%v preflight_ms=%v agent_styles=%v", te.JSON["preflight_result"], te.JSON["preflight_ms"], te.JSON["agent_styles"])
	}
	transcript := transcribeReply(s, ctx, rec)
	assertWordsHeard(s, transcript, 2, "alpha", "bravo", "charlie", "delta")
	assertNoTagWords(s, transcript, resp.String("response"), emotionTagWords)
	assertNoTTSError(s, cbs)
}
