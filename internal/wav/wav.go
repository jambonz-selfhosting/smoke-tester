// Package wav reads and writes the harness's telephony WAV: mono, 8 kHz,
// 16-bit little-endian PCM in a 44-byte RIFF/WAVE header.
package wav

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
)

const (
	SampleRate    = 8000
	numChannels   = 1
	bitsPerSample = 16
)

// Wrap prepends the RIFF/WAVE header to raw LPCM.
func Wrap(lpcm []byte) []byte {
	b := make([]byte, 0, 44+len(lpcm))
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+len(lpcm)))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16)
	b = binary.LittleEndian.AppendUint16(b, 1) // PCM
	b = binary.LittleEndian.AppendUint16(b, numChannels)
	b = binary.LittleEndian.AppendUint32(b, SampleRate)
	b = binary.LittleEndian.AppendUint32(b, SampleRate*numChannels*bitsPerSample/8)
	b = binary.LittleEndian.AppendUint16(b, numChannels*bitsPerSample/8)
	b = binary.LittleEndian.AppendUint16(b, bitsPerSample)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(len(lpcm)))
	return append(b, lpcm...)
}

// Write writes raw LPCM to path as a WAV file.
func Write(path string, lpcm []byte) error {
	return os.WriteFile(path, Wrap(lpcm), 0o644)
}

// WriteSamples writes samples to path as a WAV file.
func WriteSamples(path string, pcm []int16) error {
	return Write(path, Bytes(pcm))
}

// Read returns the LPCM payload of a mono 8 kHz 16-bit WAV file.
func Read(path string) ([]byte, error) {
	blob, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(blob)
}

// Parse returns the LPCM payload of a mono 8 kHz 16-bit WAV blob. Truncated or
// malformed chunks are errors, never panics.
func Parse(blob []byte) ([]byte, error) {
	if len(blob) < 12 || string(blob[0:4]) != "RIFF" || string(blob[8:12]) != "WAVE" {
		return nil, errors.New("not a RIFF/WAVE file")
	}
	var (
		haveFmt                   bool
		format, channels, bitsPer uint16
		rate                      uint32
		data                      []byte
	)
	for i := 12; i+8 <= len(blob); {
		size := int(binary.LittleEndian.Uint32(blob[i+4 : i+8]))
		if size < 0 || i+8+size > len(blob) {
			return nil, fmt.Errorf("%q chunk runs past the end of the file", blob[i:i+4])
		}
		body := blob[i+8 : i+8+size]
		switch string(blob[i : i+4]) {
		case "fmt ":
			if size < 16 {
				return nil, fmt.Errorf("fmt chunk too small (%d bytes)", size)
			}
			format = binary.LittleEndian.Uint16(body[0:2])
			channels = binary.LittleEndian.Uint16(body[2:4])
			rate = binary.LittleEndian.Uint32(body[4:8])
			bitsPer = binary.LittleEndian.Uint16(body[14:16])
			haveFmt = true
		case "data":
			data = body
		}
		i += 8 + size + size%2 // chunks pad to even length
	}
	if !haveFmt {
		return nil, errors.New("fmt chunk not found")
	}
	if data == nil {
		return nil, errors.New("data chunk not found")
	}
	if format != 1 {
		return nil, fmt.Errorf("WAV format %d not PCM", format)
	}
	if channels != numChannels || rate != SampleRate || bitsPer != bitsPerSample {
		return nil, fmt.Errorf("WAV must be mono 8000 Hz 16-bit; got %d ch / %d Hz / %d-bit",
			channels, rate, bitsPer)
	}
	return data, nil
}

// Samples decodes LPCM bytes to samples; an odd trailing byte is dropped.
func Samples(lpcm []byte) []int16 {
	pcm := make([]int16, len(lpcm)/2)
	for i := range pcm {
		pcm[i] = int16(binary.LittleEndian.Uint16(lpcm[2*i:]))
	}
	return pcm
}

// Bytes encodes samples as LPCM bytes.
func Bytes(pcm []int16) []byte {
	b := make([]byte, 2*len(pcm))
	for i, v := range pcm {
		binary.LittleEndian.PutUint16(b[2*i:], uint16(v))
	}
	return b
}
