package sfutest

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// spsProfile returns the profile key of the SPS in an Annex B access unit ("" without one).
func spsProfile(au []byte) string {
	i := bytes.Index(au, []byte{0, 0, 1, 0x67})
	if i < 0 || len(au) < i+6 {
		return ""
	}
	const hex = "0123456789abcdef"
	idc, flags := au[i+4], au[i+5]
	return string([]byte{hex[idc>>4], hex[idc&15], hex[flags>>4], hex[flags&15]})
}

// TestProfileSource: a switch makes a new Source in the new profile, whose media goes on on the old one's timeline,
// starting with a keyframe; the same profile again changes nothing; Close ends Next.
func TestProfileSource(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var made []string
		src, err := NewProfileSource("6400", func(profile string) (*fake.Source, error) {
			made = append(made, profile)
			return fake.New(fake.Config{
				Profile: profile, Layers: []fake.VideoLayer{{RID: "f", Width: 320, Height: 180, FPS: 30, Bitrate: 200_000}},
				GOP: time.Minute, Audio: true,
			})
		})
		if err != nil {
			t.Fatal(err)
		}
		var last int64
		next := func() fake.Packet {
			t.Helper()
			p, err := src.Next(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if p.CaptureNS < last {
				t.Fatalf("capture time goes back: %v after %v", time.Duration(p.CaptureNS), time.Duration(last))
			}
			last = p.CaptureNS
			return p
		}
		for p := next(); p.CaptureNS < int64(500*time.Millisecond); p = next() {
			if p.Kind == fake.Video && p.Keyframe && spsProfile(p.Data) != "6400" {
				t.Fatalf("a keyframe of the first source is in %q", spsProfile(p.Data))
			}
		}
		before := last
		time.Sleep(100 * time.Millisecond) // nobody pulls for a while: the timeline goes on all the same

		if err := src.SetProfile("42e0"); err != nil {
			t.Fatal(err)
		}
		if err := src.SetProfile("42e0"); err != nil {
			t.Fatal(err)
		}
		if p, n := src.Profile(); p != "42e0" || n != 1 || !slices.Equal(made, []string{"6400", "42e0"}) {
			t.Fatalf("profile %s after %d switches, sources made %v", p, n, made)
		}
		var first fake.Packet
		for first = next(); first.Kind != fake.Video; first = next() {
		}
		if !first.Keyframe || spsProfile(first.Data) != "42e0" {
			t.Errorf("the new source starts with keyframe %v in %q, want a 42e0 keyframe", first.Keyframe,
				spsProfile(first.Data))
		}
		if d := time.Duration(first.CaptureNS - before); d < 100*time.Millisecond || d > 200*time.Millisecond {
			t.Errorf("the new source's media starts %v after the old one's last, want the 100 ms nobody pulled", d)
		}
		src.RequestKeyframe("f")
		for p := next(); ; p = next() {
			if p.Kind == fake.Video {
				if !p.Keyframe {
					t.Error("the keyframe request went to the old source")
				}
				break
			}
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := src.Next(t.Context()); !errors.Is(err, fake.ErrClosed) {
			t.Errorf("Next after Close = %v", err)
		}
		if err := src.SetProfile("6400"); err == nil {
			t.Error("SetProfile after Close")
		}
	})
}
