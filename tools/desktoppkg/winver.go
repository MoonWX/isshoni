package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// finalBuild is the fourth number of a final release's winver: above every prerelease's N mod 65000, so an rc sorts
// below its final release in NSIS's ${VersionCompare} (06 §5.3, §6.6).
const finalBuild = 65000

// semverBuildRE is SemVer 2.0 with optional build metadata (semver.org's regular expression): what `task
// desktop:exe:windows VERSION=…` and the installer accept (0.0.0-dev+1a2b3c included).
var semverBuildRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
	`(?:-((?:0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?` +
	`(?:\+([0-9a-zA-Z-]+(?:\.[0-9a-zA-Z-]+)*))?$`)

// winver is 06 §5.3's four-part Windows version of a SemVer X.Y.Z[-pre][+build]: X.Y.Z.W with W = 65000 for a
// final release, N mod 65000 for a prerelease whose last identifier is a number N, and 0 for any other prerelease.
// X, Y or Z above 65535 is an error: PE version resources and NSIS's VIProductVersion hold 16-bit numbers.
func winver(v string) (string, error) {
	m := semverBuildRE.FindStringSubmatch(v)
	if m == nil {
		return "", fmt.Errorf("version %q is not X.Y.Z[-pre][+build]", v)
	}
	parts := make([]string, 0, 4)
	for i, name := range []string{"X", "Y", "Z"} {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil || n > 65535 {
			return "", fmt.Errorf("version %q: %s is above 65535", v, name)
		}
		parts = append(parts, strconv.FormatUint(n, 10))
	}
	w := uint64(finalBuild)
	if pre := m[4]; pre != "" {
		w = 0
		// N mod 65000 digit by digit, so a number of any length works.
		if last := pre[strings.LastIndexByte(pre, '.')+1:]; allDigits(last) {
			for i := range len(last) {
				w = (w*10 + uint64(last[i]-'0')) % finalBuild
			}
		}
	}
	parts = append(parts, strconv.FormatUint(w, 10))
	return strings.Join(parts, "."), nil
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// runWinver is `desktoppkg winver <semver>` (06 §5.3): prints the four 16-bit numbers of the PE version resources
// and NSIS's VIProductVersion (0.2.0 → 0.2.0.65000, 0.2.0-rc.2 → 0.2.0.2, 0.0.0-dev+x → 0.0.0.0).
func runWinver(_ context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("winver", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 1 {
		return fmt.Errorf("%w: winver takes one SemVer", errUsage)
	}
	w, err := winver(fs.Arg(0))
	if err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, w)
	return err
}
