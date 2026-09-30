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
	// Video only: the simulcast rids, the ids of the a=rid lines in offer order as Pion reads them (empty means a
	// single "f" layer), and the H.264 profiles of the PT table offered with packetization-mode=1, in offer order
	// without repeats.
	rids     []string
	profiles []ProfileKey
}

// pubOffer is a pub offer that passed checkPubOffer.
type pubOffer struct {
	desc     *sdp.SessionDescription
	sections []pubSection // the bound sending m-sections, in offer order
	// unbound holds the mids of the sending m-sections that carry no share, in offer order: tracks doesn't bind them,
	// or binds them to a share that isn't a pending, live or stalled share of this Conn. For example one whose share
	// ended in a race, so the hub dropped its TrackRef, or kept it while the share ended (01 §9 rule 4). Their rids passed
	// the same guard as a bound video section's (none on audio). The pub PC answers each one a=inactive
	// (setAnswerInactive on the answer sent to the client), so a compliant client doesn't send on it. Pion's local
	// description still receives on it, so a track that arrives on an unbound mid anyway is never attached to a share:
	// the pub PC (S29) drains it (reads and discards its packets) or stops its receiver, and never leaves it unread,
	// or its read buffers fill and stay allocated.
	unbound []string
}

// checkPubOffer validates a pub offer before anything is applied (02 §8.4 step 1) and returns what the pub PC needs
// from it. tracks is the offer's binding (01's TrackRefs, already filtered by the hub); ownShare reports whether a
// share belongs to this Conn and is pending, live or stalled. The checks, in order:
//   - sfu.bad_sdp: empty, larger than 64 KiB, unparsable, more than 8 m-lines, an m=application (data channel) or
//     other non-media section, or an m-section without a unique mid (Pion's SetRemoteDescription refuses an offer
//     with an m-section without a mid, rejected ones included);
//   - sfu.unknown_track: tracks binds one mid twice; a sending m-section is bound to this Conn's share with the other
//     kind; or a second video or audio m-section for one share;
//   - sfu.no_h264: a sending video m-section without H.264 packetization-mode=1 in one of the five profiles of the
//     PT table (any other profile can't be negotiated, 02 §8.1);
//   - sfu.bad_rid: on a sending video m-section, an a=rid id outside {f,q}, a repeated one, more than 2, or an
//     a=simulcast id without an a=rid line; on a sending audio m-section, any a=rid line or a=simulcast id.
//
// A sending m-section without a binding, or bound to a share that isn't this Conn's (ownShare false, or ownShare nil),
// is no error (02 §6.3, §8.4 step 1): it goes to pubOffer.unbound, and the pub PC answers it a=inactive. It skips the
// kind check. Only its rids are checked, as above (sfu.bad_rid, without a share): Pion answers and receives every rid
// of it all the same, and the rid guard (02 §12) bounds what one offer can make Pion allocate. Its codecs don't
// matter. Sending means that Pion receives on the m-section: its direction is sendrecv or sendonly. Pion ignores the
// port of an offer's m-section, so a port-0 m-section that still sends counts too. Bindings for other m-sections are
// ignored. Error.Share is set when the failing m-section maps to a share. Error messages hold positions and counts,
// never SDP text.
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
		rids, ridErr := offerRIDs(md)
		if kind == webrtc.RTPCodecTypeAudio && (len(rids) > 0 || ridErr != "") {
			ridErr = "rids on an audio m-line"
		}
		b, ok := bound[mid]
		switch {
		case !ok || ownShare == nil || !ownShare(b.Share):
			// No binding, or a binding to a share that isn't this Conn's (it ended while the offer was in flight):
			// the section carries no share. Pion answers its rids like any other's, and receives a layer on each, so
			// the rid guard (02 §12) holds for it too.
			if ridErr != "" {
				return nil, newError(CodeBadRID, fmt.Sprintf("m-line %d: %s", i, ridErr))
			}
			offer.unbound = append(offer.unbound, mid)
			continue
		case b.Kind != kind:
			return nil, shareError(CodeUnknownTrack, b.Share, fmt.Sprintf("m-line %d (%s) is bound as %s", i, kind, b.Kind))
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
		if kind == webrtc.RTPCodecTypeVideo {
			s.profiles = offeredProfiles(md)
			if len(s.profiles) == 0 {
				return nil, shareError(CodeNoH264, b.Share, fmt.Sprintf("m-line %d offers no H.264 with packetization-mode=1", i))
			}
			s.rids = rids
		}
		if ridErr != "" {
			return nil, shareError(CodeBadRID, b.Share, fmt.Sprintf("m-line %d: %s", i, ridErr))
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

// setAnswerInactive returns a copy of a pub answer for the client with the named m-sections (pubOffer.unbound) made
// inactive: their direction attributes are replaced by one a=inactive, so the client sends nothing on them and keeps
// the transceiver for a later offer that binds it. Other lines stay as they are. Like setOpusAnswerParams, it edits
// the copy sent to the client; Pion's local description may keep its recvonly answer, so a track that still arrives
// on such an m-section must be drained or its receiver stopped (pubOffer.unbound). No mids returns raw unchanged.
func setAnswerInactive(raw string, mids []string) (string, error) {
	if len(mids) == 0 {
		return raw, nil
	}
	desc, err := parseSDP(raw)
	if err != nil {
		return "", err
	}
	for _, md := range desc.MediaDescriptions {
		if mid, _ := md.Attribute(sdp.AttrKeyMID); !slices.Contains(mids, mid) {
			continue
		}
		attrs := make([]sdp.Attribute, 0, len(md.Attributes)+1)
		placed := false
		for _, a := range md.Attributes {
			switch a.Key {
			case sdp.AttrKeySendRecv, sdp.AttrKeySendOnly, sdp.AttrKeyRecvOnly, sdp.AttrKeyInactive:
				if !placed {
					attrs = append(attrs, sdp.Attribute{Key: sdp.AttrKeyInactive})
					placed = true
				}
				continue
			}
			attrs = append(attrs, a)
		}
		if !placed {
			attrs = append(attrs, sdp.Attribute{Key: sdp.AttrKeyInactive})
		}
		md.Attributes = attrs
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

// checkSections rejects data channels and other non-media sections (bad_sdp). With requireMID it also rejects an
// m-section without a unique, non-empty mid, rejected ones included: the pub offer's tracks binding relies on mids,
// and Pion's SetRemoteDescription refuses an offer with any m-section without a mid. An answer's rejected m-sections
// may lack a mid (Pion writes none), so checkSubAnswer doesn't ask for them.
func checkSections(desc *sdp.SessionDescription, requireMID bool) error {
	mids := make(map[string]bool, len(desc.MediaDescriptions))
	for i, md := range desc.MediaDescriptions {
		switch media := md.MediaName.Media; {
		case media == "application":
			return newError(CodeBadSDP, fmt.Sprintf("m-line %d is a data channel in a media PC", i))
		case mediaKind(md) == 0:
			return newError(CodeBadSDP, fmt.Sprintf("m-line %d is neither audio nor video", i))
		}
		if !requireMID {
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

// isSending reports whether the offerer sends media on an m-section, the way Pion decides whether it receives on it:
// its direction is sendrecv or sendonly, whatever its port. The direction is read as Pion reads it (getPeerDirection):
// the first direction attribute at media level, else at session level, else sendrecv.
func isSending(desc *sdp.SessionDescription, md *sdp.MediaDescription) bool {
	dir := direction(md.Attributes)
	if dir == "" {
		dir = direction(desc.Attributes)
	}
	return dir == "" || dir == sdp.AttrKeySendRecv || dir == sdp.AttrKeySendOnly
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

// offerRIDs returns the simulcast rids of an m-section the way Pion reads them (getRids): the first space-separated
// field of every a=rid line, in order, whatever its direction and without removing repeats. Pion's answer lists each
// one as a receive rid, and the publisher may then send a layer for each, so problem describes the first rule they
// break, and rids stops there: an id outside {f,q}, a repeated id, more than 2, or an id of an a=simulcast send or
// recv list (paused or an alternative) without an a=rid line (RFC 8853 §5.1). The work per id is constant.
func offerRIDs(md *sdp.MediaDescription) (rids []string, problem string) {
	for _, a := range md.Attributes {
		if a.Key != "rid" {
			continue
		}
		id, _, _ := strings.Cut(a.Value, " ")
		switch {
		case id != ridFull && id != ridPreview:
			return rids, "a rid outside {f,q}"
		case slices.Contains(rids, id):
			return rids, "a repeated rid"
		case len(rids) == maxRIDs: // unreachable while f and q are the only names; it matters once M5 adds h
			return rids, fmt.Sprintf("more than %d rids", maxRIDs)
		}
		rids = append(rids, id)
	}
	for _, a := range md.Attributes {
		if a.Key != "simulcast" {
			continue
		}
		for field := range strings.FieldsSeq(a.Value) {
			if field == "send" || field == "recv" {
				continue
			}
			for stream := range strings.SplitSeq(field, ";") {
				for alt := range strings.SplitSeq(stream, ",") {
					if !slices.Contains(rids, strings.TrimPrefix(alt, "~")) {
						return rids, "an a=simulcast rid without an a=rid line"
					}
				}
			}
		}
	}
	return rids, ""
}

// offeredProfiles returns the H.264 profiles of the PT table (ProfileKey.known) that an m-section lists with
// packetization-mode=1, in the order of the m-line's formats, without repeats. Codecs are read as Pion reads them
// (sdpCodecs).
func offeredProfiles(md *sdp.MediaDescription) []ProfileKey {
	var out []ProfileKey
	for _, c := range sdpCodecs(md) {
		if !strings.EqualFold(c.name, "h264") {
			continue
		}
		key, mode1 := parseH264Fmtp(c.fmtp)
		if mode1 && key.known() && !slices.Contains(out, key) {
			out = append(out, key)
		}
	}
	return out
}

// rtpmapPTs returns the payload types of the m-line's formats whose codec name (sdpCodecs) is codec, without case, in
// the m-line's format order.
func rtpmapPTs(md *sdp.MediaDescription, codec string) []string {
	var out []string
	for _, c := range sdpCodecs(md) {
		if strings.EqualFold(c.name, codec) {
			out = append(out, c.pt)
		}
	}
	return out
}

// sdpCodec is one format of an m-line with its codec name and fmtp parameters.
type sdpCodec struct {
	pt         string // as the m-line writes it
	name, fmtp string
}

// sdpCodecs returns the m-line's formats, in order and without repeats, with the codec each one names the way Pion
// builds a remote m-section's codecs (codecsFromMediaDescription, pion/sdp GetCodecMap): payload types are compared as
// numbers; the first rtpmap line of a PT that parses ("<pt> <name>[/<clock rate>[/<parameters>]]", a single space,
// a numeric clock rate) names it, and the first fmtp line with parameters gives them. PTs 0, 8 and 9 are named
// statically (PCMU, PCMA, G722). A format that isn't a PT number is left out.
func sdpCodecs(md *sdp.MediaDescription) []sdpCodec {
	var names, fmtps [256]string
	names[0], names[8], names[9] = "PCMU", "PCMA", "G722"
	for _, a := range md.Attributes {
		switch a.Key {
		case "rtpmap":
			ptStr, enc, ok := strings.Cut(a.Value, " ")
			pt, err := strconv.ParseUint(ptStr, 10, 8)
			if !ok || err != nil || strings.Contains(enc, " ") {
				continue
			}
			name, rest, hasRate := strings.Cut(enc, "/")
			if hasRate {
				rate, _, _ := strings.Cut(rest, "/")
				if _, err := strconv.ParseUint(rate, 10, 32); err != nil {
					continue
				}
			}
			if names[pt] == "" {
				names[pt] = name
			}
		case "fmtp":
			ptStr, params, ok := strings.Cut(a.Value, " ")
			pt, err := strconv.ParseUint(ptStr, 10, 8)
			if ok && err == nil && fmtps[pt] == "" {
				fmtps[pt] = params
			}
		}
	}
	var seen [256]bool
	var out []sdpCodec
	for _, f := range md.MediaName.Formats {
		pt, err := strconv.ParseUint(f, 10, 8)
		if err != nil || seen[pt] {
			continue
		}
		seen[pt] = true
		out = append(out, sdpCodec{pt: f, name: names[pt], fmtp: fmtps[pt]})
	}
	return out
}
