package signal

import (
	"strconv"
	"strings"
)

// versionBelow reports whether the client version v is below the floor min (01 §6.1, Policy.MinClientVersion),
// comparing by SemVer 2.0 precedence; a leading "v" is accepted on both. A v that is not SemVer counts as below, so
// an unknown build can't slip under the floor. ok is false when min itself is not SemVer: the floor then can't be
// enforced.
func versionBelow(v, floor string) (below, ok bool) {
	f, ok := parseSemver(floor)
	if !ok {
		return false, false
	}
	cv, ok := parseSemver(v)
	if !ok {
		return true, true
	}
	return cv.compare(f) < 0, true
}

// semver is a parsed SemVer 2.0 version; build metadata is dropped (it has no precedence).
type semver struct {
	major, minor, patch uint64
	pre                 []string
}

func parseSemver(s string) (semver, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !validIdents(s[i+1:], false) {
			return semver{}, false
		}
		s = s[:i]
	}
	var v semver
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre {
		if !validIdents(pre, true) {
			return semver{}, false
		}
		v.pre = strings.Split(pre, ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return semver{}, false
	}
	nums := [3]*uint64{&v.major, &v.minor, &v.patch}
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return semver{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return semver{}, false
		}
		*nums[i] = n
	}
	return v, true
}

// validIdents checks dot-separated SemVer identifiers: non-empty, [0-9A-Za-z-]; with noLeadingZero (pre-release),
// numeric identifiers have no leading zero.
func validIdents(s string, noLeadingZero bool) bool {
	for _, id := range strings.Split(s, ".") {
		if id == "" {
			return false
		}
		for i := range len(id) {
			if !isIdentChar(id[i]) {
				return false
			}
		}
		if noLeadingZero && isNumeric(id) && len(id) > 1 && id[0] == '0' {
			return false
		}
	}
	return true
}

// isIdentChar reports whether c may appear in a SemVer identifier: [0-9A-Za-z-].
func isIdentChar(c byte) bool {
	switch {
	case c >= '0' && c <= '9', c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c == '-':
		return true
	}
	return false
}

func isNumeric(s string) bool {
	if s == "" {
		return false
	}
	for i := range len(s) {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// compare returns -1, 0 or +1 by SemVer precedence.
func (a semver) compare(b semver) int {
	for _, d := range [3][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	switch {
	case len(a.pre) == 0 && len(b.pre) == 0:
		return 0
	case len(a.pre) == 0: // a release has higher precedence than its pre-releases
		return 1
	case len(b.pre) == 0:
		return -1
	}
	for i := 0; i < len(a.pre) && i < len(b.pre); i++ {
		if c := compareIdent(a.pre[i], b.pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.pre) < len(b.pre):
		return -1
	case len(a.pre) > len(b.pre):
		return 1
	}
	return 0
}

// compareIdent compares pre-release identifiers: numeric ones numerically and below alphanumeric ones, which compare
// in ASCII order.
func compareIdent(a, b string) int {
	an, bn := isNumeric(a), isNumeric(b)
	switch {
	case an && bn:
		if len(a) != len(b) { // no leading zeros, so the longer number is larger
			if len(a) < len(b) {
				return -1
			}
			return 1
		}
		return strings.Compare(a, b)
	case an:
		return -1
	case bn:
		return 1
	}
	return strings.Compare(a, b)
}
