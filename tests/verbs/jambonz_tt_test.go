package verbs

import (
	"testing"
	"time"
)

// TestVerb_JambonzTurnDetection_Replies — with `turnDetection: "jambonz"`
// the agent ends the caller's turn with the in-house end-of-turn model that
// runs inside mediajam. Same proof as the krisp test: the agent must speak a
// reply containing the prompt's content words, because a model that never
// fires leaves the call silent while RTP keeps flowing.
//
// Steps:
//  1. preflight-skips
//  2. ensure-prompt-wav
//  3. script-agent-verb (turnDetection=jambonz)
//  4. place-call
//  5. answer-record-and-silence / wait-for-stt / send-prompt-wav / wait-for-reply
//  6. assert-agent-replied
//  7. hangup-and-wait-ended
func TestVerb_JambonzTurnDetection_Replies(t *testing.T) {
	jambonzTurnReplies(t, "jambonz-turn-detection", map[string]any{"turnDetection": "jambonz"})
}

func jambonzTurnReplies(t *testing.T, tag string, extra map[string]any) {
	t.Parallel()
	requireWebhook(t)

	s := Step(t, "preflight-skips")
	if !agentSkipPreflight(t, s) {
		return
	}
	s.Done()

	ctx := WithTimeout(t, 120*time.Second)
	uas := claimUAS(t, ctx)

	s = Step(t, "ensure-prompt-wav")
	wavPath := krispEnsurePromptWAV(ctx, s)
	s.Done()

	_, sess := claimSession(t)

	s = Step(t, "script-agent-verb")
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{}, extra))
	s.Done()

	s = Step(t, "place-call")
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(90))
	s.Done()

	rec := RunAudioRoundtrip(t, ctx, call, AudioRoundtripOpts{
		PromptWAV: wavPath,
		RecordTag: tag,
	})
	call.StopRecording()

	s = Step(t, "assert-agent-replied")
	krispReportTurnPipeline(s, sess, 2*time.Second)
	AssertTranscriptHasMost(s, ctx, rec, krispMinKeywordHits, krispEchoKeywords...)
	s.Done()

	HangupAndWaitEnded(t, ctx, call)
}
