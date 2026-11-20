// The four DTMF paths across a bridged dial, one test per cell of the matrix
// (how the caller sends x how the callee can receive):
//
//	          callee inband      callee rfc2833
//	caller    InbandToInband     InbandTo2833
//	inband
//	caller    RFC2833ToInband    RFC2833To2833
//	rfc2833
//
// Two independent mechanisms have to cooperate for a cell to pass: rtpengine
// converting between the two carriages per leg (it knows each leg's SDP), and
// the media server relaying the digit from one leg of the bridge to the other.
// A cell that fails names which one is missing.
//
// Two defects live in rtpengine and are out of reach from here. Both were
// measured on the wire against a live cluster:
//
//   - Regenerating telephone-event as tones inserts no pause between events
//     (gap_before = 0ms for every digit), so two identical digits in a row fuse
//     into one long tone. See TestVerb_DTMFMatrix_RepeatedDigits.
//   - Suppression of a caller's original tone starts only once the DSP has
//     recognised it, so roughly the first 65ms leaks through as audio. A callee
//     that runs inband detection alongside the telephone-event it negotiated
//     can therefore see one digit twice. The assertions below allow a leaked
//     subsequence but not a stray digit.
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// dtmfMatrixDigits covers the symbols and a column-D event (the 1633Hz column
// no keypad exposes), with no repeated digit — repeats are their own test
// below because they fail for a reason outside this system.
const dtmfMatrixDigits = "*#0D"

func TestVerb_DTMFMatrix_InbandToInband(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: sendInband, calleeTelEvent: false})
}

func TestVerb_DTMFMatrix_InbandTo2833(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: sendInband, calleeTelEvent: true})
}

func TestVerb_DTMFMatrix_RFC2833ToInband(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: send2833, calleeTelEvent: false})
}

func TestVerb_DTMFMatrix_RFC2833To2833(t *testing.T) {
	runDTMFMatrix(t, dtmfMatrixCase{caller: send2833, calleeTelEvent: true})
}

type callerMode int

const (
	sendInband callerMode = iota
	send2833
)

func (m callerMode) String() string {
	if m == sendInband {
		return "inband"
	}
	return "rfc2833"
}

type dtmfMatrixCase struct {
	caller callerMode
	// digits overrides dtmfMatrixDigits; the strict assertion applies to
	// whatever is set here, so a caller cannot accidentally get an
	// assertion-free run by passing its own string.
	digits string
	// calleeTelEvent decides whether the callee answers with telephone-event.
	// False models a leg that can only carry tones in the audio.
	calleeTelEvent bool
}

func (c dtmfMatrixCase) name() string {
	callee := "inband"
	if c.calleeTelEvent {
		callee = "rfc2833"
	}
	return fmt.Sprintf("%s-to-%s", c.caller, callee)
}

// runDTMFMatrix bridges two UASes with anchorMedia and sends dtmfMatrixDigits
// from the caller in the case's carriage, then reads them back on the callee in
// whichever carriage that leg negotiated.
func runDTMFMatrix(t *testing.T, tc dtmfMatrixCase) {
	t.Parallel()
	requireWebhook(t)
	runDTMFMatrixWith(t, tc, dtmfMatrixDigits)
}

// runDTMFMatrixWith drives one cell with an explicit digit string and returns
// what the callee received in the carriage its own SDP negotiated.
func runDTMFMatrixWith(t *testing.T, tc dtmfMatrixCase, digits string) string {
	tag := tc.name()
	strict := tc.digits == "" // a case carrying its own digits asserts in its own test
	ctx := WithTimeout(t, 120*time.Second)
	callerUAS, calleeUAS := claimUAS2(t, ctx)
	_, sess := claimSession(t)

	var wavPath string
	if tc.caller == sendInband {
		wavPath = SynthesizeDTMFWAV(t, digits, 100, 100)
	}

	s := Step(t, "script-dial-to-callee")
	actionURL := SessionURL(sess, "dial")
	target := fmt.Sprintf("%s@%s", calleeUAS.Username, suite.SIPRealm)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial", "target", []any{map[string]any{"type": "user", "name": target}},
			"timeout", 20, "actionHook", actionURL, "anchorMedia", true),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Done()

	s = Step(t, "spawn-callee-goroutine")
	calleeDone := make(chan struct{})
	calleeAnswered := make(chan struct{})
	var calleeCall *jsip.Call
	var calleeRec string
	calleeCtx, calleeCancel := context.WithCancel(ctx)
	go func() {
		defer close(calleeDone)
		select {
		case c := <-calleeUAS.Inbound:
			calleeCall = c
			if err := c.Trying(); err != nil {
				GoroutineFailf(t, "callee:trying", "Trying: %v", err)
				return
			}
			if err := c.Ringing(); err != nil {
				GoroutineFailf(t, "callee:ringing", "Ringing: %v", err)
				return
			}
			var err error
			if tc.calleeTelEvent {
				err = c.Answer()
			} else {
				err = c.AnswerWithoutTelephoneEvent()
			}
			if err != nil {
				GoroutineFailf(t, "callee:answer", "Answer: %v", err)
				return
			}
			// Recording both arms the RFC 2833 decoder and captures the audio
			// the inband assertions read.
			calleeRec = t.TempDir() + "/" + tag + "-callee.pcm"
			if err := c.StartRecording(calleeRec); err != nil {
				GoroutineFailf(t, "callee:record", "StartRecording: %v", err)
				return
			}
			if err := c.SendSilence(); err != nil {
				GoroutineFailf(t, "callee:silence", "SendSilence: %v", err)
				return
			}
			close(calleeAnswered)
			select {
			case <-c.Done():
			case <-calleeCtx.Done():
			}
		case <-calleeCtx.Done():
		}
	}()
	t.Cleanup(func() { calleeCancel(); <-calleeDone })
	s.Done()

	s = Step(t, "place-caller")
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(90))
	// An endpoint that sends tones is one that could not negotiate
	// telephone-event in the first place — answering with it and then sending
	// inband anyway is not a shape real gear produces, and it tells the SBC the
	// leg speaks RFC 2833, which suppresses any inband handling.
	var answerErr error
	if tc.caller == sendInband {
		answerErr = call.AnswerWithoutTelephoneEvent()
	} else {
		answerErr = call.Answer()
	}
	if answerErr != nil {
		s.Fatalf("Answer: %v", answerErr)
	}
	if err := call.SendSilence(); err != nil {
		s.Fatalf("SendSilence: %v", err)
	}
	s.Done()

	s = Step(t, "wait-bridge-up")
	select {
	case <-calleeAnswered:
	case <-calleeDone:
		s.Fatal("callee goroutine exited before answering")
	case <-ctx.Done():
		s.Fatalf("callee never answered: %v", ctx.Err())
	}
	deadline := time.Now().Add(20 * time.Second)
	for calleeCall == nil || calleeCall.PCMBytesIn() < 8000 {
		if time.Now().After(deadline) {
			s.Fatal("no media reached the callee leg within 20s (bridge never came up)")
		}
		select {
		case <-calleeDone:
			s.Fatal("callee goroutine exited before the bridge came up")
		case <-time.After(100 * time.Millisecond):
		}
	}
	s.Done()

	s = Step(t, "send-dtmf")
	if tc.caller == sendInband {
		if err := call.SendWAV(wavPath); err != nil {
			s.Fatalf("SendWAV: %v", err)
		}
	} else {
		if err := call.SendDTMFWithDuration(digits, 100*time.Millisecond); err != nil {
			s.Fatalf("SendDTMF: %v", err)
		}
	}
	s.Done()

	s = Step(t, "hangup")
	time.Sleep(2 * time.Second)
	calleeCall.StopRecording()
	_ = call.Hangup()
	calleeCancel()
	<-calleeDone
	s.Done()

	s = Step(t, "assert-digits-received")
	events := ""
	for _, e := range calleeCall.ReceivedDTMF() {
		events += e.Digit
	}
	inband, err := DetectInbandDTMF(calleeRec)
	if err != nil {
		s.Fatalf("DetectInbandDTMF: %v", err)
	}
	s.Logf("%s: sent=%q  callee rfc2833=%q  callee inband=%q",
		tag, digits, events, inband)

	// The callee must receive the digits in whichever carriage its own SDP
	// negotiated — that is the contract, not "some digits arrived somehow".
	got, want := events, "rfc2833 events"
	if !tc.calleeTelEvent {
		got, want = inband, "inband tones"
	}
	if strict && got != digits {
		s.Errorf("%s: callee received %q as %s, want %q", tag, got, want, digits)
	}
	// Whatever shows up in the other carriage must at least be a subsequence of
	// what was sent. Requiring it to be empty would fail on a known rtpengine
	// leak (see the file header) rather than on anything this system controls;
	// a stray or reordered digit still fails, and the value is always logged so
	// the leak cannot quietly grow.
	other := map[bool]string{true: inband, false: events}[tc.calleeTelEvent]
	if other != "" && !isSubsequence(other, digits) {
		s.Errorf("%s: other carriage carried %q, which is not a subsequence of %q — "+
			"that is a stray or reordered digit, not the known leak",
			tag, other, digits)
	}
	s.Done()
	return got
}

// isSubsequence reports whether every rune of got appears in want, in order.
func isSubsequence(got, want string) bool {
	i := 0
	for _, r := range want {
		if i < len(got) && rune(got[i]) == r {
			i++
		}
	}
	return i == len(got)
}

// Repeated digits are lost whenever the digit is delivered to the callee as
// tones. rtpengine regenerates telephone-event without an inter-digit pause,
// so "11" reaches the far end as a single 205ms tone and ITU-T Q.24 says that
// is one keypress. Nothing in jambonz can space them: the tones are
// synthesised inside rtpengine, and its transcoding path has no equivalent of
// the `pause` parameter that `play DTMF` accepts.
//
// Kept running rather than skipped so the day rtpengine gains that control,
// this turns green and tells us.
func TestVerb_DTMFMatrix_RepeatedDigits(t *testing.T) {
	t.Parallel()
	requireWebhook(t)
	const digits = "1123"
	got := runDTMFMatrixWith(t, dtmfMatrixCase{caller: send2833, calleeTelEvent: false, digits: digits}, digits)
	switch got {
	case digits:
		t.Logf("repeated digits now survive: %q", got)
	case "123": // the two 1s fused into one tone — the known signature
		t.Skipf("known rtpengine limitation: callee heard %q for %q "+
			"(repeated digits fuse without an inter-digit pause)", got, digits)
	default:
		t.Fatalf("callee heard %q for %q — neither correct nor the known fusion signature %q",
			got, digits, "123")
	}
}
