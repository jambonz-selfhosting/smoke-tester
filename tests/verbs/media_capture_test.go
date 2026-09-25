// X-VoipMonitor-norecord for accounts opted out of audio capture. Not parallel: it
// flips the suite account and SP flags (restored in Cleanup).
package verbs

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

const noRecordHeader = "X-VoipMonitor-norecord"

// TestMediaCapture_NoRecordHeader — for each combination of SP flag and
// account flag, place one inbound call (UAC → SBC) and one outbound call
// (POST /Calls → UAS) and assert the header is present exactly when either
// flag is set.
//
// Steps:
//  1. read-initial-flags — remember the SP's current flag for cleanup
//  2. provision-application — webhook Application the UAC dials
//  3. set-flags — PUT account (account scope) + PUT SP (SP scope)
//  4. assert-derived-field — GET /Accounts/{sid} reflects both flags
//  5. inbound-call — UAC INVITE sip:app-<sid>@realm, [answer, pause 1s, hangup]
//  6. assert-inbound-header — 18x/200 from the SBC carry the header iff opted out
//  7. outbound-call — POST /Calls to the UAS, [pause 1s, hangup]
//  8. assert-outbound-header — INVITE from the SBC carries the header iff opted out
//
// Test     --PUT disable_media_capture-->                  api-server
// Test     --INVITE sip:app-<sid>@realm-->                  SBC(inbound)
// SBC      --200 OK [X-VoipMonitor-norecord: 1]-->          UAC   // assert
// Test     --POST /Calls to=uas-->                          api-server
// SBC      --INVITE [X-VoipMonitor-norecord: 1]-->          UAS   // assert
func TestMediaCapture_NoRecordHeader(t *testing.T) {
	requireWebhook(t)
	if spClient == nil {
		t.Skip("SP scope not configured (JAMBONZ_SP_API_KEY / JAMBONZ_SP_SID)")
	}
	ctx := WithTimeout(t, 240*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "read-initial-flags")
	sp, err := spClient.GetServiceProvider(ctx, cfg.SPSID)
	if err != nil {
		s.Fatalf("get service provider: %v", err)
	}
	spInitial := bool(sp.DisableMediaCapture)
	s.Logf("service provider disable_media_capture=%v", spInitial)
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := spClient.UpdateServiceProvider(cctx, cfg.SPSID,
			provision.ServiceProviderUpdate{DisableMediaCapture: &spInitial}); err != nil {
			t.Logf("cleanup: restore SP disable_media_capture: %v", err)
		}
		off := false
		if err := client.UpdateAccount(cctx, suite.AccountSID,
			provision.AccountUpdate{DisableMediaCapture: &off}); err != nil {
			t.Logf("cleanup: restore account disable_media_capture: %v", err)
		}
	})
	s.Done()

	s = Step(t, "provision-application")
	appSID := provisionWebhookApp(t, ctx, "media-capture-app")
	s.Done()

	cases := []struct{ sp, acc bool }{
		{false, false},
		{false, true},
		{true, false},
		{true, true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(fmt.Sprintf("sp_%s_account_%s", onOff(tc.sp), onOff(tc.acc)), func(t *testing.T) {
			want := tc.sp || tc.acc
			ctx := WithTimeout(t, 60*time.Second)

			s := Step(t, "set-flags")
			if err := client.UpdateAccount(ctx, suite.AccountSID,
				provision.AccountUpdate{DisableMediaCapture: &tc.acc}); err != nil {
				s.Fatalf("account-scope PUT disable_media_capture=%v: %v", tc.acc, err)
			}
			if err := spClient.UpdateServiceProvider(ctx, cfg.SPSID,
				provision.ServiceProviderUpdate{DisableMediaCapture: &tc.sp}); err != nil {
				s.Fatalf("SP-scope PUT disable_media_capture=%v: %v", tc.sp, err)
			}
			s.Done()

			s = Step(t, "assert-derived-field")
			acct, err := client.GetAccount(ctx, suite.AccountSID)
			if err != nil {
				s.Fatalf("get account: %v", err)
			}
			if bool(acct.DisableMediaCapture) != tc.acc {
				s.Errorf("disable_media_capture: got %v want %v", acct.DisableMediaCapture, tc.acc)
			}
			if acct.ServiceProviderDisableMediaCapture != tc.sp {
				s.Errorf("service_provider_disable_media_capture: got %v want %v",
					acct.ServiceProviderDisableMediaCapture, tc.sp)
			}
			s.Done()

			testID, sess := claimSession(t)

			s = Step(t, "inbound-call")
			sess.ScriptCallHook(webhook.Script{
				V("answer"),
				V("pause", "length", 1),
				V("hangup"),
			})
			dest := fmt.Sprintf("sip:app-%s@%s", appSID, suite.SIPRealm)
			in, err := uas.Stack.Invite(ctx, dest, jsip.InviteOptions{
				Transport: "tcp",
				FromUser:  uas.Username,
				Username:  uas.Username,
				Password:  uas.Password,
				Headers:   jsip.H{webhook.CorrelationHeader: testID},
			})
			if err != nil {
				s.Fatalf("Invite: %v", err)
			}
			t.Cleanup(func() { _ = in.Hangup() })
			if err := in.WaitState(ctx, jsip.StateEnded); err != nil {
				s.Fatalf("wait end: %v", err)
			}
			s.Done()

			s = Step(t, "assert-inbound-header")
			if _, ok := in.AnsweredResponse(); !ok {
				s.Fatalf("inbound call was never answered")
			}
			for _, m := range in.Received() {
				if m.Method != "INVITE" || m.StatusCode <= 100 || m.StatusCode > 200 {
					continue
				}
				got := m.Header(noRecordHeader)
				s.Logf("%d to INVITE: %s=%q", m.StatusCode, noRecordHeader, got)
				if want && got != "1" {
					s.Errorf("%d to INVITE: %s=%q, want \"1\"", m.StatusCode, noRecordHeader, got)
				}
				if !want && got != "" {
					s.Errorf("%d to INVITE: unexpected %s=%q", m.StatusCode, noRecordHeader, got)
				}
			}
			s.Done()

			s = Step(t, "outbound-call")
			sess.ScriptCallHook(webhook.Script{
				V("pause", "length", 1),
				V("hangup"),
			})
			out := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(30))
			got := out.Header(noRecordHeader)
			AnswerRecordAndWaitEnded(s, ctx, out, WithSilence())
			s.Done()

			s = Step(t, "assert-outbound-header")
			s.Logf("INVITE to callee: %s=%q", noRecordHeader, got)
			if want && got != "1" {
				s.Errorf("INVITE to callee: %s=%q, want \"1\"", noRecordHeader, got)
			}
			if !want && got != "" {
				s.Errorf("INVITE to callee: unexpected %s=%q", noRecordHeader, got)
			}
			s.Done()
		})
	}
}

func onOff(optedOut bool) string {
	if optedOut {
		return "optout"
	}
	return "optin"
}
