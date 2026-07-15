//go:build drachtio

package drachtio

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"testing"
	"time"
)

// audioPortRe matches the port field of the audio m-line: "m=audio <port> ...".
var audioPortRe = regexp.MustCompile(`(?m)^m=audio (\d+) `)

// oVersionRe matches the session-version (6th) field of the o= line:
// "o=<user> <sess-id> <sess-version> IN IP4 <addr>".
var oVersionRe = regexp.MustCompile(`(?m)^(o=\S+ \S+ )(\d+)( )`)

// bumpMediaPort returns a copy of sdp with the audio port changed and the o=
// session-version incremented — i.e. an in-dialog re-INVITE that renegotiates
// media (RFC 3264 requires the version bump for a new offer). This mirrors a
// carrier moving its RTP port mid-call: same codecs, same direction, only the
// port (and mandatory version) differ.
func bumpMediaPort(sdp []byte) ([]byte, int, error) {
	m := audioPortRe.FindSubmatch(sdp)
	if m == nil {
		return nil, 0, fmt.Errorf("no audio m-line in SDP:\n%s", sdp)
	}
	oldPort, _ := strconv.Atoi(string(m[1]))
	// A different even port. The value need not be one we listen on: the bug is
	// in signalling (the far end rejects the re-INVITE in ~5ms before any RTP),
	// so we assert on the SIP response, not on media.
	newPort := oldPort + 1000
	if newPort%2 != 0 {
		newPort++
	}

	out := audioPortRe.ReplaceAll(sdp, []byte(fmt.Sprintf("m=audio %d ", newPort)))
	out = oVersionRe.ReplaceAllFunc(out, func(b []byte) []byte {
		g := oVersionRe.FindSubmatch(b)
		v, _ := strconv.Atoi(string(g[2]))
		return []byte(fmt.Sprintf("%s%d%s", g[1], v+1, g[3]))
	})
	return out, newPort, nil
}

// TestDrachtio_Reinvite_MediaPortChange — reproduces the production 488 seen
// when a carrier sends an in-dialog re-INVITE that only moves its RTP port
// (jambonz.org support case: account 5d47574c-2f39-4f7b-b33a-2c79f2f41848,
// transfer-to-queue fails). The feature-server handles the re-INVITE by calling
// ep.modify(offer) then res.send(200, {body: newSdp}); if the media endpoint
// renegotiation fails locally it returns 488 Not Acceptable Here in ~5ms and
// the transferred caller hears silence.
//
// This drives the same code path against the A-leg: place a UAC call to the
// answer+pause app (media anchored on the media server), then send a
// re-INVITE that changes only the audio port. A healthy cluster answers 200;
// the bug answers 488.
//
// Steps:
//  1. INVITE the inline answer+pause Application; assert 200.
//  2. Build a re-INVITE offer from the negotiated local SDP with only the
//     audio port (and mandatory o= version) changed.
//  3. Send it in-dialog and assert the final response is 200, not 488.
func TestDrachtio_Reinvite_MediaPortChange(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	uas := claimUAS(t, ctx)

	call, err := inviteApp(t, ctx, uas, nil)
	if err != nil {
		t.Fatalf("inviteApp: %v", err)
	}
	defer call.Hangup()

	if got := call.AnsweredStatus(); got != 200 {
		t.Fatalf("answered status: got %d want 200", got)
	}

	localSDP := call.LocalSDP()
	if len(localSDP) == 0 {
		t.Fatalf("no negotiated local SDP after answer")
	}

	offer, newPort, err := bumpMediaPort(localSDP)
	if err != nil {
		t.Fatalf("bumpMediaPort: %v", err)
	}
	t.Logf("sending media-changing re-INVITE (audio port -> %d):\n%s", newPort, offer)

	reCtx, reCancel := context.WithTimeout(ctx, 10*time.Second)
	defer reCancel()

	res, err := call.SendReinviteWithSDP(reCtx, offer, nil)
	if err != nil {
		t.Fatalf("SendReinviteWithSDP: %v", err)
	}
	if res == nil {
		t.Fatalf("SendReinviteWithSDP: no final response to re-INVITE")
	}

	t.Logf("re-INVITE final response: %d %s", res.StatusCode, res.Reason)
	if res.StatusCode == 488 {
		t.Fatalf("REPRODUCED: media-changing re-INVITE rejected with 488 Not Acceptable Here "+
			"(feature-server ep.modify renegotiation failed locally). body:\n%s", res.Body())
	}
	if res.StatusCode != 200 {
		t.Fatalf("media-changing re-INVITE: got %d %s, want 200", res.StatusCode, res.Reason)
	}
}
