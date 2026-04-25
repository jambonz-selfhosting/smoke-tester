// Tests for the `alert` verb.
//
// Schema: schemas/verbs/alert — emits a 180 Ringing with a custom
// Alert-Info header to the *caller of jambonz*.
//
// Why this test is deferred:
//
//   Our Phase-1 test shape is "test -> POST /Calls -> jambonz -> INVITE -> UAS".
//   In that shape jambonz is the *caller* of our UAS; any 180 it sends to our
//   UAS we could observe, but `alert` targets the caller-of-jambonz leg, not
//   ours. The originator of the call to jambonz here is the REST `POST /Calls`
//   request — REST doesn't surface SIP provisional responses to us.
//
//   The correct shape to verify `alert` end-to-end is UAC origination: our
//   harness places an outbound INVITE at a jambonz-served URI, jambonz
//   executes `alert`, and we (the UAC) observe the 180 with Alert-Info on
//   the wire. The SIP stack already has `diago.Invite` machinery (the
//   spike at spikes/001-sipgo-diago proved it); it just isn't wired into
//   tests/verbs/ yet. Tracked in HANDOFF.md.
//
// Once UAC origination is wired, this file should contain:
//
//   1. Provision a phone number / SIP URI routed to an application whose
//      call_hook returns [{alert message:"info=alert-internal"}, {hangup}].
//   2. UAC: place an INVITE to that URI.
//   3. Observe 180 Ringing arrive in call.Received(); assert
//      Received()[...].Headers["Alert-Info"] == "info=alert-internal".
//
// Skipped unconditionally for now.
package verbs

import "testing"

// TestVerb_Alert_Basic — skip stub. Once UAC origination lands, the real
// steps will be: provision-application → uac-invite → assert-180-ringing-alert-info.
func TestVerb_Alert_Basic(t *testing.T) {
	t.Skip("alert requires UAC origination — see file doc comment")
}
