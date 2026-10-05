// Regression test for `dial` with several targets answering at once.
//
// When two dialed legs return 200 OK within milliseconds of each other,
// feature-server used to strand one of them: either the loser was "killed"
// while still connecting its media (kill() tried to CANCEL an already
// answered INVITE, so no BYE ever went out), or both legs reached `accept`
// and the second overwrote the first as the dial's selected leg, so the
// bridged winner was never hung up. Either way the stranded dialog stays up
// forever and holds an SBC call-count slot (and, for the winner, media).
//
// Each round holds both callees at a barrier after 180 and releases them
// together, so their 200 OKs leave within the same millisecond.
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const simultaneousAnswerRounds = 5

// TestVerb_Dial_SimultaneousAnswer — two dial targets answer in the same
// instant; exactly one must survive, and every leg must be torn down once
// the caller hangs up.
//
// Steps (per round):
//  1. script-dial-two-targets — [dial target=[callee-a, callee-b], hangup]
//  2. place-caller — POST /Calls to the caller UAS, answer it
//  3. both-callees-ringing — both INVITEs arrived and sent 180
//  4. simultaneous-answer — release the barrier; both send 200 OK at once
//  5. loser-gets-bye — within 5s exactly one callee leg has been hung up
//  6. caller-hangup — caller sends BYE
//  7. all-legs-released — within 5s every callee leg has been hung up
func TestVerb_Dial_SimultaneousAnswer(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	ctx := WithTimeout(t, 240*time.Second)
	callerUAS, calleeA := claimUAS2(t, ctx)
	calleeB := claimUAS(t, ctx)

	for i := 1; i <= simultaneousAnswerRounds; i++ {
		ok := t.Run(fmt.Sprintf("round-%d", i), func(t *testing.T) {
			runSimultaneousAnswerRound(t, ctx, callerUAS, []*UAS{calleeA, calleeB})
		})
		if !ok {
			return
		}
	}
}

func runSimultaneousAnswerRound(t *testing.T, ctx context.Context, callerUAS *UAS, callees []*UAS) {
	_, sess := claimSession(t)

	s := Step(t, "script-dial-two-targets")
	targets := make([]any, 0, len(callees))
	for _, c := range callees {
		targets = append(targets, map[string]any{
			"type": "user",
			"name": fmt.Sprintf("%s@%s", c.Username, suite.SIPRealm),
		})
	}
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", targets,
			"timeout", 20,
			"actionHook", SessionURL(sess, "dial"),
			"anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	ringing := make(chan *jsip.Call, len(callees))
	release := make(chan struct{})
	rctx, rcancel := context.WithCancel(ctx)
	legs := make([]*jsip.Call, 0, len(callees))
	t.Cleanup(func() {
		rcancel()
		// Free any leg jambonz stranded so it doesn't hold a slot on the cluster.
		for _, c := range legs {
			_ = c.Hangup()
		}
	})
	for _, uas := range callees {
		go func(uas *UAS) {
			select {
			case c := <-uas.Inbound:
				if err := c.Trying(); err != nil {
					GoroutineFailf(t, "callee:trying", "%s Trying: %v", uas.Username, err)
					return
				}
				if err := c.Ringing(); err != nil {
					GoroutineFailf(t, "callee:ringing", "%s Ringing: %v", uas.Username, err)
					return
				}
				ringing <- c
				select {
				case <-release:
				case <-rctx.Done():
					return
				}
				if err := c.Answer(); err != nil {
					GoroutineFailf(t, "callee:answer", "%s Answer: %v", uas.Username, err)
					return
				}
				// The loser may already be hung up by now; that's the expected outcome.
				_ = c.SendSilence()
			case <-rctx.Done():
			}
		}(uas)
	}

	s = Step(t, "place-caller")
	caller := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	if err := caller.Answer(); err != nil {
		s.Fatalf("caller Answer: %v", err)
	}
	_ = caller.SendSilence()
	t.Cleanup(func() { _ = caller.Hangup() })
	s.Done()

	s = Step(t, "both-callees-ringing")
	ringCtx, ringCancel := context.WithTimeout(ctx, 20*time.Second)
	defer ringCancel()
	for len(legs) < len(callees) {
		select {
		case c := <-ringing:
			legs = append(legs, c)
		case <-ringCtx.Done():
			s.Fatalf("only %d of %d callees received the dial INVITE", len(legs), len(callees))
		}
	}
	s.Done()

	s = Step(t, "simultaneous-answer")
	close(release)
	for _, c := range legs {
		// The loser can be hung up before we look, so ended counts as answered here.
		if err := c.WaitState(ctx, jsip.StateAnswered); err != nil && c.State() != jsip.StateEnded {
			s.Fatalf("callee %s never reached answered: %v", c.CallID(), err)
		}
	}
	s.Done()

	s = Step(t, "loser-gets-bye")
	ended := waitLegsEnded(ctx, legs, 1, 5*time.Second)
	if len(ended) != 1 {
		s.Errorf("%d of %d callee legs hung up 5s after a simultaneous answer, want exactly 1 (still up: %s)",
			len(ended), len(legs), legsUp(legs))
	}
	s.Done()

	s = Step(t, "caller-hangup")
	if err := caller.Hangup(); err != nil {
		s.Fatalf("caller Hangup: %v", err)
	}
	s.Done()

	s = Step(t, "all-legs-released")
	if ended := waitLegsEnded(ctx, legs, len(legs), 5*time.Second); len(ended) != len(legs) {
		s.Errorf("callee legs never got a BYE after the caller hung up (leaked): %s", legsUp(legs))
	}
	s.Done()
}

// waitLegsEnded polls until at least want legs have ended or within elapses,
// and returns the ended ones.
func waitLegsEnded(ctx context.Context, legs []*jsip.Call, want int, within time.Duration) []*jsip.Call {
	deadline := time.Now().Add(within)
	for {
		var ended []*jsip.Call
		for _, c := range legs {
			if c.State() == jsip.StateEnded {
				ended = append(ended, c)
			}
		}
		if len(ended) >= want || time.Now().After(deadline) || ctx.Err() != nil {
			return ended
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func legsUp(legs []*jsip.Call) string {
	var up []string
	for _, c := range legs {
		if c.State() != jsip.StateEnded {
			up = append(up, c.CallID())
		}
	}
	return fmt.Sprint(up)
}
