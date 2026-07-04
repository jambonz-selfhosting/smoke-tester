package recording

import (
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"
)

// writePCM drops a raw LPCM fixture and returns its path.
func writePCM(t *testing.T, name string, b []byte) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, b, 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	return p
}

func TestArchive_WritesPlayableWAV(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	pcm := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	src := writePCM(t, "dial-caller.pcm", pcm)

	out, err := a.Archive("TestVerb_Dial", "dial-caller", src)
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	want := filepath.Join(base, "TestVerb_Dial", "dial-caller.wav")
	if out != want {
		t.Fatalf("path = %q, want %q", out, want)
	}

	blob, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if len(blob) != 44+len(pcm) {
		t.Fatalf("wav size = %d, want %d", len(blob), 44+len(pcm))
	}
	if string(blob[0:4]) != "RIFF" || string(blob[8:12]) != "WAVE" {
		t.Fatalf("not a RIFF/WAVE header: % x", blob[:12])
	}
	if got := binary.LittleEndian.Uint32(blob[24:28]); got != sampleRate {
		t.Errorf("sample rate = %d, want %d", got, sampleRate)
	}
	if got := binary.LittleEndian.Uint32(blob[40:44]); got != uint32(len(pcm)) {
		t.Errorf("data length = %d, want %d", got, len(pcm))
	}
	if string(blob[44:]) != string(pcm) {
		t.Errorf("payload mismatch")
	}
}

func TestArchive_WipesTestDirOncePerRun(t *testing.T) {
	base := t.TempDir()
	testDir := filepath.Join(base, "TestVerb_X")

	// Simulate a leftover from a previous run.
	if err := os.MkdirAll(testDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(testDir, "stale-leg.wav")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}

	a := New(base)
	if _, err := a.Archive("TestVerb_X", "caller", writePCM(t, "a.pcm", []byte{1, 2})); err != nil {
		t.Fatalf("Archive #1: %v", err)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Errorf("stale file survived the first archive of the run: %v", err)
	}

	// Second archive in the SAME run must NOT wipe the first one.
	if _, err := a.Archive("TestVerb_X", "callee", writePCM(t, "b.pcm", []byte{3, 4})); err != nil {
		t.Fatalf("Archive #2: %v", err)
	}
	for _, f := range []string{"caller.wav", "callee.wav"} {
		if _, err := os.Stat(filepath.Join(testDir, f)); err != nil {
			t.Errorf("%s missing after same-run archives: %v", f, err)
		}
	}
}

func TestArchive_CollidingRolesGetSuffix(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	p1, _ := a.Archive("TestVerb_Y", "caller", writePCM(t, "a.pcm", []byte{1, 2}))
	p2, _ := a.Archive("TestVerb_Y", "caller", writePCM(t, "b.pcm", []byte{3, 4}))

	if filepath.Base(p1) != "caller.wav" {
		t.Errorf("first = %q, want caller.wav", filepath.Base(p1))
	}
	if filepath.Base(p2) != "caller-1.wav" {
		t.Errorf("second = %q, want caller-1.wav", filepath.Base(p2))
	}
}

func TestArchive_SkipsEmptyAndMissing(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	// Missing pcm file — no error, no output.
	if out, err := a.Archive("T", "r", filepath.Join(t.TempDir(), "nope.pcm")); err != nil || out != "" {
		t.Errorf("missing pcm: out=%q err=%v, want no-op", out, err)
	}
	// Empty pcm file — no error, no output (a leg that never received audio).
	if out, err := a.Archive("T", "r", writePCM(t, "empty.pcm", nil)); err != nil || out != "" {
		t.Errorf("empty pcm: out=%q err=%v, want no-op", out, err)
	}
	// Missing identity — no-op (ownerless call).
	if out, err := a.Archive("", "r", writePCM(t, "x.pcm", []byte{1})); err != nil || out != "" {
		t.Errorf("no test name: out=%q err=%v, want no-op", out, err)
	}
	// Nothing should have been created under base.
	entries, _ := os.ReadDir(base)
	if len(entries) != 0 {
		t.Errorf("base dir not empty after no-ops: %v", entries)
	}
}

func TestArchive_SanitizesSubtestNames(t *testing.T) {
	base := t.TempDir()
	a := New(base)

	// Go subtest names contain slashes; they must become one path segment.
	out, err := a.Archive("TestVerb_Z/sub_case", "caller", writePCM(t, "a.pcm", []byte{1, 2}))
	if err != nil {
		t.Fatalf("Archive: %v", err)
	}
	rel, err := filepath.Rel(base, out)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("TestVerb_Z-sub_case", "caller.wav"); rel != want {
		t.Errorf("rel path = %q, want %q", rel, want)
	}
}
