package sfu

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"testing"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
)

const (
	shareA ShareID = "s_aaaaaaaaaaaaaaaa"
	shareB ShareID = "s_bbbbbbbbbbbbbbbb"
	// audioMsid starts the audio m-section's msid line in the Chrome fixture (both sections share the stream id).
	audioMsid = "a=msid:d1a9cf22-4b16-4ff3-8c95-c07ba0d9fffe 605ff851"
)

var (
	video = webrtc.RTPCodecTypeVideo
	audio = webrtc.RTPCodecTypeAudio
	// tracksA binds the Chrome offer's two m-sections to shareA.
	tracksA = []TrackBinding{{MID: "0", Share: shareA, Kind: video}, {MID: "1", Share: shareA, Kind: audio}}
)

// owns returns an ownShare func for the given shares.
func owns(ids ...ShareID) func(ShareID) bool {
	return func(id ShareID) bool { return slices.Contains(ids, id) }
}

// edit applies old → new replacements to an SDP (each old must occur) and returns it.
func edit(t *testing.T, sdp string, pairs ...string) string {
	t.Helper()
	for i := 0; i+1 < len(pairs); i += 2 {
		if !strings.Contains(sdp, pairs[i]) {
			t.Fatalf("fixture has no %q", pairs[i])
		}
		sdp = strings.ReplaceAll(sdp, pairs[i], pairs[i+1])
	}
	return sdp
}

// section returns the m-section of an SDP that starts at the line "m=<media>" with the given mid, for copying.
func section(t *testing.T, sdp, mid string) string {
	t.Helper()
	for part := range strings.SplitSeq(sdp, "\r\nm=") {
		if strings.Contains(part, "\r\na=mid:"+mid+"\r\n") {
			return "m=" + strings.TrimSuffix(part, "\r\n") + "\r\n"
		}
	}
	t.Fatalf("no m-section with mid %s", mid)
	return ""
}

// withMID returns an m-section copy with another mid.
func withMID(t *testing.T, sec, from, to string) string {
	t.Helper()
	return edit(t, sec, "a=mid:"+from+"\r\n", "a=mid:"+to+"\r\n")
}

func wantCode(t *testing.T, name string, err error, code string, share ShareID) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) {
		t.Errorf("%s: got %v, want %s", name, err, code)
		return
	}
	if e.Code != code || e.Share != share || e.Retryable {
		t.Errorf("%s: got code %s share %q retryable %v (%v), want %s share %q", name, e.Code, e.Share, e.Retryable, e,
			code, share)
	}
	if !errors.Is(err, &Error{Code: code}) {
		t.Errorf("%s: errors.Is doesn't match its code", name)
	}
}

func TestCheckPubOfferChrome(t *testing.T) {
	offer, err := checkPubOffer(readSDP(t, "sdp/chrome154-pub-offer.sdp"), tracksA, owns(shareA))
	if err != nil {
		t.Fatal(err)
	}
	if len(offer.sections) != 2 {
		t.Fatalf("got %d sections", len(offer.sections))
	}
	v, a := offer.sections[0], offer.sections[1]
	if v.index != 0 || v.mid != "0" || v.kind != video || v.share != shareA || !slices.Equal(v.rids, []string{"f", "q"}) ||
		!slices.Equal(v.profiles, []ProfileKey{"6400", "4200", "42e0", "4d00"}) {
		t.Errorf("video section %+v", v)
	}
	if a.index != 1 || a.mid != "1" || a.kind != audio || a.share != shareA || a.rids != nil || a.profiles != nil {
		t.Errorf("audio section %+v", a)
	}
	if offer.desc == nil || len(offer.desc.MediaDescriptions) != 2 {
		t.Error("the parsed description must be kept")
	}
}

func TestCheckPubOfferAccepts(t *testing.T) {
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	videoSec, audioSec := section(t, chrome, "0"), section(t, chrome, "1")
	simulcast := "a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\n"
	cases := []struct {
		name   string
		sdp    string
		tracks []TrackBinding
		own    func(ShareID) bool
		check  func(*pubOffer) error
	}{
		{"single layer (no rids)", edit(t, chrome, simulcast, ""), tracksA, owns(shareA), func(o *pubOffer) error {
			if len(o.sections[0].rids) != 0 {
				return fmt.Errorf("rids %v", o.sections[0].rids)
			}
			return nil
		}},
		{"preview layer only", edit(t, chrome, simulcast, "a=rid:q send\r\na=simulcast:send q\r\n"), tracksA, owns(shareA),
			func(o *pubOffer) error {
				if !slices.Equal(o.sections[0].rids, []string{"q"}) {
					return fmt.Errorf("rids %v", o.sections[0].rids)
				}
				return nil
			}},
		{"paused rid and receive rids are fine", edit(t, chrome, simulcast,
			"a=rid:f send\r\na=rid:q send\r\na=rid:h recv\r\na=simulcast:send f;~q recv h\r\n"), tracksA, owns(shareA), nil},
		{"video only", edit(t, chrome, audioSec, "", "a=group:BUNDLE 0 1", "a=group:BUNDLE 0"),
			tracksA[:1], owns(shareA), nil},
		{"audio recvonly needs no binding", edit(t, chrome, "a=sendonly\r\n"+audioMsid, "a=recvonly\r\n"+audioMsid),
			tracksA[:1], owns(shareA), func(o *pubOffer) error {
				if len(o.sections) != 1 {
					return fmt.Errorf("%d sections", len(o.sections))
				}
				return nil
			}},
		{"audio inactive after its share ended", edit(t, chrome, "a=sendonly\r\n"+audioMsid, "a=inactive\r\n"+audioMsid),
			tracksA[:1], owns(shareA), nil},
		{"rejected audio (port 0)", edit(t, chrome, "m=audio 9 ", "m=audio 0 "), tracksA[:1], owns(shareA), nil},
		{"session-level recvonly", strings.Replace(edit(t, chrome, "a=sendonly\r\n", ""), "t=0 0\r\n", "t=0 0\r\na=recvonly\r\n", 1),
			nil, nil, func(o *pubOffer) error {
				if len(o.sections) != 0 {
					return fmt.Errorf("%d sections", len(o.sections))
				}
				return nil
			}},
		{"no direction means sendrecv", edit(t, chrome, "a=sendonly\r\n", ""), tracksA, owns(shareA), nil},
		{"the first direction wins, as in Pion", edit(t, chrome, "a=mid:1\r\na=extmap:12", "a=mid:1\r\na=inactive\r\na=extmap:12"),
			tracksA[:1], owns(shareA), nil},
		{"extra bindings are ignored", chrome, append(slices.Clone(tracksA), TrackBinding{MID: "7", Share: shareB, Kind: video}),
			owns(shareA), nil},
		{"two shares", chrome + withMID(t, videoSec, "0", "2") + withMID(t, audioSec, "1", "3"),
			append(slices.Clone(tracksA), TrackBinding{MID: "2", Share: shareB, Kind: video},
				TrackBinding{MID: "3", Share: shareB, Kind: audio}), owns(shareA, shareB), func(o *pubOffer) error {
				if len(o.sections) != 4 || o.sections[2].share != shareB || o.sections[2].index != 2 {
					return fmt.Errorf("sections %+v", o.sections)
				}
				return nil
			}},
		{"eight m-lines", chrome + withMID(t, audioSec, "1", "2") + withMID(t, audioSec, "1", "3") +
			withMID(t, audioSec, "1", "4") + withMID(t, audioSec, "1", "5") + withMID(t, audioSec, "1", "6") +
			withMID(t, edit(t, audioSec, "a=sendonly", "a=inactive"), "1", "7"),
			append(slices.Clone(tracksA), TrackBinding{MID: "2", Share: "s_2", Kind: audio},
				TrackBinding{MID: "3", Share: "s_3", Kind: audio}, TrackBinding{MID: "4", Share: "s_4", Kind: audio},
				TrackBinding{MID: "5", Share: "s_5", Kind: audio}, TrackBinding{MID: "6", Share: "s_6", Kind: audio}),
			owns(shareA, "s_2", "s_3", "s_4", "s_5", "s_6"), nil},
		{"lowercase h264 and packetization-mode 0 PTs skipped", edit(t, chrome, "a=rtpmap:118 H264/90000", "a=rtpmap:118 h264/90000",
			"a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=1", "a=fmtp:102 level-asymmetry-allowed=1;packetization-mode=0"),
			tracksA, owns(shareA), func(o *pubOffer) error {
				if !slices.Equal(o.sections[0].profiles, []ProfileKey{"6400", "42e0", "4d00"}) {
					return fmt.Errorf("profiles %v", o.sections[0].profiles)
				}
				return nil
			}},
		{"rejected section without a mid", edit(t, chrome, "m=audio 9 ", "m=audio 0 ", "a=mid:1\r\n", ""),
			tracksA[:1], owns(shareA), nil},
		{"two rejected sections without mids", chrome + "m=video 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\n" +
			"m=audio 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\n", tracksA, owns(shareA), nil},
		{"unknown profiles are left out", edit(t, chrome, "profile-level-id=4d001f", "profile-level-id=f4001f"),
			tracksA, owns(shareA), func(o *pubOffer) error {
				if !slices.Equal(o.sections[0].profiles, []ProfileKey{"6400", "4200", "42e0"}) {
					return fmt.Errorf("profiles %v", o.sections[0].profiles)
				}
				return nil
			}},
		{"bundle-only sending section", edit(t, chrome, "m=audio 9 ", "m=audio 0 ", "a=mid:1\r\n", "a=mid:1\r\na=bundle-only\r\n"),
			tracksA, owns(shareA), func(o *pubOffer) error {
				if len(o.sections) != 2 {
					return fmt.Errorf("%d sections", len(o.sections))
				}
				return nil
			}},
		{"LF line endings", strings.ReplaceAll(chrome, "\r\n", "\n"), tracksA, owns(shareA), nil},
	}
	for _, c := range cases {
		o, err := checkPubOffer(c.sdp, c.tracks, c.own)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if c.check != nil {
			if err := c.check(o); err != nil {
				t.Errorf("%s: %v", c.name, err)
			}
		}
	}
}

func TestCheckPubOfferCodes(t *testing.T) {
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	videoSec, audioSec := section(t, chrome, "0"), section(t, chrome, "1")
	simulcast := "a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\n"
	nine := chrome
	for i := 2; i < 9; i++ {
		nine += withMID(t, edit(t, audioSec, "a=sendonly", "a=inactive"), "1", fmt.Sprint(i))
	}
	big := strings.Replace(chrome, "t=0 0\r\n", "t=0 0\r\n"+strings.Repeat("a=x-pad:"+strings.Repeat("p", 100)+"\r\n", 700), 1)
	cases := []struct {
		name   string
		sdp    string
		tracks []TrackBinding
		own    func(ShareID) bool
		code   string
		share  ShareID
	}{
		{"larger than 64 KiB", big, tracksA, owns(shareA), CodeBadSDP, ""},
		{"not SDP", "hello", tracksA, owns(shareA), CodeBadSDP, ""},
		{"empty", "", tracksA, owns(shareA), CodeBadSDP, ""},
		{"nine m-lines", nine, tracksA, owns(shareA), CodeBadSDP, ""},
		{"data channel", chrome + "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\nc=IN IP4 0.0.0.0\r\na=mid:2\r\n" +
			"a=sctp-port:5000\r\n", tracksA, owns(shareA), CodeBadSDP, ""},
		{"rejected data channel", chrome + "m=application 0 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:2\r\n",
			tracksA, owns(shareA), CodeBadSDP, ""},
		{"text section", chrome + "m=text 9 RTP/AVP 98\r\na=mid:2\r\n", tracksA, owns(shareA), CodeBadSDP, ""},
		{"missing mid", edit(t, chrome, "a=mid:1\r\n", ""), tracksA, owns(shareA), CodeBadSDP, ""},
		{"missing mid on an inactive section", edit(t, chrome, "a=mid:1\r\n", "", "a=sendonly\r\n"+audioMsid, "a=inactive\r\n"+audioMsid),
			tracksA[:1], owns(shareA), CodeBadSDP, ""},
		{"repeated mid", edit(t, chrome, "a=mid:1\r\n", "a=mid:0\r\n"), tracksA, owns(shareA), CodeBadSDP, ""},
		{"empty mid", edit(t, chrome, "a=mid:1\r\n", "a=mid:\r\n"), tracksA, owns(shareA), CodeBadSDP, ""},

		{"sending audio not in tracks", chrome, tracksA[:1], owns(shareA), CodeUnknownTrack, ""},
		{"no tracks at all", chrome, nil, owns(shareA), CodeUnknownTrack, ""},
		{"bound with the other kind", chrome, []TrackBinding{tracksA[0], {MID: "1", Share: shareA, Kind: video}},
			owns(shareA), CodeUnknownTrack, shareA},
		{"bound to another connection's share", chrome, tracksA, owns(shareB), CodeUnknownTrack, shareA},
		{"no ownShare func", chrome, tracksA, nil, CodeUnknownTrack, shareA},
		{"one mid bound twice", chrome, append(slices.Clone(tracksA), TrackBinding{MID: "1", Share: shareB, Kind: audio}),
			owns(shareA, shareB), CodeUnknownTrack, shareB},
		{"second video m-line for a share", chrome + withMID(t, videoSec, "0", "2"),
			append(slices.Clone(tracksA), TrackBinding{MID: "2", Share: shareA, Kind: video}), owns(shareA), CodeUnknownTrack, shareA},
		{"second audio m-line for a share", chrome + withMID(t, audioSec, "1", "2"),
			append(slices.Clone(tracksA), TrackBinding{MID: "2", Share: shareA, Kind: audio}), owns(shareA), CodeUnknownTrack, shareA},

		{"VP8 only", edit(t, chrome, "H264/90000", "VP8/90000"), tracksA, owns(shareA), CodeNoH264, shareA},
		{"H.264 packetization-mode 0 only", edit(t, chrome, "packetization-mode=1", "packetization-mode=0"),
			tracksA, owns(shareA), CodeNoH264, shareA},
		{"H.264 without profile-level-id", edit(t, chrome, ";profile-level-id=", ";x-profile="), tracksA, owns(shareA), CodeNoH264, shareA},
		{"H.264 only in profiles outside the PT table", edit(t, chrome, "profile-level-id=64001f", "profile-level-id=f4001f",
			"profile-level-id=42001f", "profile-level-id=f4001f", "profile-level-id=42e01f", "profile-level-id=6e001f",
			"profile-level-id=4d001f", "profile-level-id=58001f"), tracksA, owns(shareA), CodeNoH264, shareA},
		{"no H.264 beats a bad rid", edit(t, chrome, "H264/90000", "VP8/90000", "a=rid:q send", "a=rid:h send"),
			tracksA, owns(shareA), CodeNoH264, shareA},

		{"rid h (M5)", edit(t, chrome, simulcast, "a=rid:f send\r\na=rid:h send\r\na=simulcast:send f;h\r\n"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"three rids", edit(t, chrome, simulcast, "a=rid:f send\r\na=rid:q send\r\na=rid:x send\r\na=simulcast:send f;q;x\r\n"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"unknown rid only in a=simulcast", edit(t, chrome, "a=simulcast:send f;q", "a=simulcast:send f;q,z"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"paused unknown rid", edit(t, chrome, "a=simulcast:send f;q", "a=simulcast:send f;~h"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"Chrome's default rid names", edit(t, chrome, simulcast, "a=rid:0 send\r\na=rid:1 send\r\na=simulcast:send 0;1\r\n"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"rids on audio", edit(t, chrome, "a=mid:1\r\n", "a=mid:1\r\na=rid:f send\r\n"), tracksA, owns(shareA), CodeBadRID, shareA},
	}
	for _, c := range cases {
		o, err := checkPubOffer(c.sdp, c.tracks, c.own)
		if err == nil {
			t.Errorf("%s: accepted (%d sections), want %s", c.name, len(o.sections), c.code)
			continue
		}
		wantCode(t, c.name, err, c.code, c.share)
	}
	if len(big) <= maxPubOfferBytes || len(big) > maxSubAnswerBytes {
		t.Fatalf("the large fixture is %d bytes", len(big))
	}
}

func TestCheckPubOfferMessages(t *testing.T) {
	// Error messages are for logs: positions and counts, never SDP text or client strings.
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	_, err := checkPubOffer(edit(t, chrome, "a=rid:q send", "a=rid:secretrid send", "a=simulcast:send f;q", "a=simulcast:send f;secretrid"),
		tracksA, owns(shareA))
	if err == nil || strings.Contains(err.Error(), "secretrid") || !strings.HasPrefix(err.Error(), CodeBadRID+": ") {
		t.Errorf("message %q", err)
	}
	_, err = checkPubOffer("v=0 secret", nil, nil)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Errorf("message %q", err)
	}
}

// subOfferAndAnswer builds a real sub offer from the sub engine (two video m-lines and one audio m-line) and a
// viewer's answer from a Pion PC with the given video codecs (none = a viewer without any video decoder).
func subOfferAndAnswer(t *testing.T, viewerVideo ...webrtc.RTPCodecParameters) (offer, answer string) {
	t.Helper()
	m, ir, err := newSubEngine()
	if err != nil {
		t.Fatal(err)
	}
	server := newTestPC(t, testAPI(m, ir))
	for _, kind := range []webrtc.RTPCodecType{video, video, audio} {
		if _, err := server.AddTransceiverFromKind(kind, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}); err != nil {
			t.Fatal(err)
		}
	}
	o, err := server.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	vm := &webrtc.MediaEngine{}
	for _, c := range viewerVideo {
		if err := vm.RegisterCodec(c, video); err != nil {
			t.Fatal(err)
		}
	}
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        109,
	}
	if err := vm.RegisterCodec(opus, audio); err != nil {
		t.Fatal(err)
	}
	viewer := newTestPC(t, testAPI(vm, &interceptor.Registry{}))
	if err := viewer.SetRemoteDescription(o); err != nil {
		t.Fatal(err)
	}
	a, err := viewer.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return o.SDP, a.SDP
}

func TestCheckSubAnswer(t *testing.T) {
	_, withH264 := subOfferAndAnswer(t, h264(126, "42e01f"))
	if got, err := checkSubAnswer(withH264); err != nil || len(got) != 0 {
		t.Errorf("CB viewer: %v, %v", got, err)
	}
	vp8 := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeVP8, ClockRate: 90000}, PayloadType: 120,
	}
	_, noH264 := subOfferAndAnswer(t, vp8)
	// Pion rejects both video m-lines (port 0, no mid).
	got, err := checkSubAnswer(noH264)
	if err != nil || !slices.Equal(got, []int{0, 1}) {
		t.Errorf("viewer without H.264 (Pion answer): %v, %v; want [0 1]", got, err)
	}

	// A hand-written answer like a fresh Firefox profile's: one video m-line with VP8 only, one rejected, one with
	// H.264 packetization-mode 0 only, one fine, plus audio.
	firefox := crlf(`v=0
o=mozilla...THIS_IS_SDPARTA-99.0 1 0 IN IP4 0.0.0.0
s=-
t=0 0
a=group:BUNDLE 0 2 3 4
m=video 9 UDP/TLS/RTP/SAVPF 120
c=IN IP4 0.0.0.0
a=mid:0
a=recvonly
a=rtpmap:120 VP8/90000
m=video 0 UDP/TLS/RTP/SAVPF 120
c=IN IP4 0.0.0.0
a=mid:1
a=inactive
a=rtpmap:120 VP8/90000
m=video 9 UDP/TLS/RTP/SAVPF 97
c=IN IP4 0.0.0.0
a=mid:2
a=recvonly
a=rtpmap:97 H264/90000
a=fmtp:97 profile-level-id=42e01f;level-asymmetry-allowed=1
m=video 9 UDP/TLS/RTP/SAVPF 126
c=IN IP4 0.0.0.0
a=mid:3
a=recvonly
a=rtpmap:126 H264/90000
a=fmtp:126 profile-level-id=42e01f;level-asymmetry-allowed=1;packetization-mode=1
m=audio 9 UDP/TLS/RTP/SAVPF 109
c=IN IP4 0.0.0.0
a=mid:4
a=recvonly
a=rtpmap:109 opus/48000/2
`)
	got, err = checkSubAnswer(firefox)
	if err != nil || !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("Firefox-like answer: %v, %v; want [0 1 2]", got, err)
	}
	if got, err := checkSubAnswer(edit(t, firefox, "a=mid:1\r\n", "")); err != nil || !slices.Equal(got, []int{0, 1, 2}) {
		t.Errorf("answer without a mid: %v, %v", got, err)
	}

	bad := []struct{ name, sdp string }{
		{"larger than 256 KiB", strings.Replace(withH264, "t=0 0\r\n", "t=0 0\r\n"+strings.Repeat("a=x-pad:"+strings.Repeat("p", 100)+"\r\n", 2700), 1)},
		{"not SDP", "nope"},
		{"empty", " \r\n"},
		{"data channel", withH264 + "m=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\na=mid:9\r\n"},
		{"text section", withH264 + "m=text 9 RTP/AVP 98\r\na=mid:9\r\n"},
	}
	for _, c := range bad {
		_, err := checkSubAnswer(c.sdp)
		wantCode(t, c.name, err, CodeBadSDP, "")
	}
	// Many m-lines are fine in a sub answer (one pair per subscription): only the pub offer has the 8 m-line limit.
	many := withH264
	audioSec := section(t, withH264, "2")
	for i := 3; i < 20; i++ {
		many += withMID(t, audioSec, "2", fmt.Sprint(i))
	}
	if _, err := checkSubAnswer(many); err != nil {
		t.Errorf("a sub answer with 20 m-lines: %v", err)
	}
}

// pubAnswer returns the pub engine's answer to the Chrome fixture.
func pubAnswer(t *testing.T) string {
	t.Helper()
	m, ir, err := newPubEngine()
	if err != nil {
		t.Fatal(err)
	}
	pc := newTestPC(t, testAPI(m, ir))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer,
		SDP: readSDP(t, "sdp/chrome154-pub-offer.sdp")}); err != nil {
		t.Fatal(err)
	}
	a, err := pc.CreateAnswer(nil)
	if err != nil {
		t.Fatal(err)
	}
	return a.SDP
}

// lineDiff returns the lines of b that aren't in a, and the lines of a that aren't in b.
func lineDiff(a, b string) (added, removed []string) {
	count := map[string]int{}
	for l := range strings.SplitSeq(a, "\r\n") {
		count[l]++
	}
	for l := range strings.SplitSeq(b, "\r\n") {
		if count[l] > 0 {
			count[l]--
			continue
		}
		added = append(added, l)
	}
	for _, l := range slices.Sorted(maps.Keys(count)) {
		for range count[l] {
			removed = append(removed, l)
		}
	}
	return added, removed
}

func TestSetOpusAnswerParams(t *testing.T) {
	answer := pubAnswer(t)
	// Pion's answer carries Chrome's Opus fmtp, without stereo: this is why the edit sets stereo too.
	if !strings.Contains(answer, "a=fmtp:111 minptime=10;useinbandfec=1\r\n") {
		t.Fatalf("unexpected Opus fmtp in Pion's answer:\n%s", answer)
	}
	same, err := setOpusAnswerParams(answer, nil)
	if err != nil || same != answer {
		t.Errorf("no bitrates: the SDP must round-trip unchanged (%v)", err)
	}
	for _, br := range []int{128000, 256000} {
		got, err := setOpusAnswerParams(answer, map[string]int{"1": br, "0": 999})
		if err != nil {
			t.Fatal(err)
		}
		added, removed := lineDiff(answer, got)
		wantLine := fmt.Sprintf("a=fmtp:111 minptime=10;useinbandfec=1;stereo=1;sprop-stereo=1;maxaveragebitrate=%d", br)
		if !slices.Equal(added, []string{wantLine}) || !slices.Equal(removed, []string{"a=fmtp:111 minptime=10;useinbandfec=1"}) {
			t.Errorf("bitrate %d: added %q, removed %q", br, added, removed)
		}
	}
	for _, m := range []map[string]int{{"1": 0}, {"1": -5}, {"9": 256000}, {"0": 256000}} {
		if got, err := setOpusAnswerParams(answer, m); err != nil || got != answer {
			t.Errorf("%v: the answer must stay unchanged (%v)", m, err)
		}
	}

	// Hand-written cases: existing parameters are replaced in place (names without case), a missing fmtp is added,
	// two audio m-sections get their own bitrates, and non-Opus codecs are left alone.
	sdp := crlf(`v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111 0
a=mid:a
a=rtpmap:111 OPUS/48000/2
a=fmtp:111 MaxAverageBitrate=64000;stereo=0;;minptime=10
a=rtpmap:0 PCMU/8000
a=fmtp:0 maxaveragebitrate=1
m=audio 9 UDP/TLS/RTP/SAVPF 96
a=mid:b
a=rtpmap:96 opus/48000/2
m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:c
a=rtpmap:111 opus/48000/2
a=fmtp:111 minptime=10
`)
	got, err := setOpusAnswerParams(sdp, map[string]int{"a": 256000, "b": 128000})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"a=fmtp:111 maxaveragebitrate=256000;stereo=1;minptime=10;sprop-stereo=1\r\n",
		"a=fmtp:0 maxaveragebitrate=1\r\n",
		"a=mid:b\r\na=rtpmap:96 opus/48000/2\r\na=fmtp:96 stereo=1;sprop-stereo=1;maxaveragebitrate=128000\r\n",
		"a=mid:c\r\na=rtpmap:111 opus/48000/2\r\na=fmtp:111 minptime=10\r\n",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in\n%s", want, got)
		}
	}
	if _, err := setOpusAnswerParams("garbage", map[string]int{"a": 1}); !errors.Is(err, &Error{Code: CodeBadSDP}) {
		t.Errorf("garbage: %v", err)
	}
}

func TestSetFmtpParams(t *testing.T) {
	set := []fmtpKV{{"stereo", "1"}, {"maxaveragebitrate", "256000"}}
	cases := []struct{ in, want string }{
		{"", "stereo=1;maxaveragebitrate=256000"},
		{"minptime=10", "minptime=10;stereo=1;maxaveragebitrate=256000"},
		{"STEREO=0;minptime=10", "stereo=1;minptime=10;maxaveragebitrate=256000"},
		{"maxaveragebitrate=1;;stereo", "maxaveragebitrate=256000;stereo=1"},
		{" stereo = 0 ", "stereo=1;maxaveragebitrate=256000"},
	}
	for _, c := range cases {
		if got := setFmtpParams(c.in, set); got != c.want {
			t.Errorf("setFmtpParams(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func FuzzSDPCheck(f *testing.F) {
	f.Add(readSDP(f, "sdp/chrome154-pub-offer.sdp"))
	f.Add("v=0\r\no=- 1 2 IN IP4 127.0.0.1\r\ns=-\r\nt=0 0\r\nm=video 9 UDP/TLS/RTP/SAVPF 96\r\na=mid:0\r\n" +
		"a=rtpmap:96 H264/90000\r\na=fmtp:96 packetization-mode=1;profile-level-id=42e01f\r\na=rid:f send\r\n" +
		"a=simulcast:send f;~q,h\r\nm=audio 9 UDP/TLS/RTP/SAVPF 111\r\na=mid:1\r\na=rtpmap:111 opus/48000/2\r\n" +
		"a=fmtp:111 minptime=10\r\n")
	f.Add("v=0\r\no=- 1 2 IN IP4 0.0.0.0\r\ns=-\r\nt=0 0\r\nm=application 9 UDP/DTLS/SCTP webrtc-datachannel\r\n")
	f.Add("")
	tracks := []TrackBinding{
		{MID: "0", Share: shareA, Kind: video}, {MID: "1", Share: shareA, Kind: audio},
		{MID: "2", Share: shareB, Kind: video}, {MID: "3", Share: shareB, Kind: audio},
	}
	f.Fuzz(func(t *testing.T, raw string) {
		offer, err := checkPubOffer(raw, tracks, owns(shareA, shareB))
		if err != nil {
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("checkPubOffer: %v is not an *Error", err)
			}
		} else {
			if len(offer.sections) > maxPubMLines {
				t.Fatalf("%d sections", len(offer.sections))
			}
			for _, s := range offer.sections {
				if s.share != shareA && s.share != shareB {
					t.Fatalf("section bound to %q", s.share)
				}
				switch s.kind {
				case video:
					if len(s.profiles) == 0 || len(s.rids) > maxRIDs {
						t.Fatalf("video section %+v", s)
					}
					for _, r := range s.rids {
						if r != ridFull && r != ridPreview {
							t.Fatalf("rid %q accepted", r)
						}
					}
				case audio:
					if len(s.rids) != 0 {
						t.Fatalf("audio section %+v", s)
					}
				default:
					t.Fatalf("section kind %v", s.kind)
				}
			}
		}
		if _, err := checkSubAnswer(raw); err != nil {
			var e *Error
			if !errors.As(err, &e) {
				t.Fatalf("checkSubAnswer: %v is not an *Error", err)
			}
		}
		if len(raw) <= maxPubOfferBytes {
			_, _ = setOpusAnswerParams(raw, map[string]int{"0": 1, "1": 256000})
		}
	})
}
