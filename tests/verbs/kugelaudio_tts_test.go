// KugelAudio TTS. Streaming goes over mediajam's /ws/tts/stream dialect, one
// socket per call; a one-shot say goes over POST /v1/tts/generate. All tests
// skip without KUGELAUDIO_API_KEY.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// German voice for the language test (Antonia Meier, de-DE).
const kugelaudioGermanVoice = "1930"

func requireKugelaudio(t *testing.T) {
	t.Helper()
	if !cfg.HasKugelaudio() || kugelaudioLabel == "" {
		t.Skip("KugelAudio tests need KUGELAUDIO_API_KEY (credential missing — skipping, not a failure)")
	}
}

func kugelaudioSynth(voice, language string) map[string]any {
	return map[string]any{
		"vendor":   "kugelaudio",
		"label":    kugelaudioLabel,
		"voice":    voice,
		"language": language,
	}
}

// TestVerb_Say_Kugelaudio — one-shot (HTTP) say.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Kugelaudio(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-kugelaudio",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", kugelaudioSynth(kugelaudioVoice, "en-US")),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Kugelaudio — streaming say over /ws/tts/stream.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Kugelaudio(t *testing.T) {
	requireKugelaudio(t)
	t.Parallel()
	runStreamingSay(t, "say-stream-kugelaudio", kugelaudioSynth(kugelaudioVoice, "en-US"))
}

// runKugelaudioScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in that order.
func runKugelaudioScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
	t.Helper()
	ctx := WithTimeout(t, 40*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-say")
	script := webhook.Script{}
	for _, v := range says {
		script = append(script, v)
	}
	sess.ScriptCallHook(WithWarmupScript(append(script, V("hangup"))))
	s.Done()

	s = Step(t, "place-ws-call")
	call := placeWSCallTo(ctx, t, uas, sess, withTimeLimit(40))
	s.Done()

	s = Step(t, "answer-record-and-wait-end")
	wav := AnswerRecordAndWaitEnded(s, ctx, call, WithRecord(tag), WithSilence())
	s.Done()

	s = Step(t, "assert-audio-duration")
	AssertAudioDuration(s, call, minDur, maxDur, tag)
	s.Done()

	if wav != "" && len(wantOrdered) > 0 {
		s = Step(t, "assert-transcript-ordered")
		AssertTranscriptContainsInOrder(s, ctx, wav, wantOrdered...)
		s.Done()
	}
}
