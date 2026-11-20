//go:build detail

package verbs

// Multi-turn variant of TestBench_TurnDetection: several eot-bench caller
// turns in one call, each played once the agent has answered the previous
// one, as a caller would. With TT_BENCH_BARGE=1 every second turn starts while
// the agent is still answering, so the call goes through real barge-ins, and
// a turn the agent never answers afterwards shows up as a stall.
//
//	TT_BENCH_DIR=<dir> TT_BENCH_OUT=multi.jsonl TT_BENCH_CONFIGS=krisp,jambonz \
//	TT_BENCH_MULTI=4 [TT_BENCH_BARGE=1] [TT_BENCH_KEYS=...] \
//	go test -tags detail -run '^TestBench_TurnDetectionMulti$' -parallel 2 ./tests/verbs/

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// a two-sentence answer leaves the agent talking long enough to be barged in on
const benchMultiPrompt = "You are a phone agent in an automated test. Start the call by saying exactly: " +
	"'Hello, how can I help you?' After that, whenever the caller finishes speaking, reply with exactly two short " +
	"sentences, for example 'Okay, I understand. Let me note that down.' Never ask a question."

const (
	benchBargeAfter = 700 * time.Millisecond // into the agent's answer before the caller talks over it
	benchQuietGap   = 8000                   // 1 s of agent silence ends its answer
	benchStopQuiet  = 6400                   // 800 ms: longer than the gap between two sentences of one answer
	benchLateReply  = 3 * time.Second        // extra wait after an unanswered turn, so a late reply can't land in the next
)

type benchMultiResult struct {
	benchResult
	Call    string   `json:"call"`
	Index   int      `json:"index"`
	Mode    string   `json:"mode"`              // "wait" or "barge"
	StopMs  *float64 `json:"stop_ms,omitempty"` // barge: caller start -> agent quiet
	Replied bool     `json:"replied"`           // the agent started an answer after the caller's turn end
}

// Steps:
//  1. load-bench
//  2. one subtest per call and config: greet, then per turn wait for the agent (or barge in), play the turn,
//     measure the agent's first audio and whether it answered
func TestBench_TurnDetectionMulti(t *testing.T) {
	k, _ := strconv.Atoi(os.Getenv("TT_BENCH_MULTI"))
	if k < 2 {
		t.Skip("TT_BENCH_MULTI not set")
	}
	turns, configs, write := benchLoad(t, []string{"krisp", "jambonz"}, "results-multi.jsonl")
	if turns == nil {
		return
	}
	dir := os.Getenv("TT_BENCH_DIR")
	barge := os.Getenv("TT_BENCH_BARGE") == "1"
	// one STT language per call: chunk each language's turns separately
	byLang := map[string][]benchTurn{}
	var langs []string
	for _, tn := range turns {
		if _, ok := byLang[tn.Lang]; !ok {
			langs = append(langs, tn.Lang)
		}
		byLang[tn.Lang] = append(byLang[tn.Lang], tn)
	}
	var calls [][]benchTurn
	for _, l := range langs {
		ts := byLang[l]
		for i := 0; i+k <= len(ts); i += k {
			calls = append(calls, ts[i:i+k])
		}
	}
	t.Logf("%d calls of %d turns, barge=%v", len(calls), k, barge)
	for _, ts := range calls {
		for _, c := range configs {
			ts, c := ts, c
			name := ts[0].Key + "+" + strconv.Itoa(len(ts)-1)
			t.Run(c+"/"+name, func(t *testing.T) {
				t.Parallel()
				benchMultiCall(t, c, dir, name, ts, barge, func(r benchMultiResult) { writeBench(write, r) })
			})
		}
	}
}

// recSamples is how many 8 kHz samples the recording holds now.
func recSamples(rec string) (int, []byte, error) {
	pcm, err := os.ReadFile(rec)
	return len(pcm) / 2, pcm, err
}

func benchMultiCall(t *testing.T, config, dir, name string, turns []benchTurn, barge bool,
	write func(benchMultiResult)) {
	budget := 60
	for _, tn := range turns {
		budget += int(tn.Dur+benchListenAfterEot.Seconds()+benchLateReply.Seconds()) + 10
	}
	ctx := WithTimeout(t, time.Duration(budget)*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)
	off := 0
	overrides := benchTurnDetection(config)
	benchSTT(overrides, turns[0].Lang)
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{SystemPrompt: benchMultiPrompt, NoResponseTimeout: &off,
		BargeIn: true, Greeting: true}, overrides))
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(budget))
	defer HangupAndWaitEnded(t, ctx, call)
	fail := func(i int, err error) {
		for ; i < len(turns); i++ {
			write(benchMultiResult{benchResult: benchResult{Config: config, Key: turns[i].Key, ID: turns[i].ID,
				Cohort: turns[i].Cohort, Lang: turns[i].Lang, EotStart: turns[i].EotStart, Holds: turns[i].Holds,
				Error: err.Error()}, Call: name, Index: i})
		}
	}
	if err := call.Answer(); err != nil {
		fail(0, err)
		return
	}
	rec := filepath.Join(t.TempDir(), "agent.pcm")
	if err := call.StartRecording(rec); err != nil {
		fail(0, err)
		return
	}
	defer call.StopRecording()
	if err := call.SendSilence(); err != nil {
		fail(0, err)
		return
	}
	time.Sleep(RecognizerArmDelay)
	if _, err := benchWaitGreeting(ctx, rec); err != nil {
		n, _, _ := recSamples(rec)
		fail(0, fmt.Errorf("%w (recorded %.1f s)", err, float64(n)/8000))
		return
	}
	replyAt := -1 // recording sample where the agent's answer to the previous turn started
	for i, turn := range turns {
		mode := "wait"
		if barge && i%2 == 1 && replyAt >= 0 {
			mode = "barge"
			if err := benchWaitSamples(ctx, rec, replyAt+int(benchBargeAfter.Seconds()*8000)); err != nil {
				fail(i, err)
				return
			}
		} else if i > 0 {
			if replyAt < 0 {
				time.Sleep(benchLateReply)
			}
			if err := benchWaitQuiet(ctx, rec, 20*time.Second); err != nil {
				fail(i, err)
				return
			}
		}
		t0, _, err := recSamples(rec)
		if err != nil {
			fail(i, err)
			return
		}
		start := time.Now()
		if err := call.SendWAV(filepath.Join(dir, "wav", turn.Key+".wav")); err != nil {
			fail(i, err)
			return
		}
		if err := call.SendSilence(); err != nil {
			fail(i, err)
			return
		}
		r := benchMultiResult{benchResult: benchResult{Config: config, Key: turn.Key, ID: turn.ID, Cohort: turn.Cohort,
			Lang: turn.Lang, EotStart: turn.EotStart, Holds: turn.Holds}, Call: name, Index: i, Mode: mode}
		eot := t0 + int(turn.EotStart*8000)
		deadline := start.Add(time.Duration(turn.EotStart*float64(time.Second)) + benchListenAfterEot)
		replyAt = -1
		for time.Now().Before(deadline) {
			_, pcm, err := recSamples(rec)
			if err != nil {
				fail(i, err)
				return
			}
			// from: where this turn's answer can start. In a barge turn that is after the agent has gone
			// quiet, so the answer it was giving (or its next sentence) is never taken for a new one.
			from := t0
			if mode == "barge" {
				from = benchQuietFrom(pcm, t0, benchStopQuiet)
				if from >= 0 && r.StopMs == nil {
					ms := float64(from-t0) / 8
					r.StopMs = &ms
				}
			}
			if from >= 0 {
				if r.Onset == nil {
					if on := benchOnset(pcm, from); on >= 0 {
						v := float64(from-t0)/8000 + on
						r.Onset = &v
					}
				}
				if on := benchOnset(pcm, max(from, eot)); on >= 0 {
					r.Replied, replyAt = true, max(from, eot)+int(on*8000)
					break
				}
			}
			select {
			case <-time.After(100 * time.Millisecond):
			case <-ctx.Done():
				fail(i, context.Cause(ctx))
				return
			}
		}
		write(r)
	}
}

// benchQuietFrom is the first sample at or after from that starts n samples
// of agent silence, or -1.
func benchQuietFrom(pcm []byte, from, n int) int {
	for s := from; (s+n)*2 <= len(pcm); s += 160 {
		if benchOnset(pcm[:(s+n)*2], s) < 0 {
			return s
		}
	}
	return -1
}

func benchWaitSamples(ctx context.Context, rec string, n int) error {
	for {
		have, _, err := recSamples(rec)
		if err != nil || have >= n {
			return err
		}
		select {
		case <-time.After(20 * time.Millisecond):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
}

// benchWaitQuiet returns once the agent has been silent for a second.
func benchWaitQuiet(ctx context.Context, rec string, max time.Duration) error {
	deadline := time.Now().Add(max)
	for time.Now().Before(deadline) {
		n, pcm, err := recSamples(rec)
		if err != nil {
			return err
		}
		if n > benchQuietGap && benchOnset(pcm, n-benchQuietGap) < 0 {
			return nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return context.Cause(ctx)
		}
	}
	return fmt.Errorf("agent still talking after %v", max)
}
