package sip

import (
	"context"
	"errors"
	"fmt"
	"net"

	"github.com/emiago/diago"
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
func (s *Stack) Invite(ctx context.Context, dest string, opts InviteOptions) (*Call, error) {
	var destURI sip.Uri
	if err := sip.ParseUri(dest, &destURI); err != nil {
		return nil, fmt.Errorf("parse dest uri: %w", err)
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

	// Build sipgo headers from opts.Headers.
	var hdrs []sip.Header
	for k, v := range opts.Headers {
		hdrs = append(hdrs, sip.NewHeader(k, v))
	}

	dialog, err := s.dg.Invite(ctx, destURI, diago.InviteOptions{
		Transport: opts.Transport,
		Username:  opts.Username,
		Password:  opts.Password,
		Headers:   hdrs,
	})
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
	call := newOutboundCall(dialog)
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

	// Start a watcher that flips to StateEnded when the dialog terminates.
	go func() {
		<-ctx.Done()
		// best-effort: if dialog still active when ctx is cancelled, we
		// rely on the caller's Hangup; no extra action.
	}()
	return call, nil
}
