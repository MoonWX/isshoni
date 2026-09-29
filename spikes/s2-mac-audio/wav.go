package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
)

// writeWAV writes interleaved stereo float32 samples as a 48 kHz IEEE-float WAV.
func writeWAV(path string, interleaved []float32) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	dataLen := uint32(len(interleaved) * 4)
	hdr := []any{
		[4]byte{'R', 'I', 'F', 'F'}, 36 + dataLen, [4]byte{'W', 'A', 'V', 'E'},
		[4]byte{'f', 'm', 't', ' '}, uint32(16), uint16(3), uint16(2), uint32(sampleRate),
		uint32(sampleRate * 8), uint16(8), uint16(32),
		[4]byte{'d', 'a', 't', 'a'}, dataLen,
	}
	for _, v := range hdr {
		if err := binary.Write(f, binary.LittleEndian, v); err != nil {
			return err
		}
	}
	return binary.Write(f, binary.LittleEndian, interleaved)
}

// toneWAV builds an in-memory 16-bit stereo WAV with a sine of the given
// frequency, with 20 ms fades at both ends so starting/stopping never clicks.
func toneWAV(freq, amp, seconds float64) []byte {
	n := int(seconds * sampleRate)
	fade := sampleRate / 50
	data := make([]byte, 44+n*4)
	le := binary.LittleEndian
	copy(data[0:], "RIFF")
	le.PutUint32(data[4:], uint32(36+n*4))
	copy(data[8:], "WAVEfmt ")
	le.PutUint32(data[16:], 16)
	le.PutUint16(data[20:], 1) // PCM
	le.PutUint16(data[22:], 2)
	le.PutUint32(data[24:], sampleRate)
	le.PutUint32(data[28:], sampleRate*4)
	le.PutUint16(data[32:], 4)
	le.PutUint16(data[34:], 16)
	copy(data[36:], "data")
	le.PutUint32(data[40:], uint32(n*4))
	for i := 0; i < n; i++ {
		g := 1.0
		if i < fade {
			g = float64(i) / float64(fade)
		} else if n-i < fade {
			g = float64(n-i) / float64(fade)
		}
		v := int16(math.Round(amp * g * math.Sin(2*math.Pi*freq*float64(i)/sampleRate) * 32767))
		le.PutUint16(data[44+i*4:], uint16(v))
		le.PutUint16(data[46+i*4:], uint16(v))
	}
	return data
}

// readWAV reads a 16-bit PCM or 32-bit float stereo (or mono) WAV.
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
				return nil, nil, io.ErrUnexpectedEOF
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
	sample := func(i int) float64 {
		switch {
		case format == 3 && bits == 32:
			return float64(math.Float32frombits(le.Uint32(data[i*4:])))
		case format == 1 && bits == 16:
			return float64(int16(le.Uint16(data[i*2:]))) / 32768
		}
		return 0
	}
	if !(format == 3 && bits == 32) && !(format == 1 && bits == 16) {
		return nil, nil, fmt.Errorf("unsupported WAV format %d/%d bits", format, bits)
	}
	frames := len(data) / int(bits/8) / int(channels)
	left, right = make([]float64, frames), make([]float64, frames)
	for i := 0; i < frames; i++ {
		left[i] = sample(i * int(channels))
		right[i] = left[i]
		if channels > 1 {
			right[i] = sample(i*int(channels) + 1)
		}
	}
	return left, right, nil
}
