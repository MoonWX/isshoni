// Package version reports which build of isshoni is running.
//
// Release and task builds set three symbols with the linker (04 §15, 06 §7.2):
//
//	-X github.com/MoonWX/isshoni/internal/version.version=0.3.0         (SemVer, no leading v)
//	-X github.com/MoonWX/isshoni/internal/version.commit=<full commit>
//	-X github.com/MoonWX/isshoni/internal/version.date=<commit date>    (RFC 3339)
//
// The date is the commit date, not the build time, so builds stay reproducible.
//
// Without those flags (go run, go build, go install) the Go build info fills in: a released module version such as
// v0.3.0 from "go install …@v0.3.0"; otherwise 0.0.0-dev+<12-char commit>, plus -dirty for a modified tree; without
// VCS information either, 0.0.0-dev.
//
// This package is a leaf: it imports nothing from this repository.
package version

import (
	"regexp"
	"runtime"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

// Set by the linker (-ldflags -X). Read them only through the functions below, which apply the build-info fallback.
var (
	version string
	commit  string
	date    string
)

// DocsURL is the project site; product links use it plus 06's anchor contract (06 §10.4).
const DocsURL = "https://moonwx.github.io/isshoni/"

const (
	repoURL    = "https://github.com/MoonWX/isshoni"
	devVersion = "0.0.0-dev"
	// shortCommit is the commit length in dev versions, the same as the Taskfile's `git rev-parse --short=12`.
	shortCommit = 12
)

// BuildInfo is what `isshoni version --json` prints.
type BuildInfo struct {
	Version  string `json:"version"`            // "0.3.0"
	Commit   string `json:"commit"`             // full commit hash; "" when unknown
	Date     string `json:"date"`               // commit date, RFC 3339 in UTC; "" when unknown
	Go       string `json:"go"`                 // "go1.26.5"
	OS       string `json:"os"`                 // runtime.GOOS
	Arch     string `json:"arch"`               // runtime.GOARCH
	Protocol int    `json:"protocol,omitempty"` // filled by cmd/isshoni from protocol.Version (01)
	Schema   int    `json:"schema,omitempty"`   // filled by cmd/isshoni from 03's latest migration number
}

// Version returns the SemVer version without a leading "v", e.g. "0.3.0" or "0.0.0-dev+1a2b3c4d5e6f".
func Version() string { return current().version }

// Commit returns the full commit hash of the build, or "" when it is unknown. A version from "go install …@<pseudo
// version>" carries only the 12-character short hash.
func Commit() string { return current().commit }

// BuildDate returns the commit date of the build, or the zero time when it is unknown.
func BuildDate() time.Time { return current().date }

// IsDev reports whether this is a dev build: one whose SemVer prerelease starts with "dev" (0.0.0-dev,
// 0.0.0-dev+1a2b3c4, 0.1.1-dev.1a2b3c4). CI builds (0.0.0-ci.N), e2e builds (0.0.0-e2e.local) and releases are
// not. 01 and 05 apply the same rule.
func IsDev() bool { return isDev(Version()) }

// UserAgent is the User-Agent of every outbound HTTP request, e.g. "isshoni/0.3.0 (+https://github.com/MoonWX/isshoni)".
func UserAgent() string { return "isshoni/" + Version() + " (+" + repoURL + ")" }

// Info returns the build information. Protocol and Schema are zero: this package imports nothing from internal/, so
// cmd/isshoni fills them in.
func Info() BuildInfo {
	b := current()
	return BuildInfo{
		Version: b.version,
		Commit:  b.commit,
		Date:    b.dateString(),
		Go:      runtime.Version(),
		OS:      runtime.GOOS,
		Arch:    runtime.GOARCH,
	}
}

// build is the resolved version information.
type build struct {
	version string
	commit  string
	date    time.Time
	rawDate string // the linker's date when it is not RFC 3339, so Info still shows what was injected
}

func (b build) dateString() string {
	if !b.date.IsZero() {
		return b.date.UTC().Format(time.RFC3339)
	}
	return b.rawDate
}

var current = sync.OnceValue(func() build {
	bi, _ := debug.ReadBuildInfo()
	return resolve(version, commit, date, bi)
})

// resolve combines the linker values with the Go build info (nil when unavailable).
func resolve(ldVersion, ldCommit, ldDate string, bi *debug.BuildInfo) build {
	var mainVersion, vcsRevision, vcsTime string
	var vcsModified bool
	if bi != nil {
		mainVersion = bi.Main.Version
		for _, s := range bi.Settings {
			switch s.Key {
			case "vcs.revision":
				vcsRevision = s.Value
			case "vcs.time":
				vcsTime = s.Value
			case "vcs.modified":
				vcsModified = s.Value == "true"
			}
		}
	}
	pseudoRev := pseudoRevision(mainVersion)

	var b build
	b.commit = firstNonEmpty(ldCommit, vcsRevision, pseudoRev)

	rawDate := firstNonEmpty(ldDate, vcsTime)
	if t, err := time.Parse(time.RFC3339, rawDate); err == nil {
		b.date = t
	} else {
		b.rawDate = rawDate
	}

	switch {
	case ldVersion != "":
		b.version = strings.TrimPrefix(ldVersion, "v")
	case isModuleRelease(mainVersion):
		b.version = strings.TrimPrefix(mainVersion, "v")
	default:
		b.version = devVersion
		if rev := firstNonEmpty(vcsRevision, pseudoRev); rev != "" {
			b.version += "+" + shorten(rev, shortCommit)
			if vcsModified {
				b.version += "-dirty"
			}
		}
	}
	return b
}

// isDev reports whether the SemVer prerelease of v starts with "dev".
func isDev(v string) bool {
	v, _, _ = strings.Cut(v, "+") // build metadata
	_, pre, ok := strings.Cut(v, "-")
	return ok && strings.HasPrefix(pre, "dev")
}

// semverRE matches a module version as Go stamps it: vMAJOR.MINOR.PATCH, an optional prerelease and build metadata.
var semverRE = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// pseudoRE matches a Go pseudo-version (golang.org/x/mod/module), capturing its revision.
var pseudoRE = regexp.MustCompile(`^v[0-9]+\.(?:0\.0-|\d+\.\d+-(?:[^+]*\.)?0\.)\d{14}-([A-Za-z0-9]+)(?:\+[0-9A-Za-z-]+(?:\.[0-9A-Za-z-]+)*)?$`)

// isModuleRelease reports whether v is a module version that names a release or tag, not "(devel)" or a
// pseudo-version (which Go 1.24+ stamps into every "go build" of an untagged commit).
func isModuleRelease(v string) bool {
	return semverRE.MatchString(v) && !pseudoRE.MatchString(v)
}

// pseudoRevision returns the revision of a pseudo-version, or "".
func pseudoRevision(v string) string {
	if m := pseudoRE.FindStringSubmatch(v); m != nil {
		return m[1]
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func shorten(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
