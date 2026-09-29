package main

import "strings"

// H.264 NAL unit types (RFC 6184) the SFU needs to recognise.
const (
	naluTypeSPS   = 7
	naluTypeSTAPA = 24
)

// isH264KeyframeStart reports whether an RTP payload begins a keyframe that a
// decoder can start from, i.e. it carries an SPS, alone or inside a STAP-A.
// Switching layers on a later packet (e.g. a bare IDR) would leave the decoder
// without the new layer's SPS/PPS, whose resolution differs from the old layer.
func isH264KeyframeStart(payload []byte) bool {
	if len(payload) < 1 {
		return false
	}
	switch payload[0] & 0x1F {
	case naluTypeSPS:
		return true
	case naluTypeSTAPA:
		for i := 1; i+2 < len(payload); {
			size := int(payload[i])<<8 | int(payload[i+1])
			i += 2
			if size == 0 || i+size > len(payload) {
				return false
			}
			if payload[i]&0x1F == naluTypeSPS {
				return true
			}
			i += size
		}
	}
	return false
}

// h264FmtpKey extracts what must match for two H.264 fmtp lines to be
// interoperable: profile_idc + profile-iop (first 4 hex digits of
// profile-level-id; the level may differ) and packetization-mode.
func h264FmtpKey(fmtp string) (profile, packetizationMode string) {
	packetizationMode = "0"
	for _, kv := range strings.Split(fmtp, ";") {
		k, v, _ := strings.Cut(strings.TrimSpace(kv), "=")
		switch strings.ToLower(k) {
		case "profile-level-id":
			if len(v) >= 4 {
				profile = strings.ToLower(v[:4])
			}
		case "packetization-mode":
			packetizationMode = v
		}
	}
	return profile, packetizationMode
}

// h264ProfileName names the profiles the spike registers, for readable stats.
func h264ProfileName(fmtp string) string {
	profile, _ := h264FmtpKey(fmtp)
	switch profile {
	case "42e0":
		return "constrained-baseline"
	case "4200":
		return "baseline"
	case "4d00":
		return "main"
	case "640c":
		return "constrained-high"
	case "6400":
		return "high"
	case "":
		return ""
	}
	return "other(" + profile + ")"
}
