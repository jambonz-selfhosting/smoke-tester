// X-VoipMonitor-norecord on each response class, one subtest per case so each can
// be run and packet-captured alone. Not parallel: it flips the account flag and SP policy.
package verbs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/emiago/sipgo/sip"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
	jsip "github.com/jambonz-selfhosting/smoke-tester/internal/sip"
	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

// TestMediaCapture_Responses — for each capture mode, each subtest drives one
// response class in one direction and asserts the header on the public leg.
//
//	go test ./tests/verbs/ -run 'TestMediaCapture_Responses/^disable_all$/^inbound_4xx$'
//
// Steps:
//  1. read-initial-policy — remember the SP policy for cleanup
//  2. provision-application — webhook Application the UAC dials
//  3. (per mode) set-flags — account flag + SP policy
//  4. (per case) drive the call, then assert-header on what the harness received
func TestMediaCapture_Responses(t *testing.T) {
	requireWebhook(t)
	if spClient == nil {
		t.Skip("SP scope not configured (JAMBONZ_SP_API_KEY / JAMBONZ_SP_SID)")
	}
	requireMediaCapture(t)
	ctx := WithTimeout(t, 900*time.Second)
	uas := claimUAS(t, ctx)

	s := Step(t, "read-initial-policy")
	sp, err := spClient.GetServiceProvider(ctx, cfg.SPSID)
	if err != nil {
		s.Fatalf("get service provider: %v", err)
	}
	spInitial := sp.SupportAudioCapturePolicy
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		off := false
		if err := client.UpdateAccount(cctx, suite.AccountSID, provision.AccountUpdate{DisableMediaCapture: &off}); err != nil {
			t.Logf("cleanup: restore account disable_media_capture: %v", err)
		}
		if cfg.AllowSPPolicyChanges {
			if err := spClient.UpdateServiceProvider(cctx, cfg.SPSID,
				provision.ServiceProviderUpdate{SupportAudioCapturePolicy: spInitial}); err != nil {
				t.Logf("cleanup: restore SP support_audio_capture_policy: %v", err)
			}
		}
	})
	s.Done()

	s = Step(t, "provision-application")
	appSID := provisionWebhookApp(t, ctx, "media-capture-resp")
	s.Done()

	for _, mode := range []struct {
		name   string
		policy string
		acc    bool
		want   bool
	}{
		{"account_optout", "per_account", true, true},
		{"disable_all", "disable_all", false, true},
		{"allow_all_overrides_account", "allow_all", true, false},
		{"default", "per_account", false, false},
	} {
		mode := mode
		t.Run(mode.name, func(t *testing.T) {
			if mode.policy != spInitial && !cfg.AllowSPPolicyChanges {
				t.Skipf("needs JAMBONZ_ALLOW_SP_POLICY_CHANGES=true to set the SP policy to %s", mode.policy)
			}
			s := Step(t, "set-flags")
			if err := client.UpdateAccount(ctx, suite.AccountSID,
				provision.AccountUpdate{DisableMediaCapture: &mode.acc}); err != nil {
				s.Fatalf("PUT disable_media_capture=%v: %v", mode.acc, err)
			}
			if cfg.AllowSPPolicyChanges {
				if err := spClient.UpdateServiceProvider(ctx, cfg.SPSID,
					provision.ServiceProviderUpdate{SupportAudioCapturePolicy: mode.policy}); err != nil {
					s.Fatalf("PUT support_audio_capture_policy=%s: %v", mode.policy, err)
				}
			}
			s.Done()
			runResponseCases(t, uas, appSID, mode.want)
		})
	}
}

func runResponseCases(t *testing.T, uas *UAS, appSID string, want bool) {
	/* inbound: UAC → sbc-inbound → feature-server; the header must be on the
	   responses sbc-inbound sends back to the UAC */
	t.Run("inbound_1xx", func(t *testing.T) {
		ctx := WithTimeout(t, 45*time.Second)
		testID, sess := claimSession(t)
		sess.ScriptCallHook(webhook.Script{
			V("say", "text", "Early media for the audio capture opt out test, please keep listening.",
				"earlyMedia", true),
		})
		s := Step(t, "invite-and-await-183")
		pending, err := uas.Stack.InviteEarlyMedia(ctx, appURI(appSID), inviteOpts(uas, testID))
		if err != nil {
			s.Fatalf("InviteEarlyMedia: %v", err)
		}
		t.Cleanup(func() { _ = pending.Close() })
		s.Done()
		s = Step(t, "assert-header")
		assertNoRecord(s, pending.EarlyResponse(), want)
		s.Done()
		s = Step(t, "cancel")
		if err := pending.CancelWithHeaders(ctx); err != nil {
			s.Fatalf("Cancel: %v", err)
		}
		s.Done()
	})

	t.Run("inbound_2xx", func(t *testing.T) {
		ctx := WithTimeout(t, 45*time.Second)
		testID, sess := claimSession(t)
		sess.ScriptCallHook(webhook.Script{V("answer"), V("pause", "length", 1), V("hangup")})
		s := Step(t, "invite-and-answer")
		call, err := inviteApp(ctx, uas, appSID, testID)
		if err != nil {
			s.Fatalf("Invite: %v", err)
		}
		t.Cleanup(func() { _ = call.Hangup() })
		if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
			s.Fatalf("wait end: %v", err)
		}
		s.Done()
		s = Step(t, "assert-header")
		m, ok := call.AnsweredResponse()
		if !ok {
			s.Fatalf("no 200 OK recorded")
		}
		assertNoRecord(s, m.RawResponse, want)
		s.Done()
	})

	for _, tc := range []struct {
		name   string
		status int
		reason string
	}{
		{"inbound_4xx", 486, "Busy Here"},
		{"inbound_5xx", 503, "Service Unavailable"},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithTimeout(t, 45*time.Second)
			testID, sess := claimSession(t)
			sess.ScriptCallHook(webhook.Script{V("sip:decline", "status", tc.status, "reason", tc.reason)})
			s := Step(t, "invite-and-get-rejected")
			_, err := inviteApp(ctx, uas, appSID, testID)
			var rej *jsip.InviteRejected
			if !errors.As(err, &rej) {
				s.Fatalf("want a %d rejection, got %v", tc.status, err)
			}
			if rej.StatusCode != tc.status {
				s.Errorf("final status %d, want %d", rej.StatusCode, tc.status)
			}
			s.Done()
			s = Step(t, "assert-header")
			assertNoRecord(s, rej.Response, want)
			s.Done()
		})
	}

	/* outbound: POST /Calls → feature-server → sbc-outbound → UAS; the header
	   must be on the INVITE sbc-outbound sends to the UAS */
	for _, tc := range []struct {
		name   string
		answer func(c *jsip.Call) error
	}{
		{"outbound_1xx", func(c *jsip.Call) error {
			if err := c.Ringing(); err != nil {
				return err
			}
			sdp, err := jsip.EarlyMediaSDP("PCMU", earlyMediaSDPHost, earlyMediaSDPPort, 1000)
			if err != nil {
				return err
			}
			if err := c.SendEarlyMedia183(sdp); err != nil {
				return err
			}
			time.Sleep(500 * time.Millisecond)
			return c.Answer()
		}},
		{"outbound_2xx", func(c *jsip.Call) error { return c.Answer() }},
		{"outbound_4xx", func(c *jsip.Call) error { return c.Reject(486, "Busy Here") }},
		{"outbound_5xx", func(c *jsip.Call) error { return c.Reject(503, "Service Unavailable") }},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := WithTimeout(t, 45*time.Second)
			_, sess := claimSession(t)
			sess.ScriptCallHook(webhook.Script{V("pause", "length", 1), V("hangup")})
			s := Step(t, "place-call")
			call := placeWebhookCallTo(ctx, t, uas, sess, withTimeLimit(20))
			s.Done()
			s = Step(t, "assert-header")
			got := call.Header(noRecordHeader)
			s.Logf("INVITE to callee: %s=%q", noRecordHeader, got)
			if (got == "1") != want {
				s.Errorf("INVITE to callee: %s=%q, want header=%v", noRecordHeader, got, want)
			}
			s.Done()
			s = Step(t, "respond")
			if err := tc.answer(call); err != nil {
				s.Fatalf("respond: %v", err)
			}
			if call.State() == jsip.StateAnswered {
				if err := call.SendSilence(); err != nil {
					s.Fatalf("SendSilence: %v", err)
				}
				if err := call.WaitState(ctx, jsip.StateEnded); err != nil {
					s.Fatalf("wait end: %v", err)
				}
			}
			s.Done()
		})
	}
}

func appURI(appSID string) string { return "sip:app-" + appSID + "@" + suite.SIPRealm }

func assertNoRecord(s *StepCtx, res *sip.Response, want bool) {
	if res == nil {
		s.Fatalf("no response to inspect")
	}
	got := ""
	if h := res.GetHeader(noRecordHeader); h != nil {
		got = h.Value()
	}
	s.Logf("%d %s: %s=%q", res.StatusCode, res.Reason, noRecordHeader, got)
	if (got == "1") != want {
		s.Errorf("%d %s: %s=%q, want header=%v", res.StatusCode, res.Reason, noRecordHeader, got, want)
	}
}
