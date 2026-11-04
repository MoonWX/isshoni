package main

// The few H.264 facts capture checks in the engine's access units (Annex B, 4-byte start codes after the
// normalizer, 04 §9.4; 3-byte ones are accepted too).

const (
	nalSlice = 1
	nalIDR   = 5
	nalSPS   = 7
	nalPPS   = 8
)

// nalUnits splits an Annex B access unit into its NAL units (without start codes; trailing zero bytes dropped).
func nalUnits(au []byte) [][]byte {
	var units [][]byte
	start := -1
	for i := 0; i+2 < len(au); i++ {
		if au[i] != 0 || au[i+1] != 0 || au[i+2] != 1 {
			continue
		}
		if start >= 0 {
			units = append(units, trimZeros(au[start:i]))
		}
		start = i + 3
		i += 2
	}
	if start >= 0 && start <= len(au) {
		units = append(units, trimZeros(au[start:]))
	}
	out := units[:0]
	for _, u := range units {
		if len(u) > 0 {
			out = append(out, u)
		}
	}
	return out
}

func trimZeros(b []byte) []byte {
	for len(b) > 0 && b[len(b)-1] == 0 {
		b = b[:len(b)-1]
	}
	return b
}

// nalTypes is each unit's nal_unit_type, in order.
func nalTypes(au []byte) []uint8 {
	units := nalUnits(au)
	types := make([]uint8, 0, len(units))
	for _, u := range units {
		types = append(types, u[0]&0x1F)
	}
	return types
}

// spsProfile is the profile_idc and constraint byte of the access unit's first SPS; ok false without one.
func spsProfile(au []byte) (profileIDC, constraints uint8, ok bool) {
	for _, u := range nalUnits(au) {
		if u[0]&0x1F == nalSPS && len(u) >= 3 {
			return u[1], u[2], true
		}
	}
	return 0, 0, false
}

// isKeyframeAU is 04 §9.4's keyframe shape: SPS, PPS, then IDR slices only.
func isKeyframeAU(types []uint8) bool {
	if len(types) < 3 || types[0] != nalSPS || types[1] != nalPPS {
		return false
	}
	for _, t := range types[2:] {
		if t != nalIDR {
			return false
		}
	}
	return true
}
