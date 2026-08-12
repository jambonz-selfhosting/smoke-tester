package provision

import (
	"testing"
	"time"
)

func TestShouldSweep(t *testing.T) {
	now := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	ttl := 2 * time.Hour
	iso := func(age time.Duration) string {
		return now.Add(-age).Format("2006-01-02T15:04:05.000Z")
	}
	const currentPrefix = "it-aabbccdd-"

	tests := []struct {
		name   string
		a      Account
		minAge time.Duration
		want   bool
	}{
		{
			name:   "non-it account never swept regardless of age",
			a:      Account{Name: "production", CreatedAt: iso(100 * time.Hour)},
			minAge: ttl,
			want:   false,
		},
		{
			name:   "current run's account never swept regardless of age",
			a:      Account{Name: "it-aabbccdd-rest", CreatedAt: iso(100 * time.Hour)},
			minAge: ttl,
			want:   false,
		},
		{
			name:   "other run's account younger than TTL is protected",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: iso(time.Minute)},
			minAge: ttl,
			want:   false,
		},
		{
			name:   "other run's account older than TTL is swept",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: iso(3 * time.Hour)},
			minAge: ttl,
			want:   true,
		},
		{
			name:   "age exactly at TTL is swept",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: iso(ttl)},
			minAge: ttl,
			want:   true,
		},
		{
			name:   "missing created_at is never swept when TTL active",
			a:      Account{Name: "it-11223344-verbs"},
			minAge: ttl,
			want:   false,
		},
		{
			name:   "unparseable created_at is never swept when TTL active",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: "yesterday"},
			minAge: ttl,
			want:   false,
		},
		{
			name:   "mysql dateStrings layout accepted",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: now.Add(-3 * time.Hour).Format("2006-01-02 15:04:05")},
			minAge: ttl,
			want:   true,
		},
		{
			name:   "zero TTL disables the age gate",
			a:      Account{Name: "it-11223344-verbs", CreatedAt: iso(time.Second)},
			minAge: 0,
			want:   true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldSweep(tc.a, currentPrefix, tc.minAge, now); got != tc.want {
				t.Errorf("shouldSweep(%q, created=%q, minAge=%v) = %v, want %v",
					tc.a.Name, tc.a.CreatedAt, tc.minAge, got, tc.want)
			}
		})
	}
}
