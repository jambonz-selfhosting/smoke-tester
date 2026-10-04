package verbs

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// dtmfRow/dtmfCol are the standard DTMF tone pairs.
var dtmfRow = map[rune]float64{'1': 697, '2': 697, '3': 697, 'A': 697,
	'4': 770, '5': 770, '6': 770, 'B': 770,
	'7': 852, '8': 852, '9': 852, 'C': 852,
	'*': 941, '0': 941, '#': 941, 'D': 941}
var dtmfCol = map[rune]float64{'1': 1209, '2': 1336, '3': 1477, 'A': 1633,
	'4': 1209, '5': 1336, '6': 1477, 'B': 1633,
	'7': 1209, '8': 1336, '9': 1477, 'C': 1633,
	'*': 1209, '0': 1336, '#': 1477, 'D': 1633}

// SynthesizeDTMFWAV writes digits as inband tones to an 8kHz PCM16 mono WAV and
// returns its path. toneMS/gapMS default to 100/100 — comfortably above the
// 40ms ITU-T Q.24 floor, so a missed digit means the path dropped it rather
// than that the tone was marginal.
func SynthesizeDTMFWAV(t *testing.T, digits string, toneMS, gapMS int) string {
	t.Helper()
	const rate = 8000
	const amp = 8000.0
	var pcm []int16
	pad := make([]int16, rate/2) // 0.5s lead-in and trail
	pcm = append(pcm, pad...)
	for _, d := range digits {
		lo, ok := dtmfRow[d]
		if !ok {
			t.Fatalf("SynthesizeDTMFWAV: unsupported digit %q", d)
		}
		hi := dtmfCol[d]
		for n := 0; n < rate*toneMS/1000; n++ {
			s := float64(n) / rate
			pcm = append(pcm, int16(amp*(math.Sin(2*math.Pi*lo*s)+math.Sin(2*math.Pi*hi*s))/2))
		}
		pcm = append(pcm, make([]int16, rate*gapMS/1000)...)
	}
	pcm = append(pcm, pad...)

	body := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(body[i*2:], uint16(v))
	}
	var hdr []byte
	hdr = append(hdr, []byte("RIFF")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(36+len(body)))
	hdr = append(hdr, []byte("WAVEfmt ")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, 16)
	hdr = binary.LittleEndian.AppendUint16(hdr, 1)
	hdr = binary.LittleEndian.AppendUint16(hdr, 1)
	hdr = binary.LittleEndian.AppendUint32(hdr, rate)
	hdr = binary.LittleEndian.AppendUint32(hdr, rate*2)
	hdr = binary.LittleEndian.AppendUint16(hdr, 2)
	hdr = binary.LittleEndian.AppendUint16(hdr, 16)
	hdr = append(hdr, []byte("data")...)
	hdr = binary.LittleEndian.AppendUint32(hdr, uint32(len(body)))

	path := filepath.Join(t.TempDir(), "dtmf-inband.wav")
	if err := os.WriteFile(path, append(hdr, body...), 0o644); err != nil {
		t.Fatalf("SynthesizeDTMFWAV: %v", err)
	}
	return path
}

var dtmfKeys = [4][4]string{
	{"1", "2", "3", "A"}, {"4", "5", "6", "B"}, {"7", "8", "9", "C"}, {"*", "0", "#", "D"},
}
var dtmfLow = [4]float64{697, 770, 852, 941}
var dtmfHigh = [4]float64{1209, 1336, 1477, 1633}

// goertzel returns the energy of freq in x[off:off+n] at 8kHz.
func goertzel(x []int16, freq float64, n, off int) float64 {
	w := 2 * math.Pi * freq / 8000
	c := 2 * math.Cos(w)
	var s1, s2 float64
	for i := off; i < off+n; i++ {
		s0 := float64(x[i]) + c*s1 - s2
		s2, s1 = s1, s0
	}
	return s1*s1 + s2*s2 - c*s1*s2
}

// DetectInbandDTMF reads an 8kHz PCM16 file and returns the DTMF digits present
// as audio tones. This is how a leg that never negotiated telephone-event is
// verified: there are no RTP events to count, only sound.
func DetectInbandDTMF(pcmPath string) (string, error) {
	raw, err := os.ReadFile(pcmPath)
	if err != nil {
		return "", fmt.Errorf("read pcm: %w", err)
	}
	s := make([]int16, len(raw)/2)
	for i := range s {
		s[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	const win, hop = 160, 40 // 20ms window, 5ms hop
	// A real receiver needs an inter-digit pause before it will register a
	// second keypress (ITU-T Q.24 puts the minimum at 40ms). Closing a digit on
	// a shorter dip splits one tone that briefly wobbles into two, which reads
	// as a duplicate keypress that the far end would never report.
	const minGapHops = 40 / (hop / 8) // 40ms expressed in hops
	var out []byte
	cur, run, miss := "", 0, 0
	for off := 0; off+win <= len(s); off += hop {
		var e float64
		for _, v := range s[off : off+win] {
			e += float64(v) * float64(v)
		}
		d := ""
		if e > win*4000 {
			var lo, hi [4]float64
			for i := range lo {
				lo[i] = goertzel(s, dtmfLow[i], win, off)
				hi[i] = goertzel(s, dtmfHigh[i], win, off)
			}
			li, hj := argmax(lo), argmax(hi)
			if lo[li] > 3*second(lo) && hi[hj] > 3*second(hi) && lo[li]+hi[hj] > 0.25*e*win {
				d = dtmfKeys[li][hj]
			}
		}
		switch {
		case d == cur:
			if cur != "" {
				run++
			}
			miss = 0
		case d != "":
			if cur != "" && run >= 3 {
				out = append(out, cur...)
			}
			cur, run, miss = d, 1, 0
		default:
			miss++
			if cur != "" && miss >= minGapHops {
				if run >= 3 {
					out = append(out, cur...)
				}
				cur, run = "", 0
			}
		}
	}
	if cur != "" && run >= 3 {
		out = append(out, cur...)
	}
	return string(out), nil
}

func argmax(a [4]float64) int {
	m := 0
	for i := 1; i < 4; i++ {
		if a[i] > a[m] {
			m = i
		}
	}
	return m
}

func second(a [4]float64) float64 {
	m, s := math.Inf(-1), math.Inf(-1)
	for _, v := range a {
		if v > m {
			m, s = v, m
		} else if v > s {
			s = v
		}
	}
	return s
}
