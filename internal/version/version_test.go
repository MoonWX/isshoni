package version

import (
	"encoding/json"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

const (
	testRev   = "6d4a39e1234a5b6c7d8e9f001122334455667788" // 40 hex characters
	testShort = "6d4a39e1234a"
)

func buildInfo(mainVersion string, settings ...string) *debug.BuildInfo {
	bi := &debug.BuildInfo{Main: debug.Module{Path: "github.com/MoonWX/isshoni", Version: mainVersion}}
	for i := 0; i+1 < len(settings); i += 2 {
		bi.Settings = append(bi.Settings, debug.BuildSetting{Key: settings[i], Value: settings[i+1]})
	}
	return bi
}

func TestResolve(t *testing.T) {
	commitDate := time.Date(2026, 9, 29, 10, 15, 0, 0, time.UTC)
	tests := []struct {
		name                     string
		ldVersion, ldCommit, ldD string
		bi                       *debug.BuildInfo
		wantVersion, wantCommit  string
		wantDate                 string // BuildInfo.Date
	}{
		{
			name:      "ldflags win over the build info",
			ldVersion: "0.3.0", ldCommit: testRev, ldD: "2026-09-29T10:15:00Z",
			bi:          buildInfo("v0.0.0-20260930110800-aaaaaaaaaaaa", "vcs.revision", strings.Repeat("a", 40)),
			wantVersion: "0.3.0", wantCommit: testRev, wantDate: "2026-09-29T10:15:00Z",
		},
		{
			name:      "leading v is dropped; an offset date is shown in UTC",
			ldVersion: "v1.2.3-rc.1", ldD: "2026-09-29T12:15:00+02:00",
			wantVersion: "1.2.3-rc.1", wantDate: "2026-09-29T10:15:00Z",
		},
		{
			name:      "a date that is not RFC 3339 is kept as injected",
			ldVersion: "0.3.0", ldD: "yesterday",
			wantVersion: "0.3.0", wantDate: "yesterday",
		},
		{
			name:        "ldflags version only (task dev:server): commit from VCS",
			ldVersion:   "0.0.0-dev+" + testShort,
			bi:          buildInfo("(devel)", "vcs.revision", testRev, "vcs.time", "2026-09-29T10:15:00Z"),
			wantVersion: "0.0.0-dev+" + testShort, wantCommit: testRev, wantDate: "2026-09-29T10:15:00Z",
		},
		{
			name:        "go install at a tag",
			bi:          buildInfo("v0.3.0"),
			wantVersion: "0.3.0",
		},
		{
			name:        "go build at a tag with local changes",
			bi:          buildInfo("v0.3.0+dirty", "vcs.revision", testRev, "vcs.modified", "true"),
			wantVersion: "0.3.0+dirty", wantCommit: testRev,
		},
		{
			name:        "go run: no module version, clean VCS",
			bi:          buildInfo("(devel)", "vcs.revision", testRev, "vcs.modified", "false", "vcs.time", commitDate.Format(time.RFC3339)),
			wantVersion: "0.0.0-dev+" + testShort, wantCommit: testRev, wantDate: "2026-09-29T10:15:00Z",
		},
		{
			name:        "modified tree",
			bi:          buildInfo("(devel)", "vcs.revision", testRev, "vcs.modified", "true"),
			wantVersion: "0.0.0-dev+" + testShort + "-dirty", wantCommit: testRev,
		},
		{
			name:        "go build of an untagged commit (pseudo-version) is a dev build",
			bi:          buildInfo("v0.0.0-20260930110800-"+testShort+"+dirty", "vcs.revision", testRev, "vcs.modified", "true"),
			wantVersion: "0.0.0-dev+" + testShort + "-dirty", wantCommit: testRev,
		},
		{
			name:        "go install at a pseudo-version: the revision comes from the version",
			bi:          buildInfo("v0.1.1-0.20260930110800-" + testShort),
			wantVersion: "0.0.0-dev+" + testShort, wantCommit: testShort,
		},
		{
			name:        "pseudo-version after a prerelease tag",
			bi:          buildInfo("v0.2.0-rc.1.0.20260930110800-" + testShort),
			wantVersion: "0.0.0-dev+" + testShort, wantCommit: testShort,
		},
		{
			name:        "no VCS information",
			bi:          buildInfo("(devel)"),
			wantVersion: "0.0.0-dev",
		},
		{
			name:        "no build info at all",
			wantVersion: "0.0.0-dev",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := resolve(tt.ldVersion, tt.ldCommit, tt.ldD, tt.bi)
			if b.version != tt.wantVersion {
				t.Errorf("version = %q, want %q", b.version, tt.wantVersion)
			}
			if b.commit != tt.wantCommit {
				t.Errorf("commit = %q, want %q", b.commit, tt.wantCommit)
			}
			if got := b.dateString(); got != tt.wantDate {
				t.Errorf("date = %q, want %q", got, tt.wantDate)
			}
		})
	}
}

func TestResolveBuildDate(t *testing.T) {
	b := resolve("0.3.0", "", "2026-09-29T12:15:00+02:00", nil)
	want := time.Date(2026, 9, 29, 10, 15, 0, 0, time.UTC)
	if !b.date.Equal(want) {
		t.Errorf("date = %v, want %v", b.date, want)
	}
	if b := resolve("0.3.0", "", "", nil); !b.date.IsZero() {
		t.Errorf("date = %v, want zero without a date", b.date)
	}
}

func TestIsDev(t *testing.T) {
	tests := map[string]bool{
		"0.0.0-dev":                    true,
		"0.0.0-dev+1a2b3c4":            true,
		"0.0.0-dev+1a2b3c4d5e6f-dirty": true,
		"0.1.1-dev.1a2b3c4":            true, // goreleaser snapshot default (06 §9.2)
		"0.3.0":                        false,
		"0.3.1-rc.1":                   false,
		"0.0.0-ci.42":                  false,
		"0.0.0-e2e.local":              false,
		"0.0.0-dryrun.7":               false,
		"1.0.0+dev":                    false, // build metadata, not a prerelease
		"":                             false,
	}
	for v, want := range tests {
		if got := isDev(v); got != want {
			t.Errorf("isDev(%q) = %v, want %v", v, got, want)
		}
	}
}

func TestIsModuleRelease(t *testing.T) {
	tests := map[string]bool{
		"v0.3.0":                             true,
		"v0.3.0+dirty":                       true,
		"v1.2.3-rc.1":                        true,
		"(devel)":                            false,
		"":                                   false,
		"0.3.0":                              false,
		"v0.0.0-20260930110800-6d4a39e1234a": false,
		"v0.0.0-20260930110800-6d4a39e1234a+dirty":  false,
		"v0.3.1-0.20260930110800-6d4a39e1234a":      false,
		"v0.3.0-rc.1.0.20260930110800-6d4a39e1234a": false,
	}
	for v, want := range tests {
		if got := isModuleRelease(v); got != want {
			t.Errorf("isModuleRelease(%q) = %v, want %v", v, got, want)
		}
	}
}

// The test binary has no linker values and no VCS stamp, so it reports the plain dev version.
func TestTestBinaryIsDev(t *testing.T) {
	if !IsDev() {
		t.Errorf("IsDev() = false for %q", Version())
	}
	if !strings.HasPrefix(Version(), devVersion) {
		t.Errorf("Version() = %q, want a %s version", Version(), devVersion)
	}
}

func TestUserAgent(t *testing.T) {
	want := "isshoni/" + Version() + " (+https://github.com/MoonWX/isshoni)"
	if got := UserAgent(); got != want {
		t.Errorf("UserAgent() = %q, want %q", got, want)
	}
}

func TestDocsURL(t *testing.T) {
	if !strings.HasPrefix(DocsURL, "https://") || !strings.HasSuffix(DocsURL, "/") {
		t.Errorf("DocsURL = %q: product links append paths, so it must be https and end in /", DocsURL)
	}
}

func TestInfo(t *testing.T) {
	bi := Info()
	if bi.Version != Version() || bi.Commit != Commit() {
		t.Errorf("Info() = %+v, disagrees with Version()/Commit()", bi)
	}
	if bi.Go != runtime.Version() || bi.OS != runtime.GOOS || bi.Arch != runtime.GOARCH {
		t.Errorf("Info() = %+v, want the running toolchain and platform", bi)
	}
	if bi.Protocol != 0 || bi.Schema != 0 {
		t.Errorf("Info() = %+v: Protocol and Schema are cmd/isshoni's to fill", bi)
	}
}

func TestBuildInfoJSON(t *testing.T) {
	bi := BuildInfo{Version: "0.3.0", Commit: testRev, Date: "2026-09-29T10:15:00Z", Go: "go1.26.5", OS: "linux", Arch: "amd64"}
	got, err := json.Marshal(bi)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":"0.3.0","commit":"` + testRev + `","date":"2026-09-29T10:15:00Z","go":"go1.26.5","os":"linux","arch":"amd64"}`
	if string(got) != want {
		t.Errorf("json = %s\nwant   %s", got, want)
	}
	bi.Protocol, bi.Schema = 1, 7
	got, err = json.Marshal(bi)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(got), `"arch":"amd64","protocol":1,"schema":7}`) {
		t.Errorf("json = %s, want protocol and schema at the end", got)
	}
}
