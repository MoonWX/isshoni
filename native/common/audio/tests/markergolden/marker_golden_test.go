// Package markergolden is the Go half of the native Opus stage's golden Marker test (docs/m2/04-windows-engine.md
// §10.3, §18.1): the C++ test ../opus_stage_test.cpp checks that the test tone's code 3 framing writes each packet of
// ../testdata/tone_marker_golden.txt, and this test reads every framed packet with M1's fake.ParseAudioMarker, the
// parser that sfutest.Viewer and the e2e checks use on native packets as on the fake's. (It has a folder of its own
// because Go refuses a package directory that holds C++ files.)
package markergolden

import (
	"bufio"
	"encoding/hex"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

func TestGoldenMarkersParseWithM1Parser(t *testing.T) {
	f, err := os.Open("../testdata/tone_marker_golden.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	n := 0
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fl := strings.Fields(line)
		if len(fl) != 6 || fl[0] != "packet" {
			t.Fatalf("malformed line %q", line)
		}
		frame, err := strconv.ParseUint(fl[1], 10, 32)
		if err != nil {
			t.Fatal(err)
		}
		capture, err := strconv.ParseInt(fl[2], 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		input, err := hex.DecodeString(fl[4])
		if err != nil {
			t.Fatal(err)
		}
		framed, err := hex.DecodeString(fl[5])
		if err != nil {
			t.Fatal(err)
		}
		want := fake.Marker{Beep: fl[3] == "1", Frame: uint32(frame), CaptureNS: capture}
		got, ok := fake.ParseAudioMarker(framed)
		if !ok || got != want {
			t.Errorf("frame %d: ParseAudioMarker = %+v, %v; want %+v", frame, got, ok, want)
		}
		if len(framed) != len(input)-1+2+1+fake.MarkerSize && input[0]&3 == 0 {
			t.Errorf("frame %d: a code 0 packet grows by the count byte, one padding length byte and the Marker", frame)
		}
		if _, ok := fake.ParseAudioMarker(input); ok && input[0]&3 != 3 {
			t.Errorf("frame %d: the unframed input reads as a Marker", frame)
		}
		n++
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	if n < 6 {
		t.Fatalf("%d golden packets, want at least 6", n)
	}
}
