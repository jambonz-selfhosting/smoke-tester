package verbs

import (
	"strings"
	"testing"
)

// The echo derivation is load-bearing: it is what proves the agent spoke OUR
// tool output. If it silently produced nothing the round-trip assertion would
// be vacuous, and if it produced wrong wording the test would always fail. Both
// failure modes are cheap to pin here, without a call.
func TestSpellInt(t *testing.T) {
	cases := map[int]string{
		0: "zero", 5: "five", 13: "thirteen", 20: "twenty", 21: "twenty one",
		54: "fifty four", 82: "eighty two", 90: "ninety", 100: "one hundred",
		118: "one hundred eighteen", 999: "nine hundred ninety nine",
	}
	for n, want := range cases {
		if got := spellInt(n); got != want {
			t.Errorf("spellInt(%d) = %q, want %q", n, got, want)
		}
	}
	for _, n := range []int{-1, 1000, 5000} {
		if got := spellInt(n); got != "" {
			t.Errorf("spellInt(%d) = %q, want \"\" (out of range)", n, got)
		}
	}
}

func TestCESToolEchoes(t *testing.T) {
	// Derive from the REAL fixture, not a copy: editing cesToolOutput must not be
	// able to silently desync this test from what the smoke test actually sends.
	// observed is a transcript genuinely captured from the live agent, so a
	// derived token missing from it means we would be asserting wrong wording.
	out := cesToolOutput
	const observed = "in boston its fifty four degrees fahrenheit with light rain " +
		"and the humidity is eighty two percent would you like the weather for another city"

	echoes := cesToolEchoes(out)
	if len(echoes) == 0 {
		t.Fatal("derived no echoes from cesToolOutput — the round-trip assertion would be vacuous")
	}
	for _, e := range echoes {
		if !strings.Contains(observed, e) {
			t.Errorf("derived token %q is absent from the real observed transcript", e)
		}
	}
	for _, want := range []string{"fifty four", "light rain", "eighty two"} {
		found := false
		for _, e := range echoes {
			if e == want {
				found = true
			}
		}
		if !found {
			t.Errorf("expected %q among derived echoes, got %v", want, echoes)
		}
	}
}

func TestCESToolEchoesRejectsUnverifiable(t *testing.T) {
	// Outputs with nothing assertable must derive NOTHING, so the test fails
	// loudly at config time rather than passing vacuously.
	for _, raw := range []string{
		`{}`,              // no leaves
		`{"ok":true}`,     // bool only
		`{"code":"US"}`,   // string too short to be collision-safe
		`{"ratio":0.375}`, // non-integral number
		`not-json`,        // malformed
		`{"big":123456}`,  // integer out of spellable range
	} {
		if got := cesToolEchoes(raw); len(got) != 0 {
			t.Errorf("cesToolEchoes(%s) = %v, want none (nothing verifiable)", raw, got)
		}
	}
}

func TestCESToolEchoesNested(t *testing.T) {
	// Values nested in objects/arrays must still be found.
	got := cesToolEchoes(`{"a":{"b":["light rain",{"c":54}]}}`)
	if len(got) != 2 {
		t.Fatalf("cesToolEchoes nested = %v, want 2 tokens", got)
	}
}
