//go:build ignore

// gentone writes the PCM that gen-opus.sh encodes into tone.opus (docs/m1/02-sfu.md §15.1): 48 kHz, 16-bit
// little-endian, both channels equal, periodic with a period of one second. Each second holds a 440 Hz bed
// (440 whole cycles, so the loop is phase-continuous) and a 1 kHz beep in its first 100 ms (100 whole cycles, with
// 1 ms raised-cosine edges).
//
// The output starts -shift samples into the period: the encoder delays its output by its pre-skip, so with -shift
// equal to the pre-skip, decoded sample n of the stream is sample n mod 48000 of the period, and every second of
// encoded packets starts on the beep.
package main

import (
	"bufio"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
)

const (
	rate      = 48000
	bedHz     = 440
	beepHz    = 1000
	beepLen   = rate / 10 // 100 ms
	edge      = rate / 1000
	bedLevel  = 0.1  // −20 dBFS
	beepLevel = 0.32 // about −10 dBFS
)

func main() {
	seconds := flag.Int("seconds", 2, "length in seconds")
	shift := flag.Int("shift", 0, "start this many samples into the period (the encoder's pre-skip)")
	flag.Parse()
	if *seconds < 1 || *shift < 0 || *shift >= rate {
		fmt.Fprintln(os.Stderr, "gentone: bad -seconds or -shift")
		os.Exit(2)
	}
	w := bufio.NewWriter(os.Stdout)
	var frame [4]byte
	for n := range *seconds * rate {
		s := sample((n + *shift) % rate)
		binary.LittleEndian.PutUint16(frame[0:], uint16(s))
		binary.LittleEndian.PutUint16(frame[2:], uint16(s))
		if _, err := w.Write(frame[:]); err != nil {
			fmt.Fprintln(os.Stderr, "gentone:", err)
			os.Exit(1)
		}
	}
	if err := w.Flush(); err != nil {
		fmt.Fprintln(os.Stderr, "gentone:", err)
		os.Exit(1)
	}
}

// sample returns sample t (0 ≤ t < rate) of the period.
func sample(t int) int16 {
	v := bedLevel * math.Sin(2*math.Pi*bedHz*float64(t)/rate)
	if t < beepLen {
		env := 1.0
		switch {
		case t < edge:
			env = 0.5 - 0.5*math.Cos(math.Pi*float64(t)/edge)
		case t >= beepLen-edge:
			env = 0.5 - 0.5*math.Cos(math.Pi*float64(beepLen-t)/edge)
		}
		v += beepLevel * env * math.Sin(2*math.Pi*beepHz*float64(t)/rate)
	}
	return int16(math.Round(v * math.MaxInt16))
}
