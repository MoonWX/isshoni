package sfu

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/sdp/v3"
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
func edit(t testing.TB, sdp string, pairs ...string) string {
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

// acceptCase is a pub offer that checkPubOffer accepts, with an optional check of the result.
type acceptCase struct {
	name   string
	sdp    string
	tracks []TrackBinding
	own    func(ShareID) bool
	check  func(*pubOffer) error
}

// pubAcceptCases are the offers TestCheckPubOfferAccepts expects to pass. TestCheckPubOfferMatchesPion answers each
// with the pub engine to check that Pion reads them the same way.
func pubAcceptCases(t *testing.T) []acceptCase {
	t.Helper()
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	videoSec, audioSec := section(t, chrome, "0"), section(t, chrome, "1")
	simulcast := "a=rid:f send\r\na=rid:q send\r\na=simulcast:send f;q\r\n"
	return []acceptCase{
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
		{"a paused rid", edit(t, chrome, "a=simulcast:send f;q", "a=simulcast:send f;~q"), tracksA, owns(shareA), nil},
		{"rid parameters and alternatives", edit(t, chrome, simulcast,
			"a=rid:f send pt=118;max-width=1920\r\na=rid:q send\r\na=simulcast:send f,q\r\n"), tracksA, owns(shareA),
			func(o *pubOffer) error {
				if !slices.Equal(o.sections[0].rids, []string{"f", "q"}) {
					return fmt.Errorf("rids %v", o.sections[0].rids)
				}
				return nil
			}},
		{"the rid direction is ignored, as in Pion", edit(t, chrome, simulcast,
			"a=rid:q recv\r\na=rid:f recv\r\na=simulcast:recv q;f\r\n"), tracksA, owns(shareA), func(o *pubOffer) error {
			if !slices.Equal(o.sections[0].rids, []string{"q", "f"}) {
				return fmt.Errorf("rids %v", o.sections[0].rids)
			}
			return nil
		}},
		{"rids without a=simulcast", edit(t, chrome, "a=simulcast:send f;q\r\n", ""), tracksA, owns(shareA), nil},
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
		{"stopped audio (port 0, inactive)", edit(t, chrome, "m=audio 9 ", "m=audio 0 ", "a=sendonly\r\n"+audioMsid,
			"a=inactive\r\n"+audioMsid), tracksA[:1], owns(shareA), func(o *pubOffer) error {
			if len(o.sections) != 1 {
				return fmt.Errorf("%d sections", len(o.sections))
			}
			return nil
		}},
		{"port 0 without bundle-only still sends, as in Pion", edit(t, chrome, "m=audio 9 ", "m=audio 0 "), tracksA,
			owns(shareA), func(o *pubOffer) error {
				if len(o.sections) != 2 {
					return fmt.Errorf("%d sections", len(o.sections))
				}
				return nil
			}},
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
		{"two stopped sections", chrome + "m=video 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:2\r\na=inactive\r\n" +
			"m=audio 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\na=mid:3\r\na=inactive\r\n", tracksA, owns(shareA), nil},
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

		// Sending m-sections without a TrackRef (their share ended in a race, 01 §9 rule 4) are answered inactive and
		// otherwise ignored.
		{"sending audio not in tracks", chrome, tracksA[:1], owns(shareA), wantUnbound(1, "1")},
		// Pion ignores the port of an offer's m-section and receives on it all the same.
		{"sending audio with port 0 not in tracks", edit(t, chrome, "m=audio 9 ", "m=audio 0 "), tracksA[:1],
			owns(shareA), wantUnbound(1, "1")},
		{"no tracks at all", chrome, nil, owns(shareA), wantUnbound(0, "0", "1")},
		{"no tracks and no ownShare func", chrome, nil, nil, wantUnbound(0, "0", "1")},
		// Only an unbound section's rids are checked (TestCheckPubOfferCodes), not its codecs.
		{"an unbound video section without H.264", edit(t, chrome, "H264/90000", "VP8/90000"), tracksA[1:], owns(shareA),
			wantUnbound(1, "0")},
		{"an unbound single-layer video section", edit(t, chrome, simulcast, ""), tracksA[1:], owns(shareA),
			wantUnbound(1, "0")},
		{"one share bound, one unbound", chrome + withMID(t, videoSec, "0", "2") + withMID(t, audioSec, "1", "3"),
			append(slices.Clone(tracksA), TrackBinding{MID: "3", Share: shareB, Kind: audio}), owns(shareA, shareB),
			wantUnbound(3, "2")},
	}
}

// wantUnbound returns a check that an offer has n bound sections and exactly the given unbound mids.
func wantUnbound(n int, mids ...string) func(*pubOffer) error {
	return func(o *pubOffer) error {
		if len(o.sections) != n || !slices.Equal(o.unbound, mids) {
			return fmt.Errorf("%d sections, unbound %q; want %d and %q", len(o.sections), o.unbound, n, mids)
		}
		return nil
	}
}

func TestCheckPubOfferAccepts(t *testing.T) {
	for _, c := range pubAcceptCases(t) {
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
		// Pion's SetRemoteDescription refuses any m-section without a mid, rejected ones included.
		{"rejected section without a mid", edit(t, chrome, "m=audio 9 ", "m=audio 0 ", "a=mid:1\r\n", ""),
			tracksA[:1], owns(shareA), CodeBadSDP, ""},
		{"two rejected sections without mids", chrome + "m=video 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\n" +
			"m=audio 0 UDP/TLS/RTP/SAVPF 0\r\nc=IN IP4 0.0.0.0\r\n", tracksA, owns(shareA), CodeBadSDP, ""},
		{"missing mid on an inactive section", edit(t, chrome, "a=mid:1\r\n", "", "a=sendonly\r\n"+audioMsid, "a=inactive\r\n"+audioMsid),
			tracksA[:1], owns(shareA), CodeBadSDP, ""},
		{"repeated mid", edit(t, chrome, "a=mid:1\r\n", "a=mid:0\r\n"), tracksA, owns(shareA), CodeBadSDP, ""},
		{"empty mid", edit(t, chrome, "a=mid:1\r\n", "a=mid:\r\n"), tracksA, owns(shareA), CodeBadSDP, ""},

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
		// Pion takes every a=rid line, whatever its direction, and answers each as a receive rid.
		{"receive rids count", edit(t, chrome, simulcast,
			"a=rid:f send\r\na=rid:q send\r\na=rid:h recv\r\na=simulcast:send f;~q recv h\r\n"), tracksA, owns(shareA),
			CodeBadRID, shareA},
		{"three receive rids after f and q", edit(t, chrome, "a=simulcast:send f;q\r\n",
			"a=rid:h recv\r\na=rid:x recv\r\na=rid:y recv\r\na=simulcast:send f;q\r\n"), tracksA, owns(shareA), CodeBadRID, shareA},
		{"a repeated rid", edit(t, chrome, simulcast, "a=rid:f send\r\na=rid:f send\r\na=rid:f send\r\na=simulcast:send f\r\n"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"a receive rid on audio", edit(t, chrome, "a=mid:1\r\n", "a=mid:1\r\na=rid:f recv\r\n"), tracksA, owns(shareA),
			CodeBadRID, shareA},
		{"a=simulcast on audio", edit(t, chrome, "a=mid:1\r\n", "a=mid:1\r\na=simulcast:send f\r\n"), tracksA, owns(shareA),
			CodeBadRID, shareA},
		{"a=simulcast without a=rid lines", edit(t, chrome, "a=rid:f send\r\na=rid:q send\r\n", ""), tracksA, owns(shareA),
			CodeBadRID, shareA},
		{"a receive simulcast rid without an a=rid line", edit(t, chrome, "a=simulcast:send f;q", "a=simulcast:send f;q recv h"),
			tracksA, owns(shareA), CodeBadRID, shareA},
		{"an empty rid", edit(t, chrome, "a=rid:q send", "a=rid: send"), tracksA, owns(shareA), CodeBadRID, shareA},

		// An unbound sending section maps to no share, but Pion answers and receives its rids all the same: the rid
		// guard holds for it too.
		{"an unbound section with bad rids", edit(t, chrome, "a=rid:q send", "a=rid:h send",
			"a=simulcast:send f;q", "a=simulcast:send f;h;x"), tracksA[1:], owns(shareA), CodeBadRID, ""},
		{"an unbound section with many rids", edit(t, chrome, simulcast,
			strings.Repeat("a=rid:f send\r\n", 400)+"a=simulcast:send f\r\n"), tracksA[1:], owns(shareA), CodeBadRID, ""},
		{"an unbound section without H.264 and with bad rids", edit(t, chrome, "H264/90000", "VP8/90000",
			"a=rid:q send", "a=rid:x send", "a=simulcast:send f;q", "a=simulcast:send f;x"), nil, nil, CodeBadRID, ""},
		{"a simulcast rid without an a=rid line, unbound", edit(t, chrome, "a=simulcast:send f;q", "a=simulcast:send f;q;z"),
			tracksA[1:], owns(shareA), CodeBadRID, ""},
		{"rids on unbound audio", edit(t, chrome, "a=mid:1\r\n", "a=mid:1\r\na=rid:f send\r\n"), tracksA[:1], owns(shareA),
			CodeBadRID, ""},
		{"a=simulcast on unbound audio", edit(t, chrome, "a=mid:1\r\n", "a=mid:1\r\na=simulcast:send f\r\n"), tracksA[:1],
			owns(shareA), CodeBadRID, ""},
		{"a bad unbound section before a bad bound one", edit(t, chrome, "a=rid:q send", "a=rid:h send",
			"a=simulcast:send f;q", "a=simulcast:send f;h", "a=mid:1\r\n", "a=mid:1\r\na=rid:f send\r\n"), tracksA[1:],
			owns(shareA), CodeBadRID, ""},
		{"a bad bound section before a bad unbound one", edit(t, chrome, "a=rid:q send", "a=rid:h send",
			"a=simulcast:send f;q", "a=simulcast:send f;h", "a=mid:1\r\n", "a=mid:1\r\na=rid:f send\r\n"), tracksA[:1],
			owns(shareA), CodeBadRID, shareA},
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
	a, err := pionPubAnswer(t, readSDP(t, "sdp/chrome154-pub-offer.sdp"))
	if err != nil {
		t.Fatal(err)
	}
	return a.SDP
}

// pionPubAnswer answers an offer with a new PC of the pub engine: what Pion makes of an offer.
func pionPubAnswer(t *testing.T, offer string) (webrtc.SessionDescription, error) {
	t.Helper()
	m, ir, err := newPubEngine()
	if err != nil {
		t.Fatal(err)
	}
	pc := newTestPC(t, testAPI(m, ir))
	if err := pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: offer}); err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("SetRemoteDescription: %w", err)
	}
	a, err := pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, fmt.Errorf("CreateAnswer: %w", err)
	}
	return a, nil
}

// ridIDs returns the ids of an m-section's a=rid lines, read as Pion reads them: the first space-separated field.
func ridIDs(md *sdp.MediaDescription) []string {
	var ids []string
	for _, a := range md.Attributes {
		if a.Key == "rid" {
			ids = append(ids, strings.Split(a.Value, " ")[0])
		}
	}
	return ids
}

// TestPionRefusesSectionsWithoutMid pins down why checkPubOffer asks every m-section for a mid, rejected ones
// included: Pion's SetRemoteDescription would fail after the pub PC was created (sfu.internal, retryable) instead of
// the offer being refused with nothing changed (sfu.bad_sdp).
func TestPionRefusesSectionsWithoutMid(t *testing.T) {
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	offer := edit(t, chrome, "m=audio 9 ", "m=audio 0 ", "a=mid:1\r\n", "")
	if _, err := pionPubAnswer(t, offer); err == nil {
		t.Error("Pion accepts a rejected m-section without a mid: checkPubOffer may allow it again")
	}
	_, err := checkPubOffer(offer, tracksA[:1], owns(shareA))
	wantCode(t, "rejected section without a mid", err, CodeBadSDP, "")
}

// TestCheckPubOfferMatchesPion answers every offer of pubAcceptCases with the pub engine and checks that Pion reads
// it the way checkPubOffer did: Pion receives (answers recvonly) on exactly the m-sections in offer.sections and
// offer.unbound (unless it rejects an unbound one it has no codec for), the rids it answers on each bound section are
// the section's rids, and each bound video section's answer keeps an H.264 codec. After setAnswerInactive, the
// answer receives on exactly the bound sections.
func TestCheckPubOfferMatchesPion(t *testing.T) {
	for _, c := range pubAcceptCases(t) {
		o, err := checkPubOffer(c.sdp, c.tracks, c.own)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		a, err := pionPubAnswer(t, c.sdp)
		if err != nil {
			t.Errorf("%s: Pion refuses an accepted offer: %v", c.name, err)
			continue
		}
		answer, err := parseSDP(a.SDP)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		edited, err := setAnswerInactive(a.SDP, o.unbound)
		if err != nil {
			t.Fatalf("%s: setAnswerInactive: %v", c.name, err)
		}
		client, err := parseSDP(edited)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if len(answer.MediaDescriptions) != len(o.desc.MediaDescriptions) {
			t.Errorf("%s: %d answer m-sections for %d offered", c.name, len(answer.MediaDescriptions),
				len(o.desc.MediaDescriptions))
			continue
		}
		for i, md := range answer.MediaDescriptions {
			k := slices.IndexFunc(o.sections, func(s pubSection) bool { return s.index == i })
			mid, _ := o.desc.MediaDescriptions[i].Attribute(sdp.AttrKeyMID)
			unbound := slices.Contains(o.unbound, mid)
			dir := direction(md.Attributes)
			receives := dir == sdp.AttrKeyRecvOnly || dir == sdp.AttrKeySendRecv
			if receives != (k >= 0 || unbound) && (!unbound || md.MediaName.Port.Value != 0) {
				t.Errorf("%s: m-line %d: Pion answers %s, checkPubOffer counts it as sending: %v", c.name, i, dir,
					k >= 0 || unbound)
				continue
			}
			// An unbound m-section that Pion rejected (port 0, and no mid in Pion's answer) needs no a=inactive.
			cdir := direction(client.MediaDescriptions[i].Attributes)
			rejected := md.MediaName.Port.Value == 0 && dir == ""
			if unbound && !rejected && cdir != sdp.AttrKeyInactive || !unbound && cdir != dir {
				t.Errorf("%s: m-line %d: the client's answer says %q, Pion's %q (unbound: %v)", c.name, i, cdir, dir,
					unbound)
			}
			if k < 0 {
				// Pion answers an unbound section's rids too (unless it rejects the section): the guard holds for them.
				if unbound && !guardedRIDs(mediaKind(md), ridIDs(md)) {
					t.Errorf("%s: m-line %d: Pion answers rids %q on an unbound section", c.name, i, ridIDs(md))
				}
				continue
			}
			if got := ridIDs(md); !slices.Equal(got, o.sections[k].rids) {
				t.Errorf("%s: m-line %d: Pion answers rids %q, checkPubOffer read %q", c.name, i, got, o.sections[k].rids)
			}
			if o.sections[k].kind == video && len(rtpmapPTs(md, "h264")) == 0 {
				t.Errorf("%s: m-line %d: Pion's answer has no H.264", c.name, i)
			}
		}
	}
}

// TestOfferedProfilesMatchPion checks that offeredProfiles reads rtpmap and fmtp lines the way Pion negotiates them:
// for each variant of one H.264 payload type (120), offeredProfiles of a video m-line with only that PT is non-empty
// exactly when Pion's answer keeps PT 120 (and checkPubOffer returns sfu.no_h264 exactly when it doesn't). The offer
// Pion answers also carries a control PT (125, Constrained Baseline) that always matches: Pion falls back to a partial
// match (name and clock rate only) when no offered codec matches exactly, and the control turns that fallback off.
func TestOfferedProfilesMatchPion(t *testing.T) {
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	start, end := strings.Index(chrome, "a=rtpmap:118 "), strings.Index(chrome, "a=rid:f send")
	head, tail := chrome[:start], chrome[end:]
	mline := "m=video 9 UDP/TLS/RTP/SAVPF 118 119 102 103 108 109 116 117\r\n"
	build := func(formats, codecLines string) string {
		return edit(t, head, mline, "m=video 9 UDP/TLS/RTP/SAVPF "+formats+"\r\n") + crlf(codecLines) + tail
	}
	const control = "a=rtpmap:125 H264/90000\na=fmtp:125 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42e01f\n"
	variants := []struct {
		name, lines string
		want        bool
	}{
		{"Chrome's High", "a=rtpmap:120 H264/90000\na=fmtp:120 level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=64001f\n", true},
		{"lowercase name and parameters", "a=rtpmap:120 h264/90000\na=fmtp:120 PACKETIZATION-MODE=1;PROFILE-LEVEL-ID=4D001F\n", true},
		{"no clock rate", "a=rtpmap:120 H264\na=fmtp:120 packetization-mode=1;profile-level-id=64001f\n", true},
		{"spaces around items", "a=rtpmap:120 H264/90000\na=fmtp:120  packetization-mode=1 ; profile-level-id=640c1f \n", true},
		{"space before =", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode =1;profile-level-id=64001f\n", false},
		{"space after =", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode= 1;profile-level-id=64001f\n", false},
		{"space in profile-level-id", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id= 64001f\n", false},
		{"odd profile-level-id", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=64001\n", false},
		{"non-hex level", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=6400zz\n", false},
		{"profile without level", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=4200\n", true},
		{"profile outside the table", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=f4001f\n", false},
		{"packetization-mode 0", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=0;profile-level-id=42e01f\n", false},
		{"packetization-mode 01", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=01;profile-level-id=42e01f\n", false},
		{"no fmtp", "a=rtpmap:120 H264/90000\n", false},
		{"the last repeated parameter wins",
			"a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=0;profile-level-id=42e01f;packetization-mode=1\n", true},
		{"the first fmtp line wins", "a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=0;profile-level-id=42e01f\n" +
			"a=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", false},
		{"an empty fmtp line doesn't count", "a=rtpmap:120 H264/90000\na=fmtp:120 \n" +
			"a=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", true},
		{"the first rtpmap line wins (VP8)", "a=rtpmap:120 VP8/90000\na=rtpmap:120 H264/90000\n" +
			"a=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", false},
		{"the first rtpmap line wins (H.264)", "a=rtpmap:120 H264/90000\na=rtpmap:120 VP8/90000\n" +
			"a=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", true},
		{"an rtpmap that doesn't parse doesn't count", "a=rtpmap:120 H264/fast\na=rtpmap:120 VP8/90000\n" +
			"a=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", false},
		{"an rtpmap with two spaces doesn't count", "a=rtpmap:120  H264/90000\na=rtpmap:120 H264/90000 x\n" +
			"a=rtpmap:120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", true},
		{"a PT number with a leading zero", "a=rtpmap:0120 H264/90000\na=fmtp:120 packetization-mode=1;profile-level-id=42e01f\n", true},
	}
	for _, v := range variants {
		alone := build("120", v.lines)
		desc, err := parseSDP(alone)
		if err != nil {
			t.Fatalf("%s: %v", v.name, err)
		}
		if got := len(offeredProfiles(desc.MediaDescriptions[0])) > 0; got != v.want {
			t.Errorf("%s: offeredProfiles says H.264 %v, want %v", v.name, got, v.want)
		}
		_, err = checkPubOffer(alone, tracksA, owns(shareA))
		if v.want && err != nil {
			t.Errorf("%s: %v", v.name, err)
		} else if !v.want {
			wantCode(t, v.name, err, CodeNoH264, shareA)
		}
		a, err := pionPubAnswer(t, build("120 125", v.lines+control))
		if err != nil {
			t.Errorf("%s: %v", v.name, err)
			continue
		}
		answer, err := parseSDP(a.SDP)
		if err != nil {
			t.Fatal(err)
		}
		formats := answer.MediaDescriptions[0].MediaName.Formats
		if !slices.Contains(formats, "125") {
			t.Errorf("%s: Pion dropped the control PT: %v", v.name, formats)
		}
		if got := slices.Contains(formats, "120"); got != v.want {
			t.Errorf("%s: Pion keeps PT 120: %v, want %v", v.name, got, v.want)
		}
	}
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

func TestSetAnswerInactive(t *testing.T) {
	answer := pubAnswer(t)
	if same, err := setAnswerInactive(answer, nil); err != nil || same != answer {
		t.Errorf("no mids: the SDP must come back unchanged (%v)", err)
	}
	got, err := setAnswerInactive(answer, []string{"1", "9"}) // there is no mid 9
	if err != nil {
		t.Fatal(err)
	}
	added, removed := lineDiff(answer, got)
	if !slices.Equal(added, []string{"a=inactive"}) || !slices.Equal(removed, []string{"a=recvonly"}) {
		t.Errorf("the audio m-section must only turn inactive: added %q, removed %q", added, removed)
	}
	checkInactive(t, answer, []string{"1"})

	// Without a direction attribute one is added; several become one a=inactive in the place of the first.
	odd := crlf(`v=0
o=- 1 2 IN IP4 127.0.0.1
s=-
t=0 0
m=audio 9 UDP/TLS/RTP/SAVPF 111
a=mid:0
a=rtpmap:111 opus/48000/2
m=video 9 UDP/TLS/RTP/SAVPF 96
a=mid:1
a=sendrecv
a=rtpmap:96 H264/90000
a=recvonly
`)
	got, err = setAnswerInactive(odd, []string{"0", "1"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "a=rtpmap:111 opus/48000/2\r\na=inactive\r\n") ||
		!strings.Contains(got, "a=mid:1\r\na=inactive\r\na=rtpmap:96 H264/90000\r\n") || strings.Contains(got, "only") {
		t.Errorf("unexpected result:\n%s", got)
	}
	checkInactive(t, odd, []string{"0", "1"})
	_, err = setAnswerInactive("nope", []string{"0"})
	wantCode(t, "not SDP", err, CodeBadSDP, "")
}

// TestUnboundSectionPionRoundTrip plays the race of 01 §9 rule 4 with Pion on both sides: a publisher offers video and
// audio, and the audio's share ended, so tracks binds only the video. checkPubOffer accepts the offer with the audio
// unbound; the SFU's PC answers it (recvonly on both); the client's copy makes the audio inactive, and the publisher
// accepts that answer.
func TestUnboundSectionPionRoundTrip(t *testing.T) {
	pm := &webrtc.MediaEngine{}
	opus := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2},
		PayloadType:        111,
	}
	for _, c := range []struct {
		codec webrtc.RTPCodecParameters
		kind  webrtc.RTPCodecType
	}{{h264(102, "42e01f"), video}, {opus, audio}} {
		if err := pm.RegisterCodec(c.codec, c.kind); err != nil {
			t.Fatal(err)
		}
	}
	pub := newTestPC(t, testAPI(pm, &interceptor.Registry{}))
	for _, kind := range []webrtc.RTPCodecType{video, audio} {
		sendonly := webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly}
		if _, err := pub.AddTransceiverFromKind(kind, sendonly); err != nil {
			t.Fatal(err)
		}
	}
	offer, err := pub.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := pub.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	o, err := checkPubOffer(offer.SDP, tracksA[:1], owns(shareA))
	if err != nil || len(o.sections) != 1 || !slices.Equal(o.unbound, []string{"1"}) {
		t.Fatalf("checkPubOffer: %v", err)
	}

	answer, err := pionPubAnswer(t, offer.SDP)
	if err != nil {
		t.Fatal(err)
	}
	client, err := setAnswerInactive(answer.SDP, o.unbound)
	if err != nil {
		t.Fatal(err)
	}
	desc, err := parseSDP(client)
	if err != nil {
		t.Fatal(err)
	}
	v, a := direction(desc.MediaDescriptions[0].Attributes), direction(desc.MediaDescriptions[1].Attributes)
	if v != sdp.AttrKeyRecvOnly || a != sdp.AttrKeyInactive {
		t.Fatalf("client answer: video %s, audio %s", v, a)
	}
	if err := pub.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: client}); err != nil {
		t.Fatalf("the publisher refuses the edited answer: %v", err)
	}
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
	chrome := readSDP(f, "sdp/chrome154-pub-offer.sdp")
	f.Add(chrome)
	f.Add(strings.Replace(chrome, "a=simulcast:send f;q", "a=rid:h recv\r\na=simulcast:send f;q recv h", 1))
	f.Add(strings.Replace(chrome, "m=audio 9 ", "m=audio 0 ", 1))
	f.Add(strings.Replace(chrome, "a=mid:1\r\n", "a=mid:1\r\na=rid:f recv\r\n", 1))
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
			checkAcceptedAsPion(t, offer)
			checkInactive(t, raw, offer.unbound)
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

// worstCaseOffers are pub offers of just under 64 KiB built to make checkPubOffer work hard, with the code each gets.
func worstCaseOffers(t testing.TB) []struct{ name, sdp, code string } {
	t.Helper()
	chrome := readSDP(t, "sdp/chrome154-pub-offer.sdp")
	fill := func(prefix string, item func(i int) string, sep, suffix string) string {
		var b strings.Builder
		b.WriteString(prefix)
		for i := 0; ; i++ {
			next := item(i)
			if len(chrome)+b.Len()+len(sep)+len(next)+len(suffix)+64 > maxPubOfferBytes {
				break
			}
			if i > 0 {
				b.WriteString(sep)
			}
			b.WriteString(next)
		}
		return b.String() + suffix
	}
	fq := func(i int) string { return [...]string{"f", "q"}[i%2] }
	return []struct{ name, sdp, code string }{
		// About 13k distinct ids: checking them one by one against all the others was quadratic.
		{"a long a=simulcast list", edit(t, chrome, "a=simulcast:send f;q\r\n",
			fill("a=simulcast:send f;q;", strconv.Itoa, ";", "\r\n")), CodeBadRID},
		{"a long a=simulcast list of known rids", edit(t, chrome, "a=simulcast:send f;q\r\n",
			fill("a=simulcast:send ", fq, ";", "\r\n")), ""},
		{"many a=rid lines", edit(t, chrome, "a=simulcast:send f;q\r\n",
			fill("", func(int) string { return "a=rid:f send" }, "\r\n", "\r\n")), CodeBadRID},
		{"many formats", edit(t, chrome, "SAVPF 118 119 102", fill("SAVPF ", func(int) string { return "118" }, " ", " 119 102")), ""},
		{"a long fmtp line", edit(t, chrome, "a=fmtp:118 ", fill("a=fmtp:118 ", func(int) string { return "x=1" }, ";", ";")), ""},
	}
}

// TestCheckPubOfferWorstCase checks that checkPubOffer stays linear in the offer's size: it runs on the Conn actor
// for every offer, and repeated negotiations within one gen aren't rate-limited. The yardstick is the time pion/sdp
// takes to parse the same offer, which is linear and part of the check: each worst case takes at most about 12 times
// that (3 times with -race). The bound only catches quadratic work: the long a=simulcast list once took about 1700
// times the parse.
func TestCheckPubOfferWorstCase(t *testing.T) {
	best := func(f func()) time.Duration {
		d := time.Duration(math.MaxInt64)
		for range 5 {
			start := time.Now()
			f()
			d = min(d, time.Since(start))
		}
		return d
	}
	for _, c := range worstCaseOffers(t) {
		if len(c.sdp) > maxPubOfferBytes || len(c.sdp) < maxPubOfferBytes-200 {
			t.Fatalf("%s: %d bytes", c.name, len(c.sdp))
		}
		_, err := checkPubOffer(c.sdp, tracksA, owns(shareA))
		if c.code == "" && err != nil {
			t.Fatalf("%s: %v", c.name, err)
		} else if c.code != "" {
			wantCode(t, c.name, err, c.code, shareA)
		}
		parse := best(func() { _, _ = parseSDP(c.sdp) })
		check := best(func() { _, _ = checkPubOffer(c.sdp, tracksA, owns(shareA)) })
		t.Logf("%s: parse %v, check %v", c.name, parse, check)
		if bound := 20*parse + time.Millisecond; check > bound {
			t.Errorf("%s: checkPubOffer took %v, parsing takes %v (max %v)", c.name, check, parse, bound)
		}
	}
}

func BenchmarkCheckPubOfferWorstCase(b *testing.B) {
	for _, c := range worstCaseOffers(b) {
		b.Run(strings.ReplaceAll(c.name, " ", "_"), func(b *testing.B) {
			b.SetBytes(int64(len(c.sdp)))
			for b.Loop() {
				_, _ = checkPubOffer(c.sdp, tracksA, owns(shareA))
			}
		})
	}
}

// checkInactive runs setAnswerInactive for mids on an SDP that parses and checks that each named m-section ends up
// with exactly one direction attribute, a=inactive, and every other m-section keeps its attributes.
//
// The check needs an SDP that pion/sdp round-trips (parse, marshal, parse) with the same attributes; fuzz inputs that
// don't (a value holding a CR, say) are only checked for errors.
func checkInactive(t *testing.T, raw string, mids []string) {
	t.Helper()
	out, err := setAnswerInactive(raw, mids)
	if err != nil {
		t.Fatalf("setAnswerInactive: %v", err)
	}
	before, err := parseSDP(raw)
	if err != nil {
		t.Fatal(err)
	}
	plain, err := before.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	sameAttrs := func(a, b *sdp.MediaDescription) bool { return slices.Equal(a.Attributes, b.Attributes) }
	ref, err := parseSDP(string(plain))
	if err != nil || !slices.EqualFunc(ref.MediaDescriptions, before.MediaDescriptions, sameAttrs) {
		return
	}
	after, err := parseSDP(out)
	if err != nil {
		t.Fatalf("setAnswerInactive's output doesn't parse: %v", err)
	}
	if len(after.MediaDescriptions) != len(before.MediaDescriptions) {
		t.Fatalf("%d m-sections after setAnswerInactive, %d before", len(after.MediaDescriptions),
			len(before.MediaDescriptions))
	}
	for i, md := range after.MediaDescriptions {
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		if !slices.Contains(mids, mid) {
			if !slices.Equal(md.Attributes, before.MediaDescriptions[i].Attributes) {
				t.Fatalf("m-line %d changed", i)
			}
			continue
		}
		var dirs []string
		for _, a := range md.Attributes {
			switch a.Key {
			case sdp.AttrKeySendRecv, sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
				dirs = append(dirs, a.Key)
			}
		}
		if !slices.Equal(dirs, []string{sdp.AttrKeyInactive}) {
			t.Fatalf("m-line %d: directions %q after setAnswerInactive", i, dirs)
		}
	}
}

// checkAcceptedAsPion checks an offer that checkPubOffer accepted against the way Pion reads it, restated from Pion's
// code (webrtc v4.2): every m-section has a unique, non-empty mid (SetRemoteDescription refuses one without a mid);
// every m-section Pion receives on (getPeerDirection: sendrecv or sendonly, whatever the port) is a bound section or
// in offer.unbound, and no other one is;
// and each section's rids are the first fields of all its a=rid lines (getRids): at most 2 from {f,q} without repeats
// on video, none on audio. Video sections offer at least one profile of the PT table.
func checkAcceptedAsPion(t *testing.T, offer *pubOffer) {
	t.Helper()
	desc := offer.desc
	if len(offer.sections) > maxPubMLines {
		t.Fatalf("%d sections", len(offer.sections))
	}
	mids := map[string]bool{}
	unboundSeen := 0
	for i, md := range desc.MediaDescriptions {
		mid := ""
		for _, a := range md.Attributes {
			if a.Key == "mid" {
				mid = a.Value
				break
			}
		}
		if mid == "" || mids[mid] {
			t.Fatalf("m-line %d: mid %q accepted", i, mid)
		}
		mids[mid] = true

		dir := "sendrecv"
	find:
		for _, attrs := range [][]sdp.Attribute{md.Attributes, desc.Attributes} {
			for _, a := range attrs {
				switch a.Key {
				case "sendrecv", "sendonly", "recvonly", "inactive":
					dir = a.Key
					break find
				}
			}
		}
		bound := slices.ContainsFunc(offer.sections, func(s pubSection) bool { return s.index == i })
		unbound := slices.Contains(offer.unbound, mid)
		if sending := dir == "sendrecv" || dir == "sendonly"; sending != (bound || unbound) || bound && unbound {
			t.Fatalf("m-line %d is %s; bound %v, unbound %v", i, dir, bound, unbound)
		}
		if unbound {
			unboundSeen++
			// Pion receives on every rid of an unbound section too: the rid guard holds for it.
			if !guardedRIDs(mediaKind(md), ridIDs(md)) {
				t.Fatalf("unbound m-line %d (%s): rids %q accepted", i, md.MediaName.Media, ridIDs(md))
			}
		}
	}
	if unboundSeen != len(offer.unbound) {
		t.Fatalf("unbound %q for %d unbound m-lines", offer.unbound, unboundSeen)
	}
	for _, s := range offer.sections {
		if s.share != shareA && s.share != shareB {
			t.Fatalf("section bound to %q", s.share)
		}
		ids := ridIDs(desc.MediaDescriptions[s.index])
		if !slices.Equal(ids, s.rids) {
			t.Fatalf("m-line %d: rids %q, Pion reads %q", s.index, s.rids, ids)
		}
		switch s.kind {
		case video:
			if len(s.profiles) == 0 {
				t.Fatalf("video section %+v", s)
			}
			for _, p := range s.profiles {
				if !p.known() {
					t.Fatalf("profile %q accepted", p)
				}
			}
		case audio:
		default:
			t.Fatalf("section kind %v", s.kind)
		}
		if !guardedRIDs(s.kind, ids) {
			t.Fatalf("%s section %+v: rids %q accepted", s.kind, s, ids)
		}
	}
}

// guardedRIDs reports whether rids (Pion's reading of a sending m-section's a=rid lines) pass the rid guard of 02 §12:
// on video at most 2, from {f,q}, without repeats; on audio none.
func guardedRIDs(kind webrtc.RTPCodecType, ids []string) bool {
	if kind != video {
		return len(ids) == 0
	}
	if len(ids) > maxRIDs {
		return false
	}
	for i, r := range ids {
		if r != ridFull && r != ridPreview || slices.Contains(ids[:i], r) {
			return false
		}
	}
	return true
}
