package protocol

import (
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Limits of the wire format (01 §3.3, §13). Validate methods and Decode enforce the field limits; the hub enforces
// the message sizes on the raw frame. Single-line consts: tygo turns a const group whose names share a prefix
// ("Max") into a meaningless TS union type.

const MaxMessageBytes = 64 << 10 // every message before welcome, and every non-SDP message after

const MaxSDPBytes = 256 << 10 // pc.offer and pc.answer after welcome (welcome.limits.maxSdpBytes)

const MaxStatsBytes = 16 << 10 // a client stats message

const MaxIDLen = 32 // envelope id and re, share.start ref

const MaxOpaqueIDLen = 64 // user, room, share and connection ids: [A-Za-z0-9_-]{1,64}

const MaxLabelRunes = 40 // share label, in Unicode code points after trimming

const MaxSubs = 64 // subscribe.update items

const MaxPubTracks = 8 // tracks of a pub offer (m-sections carrying a share)

const MaxSubTracks = 2 * MaxSubs // tracks of a sub offer: video and audio of every subscription

const MaxCandidateBytes = 512 // pc.ice candidate string

const MaxFeatures = 32 // hello.features

const MaxCodecs = 32 // caps.decode, caps.encode

const MaxAgentKindLen = 32 // agent.send kind: [a-z.]{1,32}

const MaxAgentPayloadBytes = 16 << 10 // agent.send payload

// Unexported limits: bounds for diagnostic strings and stats lists that the spec leaves to the implementation.
const (
	maxClientKindLen  = 16  // hello client.kind and client.os: [a-z]{1,16}
	maxBrowserLen     = 16  // hello client.browser
	maxVersionLen     = 64  // hello client.version
	maxCodecKeyLen    = 32  // one CodecKey
	maxTokenBytes     = 512 // hello auth.token
	maxMIDLen         = 32  // TrackRef.mid
	maxSDPMidLen      = 32  // ICECandidate.sdpMid
	maxUfragLen       = 256 // ICECandidate.usernameFragment (RFC 8839: 4-256 characters)
	maxStatsPCs       = 4   // a connection has at most 2 PCs (01 §9 rule 9); room for one being replaced
	maxStatsInbound   = 2 * MaxSubs
	maxStatsOutbound  = 16 // 4 shares × (2 video layers + audio), rounded up
	maxStatsStringLen = 64 // state, decoder, encoder, ...
)

func isLower(c byte) bool { return 'a' <= c && c <= 'z' }

func isTokenByte(c byte) bool {
	return isLower(c) || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || c == '_' || c == '-'
}

// validToken reports whether s is 1..maxLen characters of [A-Za-z0-9_-] (envelope ids, refs, opaque ids).
func validToken(s string, maxLen int) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if !isTokenByte(s[i]) {
			return false
		}
	}
	return true
}

// validLower reports whether s is 1..maxLen characters of [a-z] (plus the bytes in extra).
func validLower(s string, maxLen int, extra string) bool {
	if s == "" || len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; !isLower(c) && strings.IndexByte(extra, c) < 0 {
			return false
		}
	}
	return true
}

// validPrintable reports whether s is at most maxLen bytes of printable ASCII without spaces.
func validPrintable(s string, maxLen int) bool {
	if len(s) > maxLen {
		return false
	}
	for i := range len(s) {
		if c := s[i]; c <= ' ' || c > '~' {
			return false
		}
	}
	return true
}

// normalizeLabel trims s and checks the label rules (01 §5): at most 40 code points, no characters of the Unicode
// categories Cc, Cf, Zl or Zp. It returns the trimmed label and "" or a FieldError reason. An empty result means
// "no label".
func normalizeLabel(s string) (string, string) {
	s = strings.TrimSpace(s)
	if !utf8.ValidString(s) {
		return s, FieldInvalid
	}
	n := 0
	for _, r := range s {
		if unicode.In(r, unicode.Cc, unicode.Cf, unicode.Zl, unicode.Zp) {
			return s, FieldInvalid
		}
		n++
	}
	if n > MaxLabelRunes {
		return s, FieldTooLong
	}
	return s, ""
}

func fieldErr(field, reason string) *FieldError { return &FieldError{Field: field, Reason: reason} }

func indexed(field string, i int) string { return field + "[" + strconv.Itoa(i) + "]" }

// checkOpaqueID checks a user, room, share or connection id.
func checkOpaqueID(field, id string) *FieldError {
	switch {
	case id == "":
		return fieldErr(field, FieldRequired)
	case len(id) > MaxOpaqueIDLen:
		return fieldErr(field, FieldTooLong)
	case !validToken(id, MaxOpaqueIDLen):
		return fieldErr(field, FieldInvalid)
	}
	return nil
}

// checkEnum checks a required enum value.
func checkEnum(field, value string, valid bool) *FieldError {
	switch {
	case value == "":
		return fieldErr(field, FieldRequired)
	case !valid:
		return fieldErr(field, FieldInvalid)
	}
	return nil
}

func checkPC(pc PCKind, gen uint32) *FieldError {
	if err := checkEnum("pc", string(pc), pc.Valid()); err != nil {
		return err
	}
	if gen == 0 {
		return fieldErr("gen", FieldRequired)
	}
	return nil
}

func checkSDP(sdp SDP) *FieldError {
	switch {
	case sdp == "":
		return fieldErr("sdp", FieldRequired)
	case len(sdp) > MaxSDPBytes:
		return fieldErr("sdp", FieldTooLong)
	}
	return nil
}

func (c *Caps) check(prefix string) *FieldError {
	for _, l := range []struct {
		name string
		keys []CodecKey
	}{{"decode", c.Decode}, {"encode", c.Encode}} {
		field := prefix + l.name
		if len(l.keys) > MaxCodecs {
			return fieldErr(field, FieldTooMany)
		}
		for i, k := range l.keys {
			// CodecKey is open (newer clients may report codecs this build doesn't know), so only the shape is checked.
			if !validLower(string(k), maxCodecKeyLen, "0123456789/._-") {
				return fieldErr(indexed(field, i), FieldInvalid)
			}
		}
	}
	return nil
}

var capsLimits = []arrayLimit{{"caps.decode", MaxCodecs}, {"caps.encode", MaxCodecs}}

var (
	helloLimits       = newLimits(append([]arrayLimit{{"features", MaxFeatures}}, capsLimits...)...)
	capsUpdateLimits  = newLimits(capsLimits...)
	pcOfferLimits     = newLimits(arrayLimit{"tracks", MaxSubTracks})
	subscribeLimits   = newLimits(arrayLimit{"subs", MaxSubs})
	clientStatsLimits = newLimits(arrayLimit{"pcs", maxStatsPCs}, arrayLimit{"inbound", maxStatsInbound},
		arrayLimit{"outbound", maxStatsOutbound})
)

func (*Hello) arrayLimits() *limitNode           { return helloLimits }
func (*CapsUpdate) arrayLimits() *limitNode      { return capsUpdateLimits }
func (*PCOffer) arrayLimits() *limitNode         { return pcOfferLimits }
func (*SubscribeUpdate) arrayLimits() *limitNode { return subscribeLimits }
func (*ClientStats) arrayLimits() *limitNode     { return clientStatsLimits }

// Validate checks a hello (01 §8.2 step 2): client.kind matches [a-z]{1,16} (unknown kinds are accepted), role is
// known, features <= 32 entries, caps.decode and caps.encode <= 32 well-formed keys. The diagnostic strings are
// bounded: client.os [a-z]{0,16} (unknown values accepted), client.browser [a-z]{0,16}, client.version <= 64
// printable characters. auth, when present, needs scheme bearer and a token of 1-512 bytes. Protocol versions are
// not checked here: an unusable range is protocol_unsupported (Negotiate), not bad_request. The resume token is not
// checked either: a bad one only means resumed: false.
func (h *Hello) Validate() error {
	switch {
	case h.Client.Kind == "":
		return fieldErr("client.kind", FieldRequired)
	case !validLower(string(h.Client.Kind), maxClientKindLen, ""):
		return fieldErr("client.kind", FieldInvalid)
	case h.Client.OS != "" && !validLower(string(h.Client.OS), maxClientKindLen, ""):
		return fieldErr("client.os", FieldInvalid)
	case h.Client.Browser != "" && !validLower(h.Client.Browser, maxBrowserLen, ""):
		return fieldErr("client.browser", FieldInvalid)
	case !validPrintable(h.Client.Version, maxVersionLen):
		return fieldErr("client.version", FieldInvalid)
	}
	if err := checkEnum("role", string(h.Role), h.Role.Valid()); err != nil {
		return err
	}
	if len(h.Features) > MaxFeatures {
		return fieldErr("features", FieldTooMany)
	}
	if err := h.Caps.check("caps."); err != nil {
		return err
	}
	if a := h.Auth; a != nil {
		if err := checkEnum("auth.scheme", string(a.Scheme), a.Scheme.Valid()); err != nil {
			return err
		}
		switch {
		case a.Token == "":
			return fieldErr("auth.token", FieldRequired)
		case len(a.Token) > maxTokenBytes:
			return fieldErr("auth.token", FieldTooLong)
		}
	}
	return nil
}

// Validate checks the room id. A room id that isn't 1-64 characters of [A-Za-z0-9_-] gets room_not_found, not
// bad_request, matching 03's REST 404 (01 §8.4); so the error is a *Error, not a *FieldError.
func (j *RoomJoin) Validate() error {
	if !validToken(j.RoomID, MaxOpaqueIDLen) {
		e := NewError(ErrorCodeRoomNotFound, ErrorScopeRequest)
		return &e
	}
	return nil
}

// Validate checks a share.start and trims its label in place (01 §5, §8.7).
func (s *ShareStart) Validate() error {
	if err := checkEnum("kind", string(s.Kind), s.Kind.Valid()); err != nil {
		return err
	}
	label, reason := normalizeLabel(s.Label)
	if reason != "" {
		return fieldErr("label", reason)
	}
	s.Label = label
	if err := checkEnum("preset", string(s.Preset), s.Preset.Valid()); err != nil {
		return err
	}
	switch {
	case s.Ref == "":
		return fieldErr("ref", FieldRequired)
	case len(s.Ref) > MaxIDLen:
		return fieldErr("ref", FieldTooLong)
	case !validToken(s.Ref, MaxIDLen):
		return fieldErr("ref", FieldInvalid)
	}
	if s.Replaces != "" {
		if err := checkOpaqueID("replaces", s.Replaces); err != nil {
			return err
		}
	}
	return nil
}

// Validate checks a share.update and trims its label in place. A label that is empty after trimming clears it.
func (u *ShareUpdate) Validate() error {
	if err := checkOpaqueID("shareId", u.ShareID); err != nil {
		return err
	}
	if u.Label != nil {
		label, reason := normalizeLabel(*u.Label)
		if reason != "" {
			return fieldErr("label", reason)
		}
		*u.Label = label
	}
	if u.Preset != "" && !u.Preset.Valid() {
		return fieldErr("preset", FieldInvalid)
	}
	return nil
}

// Validate checks a share.stop.
func (s *ShareStop) Validate() error {
	if err := checkOpaqueID("shareId", s.ShareID); err != nil {
		return err
	}
	return nil
}

// Validate checks an offer in either direction: pc, gen >= 1, neg >= 1, a non-empty SDP of at most MaxSDPBytes, and
// tracks (at most 8 for pub, 128 for sub; unique non-empty mids; share ids; kind video or audio). Every tracks error
// has field "tracks" (01 §9 rule 4). The SFU checks the SDP itself (02: pub offers <= 64 KiB and <= 8 m-lines).
func (o *PCOffer) Validate() error {
	if err := checkPC(o.PC, o.Gen); err != nil {
		return err
	}
	if o.Neg == 0 {
		return fieldErr("neg", FieldRequired)
	}
	if err := checkSDP(o.SDP); err != nil {
		return err
	}
	maxTracks := MaxSubTracks
	if o.PC == PCKindPub {
		maxTracks = MaxPubTracks
	}
	if len(o.Tracks) > maxTracks {
		return fieldErr("tracks", FieldTooMany)
	}
	for i, t := range o.Tracks {
		switch {
		case t.MID == "" || t.ShareID == "" || t.Kind == "":
			return fieldErr("tracks", FieldRequired)
		case !validPrintable(t.MID, maxMIDLen) || !validToken(t.ShareID, MaxOpaqueIDLen) || !t.Kind.Valid():
			return fieldErr("tracks", FieldInvalid)
		}
		for _, prev := range o.Tracks[:i] {
			// A share has one video and at most one audio m-section (§9 rule 4); the server builds sub offers.
			if prev.MID == t.MID || o.PC == PCKindPub && prev.ShareID == t.ShareID && prev.Kind == t.Kind {
				return fieldErr("tracks", FieldDuplicate)
			}
		}
	}
	return nil
}

// Validate checks an answer in either direction: pc, gen >= 1, neg >= 1, a non-empty SDP of at most MaxSDPBytes.
func (a *PCAnswer) Validate() error {
	if err := checkPC(a.PC, a.Gen); err != nil {
		return err
	}
	if a.Neg == 0 {
		return fieldErr("neg", FieldRequired)
	}
	if err := checkSDP(a.SDP); err != nil {
		return err
	}
	return nil
}

// Validate checks a trickled candidate: pc, gen >= 1, and a candidate string of at most 512 bytes (empty is the
// browsers' end-of-candidates form). An absent candidate is the end-of-candidates marker.
func (c *PCICE) Validate() error {
	if err := checkPC(c.PC, c.Gen); err != nil {
		return err
	}
	if cand := c.Candidate; cand != nil {
		switch {
		case len(cand.Candidate) > MaxCandidateBytes:
			return fieldErr("candidate.candidate", FieldTooLong)
		case cand.SDPMid != nil && len(*cand.SDPMid) > maxSDPMidLen:
			return fieldErr("candidate.sdpMid", FieldTooLong)
		case cand.UsernameFragment != nil && len(*cand.UsernameFragment) > maxUfragLen:
			return fieldErr("candidate.usernameFragment", FieldTooLong)
		}
	}
	return nil
}

// Validate checks a restart request: pc, gen >= 1, mode and reason. Which PC kind may be restarted by whom is the
// hub's routing (clients: sub only; the server: pub only).
func (r *PCRestart) Validate() error {
	if err := checkPC(r.PC, r.Gen); err != nil {
		return err
	}
	if err := checkEnum("mode", string(r.Mode), r.Mode.Valid()); err != nil {
		return err
	}
	if err := checkEnum("reason", string(r.Reason), r.Reason.Valid()); err != nil {
		return err
	}
	return nil
}

// Validate checks pc and gen >= 1.
func (c *PCClose) Validate() error {
	if err := checkPC(c.PC, c.Gen); err != nil {
		return err
	}
	return nil
}

// Validate checks a subscribe.update: 1-64 items with unique share ids and known video and audio values.
func (u *SubscribeUpdate) Validate() error {
	switch {
	case len(u.Subs) == 0:
		return fieldErr("subs", FieldRequired)
	case len(u.Subs) > MaxSubs:
		return fieldErr("subs", FieldTooMany)
	}
	for i, s := range u.Subs {
		p := indexed("subs", i)
		if err := checkOpaqueID(p+".shareId", s.ShareID); err != nil {
			return err
		}
		if err := checkEnum(p+".video", string(s.Video), s.Video.Valid()); err != nil {
			return err
		}
		if err := checkEnum(p+".audio", string(s.Audio), s.Audio.Valid()); err != nil {
			return err
		}
		for _, prev := range u.Subs[:i] {
			if prev.ShareID == s.ShareID {
				return fieldErr(p+".shareId", FieldDuplicate)
			}
		}
	}
	return nil
}

// Validate checks the caps as in hello.
func (c *CapsUpdate) Validate() error {
	if err := c.Caps.check("caps."); err != nil {
		return err
	}
	return nil
}

// Validate checks a client stats report: at most 4 PCs, 128 inbound and 16 outbound entries; known PC and track
// kinds; share ids; non-negative numbers (the hub turns counter deltas into metrics), except packetsLost, which
// RTCP allows to be negative; diagnostic strings of at most 64 bytes.
func (s *ClientStats) Validate() error {
	switch {
	case s.IntervalMs < 0:
		return fieldErr("intervalMs", FieldInvalid)
	case len(s.PCs) > maxStatsPCs:
		return fieldErr("pcs", FieldTooMany)
	case len(s.Inbound) > maxStatsInbound:
		return fieldErr("inbound", FieldTooMany)
	case len(s.Outbound) > maxStatsOutbound:
		return fieldErr("outbound", FieldTooMany)
	}
	for i, pc := range s.PCs {
		p := indexed("pcs", i)
		switch {
		case !pc.PC.Valid():
			return fieldErr(p+".pc", FieldInvalid)
		case pc.RTTMs < 0 || pc.OutgoingBitrate < 0:
			return fieldErr(p, FieldInvalid)
		case len(pc.State) > maxStatsStringLen || len(pc.CandidateType) > maxStatsStringLen ||
			len(pc.Transport) > maxStatsStringLen:
			return fieldErr(p, FieldTooLong)
		}
	}
	for i, in := range s.Inbound {
		p := indexed("inbound", i)
		if err := checkOpaqueID(p+".shareId", in.ShareID); err != nil {
			return err
		}
		switch {
		case !in.Kind.Valid():
			return fieldErr(p+".kind", FieldInvalid)
		case in.Bitrate < 0 || in.JitterBufferMs < 0 || in.FPS < 0 || in.Width < 0 || in.Height < 0 ||
			in.FreezeCount < 0 || in.FramesDecoded < 0 || in.FramesDropped < 0 || in.FreezeDurationMs < 0 ||
			in.ConcealedSamples < 0 || in.TotalSamples < 0:
			return fieldErr(p, FieldInvalid)
		case len(in.Decoder) > maxStatsStringLen || len(in.Codec) > maxCodecKeyLen:
			return fieldErr(p, FieldTooLong)
		}
	}
	for i, out := range s.Outbound {
		p := indexed("outbound", i)
		if err := checkOpaqueID(p+".shareId", out.ShareID); err != nil {
			return err
		}
		switch {
		case !out.Kind.Valid():
			return fieldErr(p+".kind", FieldInvalid)
		case out.Bitrate < 0 || out.FPS < 0 || out.Width < 0 || out.Height < 0:
			return fieldErr(p, FieldInvalid)
		case len(out.RID) > maxStatsStringLen || len(out.Encoder) > maxStatsStringLen ||
			len(out.QualityLimitation) > maxStatsStringLen:
			return fieldErr(p, FieldTooLong)
		}
	}
	return nil
}

// Validate has nothing to check: on is a boolean.
func (*StatsWatch) Validate() error { return nil }

// Validate checks an agent.send (01 §8.14): exactly one of to (a connection id) and toRole (a known role); kind
// [a-z.]{1,32}; a payload of 1 byte to 16 KiB.
func (a *AgentSend) Validate() error {
	switch {
	case a.To == "" && a.ToRole == "":
		return fieldErr("to", FieldRequired)
	case a.To != "" && a.ToRole != "":
		return fieldErr("toRole", FieldInvalid)
	case a.To != "":
		if err := checkOpaqueID("to", a.To); err != nil {
			return err
		}
	case !a.ToRole.Valid():
		return fieldErr("toRole", FieldInvalid)
	}
	switch {
	case a.Kind == "":
		return fieldErr("kind", FieldRequired)
	case len(a.Kind) > MaxAgentKindLen:
		return fieldErr("kind", FieldTooLong)
	case !validLower(a.Kind, MaxAgentKindLen, "."):
		return fieldErr("kind", FieldInvalid)
	}
	switch {
	case len(a.Payload) == 0:
		return fieldErr("payload", FieldRequired)
	case len(a.Payload) > MaxAgentPayloadBytes:
		return fieldErr("payload", FieldTooLong)
	}
	return nil
}
