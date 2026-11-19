package verbs

// End-to-end turn-detection benchmark: real eot-bench caller turns streamed
// into real agent calls, once per turnDetection setting. Each turn comes with
// ground truth (where the caller pauses mid-turn and where the turn ends), so
// the agent's first audio tells whether it cut the caller off and how long it
// took to answer. Off unless TT_BENCH_DIR is set; results go to TT_BENCH_OUT.
//
//	TT_BENCH_DIR=<dir with manifest.json + wav/> TT_BENCH_OUT=bench.jsonl \
//	TT_BENCH_CONFIGS=stt,jambonz,jambonz+ip [TT_BENCH_KEYS=000,001,...] \
//	go test -run TestBench_TurnDetection -parallel 4 ./tests/verbs/

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type benchTurn struct {
	Key      string      `json:"key"`
	ID       string      `json:"id"`
	Cohort   string      `json:"cohort"`
	Lang     string      `json:"lang,omitempty"` // STT language; empty is English
	Dur      float64     `json:"dur"`
	EotStart float64     `json:"eot_start"`
	Holds    [][]float64 `json:"holds"`
}

type benchResult struct {
	Config   string      `json:"config"`
	Key      string      `json:"key"`
	ID       string      `json:"id"`
	Cohort   string      `json:"cohort"`
	Lang     string      `json:"lang,omitempty"`
	EotStart float64     `json:"eot_start"`
	Holds    [][]float64 `json:"holds"`
	Onset    *float64    `json:"onset"` // agent's first audio, seconds from the start of the caller's turn
	Error    string      `json:"error,omitempty"`
}

// the agent only has to answer something; a short fixed reply keeps LLM time small and even
const benchPrompt = "You are a phone agent in an automated test. Start the call by saying exactly: " +
	"'Hello, how can I help you?' After that, when the caller finishes speaking, reply with one short " +
	"sentence such as 'Okay, got it.' Never ask a question."

// benchListenAfterEot is how long after the caller's real turn end we keep
// listening before calling the turn missed.
const benchListenAfterEot = 6 * time.Second

// Steps:
//  1. load-bench
//  2. one subtest per turn and config: greet, play the turn, measure the agent's first audio
func TestBench_TurnDetection(t *testing.T) {
	turns, configs, write := benchLoad(t, []string{"stt", "jambonz"}, "results.jsonl")
	if turns == nil {
		return
	}
	dir := os.Getenv("TT_BENCH_DIR")
	// interleave configs per turn so time-of-day drift hits both alike
	for _, turn := range turns {
		for _, c := range configs {
			turn, c := turn, c
			t.Run(c+"/"+turn.Key, func(t *testing.T) {
				t.Parallel()
				r := benchResult{Config: c, Key: turn.Key, ID: turn.ID, Cohort: turn.Cohort, Lang: turn.Lang,
					EotStart: turn.EotStart, Holds: turn.Holds}
				onset, err := benchOneCall(t, c, filepath.Join(dir, "wav", turn.Key+".wav"), turn)
				if err != nil {
					r.Error = err.Error()
				} else if onset >= 0 {
					r.Onset = &onset
				}
				writeBench(write, r)
			})
		}
	}
}

// benchLoad reads the bench manifest (TT_BENCH_DIR, TT_BENCH_LIMIT, TT_BENCH_KEYS),
// the configs (TT_BENCH_CONFIGS) and opens the results file (TT_BENCH_OUT).
// It returns nil turns when the bench is not configured.
func benchLoad(t *testing.T, defConfigs []string, defOut string) ([]benchTurn, []string, func([]byte)) {
	dir := os.Getenv("TT_BENCH_DIR")
	if dir == "" {
		t.Skip("TT_BENCH_DIR not set")
	}
	requireWebhook(t)
	s := Step(t, "load-bench")
	raw, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		s.Fatalf("manifest: %v", err)
	}
	var turns []benchTurn
	if err := json.Unmarshal(raw, &turns); err != nil {
		s.Fatalf("manifest: %v", err)
	}
	if n, err := strconv.Atoi(os.Getenv("TT_BENCH_LIMIT")); err == nil && n > 0 && n < len(turns) {
		turns = turns[:n]
	}
	if keys := os.Getenv("TT_BENCH_KEYS"); keys != "" { // a fixed subset, e.g. the 50-turn baseline set
		want := map[string]bool{}
		for _, k := range strings.Split(keys, ",") {
			want[k] = true
		}
		kept := turns[:0]
		for _, tn := range turns {
			if want[tn.Key] {
				kept = append(kept, tn)
			}
		}
		turns = kept
	}
	configs := strings.Split(os.Getenv("TT_BENCH_CONFIGS"), ",")
	if configs[0] == "" {
		configs = defConfigs
	}
	outPath := os.Getenv("TT_BENCH_OUT")
	if outPath == "" {
		outPath = filepath.Join(dir, defOut)
	}
	out, err := os.OpenFile(outPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		s.Fatalf("results: %v", err)
	}
	t.Cleanup(func() { _ = out.Close() })
	s.Logf("%d turns x %d configs -> %s", len(turns), len(configs), outPath)
	s.Done()
	var mu sync.Mutex
	return turns, configs, func(line []byte) {
		mu.Lock()
		defer mu.Unlock()
		_, _ = out.Write(append(line, '\n'))
	}
}

func writeBench(write func([]byte), r any) {
	b, _ := json.Marshal(r)
	write(b)
}

// benchTurnDetection maps a config to agent fields: a turnDetection value, or
// "<vendor>+ip" for that vendor's turn detection with interruptPrediction barge-in.
func benchTurnDetection(c string) map[string]any {
	if v, ok := strings.CutSuffix(c, "+ip"); ok {
		return map[string]any{"turnDetection": v, "bargeIn": map[string]any{
			"enable": true, "strategy": "interruptPrediction", "vendor": v}}
	}
	return map[string]any{"turnDetection": c}
}

// benchSTT sets the recognizer for a non-English turn.
func benchSTT(overrides map[string]any, lang string) {
	if lang == "" {
		return
	}
	// name the model: jambonz falls back to deepgram "base" for languages it has no default for, which is rejected
	model := "nova-2"
	if lang == "ar" {
		model = "nova-3"
	}
	overrides["stt"] = map[string]any{"vendor": "deepgram", "label": deepgramLabel, "language": lang,
		"deepgramOptions": map[string]any{"model": model}}
}

// benchOneCall plays one caller turn into a fresh agent call and returns
// when the agent first made sound, in seconds from the start of the turn
// (-1 if it never did).
func benchOneCall(t *testing.T, config, wav string, turn benchTurn) (float64, error) {
	ctx := WithTimeout(t, 90*time.Second)
	uas := claimUAS(t, ctx)
	_, sess := claimSession(t)
	off := 0
	// barge-in on, as agents run by default: it also starts the VAD that moves the agent into
	// "user speaking", without which a turn detector's end-of-turn is ignored until STT finalizes
	// greeting first, as production agents run: mediajam reports the bot as speaking from the
	// moment TTS connects until its first utterance ends, which keeps krisp reset until then
	overrides := benchTurnDetection(config)
	benchSTT(overrides, turn.Lang)
	scriptKrispAgent(sess, krispAgentVerb(sess, agentVerbOpts{SystemPrompt: benchPrompt, NoResponseTimeout: &off,
		BargeIn: true, Greeting: true}, overrides))
	call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(60))
	defer HangupAndWaitEnded(t, ctx, call)

	if err := call.Answer(); err != nil {
		return -1, err
	}
	rec := filepath.Join(t.TempDir(), "agent.pcm")
	if err := call.StartRecording(rec); err != nil {
		return -1, err
	}
	if err := call.SendSilence(); err != nil {
		return -1, err
	}
	time.Sleep(RecognizerArmDelay)
	startSample, err := benchWaitGreeting(ctx, rec)
	if err != nil {
		return -1, err
	}
	t0 := time.Now()
	if err := call.SendWAV(wav); err != nil {
		return -1, err
	}
	if err := call.SendSilence(); err != nil {
		return -1, err
	}
	wait := time.Duration(turn.EotStart*float64(time.Second)) + benchListenAfterEot - time.Since(t0)
	select {
	case <-time.After(wait):
	case <-ctx.Done():
		return -1, context.Cause(ctx)
	}
	call.StopRecording()
	pcm, err := os.ReadFile(rec)
	if err != nil {
		return -1, err
	}
	return benchOnset(pcm, startSample), nil
}

// benchWaitGreeting waits until the agent has greeted and then been quiet
// for a second, and returns the recording position (in samples) to start
// the caller's turn from.
func benchWaitGreeting(ctx context.Context, rec string) (int, error) {
	const quiet = 8000 // 1 s at 8 kHz
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		pcm, err := os.ReadFile(rec)
		if err != nil {
			return 0, err
		}
		n := len(pcm) / 2
		if on := benchOnset(pcm, 0); on >= 0 && n-int(on*8000) > quiet && benchOnset(pcm, n-quiet) < 0 {
			return n, nil
		}
		select {
		case <-time.After(100 * time.Millisecond):
		case <-ctx.Done():
			return 0, context.Cause(ctx)
		}
	}
	return 0, errors.New("agent never finished a greeting")
}

// benchOnset finds the first 60 ms of agent audio above -35 dBFS after
// startSample in 8 kHz PCM16, as seconds from startSample.
func benchOnset(pcm []byte, startSample int) float64 {
	const frame, need = 160, 3
	const floor = 32768 * 0.0178 // -35 dBFS
	loud := 0
	for s := startSample; (s+frame)*2 <= len(pcm); s += frame {
		var e float64
		for i := s; i < s+frame; i++ {
			v := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
			e += v * v
		}
		if math.Sqrt(e/frame) > floor {
			loud++
			if loud == need {
				return float64(s-(need-1)*frame-startSample) / 8000
			}
		} else {
			loud = 0
		}
	}
	return -1
}
