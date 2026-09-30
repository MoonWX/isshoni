package protocol

import "testing"

// TestParseH264CodecKey is the codec table of 01 §19 (the TS h264Key test uses the same table).
func TestParseH264CodecKey(t *testing.T) {
	for _, c := range []struct {
		fmtp string
		want CodecKey
	}{
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f", CodecH264ConstrainedBaseline},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42001f", CodecH264Baseline},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=4d001f", CodecH264Main},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640c1f", CodecH264ConstrainedHigh},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f", CodecH264High},
		{"level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=640034", CodecH264High}, // Chrome's High: level ignored
		{"profile-level-id=42e01f;packetization-mode=1", CodecH264ConstrainedBaseline},            // order does not matter
		{"packetization-mode=1; profile-level-id=42e01f", CodecH264ConstrainedBaseline},           // spaces
		{"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f", CodecH264High},

		// mode 0 (or no mode, which means 0): not usable.
		{"level-asymmetry-allowed=1;packetization-mode=0;profile-level-id=42e01f", ""},
		{"level-asymmetry-allowed=1;profile-level-id=42e01f", ""},
		{"packetization-mode=2;profile-level-id=42e01f", ""},

		// mixed case: names and hex digits.
		{"Packetization-Mode=1;Profile-Level-Id=42E01F", CodecH264ConstrainedBaseline},
		{"packetization-mode=1;profile-level-id=640C1F", CodecH264ConstrainedHigh},
		{"PACKETIZATION-MODE=1;PROFILE-LEVEL-ID=4D001F", CodecH264Main},

		// no or malformed profile-level-id.
		{"packetization-mode=1", ""},
		{"packetization-mode=1;profile-level-id=", ""},
		{"packetization-mode=1;profile-level-id=42e0", ""},
		{"packetization-mode=1;profile-level-id=42e01f00", ""},
		{"packetization-mode=1;profile-level-id=zze01f", ""},
		{"", ""},
		{"minptime=10;useinbandfec=1", ""}, // Opus
	} {
		if got := ParseH264CodecKey(c.fmtp); got != c.want {
			t.Errorf("ParseH264CodecKey(%q) = %q, want %q", c.fmtp, got, c.want)
		}
	}
}

func TestCodecKeyHelpers(t *testing.T) {
	for _, c := range []struct {
		k       CodecKey
		profile string
	}{
		{CodecH264High, "6400"}, {CodecH264ConstrainedBaseline, "42e0"}, {"h264/640c", "640c"},
		{CodecOpus, ""}, {"h264/6400x", ""}, {"h264/64", ""}, {"h264/640C", ""}, {"h265/0100", ""}, {"", ""},
	} {
		if got := c.k.H264Profile(); got != c.profile {
			t.Errorf("%q.H264Profile() = %q, want %q", c.k, got, c.profile)
		}
		if c.k.IsH264() != (c.profile != "") {
			t.Errorf("%q.IsH264() = %v", c.k, c.k.IsH264())
		}
	}
	for in, want := range map[string]CodecKey{"6400": CodecH264High, "42E0": CodecH264ConstrainedBaseline, "640": "", "64000": "", "zz00": ""} {
		if got := H264CodecKey(in); got != want {
			t.Errorf("H264CodecKey(%q) = %q, want %q", in, got, want)
		}
	}
	for _, k := range []CodecKey{CodecH264ConstrainedBaseline, CodecH264Baseline, CodecH264Main, CodecH264ConstrainedHigh, CodecH264High} {
		if H264CodecKey(k.H264Profile()) != k {
			t.Errorf("%q does not round-trip through its profile", k)
		}
	}
}
