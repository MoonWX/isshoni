package protocol

import "strings"

// CodecKey names a codec as clients report it: "opus", or "h264/<profile_idc><constraint byte>" in lowercase hex,
// e.g. "h264/42e0" (constrained baseline), "h264/4200" (baseline), "h264/4d00" (main), "h264/640c" (constrained
// high), "h264/6400" (high). The level is ignored (S4 finding 5: Chrome advertises High as 640034).
// Declared with single-line consts (no const group) so tygo keeps it a plain string, not a closed union: clients may
// report keys this build does not know.
type CodecKey string

const CodecOpus CodecKey = "opus"

const CodecH264ConstrainedBaseline CodecKey = "h264/42e0"

const CodecH264Baseline CodecKey = "h264/4200"

const CodecH264Main CodecKey = "h264/4d00"

const CodecH264ConstrainedHigh CodecKey = "h264/640c"

const CodecH264High CodecKey = "h264/6400"

const h264Prefix = "h264/"

// ParseH264CodecKey maps an H.264 fmtp line to its CodecKey ("" if not packetization-mode=1 or no profile-level-id).
// It accepts the parameter list as browsers and Pion report it ("level-asymmetry-allowed=1;packetization-mode=1;
// profile-level-id=42e01f"), with or without an "a=fmtp:<pt> " prefix; names and hex digits are case-insensitive.
func ParseH264CodecKey(fmtp string) CodecKey {
	if rest, ok := strings.CutPrefix(fmtp, "a=fmtp:"); ok {
		_, fmtp, _ = strings.Cut(rest, " ")
	}
	var mode1 bool
	var profile string
	for param := range strings.SplitSeq(fmtp, ";") {
		name, value, _ := strings.Cut(param, "=")
		name, value = strings.TrimSpace(name), strings.TrimSpace(value)
		switch {
		case strings.EqualFold(name, "packetization-mode"):
			mode1 = value == "1"
		case strings.EqualFold(name, "profile-level-id"):
			profile = value
		}
	}
	if !mode1 || len(profile) != 6 || !isHex(profile) {
		return ""
	}
	return CodecKey(h264Prefix + strings.ToLower(profile[:4]))
}

// H264CodecKey returns the CodecKey of a 4-hex-digit H.264 profile key such as 02's ProfileKey "6400"
// ("" if profile is not 4 hex digits).
func H264CodecKey(profile string) CodecKey {
	if len(profile) != 4 || !isHex(profile) {
		return ""
	}
	return CodecKey(h264Prefix + strings.ToLower(profile))
}

// IsH264 reports whether k is a well-formed H.264 key: "h264/" and 4 lowercase hex digits.
func (k CodecKey) IsH264() bool { return k.H264Profile() != "" }

// H264Profile returns the 4 hex digits of an H.264 key ("6400" for "h264/6400"), or "" for any other key.
func (k CodecKey) H264Profile() string {
	p, ok := strings.CutPrefix(string(k), h264Prefix)
	if !ok || len(p) != 4 || !isHex(p) || strings.ToLower(p) != p {
		return ""
	}
	return p
}

func isHexByte(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func isHex(s string) bool {
	for i := range len(s) {
		if !isHexByte(s[i]) {
			return false
		}
	}
	return true
}
