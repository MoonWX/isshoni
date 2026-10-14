package publish

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"

	"github.com/MoonWX/isshoni/internal/media/fake"
)

// The Publisher's H.264 profiles (README S69): several profiles offered in order, SetProfile for a codec hint, the
// answer's choice, and the switch of each layer on a keyframe.

func TestProfilesOptions(t *testing.T) {
	src := newSource(t)
	for _, tc := range []struct {
		name     string
		profile  string
		profiles []string
		want     error
	}{
		{"one more", "6400", []string{"42e0"}, nil},
		{"all five", "42e0", []string{"4200", "4d00", "640c", "6400"}, nil},
		{"six", "42e0", []string{"4200", "4d00", "640c", "6400", "f400"}, ErrInvalidOptions},
		{"Profile again", "6400", []string{"42e0", "6400"}, ErrInvalidOptions},
		{"a repeat", "6400", []string{"42e0", "42e0"}, ErrInvalidOptions},
		{"with a level", "6400", []string{"42e01f"}, ErrInvalidOptions},
		{"upper case", "6400", []string{"42E0"}, ErrInvalidOptions},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := New(Options{Source: src, Profile: tc.profile, Profiles: tc.profiles})
			if !errors.Is(err, tc.want) || (tc.want == nil) != (err == nil) {
				t.Fatalf("New = %v, want %v", err, tc.want)
			}
			if p != nil {
				if err := p.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// videoSection returns an SDP's video m-section.
func videoSection(t *testing.T, raw string) *sdp.MediaDescription {
	t.Helper()
	var desc sdp.SessionDescription
	if err := desc.UnmarshalString(raw); err != nil {
		t.Fatal(err)
	}
	for _, md := range desc.MediaDescriptions {
		if md.MediaName.Media == "video" {
			return md
		}
	}
	t.Fatal("no video m-section")
	return nil
}

// attrsOf returns the values of an m-section's attributes with key that start with prefix.
func attrsOf(md *sdp.MediaDescription, key, prefix string) []string {
	var out []string
	for _, a := range md.Attributes {
		if a.Key == key && strings.HasPrefix(a.Value, prefix) {
			out = append(out, a.Value)
		}
	}
	return out
}

// TestOfferProfiles: every profile is offered, Profile first, each on its payload type with its RTX right after it;
// SetProfile puts another one first, with its feedback and RTX, as setCodecPreferences does in a browser.
func TestOfferProfiles(t *testing.T) {
	newPublisher := func() *Publisher {
		t.Helper()
		p, err := New(Options{
			Source: newSource(t), Audio: true, Profile: "6400", Profiles: []string{"42e0", "4200"}, RedundantRTX: true,
			Settings: loopbackSettings(),
		})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = p.Close() })
		return p
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	p := newPublisher()
	offer := func() *sdp.MediaDescription {
		t.Helper()
		o, err := p.Offer(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return videoSection(t, o.SDP)
	}
	check := func(md *sdp.MediaDescription, formats ...string) {
		t.Helper()
		if !slices.Equal(md.MediaName.Formats, formats) {
			t.Errorf("video formats %v, want %v", md.MediaName.Formats, formats)
		}
		for pt, plid := range map[string]string{"96": "64001f", "98": "42e01f", "100": "42001f"} {
			if got := attrsOf(md, "fmtp", pt+" "); len(got) != 1 || !strings.HasSuffix(got[0], "profile-level-id="+plid) {
				t.Errorf("fmtp of %s: %q, want profile-level-id=%s", pt, got, plid)
			}
			if got := attrsOf(md, "rtcp-fb", pt+" nack pli"); len(got) != 1 {
				t.Errorf("PT %s has no nack pli", pt)
			}
		}
		for rtx, apt := range map[string]string{"97": "96", "99": "98", "101": "100"} {
			if got := attrsOf(md, "fmtp", rtx+" "); !slices.Equal(got, []string{rtx + " apt=" + apt}) {
				t.Errorf("fmtp of %s: %q, want apt=%s", rtx, got, apt)
			}
		}
	}
	check(offer(), "96", "97", "98", "99", "100", "101")
	p = newPublisher() // an offer needs its answer before the next one: a new Publisher for the next
	if err := p.SetProfile("42e0"); err != nil {
		t.Fatal(err)
	}
	check(offer(), "98", "99", "96", "97", "100", "101")
	if err := p.SetProfile("4d00"); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("SetProfile of a profile the Publisher can't send = %v", err)
	}
	audioOnly, err := New(Options{Source: newSource(t), Layers: []string{}, Audio: true})
	if err != nil {
		t.Fatal(err)
	}
	defer audioOnly.Close()
	if err := audioOnly.SetProfile("6400"); !errors.Is(err, ErrInvalidOptions) {
		t.Errorf("SetProfile without video = %v", err)
	}
}

func TestAnswerProfile(t *testing.T) {
	table := h264Table{"6400", "42e0"}
	answer := func(port, formats string, fmtps ...string) string {
		s := "v=0\r\no=- 1 1 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\n" +
			"m=audio 9 UDP/TLS/RTP/SAVPF 98\r\na=mid:1\r\na=rtpmap:98 opus/48000/2\r\n" +
			"m=video " + port + " UDP/TLS/RTP/SAVPF " + formats + "\r\na=mid:0\r\n"
		for _, f := range fmtps {
			pt, fmtp, _ := strings.Cut(f, " ")
			name := "H264/90000"
			if strings.HasPrefix(fmtp, "apt=") {
				name = "rtx/90000"
			}
			s += "a=rtpmap:" + pt + " " + name + "\r\na=fmtp:" + f + "\r\n"
		}
		return s
	}
	cb := "98 " + h264FmtpPrefix + "42e01f"
	high := "96 " + h264FmtpPrefix + "64001f"
	for _, tc := range []struct {
		name, sdp, mid, want string
	}{
		{"the first one", answer("9", "96 98", high, cb), "0", "6400"},
		{"cb alone", answer("9", "98 99", cb, "99 apt=98"), "0", "42e0"},
		{"cb first; the audio's PT 98 is another section's", answer("9", "98 96", cb, high), "0", "42e0"},
		{"one the Publisher can't send first", answer("9", "100 98", "100 "+h264FmtpPrefix+"4d001f", cb), "0", "42e0"},
		{"packetization-mode 0", answer("9", "96", "96 profile-level-id=64001f"), "0", ""},
		{"rejected", answer("0", "96", high), "0", ""},
		{"another mid", answer("9", "96", high), "2", ""},
		{"no mid yet", answer("9", "96", high), "", ""},
		{"not SDP", "nope", "0", ""},
	} {
		if got := answerProfile(tc.sdp, tc.mid, table); got != tc.want {
			t.Errorf("%s: %q, want %q", tc.name, got, tc.want)
		}
	}
}

// TestTrackSwitch: a layer switches to another profile's payload types with its next keyframe, not before, and not
// when it sends that profile already or isn't bound.
func TestTrackSwitch(t *testing.T) {
	w := &recWriter{}
	tr := newTrack(webrtc.RTPCodecTypeVideo, "video", "s", "f", "f", "6400")
	tr.table = h264Table{"6400", "42e0"}
	if tr.switchTo("42e0") {
		t.Error("an unbound track would switch")
	}
	tr.b = &binding{w: w, ssrc: 11, pt: videoPT, rtxSSRC: 22, rtxPT: rtxPT}
	if tr.switchTo("6400") {
		t.Error("a switch to the profile sent now")
	}
	if !tr.switchTo("42e0") {
		t.Fatal("no switch to 42e0")
	}
	keyframe := frame(2)
	keyframe.Keyframe = true
	for _, f := range []fake.Packet{frame(0), frame(1), keyframe, frame(3)} {
		tr.write(f, 0, 0)
	}
	var pts []uint8
	for _, h := range w.hdrs {
		pts = append(pts, h.PayloadType)
	}
	if want := []uint8{96, 96, 96, 96, 96, 96, 98, 98, 98, 98, 98, 98}; !slices.Equal(pts, want) {
		t.Errorf("payload types %v, want %v: the switch on the keyframe", pts, want)
	}
	if tr.b.rtxPT != 99 || tr.b.ssrc != 11 || tr.profile != "42e0" || tr.next != "" {
		t.Errorf("after the switch: binding %+v, profile %s, next %q", tr.b, tr.profile, tr.next)
	}
}

// profileSource records what a Publisher asks of a ProfileSource.
type profileSource struct {
	*fake.Source
	mu        sync.Mutex
	profiles  []string
	keyframes []string
}

func (s *profileSource) SetProfile(profile string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.profiles = append(s.profiles, profile)
	return nil
}

func (s *profileSource) RequestKeyframe(layer string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.keyframes = append(s.keyframes, layer)
}

// TestApplyProfile: the answer's choice is the send loop's to apply: a ProfileSource is switched once, every layer
// is set to switch, and the Source is asked for their keyframes. A plain Source is only asked for the keyframes.
func TestApplyProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile bool
	}{{"profile source", true}, {"plain source", false}} {
		t.Run(tc.name, func(t *testing.T) {
			rec := &profileSource{Source: newSource(t)}
			var src Source = rec
			if !tc.profile {
				src = struct{ Source }{rec} // hides SetProfile
			}
			p, err := New(Options{Source: src, Profile: "6400", Profiles: []string{"42e0"}})
			if err != nil {
				t.Fatal(err)
			}
			defer p.Close()
			for _, tr := range p.video {
				tr.b = &binding{w: &recWriter{}, pt: videoPT}
			}
			p.chosen = "6400" // the answer chose what is sent already
			p.applyProfile()
			p.chosen = "42e0"
			p.applyProfile()
			p.applyProfile() // nothing more to do
			wantProfiles := []string{"42e0"}
			if !tc.profile {
				wantProfiles = nil
			}
			if !slices.Equal(rec.profiles, wantProfiles) || !slices.Equal(rec.keyframes, []string{"f", "q"}) {
				t.Errorf("the source got profiles %v and keyframe requests %v", rec.profiles, rec.keyframes)
			}
			for _, tr := range p.video {
				if tr.next != "42e0" {
					t.Errorf("layer %s will switch to %q", tr.layer, tr.next)
				}
			}
		})
	}
}
