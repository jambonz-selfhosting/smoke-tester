package sip

import (
	"context"
	"errors"
	"fmt"

	"github.com/emiago/diago"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

// PendingInvite is an INVITE that reached early media and has NOT been answered,
// so its transaction is still open and it can still be CANCELled.
//
// This exists to put an RFC 3326 Reason header on the teardown of a call that was
// never answered. Such a call ends with a CANCEL, not a BYE, and the status event
// jambonz emits for it is entirely self-generated - 487 with a hardcoded
// 'Request Terminated' - so without the header every abandoned inbound call looks
// identical. A Reason: SIP;cause=200;text="Call completed elsewhere" is what
// separates a forked branch losing the race from a caller who simply gave up.
type PendingInvite struct {
	dialog *diago.DialogClientSession
	early  *sip.Response
}

// InviteEarlyMedia sends an INVITE and stops as soon as the peer answers with early
// media (a 18x carrying SDP), leaving the INVITE transaction open so the caller can
// CANCEL it. Use with a jambonz app whose first verb sets earlyMedia (say/play):
// the feature server then sends 183 + SDP and streams audio WITHOUT answering.
//
// Returns an error if the peer answered outright (nothing left to cancel), rejected
// the call (*InviteRejected), or never produced early media before ctx expired.
func (s *Stack) InviteEarlyMedia(ctx context.Context, dest string, opts InviteOptions) (*PendingInvite, error) {
	destURI, hdrs, err := s.prepareInvite(dest, &opts)
	if err != nil {
		return nil, err
	}
	dialog, err := s.dg.NewDialog(destURI, diago.NewDialogOptions{Transport: opts.Transport})
	if err != nil {
		return nil, fmt.Errorf("inviteEarlyMedia: new dialog: %w", err)
	}
	if opts.SDPMode != "" {
		dialog.MediaSession().Mode = opts.SDPMode
	}

	var early *sip.Response
	err = dialog.Invite(ctx, diago.InviteClientOptions{
		Username: opts.Username,
		Password: opts.Password,
		Headers:  hdrs,
		/* stop and hand control back on early media rather than blocking to the
		   final response, which is the whole point here */
		EarlyMediaDetect: true,
		OnResponse: func(res *sip.Response) error {
			if res.IsProvisional() && len(res.Body()) > 0 {
				early = res
			}
			return nil
		},
	})
	switch {
	case errors.Is(err, diago.ErrClientEarlyMedia):
		// expected: 18x with SDP, transaction still open
	case err == nil:
		// answered outright - no CANCEL is possible, so this is a setup failure
		_ = dialog.Close()
		return nil, fmt.Errorf("inviteEarlyMedia: call was answered, expected early media " +
			"(is earlyMedia set on the first verb?)")
	default:
		_ = dialog.Close()
		var ptrErr *sipgo.ErrDialogResponse
		var valErr sipgo.ErrDialogResponse
		var res *sip.Response
		switch {
		case errors.As(err, &ptrErr) && ptrErr.Res != nil:
			res = ptrErr.Res
		case errors.As(err, &valErr) && valErr.Res != nil:
			res = valErr.Res
		}
		if res != nil {
			return nil, &InviteRejected{StatusCode: int(res.StatusCode), Reason: res.Reason, Response: res}
		}
		return nil, fmt.Errorf("inviteEarlyMedia: %w", err)
	}
	return &PendingInvite{dialog: dialog, early: early}, nil
}

// EarlyResponse returns the provisional response that carried early media.
func (p *PendingInvite) EarlyResponse() *sip.Response { return p.early }

// CancelWithHeaders CANCELs the pending INVITE, carrying extra headers.
//
// diago and sipgo only generate a CANCEL implicitly, from context cancellation, and
// give no way to add headers to it - so the request is built here. The header set
// mirrors sipgo's own newCancelRequest (RFC 3261 §9.1: same Request-URI, From, To,
// Call-ID and Route set, and ONLY the top Via). CSeq is deliberately not set: the
// transaction layer skips its own CSeq handling for CANCEL and reuses the INVITE's,
// which is what the RFC requires.
func (p *PendingInvite) CancelWithHeaders(ctx context.Context, extra ...sip.Header) error {
	invite := p.dialog.InviteRequest
	cancelReq := sip.NewRequest(sip.CANCEL, invite.Recipient)
	cancelReq.AppendHeader(sip.HeaderClone(invite.Via()))
	cancelReq.AppendHeader(sip.HeaderClone(invite.From()))
	cancelReq.AppendHeader(sip.HeaderClone(invite.To()))
	cancelReq.AppendHeader(sip.HeaderClone(invite.CallID()))
	sip.CopyHeaders("Route", invite, cancelReq)
	cancelReq.SetSource(invite.Source())
	cancelReq.Laddr = invite.Laddr
	for _, h := range extra {
		cancelReq.AppendHeader(h)
	}

	res, err := p.dialog.Do(ctx, cancelReq)
	if err != nil {
		return fmt.Errorf("cancelWithHeaders: %w", err)
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("cancelWithHeaders: CANCEL got %d %s", res.StatusCode, res.Reason)
	}
	return nil
}

// Close releases the dialog. Safe to call after CancelWithHeaders.
func (p *PendingInvite) Close() error { return p.dialog.Close() }
