package sip

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/emiago/diago"
	"github.com/emiago/diago/media/sdp"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// InviteRejected is returned by Stack.Invite when jambonz responded with a
// non-success final status (4xx/5xx/6xx). Used by tests that expect the
// UAS to reject the call — e.g. the `sip:decline` verb.
type InviteRejected struct {
	// StatusCode is the final SIP status we observed (e.g. 486, 603).
	StatusCode int
	// Reason is the SIP reason phrase from the response.
	Reason string
	// Response is the raw sipgo response for inspection (custom headers,
	// bodies, etc.). Nil if the error wasn't carrying one.
	Response *sip.Response
}

func (e *InviteRejected) Error() string {
	return fmt.Sprintf("invite rejected: %d %s", e.StatusCode, e.Reason)
}

// RejectedHeader returns the named header off the rejection response, or
// "" if absent. Convenience for tests.
func (e *InviteRejected) RejectedHeader(name string) string {
	if e.Response == nil {
		return ""
	}
	h := e.Response.GetHeader(name)
	if h == nil {
		return ""
	}
	return h.Value()
}

// InviteOptions controls a UAC INVITE.
type InviteOptions struct {
	Transport string // "tcp" (default) / "udp"
	FromUser  string // defaults to Stack cfg.User
	FromHost  string // defaults to Stack cfg.SIPDomain
	Username  string // digest username (defaults to Stack cfg.User)
	Password  string // digest password (defaults to Stack cfg.Pass)
	Headers   H      // custom request headers (e.g. X-Test-Id)
	PublicIP  net.IP // advertise this IP in SDP + Contact (optional)

	// SDPMode sets the direction attribute of the initial SDP offer:
	// "sendonly" / "recvonly" / "sendrecv" / "inactive". Empty keeps diago's
	// default (sendrecv). Used to emulate carriers that offer one-way media
	// on the first INVITE (e.g. Five9 sends a=sendonly). Anything else is
	// rejected by Invite before any SIP traffic is sent — diago never
	// validates the local mode, so a typo would land verbatim on the wire.
	//
	// ADR-0014 caveat: with "recvonly" or "inactive" diago's RTP writer
	// silently drops outbound packets, so SendSilence/SendWAV on such a call
	// sends nothing and the symmetric-RTP NAT pinhole never opens. Fine for
	// signaling-only tests; do not combine with media assertions.
	SDPMode string
}

// Invite places an outbound call to dest and returns a *Call already in
// StateAnswered (diago's Invite blocks until 200 OK). On failure, returns
// the error and a nil call.
//
// Typical use:
//
//	call, err := stack.Invite(ctx, "sip:echo@sip.jambonz.me", sip.InviteOptions{})
//	if err != nil { t.Fatal(err) }
//	defer call.Hangup()
//	call.StartRecording("/tmp/out.wav")
//	call.SendSilence()
//	<-call.Done()
//
// prepareInvite normalizes InviteOptions against the stack defaults and builds the
// sipgo header list. Shared by Invite and InviteEarlyMedia so the two cannot drift
// on defaults or validation.
func (s *Stack) prepareInvite(dest string, opts *InviteOptions) (sip.Uri, []sip.Header, error) {
	var destURI sip.Uri
	if err := sip.ParseUri(dest, &destURI); err != nil {
		return destURI, nil, fmt.Errorf("parse dest uri: %w", err)
	}
	if opts.Transport == "" {
		opts.Transport = s.cfg.Transport
		if opts.Transport == "" {
			opts.Transport = "tcp"
		}
	}
	if opts.Username == "" {
		opts.Username = s.cfg.User
	}
	if opts.Password == "" {
		opts.Password = s.cfg.Pass
	}
	switch opts.SDPMode {
	case "", sdp.ModeSendrecv, sdp.ModeSendonly, sdp.ModeRecvonly, sdp.ModeInactive:
	default:
		return destURI, nil, fmt.Errorf("invite: invalid SDPMode %q (want sendrecv/sendonly/recvonly/inactive)", opts.SDPMode)
	}
	var hdrs []sip.Header
	for k, v := range opts.Headers {
		hdrs = append(hdrs, sip.NewHeader(k, v))
	}
	return destURI, hdrs, nil
}

func (s *Stack) Invite(ctx context.Context, dest string, opts InviteOptions) (*Call, error) {
	destURI, hdrs, err := s.prepareInvite(dest, &opts)
	if err != nil {
		return nil, err
	}

	// This is diago.Diago.Invite's own NewDialog → Invite → Ack sequence,
	// inlined because Diago.Invite offers no hook between dialog creation and
	// the offer being built. The media session's Mode (default sendrecv) is
	// what lands in the initial SDP's a= line, and dialog.Invite generates
	// the offer body from the session — so setting Mode here is the only
	// extra step versus calling Diago.Invite directly.
	dialog, err := s.dg.NewDialog(destURI, diago.NewDialogOptions{Transport: opts.Transport})
	if err == nil { //nolint:nestif // inlined diago sequence, see comment above
		if opts.SDPMode != "" {
			dialog.MediaSession().Mode = opts.SDPMode
		}
		err = dialog.Invite(ctx, diago.InviteClientOptions{
			Username: opts.Username,
			Password: opts.Password,
			Headers:  hdrs,
		})
		if err == nil {
			err = dialog.Ack(ctx)
		}
		if err != nil {
			err = errors.Join(err, dialog.Close())
		}
	}
	if err != nil {
		// Non-2xx final responses from jambonz surface as ErrDialogResponse.
		// Diago returns it as both value and pointer depending on site, so
		// try both. Convert to our typed error so tests can assert status
		// + reason + headers.
		var res *sip.Response
		var ptrErr *sipgo.ErrDialogResponse
		var valErr sipgo.ErrDialogResponse
		switch {
		case errors.As(err, &ptrErr) && ptrErr.Res != nil:
			res = ptrErr.Res
		case errors.As(err, &valErr) && valErr.Res != nil:
			res = valErr.Res
		}
		if res != nil {
			return nil, &InviteRejected{
				StatusCode: int(res.StatusCode),
				Reason:     res.Reason,
				Response:   res,
			}
		}
		return nil, fmt.Errorf("invite: %w", err)
	}
	call := newOutboundCall(dialog, s.cfg.Owner)
	// Register with the stack immediately so Stop() can drain this call if
	// the test never gets around to Hangup (e.g. t.Fatalf mid-call).
	s.track(call)
	// The dialog is already in the "answered" state after a blocking Invite.
	call.setState(StateAnswered, "")
	// Record the final 2xx response from the peer so tests can assert on
	// status / reason / custom headers via call.Received(). InviteResponse
	// is set by diago after the dialog is established; treat it as
	// best-effort (nil is unlikely here but guard anyway).
	if dialog.InviteResponse != nil {
		call.recordReceived(newResponseMsg(MsgRecv, dialog.InviteResponse))
	}
	// Capture negotiated codec.
	props := diago.MediaProps{}
	_, _ = dialog.AudioReader(diago.WithAudioReaderMediaProps(&props))
	call.mediaMu.Lock()
	call.codec = props.Codec.Name
	call.mediaMu.Unlock()

	return call, nil
}
