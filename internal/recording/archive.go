// Package recording archives per-leg call audio as playable WAV files when
// enabled via RECORD_LEGS (ADR-0016).
//
// The recording of RTP is done by internal/sip (it already decodes inbound
// audio to raw 8 kHz / mono / 16-bit LPCM). This package is the *destination*:
// given a leg's captured PCM plus its (test-name, role) identity, it writes a
// double-clickable WAV under
//
//	<baseDir>/<test-name>/<role>.wav
//
// Filenames are stable across runs (no run-id, no call-id): re-running a test
// overwrites that test's own files, so the folder holds exactly the latest run
// of each test and disk stays bounded. The first time a given test archives a
// leg in a process, that test's subfolder is cleared so a re-run of a single
// test never mixes stale legs with fresh ones.
//
// internal/sip does not import this package. TestMain wires an *Archiver's
// Hook method into sip.SetArchiveHook, keeping the dependency one-way. The
// (test, role) identity arrives with each hook call: the test from the
// per-test SIP stack's Config.Owner, the role from the recording file's
// basename — neither is wired per test.
package recording

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
)

// Telephony WAV parameters. The RTP audio the harness records is already
// decoded to this shape by internal/sip.
const (
	sampleRate    = 8000
	numChannels   = 1
	bitsPerSample = 16
)

// Archiver turns captured PCM into organised WAV artifacts. Safe for
// concurrent use — verb tests run with t.Parallel().
type Archiver struct {
	baseDir string

	mu          sync.Mutex
	clearedTest map[string]bool          // test-name -> subfolder wiped this run
	usedRole    map[string]map[string]int // test-name -> role -> count (dedup)
}

// New returns an Archiver rooted at baseDir. baseDir is created lazily on the
// first Archive call, so an Archiver for a disabled run costs nothing.
func New(baseDir string) *Archiver {
	return &Archiver{
		baseDir:     baseDir,
		clearedTest: map[string]bool{},
		usedRole:    map[string]map[string]int{},
	}
}

// Archive reads raw LPCM from pcmPath and writes it as WAV under
// baseDir/<test>/<role>.wav, returning the WAV path. An empty test or role,
// or an empty/missing PCM file, is a no-op (returns "" with nil error) so
// callers can wire it unconditionally.
//
// The first Archive for a given test in this process wipes that test's
// subfolder so a re-run of the test replaces its previous recordings. If the
// same role is archived more than once within a test, later files get a
// numeric suffix (role-1.wav, role-2.wav) rather than clobbering.
func (a *Archiver) Archive(test, role, pcmPath string) (string, error) {
	if a == nil || test == "" || role == "" || pcmPath == "" {
		return "", nil
	}
	pcm, err := os.ReadFile(pcmPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", fmt.Errorf("recording: read pcm %s: %w", pcmPath, err)
	}
	if len(pcm) == 0 {
		return "", nil
	}

	dir := filepath.Join(a.baseDir, sanitize(test))
	name := a.reserve(test, dir, role)

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("recording: mkdir %s: %w", dir, err)
	}
	out := filepath.Join(dir, name+".wav")
	if err := os.WriteFile(out, wrapWAV(pcm), 0o644); err != nil {
		return "", fmt.Errorf("recording: write %s: %w", out, err)
	}
	return out, nil
}

// Hook is the fire-and-forget adapter matching sip.ArchiveHook's signature:
// install with sip.SetArchiveHook(arch.Hook). It logs failures (a debug
// artifact failing must never fail a test) and the WAV path on success.
func (a *Archiver) Hook(test, role, pcmPath string) {
	out, err := a.Archive(test, role, pcmPath)
	if err != nil {
		log.Printf("recording: archive %s/%s failed: %v", test, role, err)
		return
	}
	if out != "" {
		log.Printf("recording: wrote %s", out)
	}
}

// reserve wipes the test's subfolder once per run and returns a
// collision-free base filename for role.
func (a *Archiver) reserve(test, dir, role string) string {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.clearedTest[test] {
		_ = os.RemoveAll(dir) // best-effort; MkdirAll recreates it
		a.clearedTest[test] = true
	}

	roles := a.usedRole[test]
	if roles == nil {
		roles = map[string]int{}
		a.usedRole[test] = roles
	}
	r := sanitize(role)
	n := roles[r]
	roles[r] = n + 1
	if n == 0 {
		return r
	}
	return fmt.Sprintf("%s-%d", r, n)
}

var unsafeChars = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// sanitize makes an arbitrary test/role name safe as a single path segment.
func sanitize(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, string(filepath.Separator), "-")
	s = strings.ReplaceAll(s, "/", "-")
	s = unsafeChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-.")
	if s == "" {
		return "leg"
	}
	return s
}

// wrapWAV prepends a 44-byte RIFF/WAVE header to raw LPCM. Layout matches
// internal/tts.writeTelephonyWAV and internal/sip.readTelephonyWAV.
func wrapWAV(pcm []byte) []byte {
	byteRate := uint32(sampleRate * numChannels * bitsPerSample / 8)
	blockAlign := uint16(numChannels * bitsPerSample / 8)

	var buf bytes.Buffer
	buf.Grow(44 + len(pcm))
	buf.WriteString("RIFF")
	binary.Write(&buf, binary.LittleEndian, uint32(36+len(pcm)))
	buf.WriteString("WAVE")
	buf.WriteString("fmt ")
	binary.Write(&buf, binary.LittleEndian, uint32(16))
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // PCM
	binary.Write(&buf, binary.LittleEndian, uint16(numChannels))
	binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	binary.Write(&buf, binary.LittleEndian, byteRate)
	binary.Write(&buf, binary.LittleEndian, blockAlign)
	binary.Write(&buf, binary.LittleEndian, uint16(bitsPerSample))
	buf.WriteString("data")
	binary.Write(&buf, binary.LittleEndian, uint32(len(pcm)))
	buf.Write(pcm)
	return buf.Bytes()
}
