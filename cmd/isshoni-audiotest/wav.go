package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
)

// This file is the portable WAV helpers of the audiotest (docs/m2/05-windows-audio.md §18.3), ported from spike S1
// (spikes/s1-win-audio/wav.go). toneWAV generates the fixtures' tones in memory for PlaySoundW; writeWAV saves the
// captured tap when -keep-wav is given; readWAV reads a WAV back (tests, and WA62b's record/merge). None of it needs
// the engine, so it builds and is tested on every OS.

// toneWAV builds an in-memory 16-bit stereo 48 kHz WAV: the sum of sines at freqs, each at amplitude amp, over
// seconds, with 20 ms linear fades at both ends so starting and stopping never clicks (S1's generator, 05 §18.3).
func toneWAV(freqs []float64, amp, seconds float64) []byte {
	n := int(seconds * sampleRate)
	if n < 0 {
		n = 0
	}
	fade := sampleRate / 50 // 20 ms
	data := make([]byte, 44+n*4)
	writeWAVHeader(data, n)
	le := binary.LittleEndian
	for i := range n {
		g := 1.0
		if i < fade {
			g = float64(i) / float64(fade)
		} else if n-i < fade {
			g = float64(n-i) / float64(fade)
		}
		var sum float64
		for _, f := range freqs {
			sum += math.Sin(2 * math.Pi * f * float64(i) / sampleRate)
		}
		v := int16(math.Round(clamp1(amp*g*sum) * 32767))
		le.PutUint16(data[44+i*4:], uint16(v)) //nolint:gosec // G115: a sample's two's-complement bits
		le.PutUint16(data[46+i*4:], uint16(v)) //nolint:gosec // G115: the right channel is the same
	}
	return data
}

// clamp1 clamps a sample to [−1, 1] (a sum of partials can exceed it; WAV stores a bounded integer).
func clamp1(v float64) float64 { return math.Max(-1, math.Min(1, v)) }

// writeWAVHeader writes a 48 kHz 16-bit stereo RIFF/WAVE header for n frames into the first 44 bytes of b.
func writeWAVHeader(b []byte, n int) {
	le := binary.LittleEndian
	const blockAlign = 4 // 2 channels × 16 bits
	copy(b[0:], "RIFF")
	le.PutUint32(b[4:], uint32(36+n*blockAlign)) //nolint:gosec // G115: a WAV chunk size
	copy(b[8:], "WAVEfmt ")
	le.PutUint32(b[16:], 16) // the fmt chunk's size
	le.PutUint16(b[20:], 1)  // WAVE_FORMAT_PCM
	le.PutUint16(b[22:], 2)  // channels
	le.PutUint32(b[24:], sampleRate)
	le.PutUint32(b[28:], sampleRate*blockAlign)
	le.PutUint16(b[32:], blockAlign)
	le.PutUint16(b[34:], 16) // bits per sample
	copy(b[36:], "data")
	le.PutUint32(b[40:], uint32(n*blockAlign)) //nolint:gosec // G115: a WAV chunk size
}

// writeWAV saves interleaved stereo float32 samples as a 48 kHz IEEE-float WAV (-keep-wav; S1's writer).
func writeWAV(path string, interleaved []float32) error {
	dataLen := len(interleaved) * 4
	hdr := make([]byte, 44)
	le := binary.LittleEndian
	copy(hdr[0:], "RIFF")
	le.PutUint32(hdr[4:], uint32(36+dataLen)) //nolint:gosec // G115: a WAV chunk size
	copy(hdr[8:], "WAVEfmt ")
	le.PutUint32(hdr[16:], 16)
	le.PutUint16(hdr[20:], 3) // IEEE float
	le.PutUint16(hdr[22:], 2)
	le.PutUint32(hdr[24:], sampleRate)
	le.PutUint32(hdr[28:], sampleRate*8)
	le.PutUint16(hdr[32:], 8)
	le.PutUint16(hdr[34:], 32)
	copy(hdr[36:], "data")
	le.PutUint32(hdr[40:], uint32(dataLen)) //nolint:gosec // G115: a WAV chunk size
	body := make([]byte, dataLen)
	for i, v := range interleaved {
		le.PutUint32(body[i*4:], math.Float32bits(v))
	}
	if err := os.WriteFile(path, append(hdr, body...), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// readWAV reads a 16-bit PCM or 32-bit float stereo (or mono) 48 kHz WAV into two channels (S1's reader).
func readWAV(path string) (left, right []float64, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, nil, err
	}
	if len(b) < 12 || string(b[0:4]) != "RIFF" || string(b[8:12]) != "WAVE" {
		return nil, nil, errors.New("not a WAV file")
	}
	le := binary.LittleEndian
	var format, channels, bits uint16
	var data []byte
	for p := 12; p+8 <= len(b); {
		id, size := string(b[p:p+4]), int(le.Uint32(b[p+4:]))
		body := b[p+8 : min(p+8+size, len(b))]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return nil, nil, errors.New("WAV fmt chunk too short")
			}
			format, channels, bits = le.Uint16(body[0:]), le.Uint16(body[2:]), le.Uint16(body[14:])
		case "data":
			data = body
		}
		p += 8 + size + size%2
	}
	if channels == 0 || data == nil {
		return nil, nil, errors.New("WAV without fmt/data")
	}
	switch {
	case format == 3 && bits == 32:
	case format == 1 && bits == 16:
	default:
		return nil, nil, fmt.Errorf("unsupported WAV format %d/%d bits", format, bits)
	}
	sample := func(i int) float64 {
		switch {
		case format == 3 && bits == 32 && (i+1)*4 <= len(data):
			return float64(math.Float32frombits(le.Uint32(data[i*4:])))
		case format == 1 && bits == 16 && (i+1)*2 <= len(data):
			return float64(int16(le.Uint16(data[i*2:]))) / 32768 //nolint:gosec // G115: a 16-bit PCM sample
		}
		return 0
	}
	frames := len(data) / int(bits/8) / int(channels)
	left, right = make([]float64, frames), make([]float64, frames)
	for i := range frames {
		left[i] = sample(i * int(channels))
		right[i] = left[i]
		if channels > 1 {
			right[i] = sample(i*int(channels) + 1)
		}
	}
	return left, right, nil
}
