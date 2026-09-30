package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/protocol"
	"github.com/MoonWX/isshoni/internal/server/store"
	"github.com/MoonWX/isshoni/internal/version"
)

// TestVersionLdflags builds isshoni the way task build and goreleaser do (04 §15) and checks that
// "isshoni version --json" shows the injected values and --short prints the version.
func TestVersionLdflags(t *testing.T) {
	goBin, err := exec.LookPath("go")
	if err != nil {
		t.Skip("no go command in PATH:", err)
	}
	const (
		wantVersion = "9.8.7-rc.1"
		wantCommit  = "0123456789abcdef0123456789abcdef01234567"
		wantDate    = "2026-09-29T10:15:00Z"
		pkg         = "github.com/MoonWX/isshoni/internal/version"
	)
	bin := filepath.Join(t.TempDir(), "isshoni")
	if runtime.GOOS == "windows" {
		bin += ".exe"
	}
	ldflags := "-s -w -X " + pkg + ".version=" + wantVersion + " -X " + pkg + ".commit=" + wantCommit + " -X " + pkg + ".date=" + wantDate
	//nolint:gosec // G204: the go tool from PATH with fixed arguments
	build := exec.CommandContext(t.Context(), goBin, "build", "-trimpath", "-buildvcs=false", "-ldflags", ldflags, "-o", bin, ".")
	build.Env = append(os.Environ(), "CGO_ENABLED=0") // like the release build
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build: %v\n%s", err, out)
	}

	cli := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(t.Context(), bin, args...).Output() //nolint:gosec // G204: the binary built above
		if err != nil {
			t.Fatalf("isshoni %s: %v", strings.Join(args, " "), err)
		}
		return string(out)
	}

	var got version.BuildInfo
	out := cli("version", "--json")
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("version --json = %q: %v", out, err)
	}
	want := version.BuildInfo{
		Version: wantVersion, Commit: wantCommit, Date: wantDate,
		Go: runtime.Version(), OS: runtime.GOOS, Arch: runtime.GOARCH,
		Protocol: protocol.Version, Schema: store.LatestSchemaVersion(),
	}
	if got != want {
		t.Errorf("version --json = %+v\nwant            %+v", got, want)
	}
	if strings.Count(out, "\n") != 1 || !strings.HasSuffix(out, "}\n") {
		t.Errorf("version --json = %q, want one JSON document on one line", out)
	}

	if out := cli("version", "--short"); out != wantVersion+"\n" {
		t.Errorf("version --short = %q, want %q", out, wantVersion+"\n")
	}

	line := fmt.Sprintf("isshoni 9.8.7-rc.1 (commit 0123456, built 2026-09-29, %s, %s/%s, protocol %d, schema %d)\n",
		runtime.Version(), runtime.GOOS, runtime.GOARCH, protocol.Version, store.LatestSchemaVersion())
	if out := cli("version"); out != line {
		t.Errorf("version = %q, want %q", out, line)
	}
	if out := cli("--version"); out != line {
		t.Errorf("--version = %q, want %q", out, line)
	}
}

// In-process, the test binary reports the build-info fallback.
func TestVersionCommand(t *testing.T) {
	code, out, stderr := runCLI(t, "version", "--short")
	if code != exitOK || stderr != "" || out != version.Version()+"\n" {
		t.Errorf("version --short: exit %d, stdout %q, stderr %q", code, out, stderr)
	}

	code, out, _ = runCLI(t, "version", "--json")
	var got version.BuildInfo
	if err := json.Unmarshal([]byte(out), &got); err != nil || code != exitOK {
		t.Fatalf("version --json: exit %d, %q: %v", code, out, err)
	}
	want := version.Info()
	want.Protocol, want.Schema = protocol.Version, store.LatestSchemaVersion()
	if got != want {
		t.Errorf("version --json = %+v, want %+v", got, want)
	}
	// The numbers come from 01 and 03, not zero (which would leave them out of the JSON).
	if got.Protocol < 1 || got.Schema < 1 || !strings.Contains(out, fmt.Sprintf(`"protocol":%d,"schema":%d}`, protocol.Version, store.LatestSchemaVersion())) {
		t.Errorf("version --json = %s: want protocol %d and schema %d", out, protocol.Version, store.LatestSchemaVersion())
	}

	_, line, _ := runCLI(t, "version")
	if _, global, _ := runCLI(t, "--version"); global != line || line != versionLine(buildInfo())+"\n" {
		t.Errorf("version = %q, --version = %q", line, global)
	}
}

func TestVersionLine(t *testing.T) {
	tests := []struct {
		bi   version.BuildInfo
		want string
	}{
		{
			version.BuildInfo{
				Version: "0.3.0", Commit: "1a2b3c4d5e6f", Date: "2026-09-29T23:30:00Z", Go: "go1.27.1", OS: "linux",
				Arch: "amd64", Protocol: 1, Schema: 7,
			},
			"isshoni 0.3.0 (commit 1a2b3c4, built 2026-09-29, go1.27.1, linux/amd64, protocol 1, schema 7)",
		},
		{
			version.BuildInfo{Version: "0.0.0-dev", Go: "go1.26.5", OS: "darwin", Arch: "arm64"},
			"isshoni 0.0.0-dev (go1.26.5, darwin/arm64)",
		},
		{
			version.BuildInfo{Version: "0.0.0-dev+abc", Commit: "abc", Date: "yesterday", Go: "go1.26.5", OS: "linux", Arch: "arm64"},
			"isshoni 0.0.0-dev+abc (commit abc, built yesterday, go1.26.5, linux/arm64)",
		},
	}
	for _, tt := range tests {
		if got := versionLine(tt.bi); got != tt.want {
			t.Errorf("versionLine = %q\n          want %q", got, tt.want)
		}
	}
}
