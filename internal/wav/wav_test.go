package wav

import (
	"encoding/binary"
	"slices"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	pcm := []int16{0, 1, -1, 32767, -32768, 1234}
	got, err := Parse(Wrap(Bytes(pcm)))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(Samples(got), pcm) {
		t.Fatalf("got %v, want %v", Samples(got), pcm)
	}
}

func TestMalformedIsAnErrorNotAPanic(t *testing.T) {
	good := Wrap(Bytes([]int16{1, 2, 3}))
	shortFmt := append([]byte(nil), good...)
	binary.LittleEndian.PutUint32(shortFmt[16:], 8) // fmt chunk claims 8 bytes
	for name, blob := range map[string][]byte{
		"empty":          nil,
		"not riff":       []byte("RIFX0000WAVE"),
		"truncated data": good[:len(good)-2],
		"short fmt":      shortFmt,
		"header only":    good[:20],
	} {
		if _, err := Parse(blob); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestRejectsOtherFormats(t *testing.T) {
	blob := Wrap(Bytes([]int16{1}))
	binary.LittleEndian.PutUint32(blob[24:], 16000)
	if _, err := Parse(blob); err == nil {
		t.Fatal("16 kHz must be rejected")
	}
}
