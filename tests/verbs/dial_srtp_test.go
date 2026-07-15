//go:build manual_srtp

// Manual / opt-in test for the `dial` verb's srtpEncryption option over a
// sips:/TLS SIP URI. It is EXCLUDED from the default suite by the
// `manual_srtp` build tag (like the drachtio tests): `make test` /
// `go test ./tests/...` never compile it. Run it deliberately with:
//
//	make test-srtp \
//	  JAMBONZ_IT_SIP_TLS_DEST_DOMAIN=your-capture-host.example.com \
//	  JAMBONZ_IT_SIP_TLS_DEST_DOMAIN_PORT=5061
//
// Why manual: reaching a real SRTP/TLS callee that answers and bridges media
// isn't practical from the (NAT'd) harness. Instead this test points the dial
// at an EXTERNAL destination domain you control, so you can packet-capture the
// outbound leg on that host and confirm jambonz sends the call correctly:
// sips:/TLS signalling and an SRTP (RTP/SAVP + a=crypto) media offer. The
// destination is expected to be a passive capture point that does not answer,
// so the dial resolves as failed/no-answer — that is fine; the artifact is the
// pcap, not a completed bridge.
//
// The chain being exercised: dial srtpEncryption:"sdes" -> feature-server emits
// X-Jambonz-SRTP: sdes -> sbc-outbound forwards over TLS with an SDES SRTP
// offer to JAMBONZ_IT_SIP_TLS_DEST_DOMAIN.
package verbs

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/webhook"
)

func TestVerb_Dial_Sip_SRTP_TLS(t *testing.T) {
	requireWebhook(t)
	destDomain := os.Getenv("JAMBONZ_IT_SIP_TLS_DEST_DOMAIN")
	destPort := os.Getenv("JAMBONZ_IT_SIP_TLS_DEST_DOMAIN_PORT")
	if destDomain == "" || destPort == "" {
		t.Skip("JAMBONZ_IT_SIP_TLS_DEST_DOMAIN / JAMBONZ_IT_SIP_TLS_DEST_DOMAIN_PORT not set; " +
			"skipping manual SRTP/TLS send-out test")
	}
	ctx := WithTimeout(t, 90*time.Second)

	// Caller (A) leg: a normal registered UAS that jambonz INVITEs so the app
	// runs and executes the dial verb. We do not assert on bridged media — the
	// outbound (B) leg goes to the external capture destination.
	callerUAS := claimUAS(t, ctx)
	_, sess := claimSession(t)

	s := Step(t, "script-dial-to-sips-dest")
	actionURL := SessionURL(sess, "dial")
	// user part is arbitrary; the capture host sees whatever you set here.
	sipURI := fmt.Sprintf("sips:srtp-test@%s:%s;transport=tls", destDomain, destPort)
	sess.ScriptCallHook(WithWarmupScript(webhook.Script{
		V("dial",
			"target", []any{map[string]any{
				"type":   "sip",
				"sipUri": sipURI,
			}},
			// the option under test — request encrypted media (SDES) on the
			// outbound leg.
			"srtpEncryption", "sdes",
			"timeout", 15,
			"actionHook", actionURL),
		V("hangup"),
	}))
	SessionAckEmpty(sess, "dial")
	s.Logf("dialing %s (srtpEncryption=sdes) — capture the outbound INVITE on %s:%s",
		sipURI, destDomain, destPort)
	s.Done()

	s = Step(t, "place-caller-and-run-dial")
	// Answer the caller and hold it up while the dial attempts the outbound
	// leg. The capture destination is not expected to answer, so the dial
	// times out and the leg ends.
	call := placeWebhookCallTo(ctx, t, callerUAS, sess, withTimeLimit(60))
	AnswerRecordAndWaitEnded(s, ctx, call, WithSilence())
	s.Done()

	s = Step(t, "wait-action-dial-callback")
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cb, err := sess.WaitCallbackFor(waitCtx, "action/dial")
	if err != nil {
		// No callback means the dial verb never ran / never resolved — that's
		// a real failure of the send-out path, so fail loudly.
		s.Fatalf("no action/dial callback (dial verb did not run?): %v", err)
	}
	// Do NOT assert a completed bridge: the destination is a capture point.
	// The proof of correctness is the pcap on the destination host.
	s.Logf("action/dial: dial_call_status=%q dial_sip_status=%d",
		cb.String("dial_call_status"), cb.Int("dial_sip_status"))
	s.Logf("outbound INVITE sent — verify on %s:%s that it is sips:/TLS and the SDP "+
		"offers SRTP (RTP/SAVP + a=crypto)", destDomain, destPort)
	s.Done()
}
