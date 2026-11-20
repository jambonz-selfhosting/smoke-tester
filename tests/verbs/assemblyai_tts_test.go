// AssemblyAI TTS (assemblyai-tts-1). Streaming goes over mediajam's /v1/ws
// dialect, one socket per call; a one-shot say goes over POST /v1/speech. All
// tests skip without ASSEMBLYAI_API_KEY.
package verbs

import (
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// library voices are multilingual, so the German test keeps the same voice
const assemblyaiGermanVoice = "aai-jane"

func requireAssemblyai(t *testing.T) {
	t.Helper()
	if !cfg.HasAssemblyai() || assemblyaiLabel == "" {
		t.Skip("AssemblyAI tests need ASSEMBLYAI_API_KEY (credential missing — skipping, not a failure)")
	}
}

func assemblyaiSynth(voice, language string) map[string]any {
	return map[string]any{
		"vendor":   "assemblyai",
		"label":    assemblyaiLabel,
		"voice":    voice,
		"language": language,
	}
}

// TestVerb_Say_Assemblyai — one-shot (HTTP) say.
//
// Steps:
//  1. place-call
//  2. answer-record-and-wait-end
//  3. assert-audio-duration
//  4. assert-transcript
func TestVerb_Say_Assemblyai(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	runSay(t, sayOpts{
		ctxTimeout: 30 * time.Second,
		tag:        "say-assemblyai",
		minDur:     1 * time.Second,
		maxDur:     9 * time.Second,
		verb: V("say", "text", "This voice test is working correctly.",
			"synthesizer", assemblyaiSynth(assemblyaiVoice, "english")),
		wantWords: []string{"voice test", "working correctly"},
	})
}

// TestVerb_Say_Stream_Assemblyai — streaming say over /v1/ws.
//
// Steps:
//  1. script-streaming-say
//  2. place-ws-call
//  3. answer-record-and-wait-end
//  4. assert-audio-duration
//  5. assert-transcript
func TestVerb_Say_Stream_Assemblyai(t *testing.T) {
	requireAssemblyai(t)
	t.Parallel()
	runStreamingSay(t, "say-stream-assemblyai", assemblyaiSynth(assemblyaiVoice, "english"))
}

// runAssemblyaiScript runs the says then hangs up; wantOrdered, when set,
// must appear in the transcript in that order.
func runAssemblyaiScript(t *testing.T, tag string, minDur, maxDur time.Duration, wantOrdered []string, says ...map[string]any) {
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
