// Command studioscenarios plays a scripted caller against an Agent Studio agent
// through a Studio test session: register as the session's SIP user, then dial
// the agent (inbound) or let Studio ring us (outbound), speak each turn once the
// agent stops talking, and save the audio, the transcript and Studio's call record.
package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
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

type turn struct {
	Say string `json:"say"`
	// WaitAgent: wait for the agent to finish speaking first (default true).
	WaitAgent *bool `json:"wait_agent,omitempty"`
}

type scenario struct {
	Name      string            `json:"name"`
	AgentID   string            `json:"agent_id"`
	Direction string            `json:"direction"`     // inbound | outbound | inbound-did
	DID       string            `json:"did,omitempty"` // inbound-did: number routed to the published agent
	Language  string            `json:"language"`      // for the offline transcript
	Voice     string            `json:"voice"`         // Deepgram voice for the caller
	Env       map[string]string `json:"env,omitempty"`
	Turns     []turn            `json:"turns"`
	MaxSec    int               `json:"max_sec,omitempty"`
	TailSec   int               `json:"tail_sec,omitempty"` // wait after the last turn; 0 means 25
}

type studio struct {
	base, sid, origin string
}

func (s studio) do(method, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, s.base+path, rd)
	req.Header.Set("Cookie", "__Host-studio_sid="+s.sid)
	req.Header.Set("Origin", s.origin)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 300 {
		return fmt.Errorf("%s %s: %d %s", method, path, res.StatusCode, string(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
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

// internal/stt is fixed to en-US and returns no timings, so this asks Deepgram for per-utterance starts.
func transcribe(pcm []byte, lang string) (string, error) {
	q := url.Values{"model": {"nova-3"}, "language": {lang}, "encoding": {"linear16"}, "sample_rate": {"8000"}, "punctuate": {"true"}, "utterances": {"true"}}
	req, _ := http.NewRequest("POST", "https://api.deepgram.com/v1/listen?"+q.Encode(), bytes.NewReader(pcm))
	req.Header.Set("Authorization", "Token "+os.Getenv("DEEPGRAM_API_KEY"))
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

func main() {
	if err := run(); err != nil {
		log.Printf("studioscenarios: %v", err)
		os.Exit(1)
	}
}

// run returns errors instead of exiting, so the deferred test-session delete and SIP teardown always run.
func run() error {
	scFile := flag.String("scenario", "", "scenario JSON")
	outDir := flag.String("out", "", "output dir")
	sidFile := flag.String("sid", "", "file holding a Studio session cookie value")
	account := flag.String("account", "01a15da7-95f6-7175-9fc4-4fe05028a000", "Studio account (workspace) id")
	apiBase := flag.String("api", "https://studio.jambonz.io/v1", "Studio API base")
	sbc := flag.String("sbc", "56.10.102.117:5060", "SBC host:port")
	fromHost := flag.String("from-host", "123.21.48.183", "inbound-did: From host (a whitelisted carrier IP)")
	flag.Parse()

	raw, err := os.ReadFile(*scFile)
	if err != nil {
		return err
	}
	var sc scenario
	if err := json.Unmarshal(raw, &sc); err != nil {
		return err
	}
	if sc.MaxSec == 0 {
		sc.MaxSec = 150
	}
	sidRaw, err := os.ReadFile(*sidFile)
	if err != nil {
		return err
	}
	api, err := url.Parse(*apiBase)
	if err != nil {
		return fmt.Errorf("-api: %w", err)
	}
	// Studio rejects writes whose Origin is not its own host.
	origin := api.Scheme + "://" + api.Host
	st := studio{base: fmt.Sprintf("%s/accounts/%s", *apiBase, *account), sid: strings.TrimSpace(string(sidRaw)), origin: origin}
	dir := filepath.Join(*outDir, sc.Name)
	_ = os.MkdirAll(filepath.Join(dir, "utt"), 0o755)
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(sc.MaxSec)*time.Second)
	defer cancel()

	// Caller audio first, so TTS latency never lands inside the call.
	wavs := make([]string, len(sc.Turns))
	for i, t := range sc.Turns {
		if wavs[i], err = tts.EnsureWAV(ctx, filepath.Join(dir, "utt"), t.Say, tts.PromptOptions{Model: sc.Voice}); err != nil {
			return fmt.Errorf("tts turn %d: %w", i, err)
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
		if err := st.do("POST", fmt.Sprintf("/agents/%s/test-sessions", sc.AgentID), body, &sess); err != nil {
			return fmt.Errorf("create test session: %w", err)
		}
		log.Printf("session %s realm=%s user=%s app=%s", sess.Session.ID, sess.Webrtc.Realm, sess.Webrtc.Username, sess.Webrtc.ApplicationSID)
		defer func() {
			_ = st.do("DELETE", fmt.Sprintf("/agents/%s/test-sessions/%s", sc.AgentID, sess.Session.ID), nil, nil)
		}()
	}
	w := sess.Webrtc

	sbcHost, _, _ := net.SplitHostPort(*sbc)
	hosts := map[string]string{}
	if w.Realm != "" {
		hosts[w.Realm] = sbcHost
	}
	res, err := jsip.NewStaticResolver(hosts)
	if err != nil {
		return err
	}
	defer res.Close()

	incoming := make(chan *jsip.Call, 1)
	handler := func(hctx context.Context, c *jsip.Call) error {
		if err := c.Answer(); err != nil {
			return err
		}
		incoming <- c
		<-c.Done()
		return nil
	}
	cfg := jsip.Config{SIPDomain: w.Realm, User: w.Username, Pass: w.Password, Resolver: res.Resolver()}
	if viaDID {
		cfg = jsip.Config{User: "studioscenarios", Resolver: res.Resolver()}
	}
	stack, err := jsip.Start(ctx, cfg, handler)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}
	defer stack.Stop()

	var call *jsip.Call
	dialSid := ""
	start := time.Now()
	if viaDID {
		from := fmt.Sprintf("+1555%07d", time.Now().UnixNano()%10_000_000)
		call, err = stack.Invite(ctx, fmt.Sprintf("sip:%s@%s", sc.DID, *sbc), jsip.InviteOptions{
			Headers: jsip.H{
				"From": fmt.Sprintf("<sip:%s@%s>;tag=%08x", from, *fromHost, time.Now().UnixNano()&0xffffffff),
				"To":   fmt.Sprintf("<sip:%s@%s>", sc.DID, sbcHost),
			},
		})
		if err != nil {
			return fmt.Errorf("invite: %w", err)
		}
	} else if sc.Direction == "outbound" {
		var d struct {
			CallSID string `json:"call_sid"`
		}
		if err := st.do("POST", fmt.Sprintf("/agents/%s/test-sessions/%s/dial", sc.AgentID, sess.Session.ID), map[string]string{"token": w.Token}, &d); err != nil {
			return fmt.Errorf("dial: %w", err)
		}
		log.Printf("studio dialing us, call_sid=%s", d.CallSID)
		dialSid = d.CallSID
		select {
		case call = <-incoming:
		case <-ctx.Done():
			return errors.New("no incoming call from studio")
		}
	} else {
		call, err = stack.Invite(ctx, fmt.Sprintf("sip:app-%s@%s", w.ApplicationSID, w.Realm), jsip.InviteOptions{
			Headers: jsip.H{"X-Application-Sid": w.ApplicationSID, "X-Vab-Test-Session": w.Token},
		})
		if err != nil {
			return fmt.Errorf("invite: %w", err)
		}
	}
	log.Printf("connected call-id=%s codec=%s", call.CallID(), call.Codec())

	pcm := filepath.Join(dir, "callee-heard.pcm")
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
			if !alive {
				break
			}
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
	endedByAgent := call.State() == jsip.StateEnded
	if !endedByAgent {
		_ = call.Hangup()
	}
	call.StopRecording()
	mark("end", map[bool]string{true: "agent hung up", false: "caller hung up"}[endedByAgent])

	// Studio's own view of the call.
	time.Sleep(4 * time.Second)
	var view struct {
		Session struct {
			CallSID *string `json:"call_sid"`
		} `json:"session"`
	}
	callSid := ""
	if viaDID {
		// The newest call on this agent is ours; the record lands a few seconds after hangup.
		for i := 0; i < 15 && callSid == ""; i++ {
			var list struct {
				Calls []struct {
					CallSID string    `json:"call_sid"`
					When    time.Time `json:"when"`
				} `json:"calls"`
			}
			_ = st.do("GET", fmt.Sprintf("/calls?agent_id=%s&limit=1", sc.AgentID), nil, &list)
			if len(list.Calls) > 0 && list.Calls[0].When.After(start.Add(-10*time.Second)) {
				callSid = list.Calls[0].CallSID
			} else {
				time.Sleep(2 * time.Second)
			}
		}
	} else {
		_ = st.do("GET", fmt.Sprintf("/agents/%s/test-sessions/%s", sc.AgentID, sess.Session.ID), nil, &view)
		if view.Session.CallSID != nil {
			callSid = *view.Session.CallSID
		}
		if callSid == "" {
			callSid = dialSid
		}
	}
	var detail json.RawMessage
	if callSid != "" {
		if err := st.do("GET", "/calls/"+callSid, nil, &detail); err != nil {
			log.Printf("call detail: %v", err)
		}
	}
	heard, _ := os.ReadFile(pcm)
	transcript, err := transcribe(heard, sc.Language)
	if err != nil {
		log.Printf("transcribe: %v", err)
	}
	_ = os.WriteFile(filepath.Join(dir, "heard.txt"), []byte(transcript), 0o644)
	summary := map[string]any{
		"scenario": sc.Name, "agent_id": sc.AgentID, "direction": sc.Direction,
		"test_session": sess.Session.ID, "call_sid": callSid, "sip_call_id": call.CallID(),
		"ended_by_agent": endedByAgent, "duration_s": math.Round(call.Duration().Seconds()*10) / 10,
		"timeline": timeline, "studio_call": detail,
	}
	b, _ := json.MarshalIndent(summary, "", "  ")
	_ = os.WriteFile(filepath.Join(dir, "result.json"), b, 0o644)
	fmt.Printf("== %s call_sid=%s ended_by_agent=%v\n%s", sc.Name, callSid, endedByAgent, transcript)
	return nil
}
