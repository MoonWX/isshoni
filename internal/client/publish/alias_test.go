package publish

import (
	"slices"
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

func TestEngineProfile(t *testing.T) {
	for in, want := range map[string]string{"640c": "6400", "6400": "6400", "42e0": "42e0", "4d00": "4d00", "": ""} {
		if got := EngineProfile(in); got != want {
			t.Errorf("EngineProfile(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestOfferProfilesFor: the others in the order 6400, 640c, 42e0; 640c only next to 6400; 42e0 and 6400 when an
// available encoder lists them (§8.6).
func TestOfferProfilesFor(t *testing.T) {
	for _, tc := range []struct {
		running   string
		available []string
		want      []string
	}{
		{"6400", []string{"6400", "42e0"}, []string{"640c", "42e0"}},
		{"6400", []string{"6400"}, []string{"640c"}},
		{"6400", nil, []string{"640c"}},
		{"42e0", []string{"6400", "42e0"}, []string{"6400", "640c"}},
		{"42e0", []string{"42e0"}, nil},
		{"42e0", []string{"42e0", "4d00"}, nil},
	} {
		if got := OfferProfiles(tc.running, tc.available); !slices.Equal(got, tc.want) {
			t.Errorf("OfferProfiles(%q, %v) = %v, want %v", tc.running, tc.available, got, tc.want)
		}
	}
	// What it returns is a valid Options.Profiles next to the running profile.
	p, err := New(Options{Source: newSource(t), Profile: "6400", Profiles: OfferProfiles("6400", []string{"6400", "42e0"})})
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSPSProfileIDC(t *testing.T) {
	for _, tc := range []struct {
		name string
		au   []byte
		idc  byte
		ok   bool
	}{
		{"High keyframe", annexB(100, true, 0x64), 0x64, true},
		{"CB keyframe", annexB(100, true, 0x42), 0x42, true},
		{"3-byte start code after an AUD", []byte{0, 0, 1, 0x09, 0xf0, 0, 0, 1, 0x67, 0x4d, 0, 0x1f, 0, 0, 1, 0x65, 1}, 0x4d, true},
		{"a slice first", append(annexB(20, false, 0), annexB(20, true, 0x64)...), 0, false},
		{"no start code", []byte{0x67, 0x64, 0x00}, 0, false},
		{"an SPS cut short", []byte{0, 0, 0, 1, 0x67}, 0, false},
		{"empty", nil, 0, false},
	} {
		if idc, ok := spsProfileIDC(tc.au); idc != tc.idc || ok != tc.ok {
			t.Errorf("%s: %#x, %v; want %#x, %v", tc.name, idc, ok, tc.idc, tc.ok)
		}
	}
	if profileIDC("42e0") != 0x42 || profileIDC("6400") != 0x64 || profileIDC("640c") != 0x64 || profileIDC("x") != 0 ||
		profileIDC("zz00") != 0 {
		t.Error("profileIDC")
	}
}

// TestAliasAndReinitSwitch: a switch between 6400 and its SDP alias 640c leaves the ProfileSource alone and changes
// the payload type at the next keyframe; a switch of engine profile calls SetProfile with the engine key and changes
// the payload type at the first keyframe of the new profile_idc, not at a keyframe the old encoder made (§8.6).
func TestAliasAndReinitSwitch(t *testing.T) {
	rec := &profileSource{Source: newSource(t)}
	p, err := New(Options{Source: rec, Layers: []string{"f"}, Profile: "6400", Profiles: []string{"640c", "42e0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	w := &recWriter{}
	tr := p.video[0]
	tr.b = &binding{w: w, pt: videoPT}
	pts := func() []uint8 {
		var out []uint8
		for _, h := range w.hdrs {
			if len(out) == 0 || out[len(out)-1] != h.PayloadType {
				out = append(out, h.PayloadType)
			}
		}
		w.hdrs = nil
		return out
	}
	frame := func(key, reinit bool, idc byte) media.Packet {
		return media.Packet{Kind: media.KindVideo, Layer: "f", Keyframe: key, Reinit: reinit, Data: annexB(2000, key, idc)}
	}
	write := func(pkts ...media.Packet) {
		for _, pkt := range pkts {
			tr.write(pkt, 0, 0)
		}
	}
	choose := func(profile string) {
		p.chosen = profile
		p.applyProfile()
	}

	// 6400 → 640c: no SetProfile, the next keyframe (with the same SPS) goes out on 640c's payload type (98).
	choose("640c")
	if len(rec.profiles) != 0 || !slices.Equal(rec.keyframes, []string{"f"}) {
		t.Fatalf("the source got profiles %v and keyframe requests %v; want none and f", rec.profiles, rec.keyframes)
	}
	write(frame(false, false, 0), frame(true, false, 0x64), frame(false, false, 0))
	if got := pts(); !slices.Equal(got, []uint8{96, 98}) {
		t.Errorf("payload types %v, want 96 then 98 from the keyframe", got)
	}

	// 640c → 42e0: SetProfile(42e0); a High keyframe still on its way, and a High re-init, stay on 98; the first CB
	// keyframe switches to 100.
	choose("42e0")
	write(frame(true, false, 0x64), frame(false, false, 0), frame(true, true, 0x64), frame(true, true, 0x42),
		frame(false, false, 0))
	if got := pts(); !slices.Equal(got, []uint8{98, 100}) {
		t.Errorf("payload types %v, want 98 until the CB keyframe, then 100", got)
	}
	// 42e0 → 640c: SetProfile gets the engine key 6400, never the alias.
	choose("640c")
	if want := []string{"42e0", "6400"}; !slices.Equal(rec.profiles, want) {
		t.Errorf("SetProfile calls %v, want %v", rec.profiles, want)
	}
	// A keyframe without an SPS in front: only a Reinit one ends the wait.
	write(frame(true, false, 0), frame(true, true, 0), frame(false, false, 0))
	if got := pts(); !slices.Equal(got, []uint8{100, 98}) {
		t.Errorf("payload types %v, want 100 until the Reinit keyframe, then 98", got)
	}

	// A plain Source is never switched: the next keyframe changes the payload type, as in M1.
	plain, err := New(Options{Source: struct{ Source }{rec}, Layers: []string{"f"}, Profile: "6400", Profiles: []string{"42e0"}})
	if err != nil {
		t.Fatal(err)
	}
	defer plain.Close()
	w = &recWriter{}
	tr = plain.video[0]
	tr.b = &binding{w: w, pt: videoPT}
	plain.chosen = "42e0"
	plain.applyProfile()
	write(frame(true, false, 0x64))
	if got := pts(); !slices.Equal(got, []uint8{98}) || len(rec.profiles) != 2 {
		t.Errorf("a plain Source: payload types %v, SetProfile calls %v", got, rec.profiles)
	}
}

// FuzzSPSProfileIDC: whatever the bytes, the scan doesn't panic, and what it finds is the byte after an SPS NAL
// header that follows a start code with no slice NAL unit before it.
func FuzzSPSProfileIDC(f *testing.F) {
	for _, seed := range [][]byte{
		annexB(40, true, 0x64), annexB(40, false, 0), {0, 0, 1}, {0, 0, 1, 0x67}, {0, 0, 0, 1, 0x09, 0, 0, 1, 0x67, 0x42},
		{0, 0, 1, 0x65, 0, 0, 1, 0x67, 0x64}, nil,
	} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, au []byte) {
		idc, ok := spsProfileIDC(au)
		// The reference: the first start-code-led NAL unit that is an SPS or a slice decides.
		var wantIDC byte
		wantOK := false
	scan:
		for i := 0; i+3 < len(au); i++ {
			if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
				switch au[i+3] & 0x1f {
				case 7:
					if i+4 < len(au) {
						wantIDC, wantOK = au[i+4], true
					}
					break scan
				case 1, 2, 3, 4, 5:
					break scan
				}
			}
		}
		if idc != wantIDC || ok != wantOK {
			t.Fatalf("spsProfileIDC(%x) = %#x, %v; want %#x, %v", au, idc, ok, wantIDC, wantOK)
		}
	})
}
