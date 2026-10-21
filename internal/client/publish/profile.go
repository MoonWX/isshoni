package publish

import (
	"slices"
	"strconv"
)

// The H.264 profile keys of M2's native engine (docs/m2/01-client-core.md §8.6). The engine encodes HighProfile and
// ConstrainedBaselineProfile; HighAlias is only an SDP alias of HighProfile.
const (
	HighProfile                = DefaultProfile // "6400": High (IM_PROFILE_HIGH)
	ConstrainedBaselineProfile = "42e0"         // Constrained Baseline (IM_PROFILE_CB)
	// HighAlias is Constrained High: when the session runs High, the Publisher offers it next to HighProfile for the
	// same bitstream (the plan: native senders advertise "64001f" and "640c1f"). WebRTC encoders never emit B-frames,
	// so a High stream decodes as Constrained High (M1 02 §8.2). The engine sets no constraint flag in the SPS, so
	// its keyframes say "6400" whichever of the two is negotiated.
	HighAlias = "640c"
)

// EngineProfile returns the profile key a Source encodes for an SDP profile key: HighProfile for HighAlias, the key
// itself otherwise. A Publisher switches its ProfileSource only when the engine profile changes, and always passes
// SetProfile an engine key, so a switch between "6400" and "640c" changes only the payload type.
func EngineProfile(profile string) string {
	if profile == HighAlias {
		return HighProfile
	}
	return profile
}

// OfferProfiles returns Options.Profiles for a session that runs the profile running (the first EventLayer.Profile,
// which may be Constrained Baseline when only a CB encoder opened; 04 §9.6): the other profiles of HighProfile,
// HighAlias and ConstrainedBaselineProfile, in that order (§8.6). available are the profiles the PC's available
// encoders list (media.EncoderInfo.Profiles). HighProfile and ConstrainedBaselineProfile are offered when one of them
// lists it; HighAlias only next to HighProfile (running it, or offering it). Options.Profile is running itself.
func OfferProfiles(running string, available []string) []string {
	high := running == HighProfile || slices.Contains(available, HighProfile)
	var out []string
	for _, k := range []string{HighProfile, HighAlias, ConstrainedBaselineProfile} {
		switch {
		case k == running:
		case k == HighProfile && !high, k == HighAlias && !high:
		case k == ConstrainedBaselineProfile && !slices.Contains(available, k):
		default:
			out = append(out, k)
		}
	}
	return out
}

// profileIDC returns the profile_idc of a 4-hex-digit profile key (its first byte), or 0.
func profileIDC(profile string) byte {
	if len(profile) != 4 {
		return 0
	}
	v, err := strconv.ParseUint(profile[:2], 16, 8)
	if err != nil {
		return 0
	}
	return byte(v)
}

// spsProfileIDC returns the profile_idc of the first SPS in an Annex B access unit, looking only at the NAL units in
// front of its first slice (a keyframe's SPS and PPS come first, the plan's "Keyframes"). ok is false when there is
// none. profile_idc is the byte after the SPS's NAL header: emulation prevention never touches it, because the header
// byte is never 0.
func spsProfileIDC(au []byte) (idc byte, ok bool) {
	for i := 0; i+3 < len(au); i++ {
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			continue
		}
		nal := i + 3 // the NAL header (a 4-byte start code is a zero byte, then this 3-byte one)
		switch au[nal] & 0x1f {
		case 7: // SPS
			if nal+1 < len(au) {
				return au[nal+1], true
			}
			return 0, false
		case 1, 2, 3, 4, 5: // a slice: no SPS in front of it
			return 0, false
		}
	}
	return 0, false
}
