package studio

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/tts"
)

type RunOptions struct {
	OutDir   string // the scenario's audio, transcript and result.json go in OutDir/<scenario name>
	SBC      string // host:port
	FromHost string // inbound-did: From host (a whitelisted carrier IP)
	Logf     func(format string, args ...any)
}

type Result struct {
	Dir          string
	TestSession  string
	CallSID      string
	SIPCallID    string
	EndedByAgent bool
	Transcript   string // what the caller heard, one line per utterance
	Failures     []string
}

type sessionResp struct {
	Session struct {
		ID      string  `json:"id"`
		CallSID *string `json:"call_sid"`
	} `json:"session"`
	Webrtc struct {
		Server         string `json:"server"`
		Realm          string `json:"realm"`
		Username       string `json:"username"`
		Password       string `json:"password"`
		ApplicationSID string `json:"application_sid"`
		Token          string `json:"token"`
	} `json:"webrtc"`
}

// voiceMeter tails the call's PCM recording and reports whether the agent is speaking.
type voiceMeter struct {
	path      string
	off       int64
	lastVoice time.Time
	sawVoice  bool
}

func (m *voiceMeter) poll() {
	f, err := os.Open(m.path)
	if err != nil {
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	if st.Size()-m.off < 320 {
		return
	}
	buf := make([]byte, (st.Size()-m.off)/320*320)
	n, _ := f.ReadAt(buf, m.off)
	m.off += int64(n)
	for i := 0; i+320 <= n; i += 320 {
		var sum float64
		for j := i; j < i+320; j += 2 {
			v := float64(int16(binary.LittleEndian.Uint16(buf[j:])))
			sum += v * v
		}
		if math.Sqrt(sum/160) > 400 {
			m.lastVoice, m.sawVoice = time.Now(), true
		}
	}
}

// waitAgentDone returns once the agent has spoken and then been quiet for quiet,
// or after max with no speech at all.
func waitAgentDone(ctx context.Context, call *jsip.Call, m *voiceMeter, quiet, max time.Duration) bool {
	start := time.Now()
	m.sawVoice = false
	for {
		select {
		case <-call.Done():
			return false
		case <-ctx.Done():
			return false
		case <-time.After(100 * time.Millisecond):
		}
		m.poll()
		if m.sawVoice && time.Since(m.lastVoice) >= quiet {
			return true
		}
		if !m.sawVoice && time.Since(start) >= max {
			return true
		}
	}
}

// waitAgentSpeaking returns once the agent has been speaking for into, so the next turn barges in.
func waitAgentSpeaking(ctx context.Context, call *jsip.Call, m *voiceMeter, into, max time.Duration) bool {
	start := time.Now()
	m.sawVoice = false
	var first time.Time
	for time.Since(start) < max {
		select {
		case <-call.Done():
			return false
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
		m.poll()
		if m.sawVoice && first.IsZero() {
			first = time.Now()
		}
		if !first.IsZero() && time.Since(first) >= into {
			return true
		}
	}
	return true
}

// internal/stt is fixed to en-US and returns no timings, so this asks Deepgram for per-utterance starts.
func transcribe(ctx context.Context, pcm []byte, lang string) (string, error) {
	q := url.Values{"model": {"nova-3"}, "language": {lang}, "encoding": {"linear16"}, "sample_rate": {"8000"}, "punctuate": {"true"}, "utterances": {"true"}}
	req, _ := http.NewRequestWithContext(ctx, "POST", "https://api.deepgram.com/v1/listen?"+q.Encode(), bytes.NewReader(pcm))
	req.Header.Set("Authorization", "Token "+os.Getenv(tts.EnvKey))
	req.Header.Set("Content-Type", "application/octet-stream")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode >= 300 {
		body, _ := io.ReadAll(res.Body)
		return "", fmt.Errorf("deepgram %d: %s", res.StatusCode, body)
	}
	var out struct {
		Results struct {
			Utterances []struct {
				Start      float64 `json:"start"`
				Transcript string  `json:"transcript"`
			} `json:"utterances"`
		} `json:"results"`
	}
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		return "", err
	}
	var b strings.Builder
	for _, u := range out.Results.Utterances {
		fmt.Fprintf(&b, "[%6.1fs] %s\n", u.Start, u.Transcript)
	}
	return b.String(), nil
}

// Run plays sc against its agent and checks the call. The error is for a run that could not
// happen; unmet expectations are in Result.Failures. The test session is always deleted.
func Run(ctx context.Context, c *Client, sc Scenario, o RunOptions) (*Result, error) {
	logf := o.Logf
	if logf == nil {
		logf = log.Printf
	}
	if sc.MaxSec == 0 {
		sc.MaxSec = 150
	}
	r := &Result{Dir: filepath.Join(o.OutDir, sc.Name)}
	_ = os.MkdirAll(filepath.Join(r.Dir, "utt"), 0o755)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(sc.MaxSec)*time.Second)
	defer cancel()

	// Caller audio first, so TTS latency never lands inside the call.
	wavs := make([]string, len(sc.Turns))
	for i, t := range sc.Turns {
		var err error
		if wavs[i], err = tts.EnsureWAV(ctx, filepath.Join(r.Dir, "utt"), t.Say, tts.PromptOptions{Model: sc.Voice}); err != nil {
			return r, fmt.Errorf("tts turn %d: %w", i, err)
		}
	}

	// inbound-did goes through the published route like a carrier would; the others use a test session.
	viaDID := sc.Direction == "inbound-did"
	var sess sessionResp
	if !viaDID {
		body := map[string]any{"record": true}
		if len(sc.Env) > 0 {
			body["env"] = sc.Env
		}
		if sc.Direction == "outbound" {
			body["outbound"] = map[string]any{"ring": "browser"}
		}
		if err := c.openSession(ctx, sc.AgentID, body, &sess, logf); err != nil {
			return r, fmt.Errorf("create test session: %w", err)
		}
		r.TestSession = sess.Session.ID
		logf("session %s realm=%s user=%s app=%s", sess.Session.ID, sess.Webrtc.Realm, sess.Webrtc.Username, sess.Webrtc.ApplicationSID)
		defer func() {
			dctx, dcancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer dcancel()
			_ = c.Do(dctx, "DELETE", fmt.Sprintf("/agents/%s/test-sessions/%s", sc.AgentID, sess.Session.ID), nil, nil)
		}()
	}
	w := sess.Webrtc

	sbcHost, _, _ := net.SplitHostPort(o.SBC)
	hosts := map[string]string{}
	if w.Realm != "" {
		hosts[w.Realm] = sbcHost
	}
	res, err := jsip.NewStaticResolver(hosts)
	if err != nil {
		return r, err
	}
	defer res.Close()

	incoming := make(chan *jsip.Call, 1)
	handler := func(hctx context.Context, call *jsip.Call) error {
		if err := call.Answer(); err != nil {
			return err
		}
		incoming <- call
		<-call.Done()
		return nil
	}
	scfg := jsip.Config{SIPDomain: w.Realm, User: w.Username, Pass: w.Password, Resolver: res.Resolver()}
	if viaDID {
		scfg = jsip.Config{User: "studioscenarios", Resolver: res.Resolver()}
	}
	stack, err := jsip.Start(ctx, scfg, handler)
	if err != nil {
		return r, fmt.Errorf("register: %w", err)
	}
	defer stack.Stop()

	var call *jsip.Call
	dialSid := ""
	start := time.Now()
	if viaDID {
		from := fmt.Sprintf("+1555%07d", time.Now().UnixNano()%10_000_000)
		call, err = stack.Invite(ctx, fmt.Sprintf("sip:%s@%s", sc.DID, o.SBC), jsip.InviteOptions{
			Headers: jsip.H{
				"From": fmt.Sprintf("<sip:%s@%s>;tag=%08x", from, o.FromHost, time.Now().UnixNano()&0xffffffff),
				"To":   fmt.Sprintf("<sip:%s@%s>", sc.DID, sbcHost),
			},
		})
		if err != nil {
			return r, fmt.Errorf("invite: %w", err)
		}
	} else if sc.Direction == "outbound" {
		var d struct {
			CallSID string `json:"call_sid"`
		}
		if err := c.Do(ctx, "POST", fmt.Sprintf("/agents/%s/test-sessions/%s/dial", sc.AgentID, sess.Session.ID), map[string]string{"token": w.Token}, &d); err != nil {
			return r, fmt.Errorf("dial: %w", err)
		}
		logf("studio dialing us, call_sid=%s", d.CallSID)
		dialSid = d.CallSID
		select {
		case call = <-incoming:
		case <-ctx.Done():
			return r, errors.New("no incoming call from studio")
		}
	} else {
		call, err = stack.Invite(ctx, fmt.Sprintf("sip:app-%s@%s", w.ApplicationSID, w.Realm), jsip.InviteOptions{
			Headers: jsip.H{"X-Application-Sid": w.ApplicationSID, "X-Vab-Test-Session": w.Token},
		})
		if err != nil {
			return r, fmt.Errorf("invite: %w", err)
		}
	}
	r.SIPCallID = call.CallID()
	logf("connected call-id=%s codec=%s", call.CallID(), call.Codec())

	pcm := filepath.Join(r.Dir, "callee-heard.pcm")
	_ = call.StartRecording(pcm)
	_ = call.SendSilence()
	m := &voiceMeter{path: pcm}
	timeline := []map[string]any{}
	mark := func(ev, text string) {
		timeline = append(timeline, map[string]any{"t": math.Round(time.Since(start).Seconds()*10) / 10, "ev": ev, "text": text})
	}

	alive := true
	for i, t := range sc.Turns {
		if !alive {
			break
		}
		if t.WaitAgent == nil || *t.WaitAgent {
			// Outbound agents wait for the callee, so the first turn waits less.
			max := 12 * time.Second
			if i == 0 && sc.Direction == "outbound" {
				max = 2 * time.Second
			}
			alive = waitAgentDone(ctx, call, m, 2500*time.Millisecond, max)
		} else {
			alive = waitAgentSpeaking(ctx, call, m, 800*time.Millisecond, 12*time.Second)
		}
		if !alive {
			break
		}
		mark("caller", t.Say)
		if err := call.SendWAV(wavs[i]); err != nil {
			mark("error", err.Error())
			break
		}
		_ = call.SendSilence()
	}
	tail := sc.TailSec
	if tail == 0 {
		tail = 25
	}
	if alive {
		mark("caller_done", "")
		select {
		case <-call.Done():
		case <-ctx.Done():
		case <-time.After(time.Duration(tail) * time.Second):
		}
	}
	r.EndedByAgent = call.State() == jsip.StateEnded
	if !r.EndedByAgent {
		_ = call.Hangup()
	}
	call.StopRecording()
	mark("end", map[bool]string{true: "agent hung up", false: "caller hung up"}[r.EndedByAgent])

	// Studio's own view of the call. The call's deadline may have passed, so this gets its own.
	actx, acancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer acancel()
	time.Sleep(4 * time.Second)
	var view struct {
		Session struct {
			CallSID *string `json:"call_sid"`
		} `json:"session"`
	}
	if !viaDID {
		_ = c.Do(actx, "GET", fmt.Sprintf("/agents/%s/test-sessions/%s", sc.AgentID, sess.Session.ID), nil, &view)
		if view.Session.CallSID != nil {
			r.CallSID = *view.Session.CallSID
		}
	}
	// A test session can end without its call_sid; the newest call on this agent is ours.
	for i := 0; i < 15 && r.CallSID == ""; i++ {
		var list struct {
			Calls []struct {
				CallSID string    `json:"call_sid"`
				When    time.Time `json:"when"`
			} `json:"calls"`
		}
		_ = c.Do(actx, "GET", fmt.Sprintf("/calls?agent_id=%s&limit=1", sc.AgentID), nil, &list)
		if len(list.Calls) > 0 && list.Calls[0].When.After(start.Add(-10*time.Second)) {
			r.CallSID = list.Calls[0].CallSID
		} else {
			time.Sleep(2 * time.Second)
		}
	}
	if r.CallSID == "" {
		r.CallSID = dialSid
	}
	var detail json.RawMessage
	if r.CallSID != "" {
		if err := c.Do(actx, "GET", "/calls/"+r.CallSID, nil, &detail); err != nil {
			logf("call detail: %v", err)
		}
	}
	heard, _ := os.ReadFile(pcm)
	if r.Transcript, err = transcribe(actx, heard, sc.Language); err != nil {
		logf("transcribe: %v", err)
	}
	_ = os.WriteFile(filepath.Join(r.Dir, "heard.txt"), []byte(r.Transcript), 0o644)
	if sc.Expect != nil {
		r.Failures = Check(sc.Expect, detail, r.EndedByAgent, r.Transcript)
	}
	summary := map[string]any{
		"scenario": sc.Name, "agent_id": sc.AgentID, "direction": sc.Direction,
		"test_session": r.TestSession, "call_sid": r.CallSID, "sip_call_id": r.SIPCallID,
		"ended_by_agent": r.EndedByAgent, "duration_s": math.Round(call.Duration().Seconds()*10) / 10,
		"timeline": timeline, "studio_call": detail, "checked": sc.Expect != nil, "failures": r.Failures,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(r.Dir, "result.json"), b, 0o644)
	return r, nil
}

// openSession waits its turn when the login already has as many test sessions open as Studio allows.
func (c *Client) openSession(ctx context.Context, agentID string, body any, out *sessionResp, logf func(string, ...any)) error {
	for {
		err := c.Do(ctx, "POST", fmt.Sprintf("/agents/%s/test-sessions", agentID), body, out)
		var ae *APIError
		if !errors.As(err, &ae) || ae.Status != http.StatusConflict || !strings.Contains(ae.Body, "TEST_SESSION_LIMIT") {
			return err
		}
		logf("studio test session limit reached; waiting for one to end")
		select {
		case <-ctx.Done():
			return err
		case <-time.After(5 * time.Second):
		}
	}
}
