package version

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// SemVer 2.0 parsing and precedence (https://semver.org/spec/v2.0.0.html), shared by the hub's minClientVersion floor
// (01 §6.1), the desktop app's compatibility verdicts (M2 02 §13.2) and the updater's "no downgrades" check (M2 06).
// The code moved here from internal/server/signal/semver.go with its behavior unchanged, so client and server agree.

// ErrNotSemVer is returned (wrapped) by Parse and Compare for a string that is not a SemVer 2.0 version.
var ErrNotSemVer = errors.New("version: not a SemVer 2.0 version")

// SemVer is a parsed SemVer 2.0 version. Build metadata is dropped: it has no precedence.
type SemVer struct {
	Major, Minor, Patch uint64
	Pre                 []string // pre-release identifiers, e.g. ["rc", "1"]; nil for a release
}

// Parse parses a SemVer 2.0 version; a leading "v" is accepted. MAJOR.MINOR.PATCH are numbers without leading zeros
// that fit in a uint64; pre-release and build identifiers are non-empty [0-9A-Za-z-], and numeric pre-release
// identifiers have no leading zero. The build metadata is checked and then dropped.
func Parse(s string) (SemVer, error) {
	v, ok := parse(s)
	if !ok {
		return SemVer{}, fmt.Errorf("%w: %q", ErrNotSemVer, s)
	}
	return v, nil
}

// Compare returns -1, 0 or +1 as a's precedence is below, equal to or above b's. It returns an error wrapping
// ErrNotSemVer when either isn't SemVer. Versions that differ only in build metadata compare equal.
func Compare(a, b string) (int, error) {
	va, err := Parse(a)
	if err != nil {
		return 0, err
	}
	vb, err := Parse(b)
	if err != nil {
		return 0, err
	}
	return va.Compare(vb), nil
}

// Below reports whether the version v is below floor by SemVer 2.0 precedence; a leading "v" is accepted on both. A v
// that is not SemVer counts as below, so an unknown build can't slip under the floor. ok is false when floor itself
// is not SemVer: the floor then can't be enforced, and callers treat it as no floor. A dev build
// (0.0.0-dev+<commit>) is below every floor above 0.0.0-dev.
func Below(v, floor string) (below, ok bool) {
	f, ok := parse(floor)
	if !ok {
		return false, false
	}
	cv, ok := parse(v)
	if !ok {
		return true, true
	}
	return cv.Compare(f) < 0, true
}

// Compare returns -1, 0 or +1 as a's precedence is below, equal to or above b's.
func (a SemVer) Compare(b SemVer) int {
	for _, d := range [3][2]uint64{{a.Major, b.Major}, {a.Minor, b.Minor}, {a.Patch, b.Patch}} {
		switch {
		case d[0] < d[1]:
			return -1
		case d[0] > d[1]:
			return 1
		}
	}
	switch {
	case len(a.Pre) == 0 && len(b.Pre) == 0:
		return 0
	case len(a.Pre) == 0: // a release has higher precedence than its pre-releases
		return 1
	case len(b.Pre) == 0:
		return -1
	}
	for i := 0; i < len(a.Pre) && i < len(b.Pre); i++ {
		if c := compareIdent(a.Pre[i], b.Pre[i]); c != 0 {
			return c
		}
	}
	switch {
	case len(a.Pre) < len(b.Pre):
		return -1
	case len(a.Pre) > len(b.Pre):
		return 1
	}
	return 0
}

func parse(s string) (SemVer, bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexByte(s, '+'); i >= 0 {
		if !validIdents(s[i+1:], false) {
			return SemVer{}, false
		}
		s = s[:i]
	}
	var v SemVer
	core, pre, hasPre := strings.Cut(s, "-")
	if hasPre {
		if !validIdents(pre, true) {
			return SemVer{}, false
		}
		v.Pre = strings.Split(pre, ".")
	}
	parts := strings.Split(core, ".")
	if len(parts) != 3 {
		return SemVer{}, false
	}
	nums := [3]*uint64{&v.Major, &v.Minor, &v.Patch}
	for i, p := range parts {
		if !isNumeric(p) || (len(p) > 1 && p[0] == '0') {
			return SemVer{}, false
		}
		n, err := strconv.ParseUint(p, 10, 64)
		if err != nil {
			return SemVer{}, false
		}
		*nums[i] = n
	}
	return v, true
}

// validIdents checks dot-separated SemVer identifiers: non-empty, [0-9A-Za-z-]; with noLeadingZero (pre-release),
// numeric identifiers have no leading zero.
func validIdents(s string, noLeadingZero bool) bool {
	for id := range strings.SplitSeq(s, ".") {
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
