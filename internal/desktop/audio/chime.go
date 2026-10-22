package audio

import (
	"encoding/binary"
	"math"
	"time"
)

// The sound-check chime (05 §11.3): two partials, 880 Hz and 1320 Hz, each at −14 dBFS, 800 ms of 48 kHz 16-bit
// stereo with 10 ms raised-cosine ramps. The check passes when the engine hears both partials at −40 dBFS or more.
const (
	chimeRate     = 48000
	chimeChannels = 2
	chimeBits     = 16
	chimeLength   = 800 * time.Millisecond
	chimeRamp     = 10 * time.Millisecond
	chimeLowHz    = 880
	chimeHighHz   = 1320
	chimeLevelDB  = -14 // each partial's peak
)

// chimeWAV returns the chime as a RIFF WAVE file in memory, for PlaySoundW(SND_MEMORY).
func chimeWAV() []byte {
	frames := int(chimeLength * chimeRate / time.Second)
	ramp := int(chimeRamp * chimeRate / time.Second)
	const blockAlign = chimeChannels * chimeBits / 8
	dataLen := frames * blockAlign
	b := make([]byte, 0, 44+dataLen)
	b = append(b, "RIFF"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(36+dataLen))
	b = append(b, "WAVEfmt "...)
	b = binary.LittleEndian.AppendUint32(b, 16) // the fmt chunk's size
	b = binary.LittleEndian.AppendUint16(b, 1)  // WAVE_FORMAT_PCM
	b = binary.LittleEndian.AppendUint16(b, chimeChannels)
	b = binary.LittleEndian.AppendUint32(b, chimeRate)
	b = binary.LittleEndian.AppendUint32(b, chimeRate*blockAlign)
	b = binary.LittleEndian.AppendUint16(b, blockAlign)
	b = binary.LittleEndian.AppendUint16(b, chimeBits)
	b = append(b, "data"...)
	b = binary.LittleEndian.AppendUint32(b, uint32(dataLen))

	amp := math.Pow(10, chimeLevelDB/20.0)
	for i := range frames {
		t := float64(i) / chimeRate
		gain := 1.0
		if d := min(i, frames-1-i); d < ramp {
			gain = 0.5 - 0.5*math.Cos(math.Pi*float64(d)/float64(ramp))
		}
		x := gain * amp * (math.Sin(2*math.Pi*chimeLowHz*t) + math.Sin(2*math.Pi*chimeHighHz*t))
		s := int16(math.Round(x * math.MaxInt16))
		for range chimeChannels {
			b = binary.LittleEndian.AppendUint16(b, uint16(s)) //nolint:gosec // G115: a sample's two's complement bits, as WAVE stores them
		}
	}
	return b
}
