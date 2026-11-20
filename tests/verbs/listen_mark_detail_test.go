//go:build detail

package verbs

import (
	"context"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestVerb_Listen_Mark_Cleared — stream audio + a mark, then immediately
// kill the buffered audio with `killAudio`. jambonz must report the
// pending mark as "cleared" rather than "playout".
//
// Steps:
//  1. require-creds — skip without ngrok + Deepgram
//  2. synthesize-playback-audio — Deepgram TTS -> raw 8k linear16 PCM
//  3. register-webhook-session
//  4. script-listen-bidi
//  5. place-call
//  6. answer-and-record
//  7. wait-ws-connect
//  8. stream-audio-mark-then-kill — binary PCM, {mark name}, more PCM, {killAudio}
//  9. await-cleared-mark — assert {type:mark,event:cleared,name}
//
// 10. hangup
func TestVerb_Listen_Mark_Cleared(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	if !tts.HasKey() {
		t.Skip("skipping: DEEPGRAM_API_KEY required to synthesize playback audio")
	}
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	testID, sess := claimSession(t)

	s := Step(t, "synthesize-playback-audio")
	pcm := marksRawPCM(ctx, t, s, markClearedText)
	s.Done()

	s = Step(t, "script-listen-bidi")
	wsURL := wssURL(webhookSrv.PublicURL(), "/ws/"+testID)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("listen",
			"url", wsURL,
			"mixType", "mono",
			"sampleRate", 8000,
			"bidirectionalAudio", map[string]any{
				"enabled":    true,
				"streaming":  true,
				"sampleRate": 8000,
			}),
		V("pause", "length", 12),
		V("hangup"),
	}))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	s.Done()

	_ = marksAnswerRecord(t, call)

	s = Step(t, "wait-ws-connect")
	connCtx, ccancel := context.WithTimeout(ctx, 15*time.Second)
	if err := sess.WSConnected(connCtx); err != nil {
		ccancel()
		s.Fatalf("WS never connected: %v", err)
	}
	ccancel()
	time.Sleep(BridgeSettleDelay)
	s.Done()

	const markName = "mark-cleared-1"

	s = Step(t, "stream-audio-mark-then-kill")
	// Burst a large block of audio (no pacing) so several seconds of
	// samples are buffered ~instantly, then mark, then one more frame to
	// move the mark into the playout buffer (in-use), then killAudio
	// immediately. The kill arrives while the marked position is still
	// deep in the undrained buffer, so the mark must come back "cleared"
	// rather than "playout".
	if err := marksStreamPCM(sess, pcm, false); err != nil {
		s.Fatalf("stream PCM: %v", err)
	}
	if err := sess.SendWSText(marksMarkMsg(markName)); err != nil {
		s.Fatalf("send mark: %v", err)
	}
	if err := marksStreamPCM(sess, pcm[:min(len(pcm), 3200)], false); err != nil {
		s.Fatalf("stream trailing PCM: %v", err)
	}
	if err := sess.SendWSText(marksKillAudioMsg()); err != nil {
		s.Fatalf("send killAudio: %v", err)
	}
	s.Done()

	s = Step(t, "await-cleared-mark")
	markCtx, mcancel := context.WithTimeout(ctx, 20*time.Second)
	mark, err := sess.WaitWSMark(markCtx, markName)
	mcancel()
	if err != nil {
		s.Fatalf("never received mark %q: %v", markName, err)
	}
	if mark.Event != "cleared" {
		s.Errorf("mark %q event = %q, want %q (killAudio should clear, not play out)",
			markName, mark.Event, "cleared")
	}
	s.Logf("received mark: name=%q event=%q", mark.Name, mark.Event)
	s.Done()

	s = Step(t, "hangup")
	call.StopRecording()
	_ = call.Hangup()
	s.Done()
}
