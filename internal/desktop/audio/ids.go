package audio

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/MoonWX/isshoni/internal/media/audiorules"
)

// rulesOS is the OS whose names the rules use. M2's engines are the Windows DLL and the fake, which scripts a Windows
// PC (on the Mac and on CI ubuntu too), so the service always builds Windows exe-name rules; M3 and M4 choose by
// runtime.GOOS when their engines come (05 §20).
const rulesOS = "windows"

// The ids and names of isshoni itself (05 §14.3 row 1): locked, always kept out.
const (
	selfAppID = "isshoni"
	selfName  = "isshoni"
	selfExe   = "isshoni.exe"
	exePrefix = "exe:"
)

// The limits of an exe id's file name (05 §14.2's ErrInvalidAppID).
const (
	minExeName = len("x.exe")
	maxExeName = 255
)

// exeOf returns the exe name of an "exe:" app id, and whether id is one.
func exeOf(id string) (string, bool) {
	return strings.CutPrefix(id, exePrefix)
}

// validAppID reports whether id is "exe:" + a lower-case exe file name of 5–255 bytes ending in ".exe", without
// \ / : * ? " < > | or control characters (05 §14.2).
func validAppID(id string) bool {
	name, ok := exeOf(id)
	if !ok || len(name) < minExeName || len(name) > maxExeName || !strings.HasSuffix(name, ".exe") ||
		!utf8.ValidString(name) || strings.ToLower(name) != name || strings.ContainsAny(name, `\/:*?"<>|`) {
		return false
	}
	return !strings.ContainsFunc(name, unicode.IsControl)
}

// lockedApp reports whether id is locked (05 §14.2's ErrLockedApp): a built-in voice app's id ("discord") or one of
// its exe ids ("exe:discordptb.exe"), or isshoni's ("isshoni", "exe:isshoni.exe"). Nobody can include those in M2
// (05 §25 question 3), and decisions saved for them are ignored.
func lockedApp(id string) bool {
	if id == selfAppID || id == exePrefix+selfExe {
		return true
	}
	if _, ok := audiorules.VoiceAppByID(id); ok {
		return true
	}
	if exe, ok := exeOf(id); ok {
		_, voice := audiorules.VoiceAppByProcess(rulesOS, exe)
		return voice
	}
	return false
}

// validDecision reports whether d can be saved: keep-out or include.
func validDecision(d Decision) bool {
	return d == DecisionKeepOut || d == DecisionInclude
}
