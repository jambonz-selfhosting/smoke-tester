package rest

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jambonz-selfhosting/smoke-tester/internal/provision"
)

// TestPhoneNumber_CRUD — provisions a PhoneNumber under a fresh VoipCarrier.
// Covers Tier 1 row 1.3.
//
// Steps:
//  1. create-voip-carrier — POST /VoipCarriers (cleanup via managed helper)
//  2. create-phone-number — POST /PhoneNumbers bound to that carrier
//  3. get-phone-number — assert number (digits-only) + voip_carrier_sid match
//  4. list-and-find — GET /PhoneNumbers confirms sid is in the list
func TestPhoneNumber_CRUD(t *testing.T) {
	ctx := WithTimeout(t, 30*time.Second)

	s := Step(t, "create-voip-carrier")
	carrierSID := client.ManagedVoipCarrier(t, ctx, provision.VoipCarrierCreate{
		Name:       provision.Name("carrier-for-pn"),
		AccountSID: suite.AccountSID,
	})
	s.Done()

	s = Step(t, "create-phone-number")
	// Use an obviously-test number. E.164 test range per RFC 5733 + 444/555 zone.
	// Include runID to avoid 422 "already in inventory" collisions.
	number := fmt.Sprintf("+1555%07d", int(time.Now().UnixNano()/1e6)%10000000)
	sid := client.ManagedPhoneNumber(t, ctx, provision.PhoneNumberCreate{
		Number:         number,
		VoipCarrierSID: carrierSID,
		AccountSID:     suite.AccountSID,
	})
	s.Logf("provisioned phone_number sid=%s number=%s", sid, number)
	s.Done()

	s = Step(t, "get-phone-number")
	got, err := client.GetPhoneNumber(ctx, sid)
	if err != nil {
		s.Fatalf("get phone_number: %v", err)
	}
	// jambonz normalises phone numbers — e.g. strips a leading '+'. Compare
	// digits only.
	wantDigits := strings.TrimPrefix(number, "+")
	gotDigits := strings.TrimPrefix(got.Number, "+")
	if gotDigits != wantDigits {
		s.Errorf("number mismatch: got %q want %q (digits %q vs %q)", got.Number, number, gotDigits, wantDigits)
	}
	if got.VoipCarrierSID != carrierSID {
		s.Errorf("voip_carrier_sid mismatch: got %q want %q", got.VoipCarrierSID, carrierSID)
	}
	s.Done()

	s = Step(t, "list-and-find")
	numbers, err := client.ListPhoneNumbers(ctx)
	if err != nil {
		s.Fatalf("list phone_numbers: %v", err)
	}
	var found bool
	for _, n := range numbers {
		if n.PhoneNumberSID == sid {
			found = true
			break
		}
	}
	if !found {
		s.Fatalf("list did not include sid %q (found %d numbers)", sid, len(numbers))
	}
	s.Done()
}
