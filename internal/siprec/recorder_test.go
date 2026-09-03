package siprec

import (
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

const offerSDP = "v=0\r\n" +
	"o=- 1 1 IN IP4 127.0.0.1\r\ns=rtpengine\r\nc=IN IP4 127.0.0.1\r\nt=0 0\r\n" +
	"m=audio 30000 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=sendonly\r\na=label:1\r\n" +
	"m=audio 30002 RTP/AVP 0 101\r\na=rtpmap:0 PCMU/8000\r\na=sendonly\r\na=label:2\r\n"

func siprecInvite(callID string, cseq int) string {
	body := "--uniqueBoundary\r\nContent-Type: application/sdp\r\n\r\n" + offerSDP +
		"--uniqueBoundary\r\nContent-Type: application/rs-metadata+xml\r\n\r\n<recording/>\r\n" +
		"--uniqueBoundary--\r\n"
	return fmt.Sprintf("INVITE sip:srs@127.0.0.1 SIP/2.0\r\n"+
		"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK-%d\r\n"+
		"From: <sip:sbc@127.0.0.1>;tag=abc\r\nTo: <sip:srs@127.0.0.1>\r\n"+
		"Call-ID: %s\r\nCSeq: %d INVITE\r\nRequire: siprec\r\n"+
		"Content-Type: multipart/mixed;boundary=uniqueBoundary\r\nContent-Length: %d\r\n\r\n%s",
		cseq, callID, cseq, len(body), body)
}

// The recorder has to answer a SIPREC INVITE with one recvonly stream per forked
// leg, capture what arrives on them, and tell a re-INVITE apart from a second
// session - everything the transfer test then asserts on.
func TestRecorder_AnswersAndCapturesBothStreams(t *testing.T) {
	dir := t.TempDir()
	r := New(net.ParseIP("127.0.0.1"), 25093, 41500, dir)
	if err := r.Start(); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer r.Stop()

	uac, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 25093})
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer uac.Close()

	if _, err := uac.Write([]byte(siprecInvite("selftest-1", 1))); err != nil {
		t.Fatalf("write invite: %v", err)
	}
	buf := make([]byte, 4096)
	_ = uac.SetReadDeadline(time.Now().Add(2 * time.Second))
	n, err := uac.Read(buf)
	if err != nil {
		t.Fatalf("read answer: %v", err)
	}
	answer := string(buf[:n])
	if !strings.HasPrefix(answer, "SIP/2.0 200 OK") {
		t.Fatalf("answer status: %q", strings.SplitN(answer, "\r\n", 2)[0])
	}
	for _, want := range []string{"m=audio 41500", "m=audio 41502", "a=label:1", "a=label:2", "a=recvonly"} {
		if !strings.Contains(answer, want) {
			t.Errorf("answer missing %q:\n%s", want, answer)
		}
	}

	start := time.Now()
	for _, port := range []int{41500, 41502} {
		c, err := net.DialUDP("udp4", nil, &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: port})
		if err != nil {
			t.Fatalf("dial rtp: %v", err)
		}
		pkt := make([]byte, rtpHdrBytes+160)
		for i := range pkt[rtpHdrBytes:] {
			pkt[rtpHdrBytes+i] = 0x7f
		}
		for i := 0; i < 50; i++ {
			if _, err := c.Write(pkt); err != nil {
				t.Fatalf("write rtp: %v", err)
			}
			time.Sleep(2 * time.Millisecond)
		}
		c.Close()
	}
	time.Sleep(300 * time.Millisecond)

	if got := r.TotalPackets(start, time.Now()); got != 100 {
		t.Errorf("packets: got %d want 100", got)
	}
	for _, label := range []string{"1", "2"} {
		info, err := os.Stat(r.StreamPath(label))
		if err != nil {
			t.Fatalf("stat capture %s: %v", label, err)
		}
		if info.Size() != 50*160 {
			t.Errorf("capture %s: got %d bytes want %d", label, info.Size(), 50*160)
		}
	}

	// a re-INVITE is the SBC re-negotiating, not a second recording
	if _, err := uac.Write([]byte(siprecInvite("selftest-1", 2))); err != nil {
		t.Fatalf("write reinvite: %v", err)
	}
	time.Sleep(200 * time.Millisecond)
	if _, err := uac.Write([]byte("BYE sip:srs@127.0.0.1 SIP/2.0\r\n" +
		"Via: SIP/2.0/UDP 127.0.0.1:5060;branch=z9hG4bK-9\r\n" +
		"From: <sip:sbc@127.0.0.1>;tag=abc\r\nTo: <sip:srs@127.0.0.1>;tag=x\r\n" +
		"Call-ID: selftest-1\r\nCSeq: 3 BYE\r\nContent-Length: 0\r\n\r\n")); err != nil {
		t.Fatalf("write bye: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	var kinds []string
	for _, e := range r.Events() {
		kinds = append(kinds, e.Kind)
	}
	if got := strings.Join(kinds, ","); got != "invite,reinvite,bye" {
		t.Errorf("events: got %q want %q", got, "invite,reinvite,bye")
	}
	if r.Count("extra-invite") != 0 {
		t.Errorf("re-INVITE was counted as a second session")
	}
}

func bucketsAt(label string, base time.Time, offsets ...time.Duration) *stream {
	s := &stream{label: label}
	for _, o := range offsets {
		s.buckets = append(s.buckets, struct {
			t time.Time
			n int
		}{base.Add(o), 10})
	}
	return s
}

// A gap in the middle of a window is what "the recording went silent" looks
// like, so LongestGap has to find it rather than average it away.
func TestRecorder_LongestGapFindsSilence(t *testing.T) {
	r := New(net.ParseIP("127.0.0.1"), 25094, 41600, t.TempDir())
	base := time.Now().Truncate(bucket)
	r.streams = []*stream{bucketsAt("1", base, 0, 250*time.Millisecond, 3*time.Second, 3250*time.Millisecond)}

	got := r.LongestGap(base, base.Add(4*time.Second))
	if got < 2*time.Second || got > 3*time.Second {
		t.Errorf("longest gap: got %v want ~2.5s", got)
	}
}

// One side of a call can be quiet for the whole window - an agent on hold, or a
// one-directional fork - while the recording is perfectly healthy. Measuring
// each stream separately reported the entire window as silence and failed the
// transfer test for no reason.
func TestRecorder_LongestGapIgnoresAQuietStream(t *testing.T) {
	r := New(net.ParseIP("127.0.0.1"), 25095, 41700, t.TempDir())
	base := time.Now().Truncate(bucket)
	var offsets []time.Duration
	for d := time.Duration(0); d < 4*time.Second; d += 250 * time.Millisecond {
		offsets = append(offsets, d)
	}
	r.streams = []*stream{bucketsAt("1", base, offsets...), bucketsAt("2", base)}
	r.streams[1].buckets = nil // stream 2 never received a packet

	if got := r.LongestGap(base, base.Add(4*time.Second)); got > 500*time.Millisecond {
		t.Errorf("longest gap with one busy stream: got %v want ~0", got)
	}
}
