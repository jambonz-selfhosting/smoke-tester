// Black-box tests for the pure-logic helpers sip.ParseSessionExpires and
// sip.Message.Header. Both are exercised purely through the exported surface
// of package sip; no implementation details are relied upon.
package sip_test

import (
	"testing"

	"github.com/jambonz-selfhosting/smoke-tester/internal/sip"
)

func TestParseSessionExpires(t *testing.T) {
	cases := []struct {
		name          string
		in            string
		wantDelta     int
		wantRefresher string
		wantErr       bool
	}{
		{"delta only", "90", 90, "", false},
		{"delta with refresher uas", "90;refresher=uas", 90, "uas", false},
		{"delta with refresher uppercase lowercased", "90; refresher=UAC", 90, "uac", false},
		{"spaces around semicolon and equals", "90 ; refresher = uas", 90, "uas", false},
		{"larger delta with refresher uac", "1800;refresher=uac", 1800, "uac", false},
		{"refresher uppercase UAS lowercased", "90;refresher=UAS", 90, "uas", false},
		{"unknown param ignored, refresher empty", "90;foo=bar", 90, "", false},
		{"empty string is error", "", 0, "", true},
		{"non-numeric delta is error", "abc", 0, "", true},
		{"non-integer delta is error", "90.5", 0, "", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotDelta, gotRefresher, err := sip.ParseSessionExpires(tc.in)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("ParseSessionExpires(%q): expected error, got nil (delta=%d, refresher=%q)", tc.in, gotDelta, gotRefresher)
				}
				if gotDelta != 0 {
					t.Errorf("ParseSessionExpires(%q): delta = %d, want 0 on error", tc.in, gotDelta)
				}
				return
			}

			if err != nil {
				t.Fatalf("ParseSessionExpires(%q): unexpected error: %v", tc.in, err)
			}
			if gotDelta != tc.wantDelta {
				t.Errorf("ParseSessionExpires(%q): delta = %d, want %d", tc.in, gotDelta, tc.wantDelta)
			}
			if gotRefresher != tc.wantRefresher {
				t.Errorf("ParseSessionExpires(%q): refresher = %q, want %q", tc.in, gotRefresher, tc.wantRefresher)
			}
		})
	}
}

func TestMessageHeader(t *testing.T) {
	msg := sip.Message{
		Headers: sip.H{
			"Session-Expires": "90;refresher=uas",
		},
	}

	cases := []struct {
		name   string
		lookup string
		want   string
	}{
		{"exact case", "Session-Expires", "90;refresher=uas"},
		{"lowercase lookup", "session-expires", "90;refresher=uas"},
		{"uppercase lookup", "SESSION-EXPIRES", "90;refresher=uas"},
		{"absent key returns empty", "Contact", ""},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := msg.Header(tc.lookup)
			if got != tc.want {
				t.Errorf("Message.Header(%q) = %q, want %q", tc.lookup, got, tc.want)
			}
		})
	}
}
