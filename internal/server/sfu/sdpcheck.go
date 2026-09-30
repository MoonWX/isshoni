package sfu

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4"
)

// SDP limits (02 §6.3, §12; 01 §3.3).
const (
	maxPubOfferBytes  = 64 << 10  // a pub offer above this is sfu.bad_sdp
	maxSubAnswerBytes = 256 << 10 // a sub answer above this is sfu.bad_sdp
	maxPubMLines      = 8         // m-lines per pub offer
	maxRIDs           = 2         // simulcast rids per video m-line
)

// Simulcast rids accepted in M1 (02 §5.2). "h" (medium) is reserved for M5 and rejected until then.
const (
	ridFull    = "f"
	ridPreview = "q"
)

// pubSection is one sending m-section of a validated pub offer.
type pubSection struct {
	index int // position among the offer's m-sections
	mid   string
	kind  webrtc.RTPCodecType
	share ShareID
	// Video only: the simulcast rids in offer order (empty means a single "f" layer), and the H.264 profiles of the
	// PT table offered with packetization-mode=1, in offer order without repeats.
	rids     []string
	profiles []ProfileKey
}

// pubOffer is a pub offer that passed checkPubOffer.
type pubOffer struct {
	desc     *sdp.SessionDescription
	sections []pubSection // the sending m-sections, in offer order
}

// checkPubOffer validates a pub offer before anything is applied (02 §8.4 step 1) and returns what the pub PC needs
// from it. tracks is the offer's binding (01's TrackRefs, already filtered by the hub); ownShare reports whether a
// share belongs to this Conn and is pending, live or stalled. The checks, in order:
//   - sfu.bad_sdp: empty, larger than 64 KiB, unparsable, more than 8 m-lines, an m=application (data channel) or
//     other non-media section, or an m-section that isn't rejected and has no unique mid;
//   - sfu.unknown_track: a sending m-section whose mid isn't bound, is bound with the other kind, or is bound to a
//     share that isn't this Conn's; or a second video or audio m-section for one share;
//   - sfu.no_h264: a sending video m-section without H.264 packetization-mode=1 in one of the five profiles of the
//     PT table (any other profile can't be negotiated, 02 §8.1);
//   - sfu.bad_rid: a video rid outside {f,q} or more than 2 rids, or any rid on audio.
//
// Sending means sendrecv or sendonly and not rejected (port 0 without bundle-only). Bindings for other m-sections
// are ignored. Error.Share is set when the failing m-section maps to a share. Error messages hold positions and
// counts, never SDP text.
func checkPubOffer(raw string, tracks []TrackBinding, ownShare func(ShareID) bool) (*pubOffer, error) {
	if len(raw) > maxPubOfferBytes {
		return nil, newError(CodeBadSDP, fmt.Sprintf("pub offer is %d bytes (max %d)", len(raw), maxPubOfferBytes))
	}
	desc, err := parseSDP(raw)
	if err != nil {
		return nil, err
	}
	if n := len(desc.MediaDescriptions); n > maxPubMLines {
		return nil, newError(CodeBadSDP, fmt.Sprintf("pub offer has %d m-lines (max %d)", n, maxPubMLines))
	}
	if err := checkSections(desc, true); err != nil {
		return nil, err
	}

	bound := make(map[string]TrackBinding, len(tracks))
	for _, t := range tracks {
		if _, dup := bound[t.MID]; dup {
			return nil, shareError(CodeUnknownTrack, t.Share, "tracks binds one mid twice")
		}
		bound[t.MID] = t
	}
	type kindCount struct{ video, audio int }
	perShare := make(map[ShareID]kindCount)

	offer := &pubOffer{desc: desc}
	for i, md := range desc.MediaDescriptions {
		if !isSending(desc, md) {
			continue
		}
		kind := mediaKind(md)
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		b, ok := bound[mid]
		switch {
		case !ok:
			return nil, newError(CodeUnknownTrack, fmt.Sprintf("m-line %d (%s) is sending but not in tracks", i, kind))
		case b.Kind != kind:
			return nil, shareError(CodeUnknownTrack, b.Share, fmt.Sprintf("m-line %d (%s) is bound as %s", i, kind, b.Kind))
		case ownShare == nil || !ownShare(b.Share):
			return nil, shareError(CodeUnknownTrack, b.Share,
				fmt.Sprintf("m-line %d (%s) is bound to a share that isn't this connection's", i, kind))
		}
		c := perShare[b.Share]
		if kind == webrtc.RTPCodecTypeVideo {
			c.video++
		} else {
			c.audio++
		}
		perShare[b.Share] = c
		if c.video > 1 || c.audio > 1 {
			return nil, shareError(CodeUnknownTrack, b.Share, fmt.Sprintf("m-line %d is the share's second %s m-line", i, kind))
		}

		s := pubSection{index: i, mid: mid, kind: kind, share: b.Share}
		rids, ridErr := sendRIDs(md)
		if kind == webrtc.RTPCodecTypeVideo {
			s.profiles = offeredProfiles(md)
			if len(s.profiles) == 0 {
				return nil, shareError(CodeNoH264, b.Share, fmt.Sprintf("m-line %d offers no H.264 with packetization-mode=1", i))
			}
			if ridErr != "" {
				return nil, shareError(CodeBadRID, b.Share, fmt.Sprintf("m-line %d: %s", i, ridErr))
			}
			s.rids = rids
		} else if len(rids) > 0 {
			return nil, shareError(CodeBadRID, b.Share, fmt.Sprintf("m-line %d: rids on an audio m-line", i))
		}
		offer.sections = append(offer.sections, s)
	}
	return offer, nil
}

// checkSubAnswer checks a viewer's answer to a sub offer before SetRemoteDescription (02 §8.5). It returns
// sfu.bad_sdp for an empty answer, one larger than 256 KiB, an unparsable one, or one with a data channel or another
// non-media section. Otherwise it returns, in order, the positions of the video m-lines whose answer holds no H.264
// with packetization-mode=1, rejected ones included: a viewer whose caps were wrong or stale. Positions, not mids:
// an answer mirrors the offer's m-lines in order (JSEP), and a rejected m-line may carry no mid (Pion's answers
// don't). The SFU still applies such an answer; the DownTracks on those m-lines bind as unsupported and report
// decoder_unavailable. Pion's SetRemoteDescription checks the rest of the answer against the offer.
func checkSubAnswer(raw string) (noH264 []int, err error) {
	if len(raw) > maxSubAnswerBytes {
		return nil, newError(CodeBadSDP, fmt.Sprintf("sub answer is %d bytes (max %d)", len(raw), maxSubAnswerBytes))
	}
	desc, err := parseSDP(raw)
	if err != nil {
		return nil, err
	}
	if err := checkSections(desc, false); err != nil {
		return nil, err
	}
	for i, md := range desc.MediaDescriptions {
		if mediaKind(md) == webrtc.RTPCodecTypeVideo && len(offeredProfiles(md)) == 0 {
			noH264 = append(noH264, i)
		}
	}
	return noH264, nil
}

// setOpusAnswerParams returns a copy of a pub answer for the client (never the local description) with each named
// audio m-section's Opus fmtp asking for stereo at its share's bitrate (02 §8.4 step 5): stereo=1, sprop-stereo=1
// and maxaveragebitrate from bitrates (mid → bit/s). The parameters only tell the remote encoder what to send.
// Pion's answer carries the offer's Opus fmtp, not the engine's (TestPubEngineAnswersChromeOffer: Chrome offers
// "minptime=10;useinbandfec=1"), so stereo has to be set here too: a sender encodes stereo only when the receiver's
// SDP asks for it (RFC 7587; S4: receivers must ask for stereo themselves). Other parameters keep their order; a
// missing fmtp line is added. Sections not named, or with a bitrate ≤ 0, are left alone, so the caller
// names every audio m-section of the answer.
func setOpusAnswerParams(raw string, bitrates map[string]int) (string, error) {
	desc, err := parseSDP(raw)
	if err != nil {
		return "", err
	}
	for _, md := range desc.MediaDescriptions {
		if mediaKind(md) != webrtc.RTPCodecTypeAudio {
			continue
		}
		mid, _ := md.Attribute(sdp.AttrKeyMID)
		bitrate, ok := bitrates[mid]
		if !ok || bitrate <= 0 {
			continue
		}
		set := []fmtpKV{{"stereo", "1"}, {"sprop-stereo", "1"}, {"maxaveragebitrate", strconv.Itoa(bitrate)}}
		for _, pt := range rtpmapPTs(md, "opus") {
			found := false
			for i, a := range md.Attributes {
				if a.Key != "fmtp" {
					continue
				}
				fpt, params, _ := strings.Cut(a.Value, " ")
				if fpt != pt {
					continue
				}
				found = true
				md.Attributes[i].Value = pt + " " + setFmtpParams(params, set)
			}
			if !found {
				md.Attributes = append(md.Attributes, sdp.Attribute{Key: "fmtp", Value: pt + " " + setFmtpParams("", set)})
			}
		}
	}
	out, err := desc.Marshal()
	if err != nil {
		return "", newError(CodeInternal, "marshal answer: "+err.Error())
	}
	return string(out), nil
}

// fmtpKV is one name=value pair of an fmtp line.
type fmtpKV struct{ name, value string }

// setFmtpParams sets each parameter in an fmtp parameter list: an existing one (name matched without case) gets the
// new value in place, a missing one is appended. Empty items are dropped.
func setFmtpParams(params string, set []fmtpKV) string {
	var parts []string
	for p := range strings.SplitSeq(params, ";") {
		if strings.TrimSpace(p) != "" {
			parts = append(parts, p)
		}
	}
	for _, s := range set {
		replaced := false
		for i, p := range parts {
			k, _, _ := strings.Cut(p, "=")
			if strings.EqualFold(strings.TrimSpace(k), s.name) {
				parts[i] = s.name + "=" + s.value
				replaced = true
			}
		}
		if !replaced {
			parts = append(parts, s.name+"="+s.value)
		}
	}
	return strings.Join(parts, ";")
}

// shareError returns an *Error for a failure that maps to a share.
func shareError(code string, share ShareID, msg string) *Error {
	e := newError(code, msg)
	e.Share = share
	return e
}

// parseSDP parses an SDP; an empty or unparsable one is sfu.bad_sdp.
func parseSDP(raw string) (*sdp.SessionDescription, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, newError(CodeBadSDP, "empty SDP")
	}
	desc := &sdp.SessionDescription{}
	if err := desc.UnmarshalString(raw); err != nil {
		return nil, newError(CodeBadSDP, "unparsable SDP")
	}
	return desc, nil
}

// checkSections rejects data channels and other non-media sections (bad_sdp). With requireMID it also rejects
// m-sections that aren't rejected and have no unique mid, which the pub offer's tracks binding relies on. A rejected
// m-section may lack a mid (Pion writes none).
func checkSections(desc *sdp.SessionDescription, requireMID bool) error {
	mids := make(map[string]bool, len(desc.MediaDescriptions))
	for i, md := range desc.MediaDescriptions {
		switch media := md.MediaName.Media; {
		case media == "application":
			return newError(CodeBadSDP, fmt.Sprintf("m-line %d is a data channel in a media PC", i))
		case mediaKind(md) == 0:
			return newError(CodeBadSDP, fmt.Sprintf("m-line %d is neither audio nor video", i))
		}
		if !requireMID || isRejected(md) {
			continue
		}
		mid, ok := md.Attribute(sdp.AttrKeyMID)
		if !ok || mid == "" || mids[mid] {
			return newError(CodeBadSDP, fmt.Sprintf("m-line %d has a missing or repeated mid", i))
		}
		mids[mid] = true
	}
	return nil
}

// mediaKind maps an m-line's media type to a codec type; 0 for anything but audio and video.
func mediaKind(md *sdp.MediaDescription) webrtc.RTPCodecType {
	switch md.MediaName.Media {
	case "video":
		return webrtc.RTPCodecTypeVideo
	case "audio":
		return webrtc.RTPCodecTypeAudio
	}
	return 0
}

// isSending reports whether the offerer sends media on an m-section: its direction is sendrecv or sendonly, and it
// isn't rejected (port 0 without bundle-only). The direction is read the way Pion reads it: the first direction
// attribute at media level, else at session level, else sendrecv.
func isSending(desc *sdp.SessionDescription, md *sdp.MediaDescription) bool {
	if isRejected(md) {
		return false
	}
	dir := direction(md.Attributes)
	if dir == "" {
		dir = direction(desc.Attributes)
	}
	return dir == "" || dir == sdp.AttrKeySendRecv || dir == sdp.AttrKeySendOnly
}

// isRejected reports whether an m-section is rejected: port 0 without bundle-only (RFC 8843 §6).
func isRejected(md *sdp.MediaDescription) bool {
	if md.MediaName.Port.Value != 0 {
		return false
	}
	_, bundleOnly := md.Attribute("bundle-only")
	return !bundleOnly
}

// direction returns the first direction attribute in attrs, or "".
func direction(attrs []sdp.Attribute) string {
	for _, a := range attrs {
		switch a.Key {
		case sdp.AttrKeySendRecv, sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
			return a.Key
		}
	}
	return ""
}

// sendRIDs returns the send-direction rids of an m-section (a=rid lines and a=simulcast:send, RFC 8851 and 8853),
// in order without repeats, and a description of the first rule they break: a rid outside {f,q}, or more than 2.
func sendRIDs(md *sdp.MediaDescription) (rids []string, problem string) {
	add := func(id string) {
		if id != "" && !slices.Contains(rids, id) {
			rids = append(rids, id)
		}
	}
	for _, a := range md.Attributes {
		switch a.Key {
		case "rid":
			fields := strings.Fields(a.Value)
			if len(fields) >= 2 && fields[1] == "send" {
				add(fields[0])
			}
		case "simulcast":
			fields := strings.Fields(a.Value)
			for i := 0; i+1 < len(fields); i += 2 {
				if fields[i] != "send" {
					continue
				}
				for stream := range strings.SplitSeq(fields[i+1], ";") {
					for alt := range strings.SplitSeq(stream, ",") {
						add(strings.TrimPrefix(alt, "~"))
					}
				}
			}
		}
	}
	for _, id := range rids {
		if id != ridFull && id != ridPreview {
			return rids, "a rid outside {f,q}"
		}
	}
	// Unreachable while f and q are the only names; it matters once M5 adds h.
	if len(rids) > maxRIDs {
		return rids, fmt.Sprintf("%d rids (max %d)", len(rids), maxRIDs)
	}
	return rids, ""
}

// offeredProfiles returns the H.264 profiles of the PT table (ProfileKey.known) that an m-section lists with
// packetization-mode=1, in the order of the m-line's formats, without repeats. For a PT with several fmtp lines the
// first one counts.
func offeredProfiles(md *sdp.MediaDescription) []ProfileKey {
	var out []ProfileKey
	h264 := rtpmapPTs(md, "h264")
	if len(h264) == 0 {
		return nil
	}
	fmtps := make(map[string]string, len(h264))
	for _, a := range md.Attributes {
		if a.Key == "fmtp" {
			pt, params, _ := strings.Cut(a.Value, " ")
			if _, dup := fmtps[pt]; !dup {
				fmtps[pt] = params
			}
		}
	}
	for _, pt := range h264 {
		key, mode1 := parseH264Fmtp(fmtps[pt])
		if mode1 && key.known() && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}

// rtpmapPTs returns the payload types of the m-line's formats whose rtpmap names the codec (without case), in the
// m-line's format order.
func rtpmapPTs(md *sdp.MediaDescription, codec string) []string {
	named := make(map[string]bool)
	for _, a := range md.Attributes {
		if a.Key != "rtpmap" {
			continue
		}
		pt, enc, _ := strings.Cut(a.Value, " ")
		name, _, _ := strings.Cut(enc, "/")
		if strings.EqualFold(name, codec) {
			named[pt] = true
		}
	}
	var out []string
	for _, f := range md.MediaName.Formats {
		if named[f] && !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out
}
