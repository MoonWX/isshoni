package update

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/version"
)

const testSHA256 = "6f4d0e1c3a2b5f8e9d7c6b5a4f3e2d1c0b9a8f7e6d5c4b3a2f1e0d9c8b7a6f5e"

// manifestFields returns the fields of a good manifest of version v, as tools/desktoppkg manifest writes them.
func manifestFields(v string) map[string]any {
	name := "isshoni-desktop_" + v + "_windows_amd64_setup.exe"
	return map[string]any{
		"schema":      1,
		"product":     "isshoni-desktop",
		"version":     v,
		"channel":     string(channelOf(mustCanonical(v))),
		"minVersion":  "0.2.0-rc.1",
		"publishedAt": "2026-12-01T10:00:00.000Z",
		"expiresAt":   "2027-05-30T10:00:00.000Z",
		"notesUrl":    "https://github.com/MoonWX/isshoni/releases/tag/v" + v,
		"artifacts": []any{map[string]any{
			"os":     "windows",
			"arch":   "amd64",
			"kind":   "nsis",
			"name":   name,
			"url":    "https://github.com/MoonWX/isshoni/releases/download/v" + v + "/" + name,
			"size":   15204352,
			"sha256": testSHA256,
		}},
		"revokedKeys": []string{},
	}
}

func mustCanonical(v string) version.SemVer {
	sv, err := canonical(v)
	if err != nil {
		panic(err)
	}
	return sv
}

// encode writes fields as JSON with sorted keys and two-space indentation, like tools/desktoppkg manifest.
func encode(t testing.TB, fields map[string]any) []byte {
	t.Helper()
	data, err := json.MarshalIndent(fields, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(data, '\n')
}

// with returns a copy of fields with each key set to its value; a nil value deletes the key.
func with(fields map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(fields))
	for k, v := range fields {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		if kv[i+1] == nil {
			delete(out, kv[i].(string))
		} else {
			out[kv[i].(string)] = kv[i+1]
		}
	}
	return out
}

// withArtifact returns a copy of fields whose first artifact has each key set to its value.
func withArtifact(fields map[string]any, kv ...any) map[string]any {
	a := make(map[string]any)
	for k, v := range fields["artifacts"].([]any)[0].(map[string]any) {
		a[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		a[kv[i].(string)] = kv[i+1]
	}
	return with(fields, "artifacts", []any{a})
}

// TestParseManifestDesktoppkgGolden parses the manifest that tools/desktoppkg manifest writes for its golden test,
// so the writer and the app's parser can't drift apart.
func TestParseManifestDesktoppkgGolden(t *testing.T) {
	golden := readText(t, filepath.Join("..", "..", "tools", "desktoppkg", "testdata", "manifest", "isshoni-desktop-update.json"))
	m, err := ParseManifest(golden)
	if err != nil {
		t.Fatalf("desktoppkg's golden manifest: %v", err)
	}
	a, ok := m.Artifact("windows", "amd64")
	if !ok || a.Name != "isshoni-desktop_0.2.1_windows_amd64_setup.exe" || m.Version != "0.2.1" ||
		m.Channel != ChannelStable || m.MinVersion != "0.2.0-rc.1" {
		t.Errorf("parsed %+v, artifact %+v", m, a)
	}
	if !m.ExpiresAt.Equal(m.PublishedAt.Add(180 * 24 * time.Hour)) {
		t.Errorf("expiresAt %v is not publishedAt %v + 180 days", m.ExpiresAt, m.PublishedAt)
	}
}

// TestParseManifestDesktoppkgTestBase parses the updater e2e manifest that tools/desktoppkg manifest writes with
// -releases for its golden test: an updatetest build whose ISSHONI_UPDATE_TEST_BASE is that releases URL accepts it
// (06 §8.11, §16), and the default rules refuse it.
func TestParseManifestDesktoppkgTestBase(t *testing.T) {
	const base = "http://127.0.0.1:8123" // desktoppkg's testBase
	golden := readText(t, filepath.Join("..", "..", "tools", "desktoppkg", "testdata", "manifest", "test-base", ManifestName))
	m, err := parseManifest(golden, rules{releases: base, testBuild: true})
	if err != nil {
		t.Fatalf("desktoppkg's test-base manifest under %s: %v", base, err)
	}
	a, ok := m.Artifact("windows", "amd64")
	if !ok || m.Version != "0.0.1-ci.2" || m.Channel != channelCI || m.NotesURL != base+"/tag/v0.0.1-ci.2" ||
		a.URL != base+"/download/v0.0.1-ci.2/isshoni-desktop_0.0.1-ci.2_windows_amd64_setup.exe" {
		t.Errorf("parsed %+v, artifact %+v", m, a)
	}
	if err := m.checkChannel(ChannelStable, true); err != nil {
		t.Errorf("an updatetest build on the stable channel: %v", err)
	}
	if _, err := ParseManifest(golden); CodeOf(err) != CodeBadManifest {
		t.Errorf("with the default rules: err = %v, want bad_manifest", err)
	}
	if _, err := parseManifest(golden, rules{releases: base + "/other", testBuild: true}); CodeOf(err) != CodeBadManifest {
		t.Errorf("under another test base: err = %v, want bad_manifest", err)
	}
}

func TestParseManifest(t *testing.T) {
	good := manifestFields("0.2.0")
	m, err := ParseManifest(encode(t, good))
	if err != nil {
		t.Fatal(err)
	}
	want := Manifest{
		Schema:      1,
		Product:     Product,
		Version:     "0.2.0",
		Channel:     ChannelStable,
		MinVersion:  "0.2.0-rc.1",
		PublishedAt: time.Date(2026, 12, 1, 10, 0, 0, 0, time.UTC),
		ExpiresAt:   time.Date(2027, 5, 30, 10, 0, 0, 0, time.UTC),
		NotesURL:    "https://github.com/MoonWX/isshoni/releases/tag/v0.2.0",
		Artifacts: []Artifact{{
			OS: "windows", Arch: "amd64", Kind: "nsis", Name: "isshoni-desktop_0.2.0_windows_amd64_setup.exe",
			URL:  "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe",
			Size: 15204352, SHA256: testSHA256,
		}},
		RevokedKeys: []string{},
	}
	if fmt.Sprintf("%+v", *m) != fmt.Sprintf("%+v", want) {
		t.Errorf("parsed\n%+v\nwant\n%+v", *m, want)
	}

	revoked := "SHA256:" + strings.Repeat("A", 43)
	beta := manifestFields("0.3.0-rc.2")
	ci := manifestFields("0.0.1-ci.7")
	accepted := []struct {
		name   string
		fields map[string]any
	}{
		{"unknown fields ignored", with(good, "future", map[string]any{"x": 1}, "signedBy", "someone")},
		{"unknown artifact field ignored", withArtifact(good, "arm64ec", true)},
		{"revokedKeys missing", with(good, "revokedKeys", nil)},
		{"revokedKeys null", with(good, "revokedKeys", json.RawMessage("null"))},
		{"one revoked key", with(good, "revokedKeys", []string{revoked})},
		{"64 revoked keys", with(good, "revokedKeys", repeat(revoked, MaxRevokedKeys))},
		{"no artifacts", with(good, "artifacts", []any{})},
		{"artifacts missing", with(good, "artifacts", nil)},
		{"beta", beta},
		{"ci", ci},
		{"dry run as ci", manifestFields("0.0.0-dryrun.12")},
		{"minVersion above version", with(good, "minVersion", "0.3.0")},
		{"another kind next to nsis", with(good, "artifacts", append(good["artifacts"].([]any), map[string]any{
			"os": "darwin", "arch": "universal", "kind": "darwin-app", "name": "isshoni-desktop_0.2.0_darwin_universal.tar.gz",
			"url":  "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_darwin_universal.tar.gz",
			"size": 1, "sha256": testSHA256,
		}))},
		{"times with an offset", with(good, "publishedAt", "2026-12-01T11:00:00+01:00")},
		{"largest installer", withArtifact(good, "size", MaxArtifactSize)},
	}
	for _, tt := range accepted {
		t.Run(tt.name, func(t *testing.T) {
			m, err := ParseManifest(encode(t, tt.fields))
			if err != nil {
				t.Fatal(err)
			}
			if m.RevokedKeys == nil {
				t.Error("RevokedKeys is nil, want an empty list")
			}
		})
	}

	otherName := "isshoni-desktop_0.2.0_windows_arm64_setup.exe"
	refused := []struct {
		name   string
		data   []byte
		code   string
		substr string
	}{
		{"schema 2", encode(t, with(good, "schema", 2)), CodeManifestUnsupported, "unsupported schema 2"},
		{"schema 2 with other field types", encode(t, with(good, "schema", 2, "version", map[string]any{"major": 1})), CodeManifestUnsupported, "schema"},
		{"schema missing", encode(t, with(good, "schema", nil)), CodeManifestUnsupported, "schema"},
		{"schema as a string", encode(t, with(good, "schema", "1")), CodeManifestUnsupported, "schema"},
		{"schema 1.0", []byte(strings.Replace(string(encode(t, good)), `"schema": 1`, `"schema": 1.0`, 1)), CodeManifestUnsupported, "schema"},
		{"not JSON", []byte("{"), CodeBadManifest, "manifest"},
		{"an array", []byte("[1]"), CodeBadManifest, "manifest"},
		{"trailing data", append(encode(t, good), "{}"...), CodeBadManifest, "manifest"},
		{"a field of the wrong type", encode(t, with(good, "version", 2)), CodeBadManifest, "version"},
		{"other product", encode(t, with(good, "product", "isshoni")), CodeBadManifest, `product "isshoni"`},
		{"version with v", encode(t, with(good, "version", "v0.2.0")), CodeBadManifest, "canonical"},
		{"version with build metadata", encode(t, with(good, "version", "0.2.0+abc")), CodeBadManifest, "canonical"},
		{"version not SemVer", encode(t, with(good, "version", "0.2")), CodeBadManifest, "not a SemVer"},
		{"version missing", encode(t, with(good, "version", nil)), CodeBadManifest, "version"},
		{"minVersion not SemVer", encode(t, with(good, "minVersion", "latest")), CodeBadManifest, "minVersion"},
		{"minVersion missing", encode(t, with(good, "minVersion", nil)), CodeBadManifest, "minVersion"},
		{"stable channel for an rc", encode(t, with(beta, "channel", "stable")), CodeBadManifest, `channel "stable" for version 0.3.0-rc.2, want "beta"`},
		{"beta channel for a release", encode(t, with(good, "channel", "beta")), CodeBadManifest, `want "stable"`},
		{"ci channel for a release", encode(t, with(good, "channel", "ci")), CodeBadManifest, `want "stable"`},
		{"beta channel for a ci build", encode(t, with(ci, "channel", "beta")), CodeBadManifest, `want "ci"`},
		{"unknown channel", encode(t, with(good, "channel", "nightly")), CodeBadManifest, `channel "nightly"`},
		{"publishedAt missing", encode(t, with(good, "publishedAt", nil)), CodeBadManifest, "no publishedAt"},
		{"publishedAt not RFC 3339", encode(t, with(good, "publishedAt", "2026-12-01")), CodeBadManifest, "manifest"},
		{"expiresAt missing", encode(t, with(good, "expiresAt", nil)), CodeBadManifest, "expiresAt"},
		{"expiresAt before publishedAt", encode(t, with(good, "expiresAt", "2026-11-01T10:00:00.000Z")), CodeBadManifest, "expiresAt"},
		{"notesUrl elsewhere", encode(t, with(good, "notesUrl", "https://example.com/notes")), CodeBadManifest, "notesUrl"},
		{"notesUrl of another version", encode(t, with(good, "notesUrl", "https://github.com/MoonWX/isshoni/releases/tag/v0.1.0")), CodeBadManifest, "notesUrl"},
		{"url on another host", encode(t, withArtifact(good, "url", "https://example.com/isshoni-desktop_0.2.0_windows_amd64_setup.exe")), CodeBadManifest, "url"},
		{"url over http", encode(t, withArtifact(good, "url", "http://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe")), CodeBadManifest, "url"},
		{"url of another version", encode(t, withArtifact(good, "url", "https://github.com/MoonWX/isshoni/releases/download/v0.1.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe")), CodeBadManifest, "url"},
		{"url of another repository", encode(t, withArtifact(good, "url", "https://github.com/MoonWX/isshoni-fork/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe")), CodeBadManifest, "url"},
		{"url with a query", encode(t, withArtifact(good, "url", "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_amd64_setup.exe?x=1")), CodeBadManifest, "url"},
		{"url of another name", encode(t, withArtifact(good, "url", "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/"+otherName)), CodeBadManifest, "url"},
		{"name of another version", encode(t, withArtifact(good, "name", "isshoni-desktop_0.1.0_windows_amd64_setup.exe")), CodeBadManifest, "name"},
		{"name with a path", encode(t, withArtifact(good, "name", "../isshoni.exe")), CodeBadManifest, "not a plain file name"},
		{"name with a backslash", encode(t, withArtifact(good, "name", `a\b.exe`)), CodeBadManifest, "not a plain file name"},
		{"name empty", encode(t, withArtifact(good, "name", "")), CodeBadManifest, "not a plain file name"},
		{"os empty", encode(t, withArtifact(good, "os", "")), CodeBadManifest, "os"},
		{"kind upper case", encode(t, withArtifact(good, "kind", "NSIS")), CodeBadManifest, "kind"},
		{"size 0", encode(t, withArtifact(good, "size", 0)), CodeBadManifest, "size 0"},
		{"size negative", encode(t, withArtifact(good, "size", -1)), CodeBadManifest, "size -1"},
		{"size above 256 MiB", encode(t, withArtifact(good, "size", MaxArtifactSize+1)), CodeBadManifest, "size 268435457"},
		{"sha256 upper case", encode(t, withArtifact(good, "sha256", strings.ToUpper(testSHA256))), CodeBadManifest, "sha256"},
		{"sha256 short", encode(t, withArtifact(good, "sha256", testSHA256[1:])), CodeBadManifest, "sha256"},
		{"two entries for windows/amd64", encode(t, with(good, "artifacts", append(good["artifacts"].([]any), good["artifacts"].([]any)[0]))), CodeBadManifest, "a second entry for windows/amd64"},
		{"revoked key not a fingerprint", encode(t, with(good, "revokedKeys", []string{"isshoni-update-1"})), CodeBadManifest, "revokedKeys[0]"},
		{"revoked key MD5", encode(t, with(good, "revokedKeys", []string{"MD5:aa:bb"})), CodeBadManifest, "revokedKeys[0]"},
		{"revoked key truncated", encode(t, with(good, "revokedKeys", []string{revoked[:len(revoked)-1]})), CodeBadManifest, "revokedKeys[0]"},
		{"65 revoked keys", encode(t, with(good, "revokedKeys", repeat(revoked, MaxRevokedKeys+1))), CodeBadManifest, "above the limit of 64"},
		{"above 64 KiB", pad(encode(t, good), MaxManifestSize+1), CodeBadManifest, "above the 65536-byte limit"},
	}
	for _, tt := range refused {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseManifest(tt.data)
			if CodeOf(err) != tt.code || !strings.Contains(fmt.Sprint(err), tt.substr) {
				t.Errorf("err = %v, want code %s and %q", err, tt.code, tt.substr)
			}
		})
	}

	t.Run("exactly 64 KiB", func(t *testing.T) {
		if _, err := ParseManifest(pad(encode(t, good), MaxManifestSize)); err != nil {
			t.Error(err)
		}
	})
}

// repeat returns n copies of s.
func repeat(s string, n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = s
	}
	return out
}

// pad appends spaces to a JSON document up to size bytes.
func pad(data []byte, size int) []byte {
	return append(bytes.Clone(data), bytes.Repeat([]byte(" "), size-len(data))...)
}

func TestManifestTestHooksRules(t *testing.T) {
	// ISSHONI_UPDATE_TEST_BASE replaces the releases URL in updatetest builds (06 §8.11).
	base := "http://127.0.0.1:8123"
	fields := manifestFields("0.0.1-ci.2")
	fields["notesUrl"] = base + "/tag/v0.0.1-ci.2"
	fields = withArtifact(fields, "url", base+"/download/v0.0.1-ci.2/isshoni-desktop_0.0.1-ci.2_windows_amd64_setup.exe")
	data := encode(t, fields)
	if _, err := parseManifest(data, rules{releases: base, testBuild: true}); err != nil {
		t.Errorf("a test manifest under the test base: %v", err)
	}
	if _, err := ParseManifest(data); CodeOf(err) != CodeBadManifest {
		t.Errorf("a test manifest with the default rules: err = %v, want bad_manifest", err)
	}
}

func TestCheckChannel(t *testing.T) {
	tests := []struct {
		manifest  Channel
		app       Channel
		testBuild bool
		ok        bool
	}{
		{ChannelStable, ChannelStable, false, true},
		{ChannelStable, ChannelBeta, false, true},
		{ChannelBeta, ChannelStable, false, false},
		{ChannelBeta, ChannelBeta, false, true},
		{channelCI, ChannelStable, false, false},
		{channelCI, ChannelBeta, false, false},
		{ChannelBeta, "", false, false},
		{ChannelBeta, "nightly", false, false},
		{ChannelStable, "", false, true},
		// updatetest builds take every channel, whatever the app's setting (06 §8.11).
		{ChannelStable, ChannelStable, true, true},
		{ChannelBeta, ChannelStable, true, true},
		{channelCI, ChannelStable, true, true},
		{channelCI, ChannelBeta, true, true},
	}
	for _, tt := range tests {
		m := &Manifest{Channel: tt.manifest}
		err := m.checkChannel(tt.app, tt.testBuild)
		switch {
		case tt.ok && err != nil:
			t.Errorf("%s manifest, app %q, test build %t: %v", tt.manifest, tt.app, tt.testBuild, err)
		case !tt.ok && CodeOf(err) != CodeWrongChannel:
			t.Errorf("%s manifest, app %q, test build %t: err = %v, want wrong_channel", tt.manifest, tt.app, tt.testBuild, err)
		}
	}
	if err := (&Manifest{Channel: channelCI}).CheckChannel(ChannelBeta); CodeOf(err) != CodeWrongChannel {
		t.Errorf("CheckChannel in a build without the updatetest hooks: err = %v, want wrong_channel", err)
	}
}

func TestEligible(t *testing.T) {
	tests := []struct {
		version       string
		release, test bool // Eligible in a release build, in an updatetest build
	}{
		{"0.2.0", true, true},
		{"1.10.3", true, true},
		{"0.2.0-rc.1", true, true},
		{"0.2.0-rc.12", true, true},
		{"0.0.1-ci.1234", false, true},
		{"0.0.0-dev+1a2b3c4d5e6f", false, false},
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", false, false},
		{"0.0.0-dev", false, false},
		{"0.0.0-dryrun.17", false, false},
		{"0.2.0-rc", false, false},
		{"0.2.0-rc.1.1", false, false},
		{"0.2.0-beta.1", false, false},
		{"0.2.0-ci", false, false},
		{"0.2.0-ci.x", false, false},
		{"0.2", false, false},
		{"", false, false},
		{"unknown", false, false},
	}
	for _, tt := range tests {
		if got := eligible(tt.version, false); got != tt.release {
			t.Errorf("eligible(%q) in a release build = %t, want %t", tt.version, got, tt.release)
		}
		if got := eligible(tt.version, true); got != tt.test {
			t.Errorf("eligible(%q) in an updatetest build = %t, want %t", tt.version, got, tt.test)
		}
		if got := Eligible(tt.version); got != tt.release {
			t.Errorf("Eligible(%q) = %t, want %t (this test binary has no updatetest hooks)", tt.version, got, tt.release)
		}
	}
}

// TestNewerAndRequired checks that versions compare by SemVer precedence, through internal/version.
func TestNewerAndRequired(t *testing.T) {
	newer := []struct {
		manifest, current string
		want              bool
	}{
		{"0.2.1", "0.2.0", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.2.1", false},
		{"0.2.0", "0.2.0-rc.3", true}, // rc < final
		{"0.2.0-rc.3", "0.2.0", false},
		{"0.2.0-rc.10", "0.2.0-rc.9", true}, // numeric identifiers compare as numbers
		{"0.10.0", "0.9.0", true},
		{"0.2.0", "0.2.0+build", false}, // build metadata has no precedence
	}
	for _, tt := range newer {
		m := &Manifest{Version: tt.manifest}
		if got, err := m.Newer(tt.current); err != nil || got != tt.want {
			t.Errorf("Newer: manifest %s, current %s = %t, %v; want %t", tt.manifest, tt.current, got, err, tt.want)
		}
	}
	if _, err := (&Manifest{Version: "0.2.0"}).Newer("dev"); err == nil {
		t.Error("Newer with a current version that is not SemVer: no error")
	}

	required := []struct {
		minVersion, current string
		want                bool
	}{
		{"0.2.0", "0.1.9", true},
		{"0.2.0", "0.2.0-rc.4", true},
		{"0.2.0", "0.2.0", false},
		{"0.2.0", "0.3.0", false},
		{"0.2.0-rc.1", "0.2.0-rc.1", false},
	}
	for _, tt := range required {
		m := &Manifest{MinVersion: tt.minVersion}
		if got, err := m.Required(tt.current); err != nil || got != tt.want {
			t.Errorf("Required: minVersion %s, current %s = %t, %v; want %t", tt.minVersion, tt.current, got, err, tt.want)
		}
	}
	if _, err := (&Manifest{MinVersion: "0.2.0"}).Required(""); err == nil {
		t.Error("Required with an empty current version: no error")
	}
}

func TestExpired(t *testing.T) {
	expires := time.Date(2027, 5, 30, 10, 0, 0, 0, time.UTC)
	m := &Manifest{ExpiresAt: expires}
	for _, tt := range []struct {
		now  time.Time
		want bool
	}{
		{expires.Add(-time.Hour), false},
		{expires.Add(-time.Nanosecond), false},
		{expires, false}, // at expiresAt the manifest is still valid
		{expires.Add(time.Nanosecond), true},
		{expires.Add(time.Millisecond), true},
		{expires.In(time.FixedZone("UTC+9", 9*3600)), false},
	} {
		if got := m.Expired(tt.now); got != tt.want {
			t.Errorf("Expired(%v) = %t, want %t", tt.now, got, tt.want)
		}
	}
}

func TestManifestArtifact(t *testing.T) {
	fields := manifestFields("0.2.0")
	arm := map[string]any{
		"os": "windows", "arch": "arm64", "kind": "msix", "name": "isshoni-desktop_0.2.0_windows_arm64.msix",
		"url":  "https://github.com/MoonWX/isshoni/releases/download/v0.2.0/isshoni-desktop_0.2.0_windows_arm64.msix",
		"size": 10, "sha256": testSHA256,
	}
	m, err := ParseManifest(encode(t, with(fields, "artifacts", append(fields["artifacts"].([]any), arm))))
	if err != nil {
		t.Fatal(err)
	}
	if a, ok := m.Artifact("windows", "amd64"); !ok || a.Name != "isshoni-desktop_0.2.0_windows_amd64_setup.exe" {
		t.Errorf("windows/amd64: %+v, %t", a, ok)
	}
	for _, p := range [][2]string{{"windows", "arm64"}, {"darwin", "arm64"}, {"linux", "amd64"}, {"windows", "386"}} {
		if a, ok := m.Artifact(p[0], p[1]); ok {
			t.Errorf("%s/%s: %+v, want none", p[0], p[1], a)
		}
	}
}

// TestVerifyManifestRevocations covers 06 §8.1's revocation rules (06 §16's manifest_test rows).
func TestVerifyManifestRevocations(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	active2 := newTestSigner(t, "isshoni-update-2")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	embedded := keys(active, active2, backup)

	manifest := func(v string, revoked ...string) []byte {
		if revoked == nil {
			revoked = []string{}
		}
		return encode(t, with(manifestFields(v), "revokedKeys", revoked))
	}
	verify := func(t *testing.T, data []byte, by testSigner, persisted []string) *Verified {
		t.Helper()
		v, err := VerifyManifest(data, by.sign(data), embedded, persisted)
		if err != nil {
			t.Fatalf("signed by %s: %v", by.Name, err)
		}
		if v.Signer.Name != by.Name || v.Signer.Backup != by.Backup {
			t.Fatalf("Signer = %s (backup %t), want %s", v.Signer.Name, v.Signer.Backup, by.Name)
		}
		return v
	}

	t.Run("an active-key manifest revoking the backup is ignored", func(t *testing.T) {
		v := verify(t, manifest("0.3.0", backup.Fingerprint()), active, nil)
		if v.Revoked != nil || v.IgnoredRevocations != 1 {
			t.Errorf("Revoked %q, ignored %d; want nothing persisted and 1 ignored", v.Revoked, v.IgnoredRevocations)
		}
		// The backup still verifies the next manifest.
		verify(t, manifest("0.3.1"), backup, v.Revoked)
	})
	t.Run("an active-key manifest revoking another active key is ignored", func(t *testing.T) {
		v := verify(t, manifest("0.3.0", active2.Fingerprint()), active, nil)
		if v.Revoked != nil || v.IgnoredRevocations != 1 {
			t.Errorf("Revoked %q, ignored %d; want nothing persisted and 1 ignored", v.Revoked, v.IgnoredRevocations)
		}
		verify(t, manifest("0.3.1"), active2, v.Revoked)
	})
	t.Run("an active-key manifest revoking itself is ignored", func(t *testing.T) {
		v := verify(t, manifest("0.3.0", active.Fingerprint(), backup.Fingerprint()), active, nil)
		if v.Revoked != nil || v.IgnoredRevocations != 2 {
			t.Errorf("Revoked %q, ignored %d", v.Revoked, v.IgnoredRevocations)
		}
	})
	t.Run("a backup-key manifest revoking the active key is persisted, also when not newer", func(t *testing.T) {
		v := verify(t, manifest("0.2.0", active.Fingerprint()), backup, nil)
		if newer, err := v.Newer("0.2.5"); err != nil || newer {
			t.Fatalf("Newer = %t, %v; the case wants a manifest that is not newer", newer, err)
		}
		if len(v.Revoked) != 1 || v.Revoked[0] != active.Fingerprint() || v.IgnoredRevocations != 0 {
			t.Fatalf("Revoked %q, ignored %d; want the active key persisted", v.Revoked, v.IgnoredRevocations)
		}
		// A later manifest signed by the revoked key is bad_signature; the other keys still verify.
		later := manifest("0.3.0")
		_, err := VerifyManifest(later, active.sign(later), embedded, v.Revoked)
		wantBadSignature(t, err, "not trusted")
		verify(t, later, active2, v.Revoked)
		verify(t, later, backup, v.Revoked)
	})
	t.Run("a backup-key manifest listing the backup itself drops that entry and honours the rest", func(t *testing.T) {
		v := verify(t, manifest("0.3.0", backup.Fingerprint(), active.Fingerprint()), backup, nil)
		if len(v.Revoked) != 1 || v.Revoked[0] != active.Fingerprint() || v.IgnoredRevocations != 1 {
			t.Errorf("Revoked %q, ignored %d; want the active key only and 1 dropped", v.Revoked, v.IgnoredRevocations)
		}
		verify(t, manifest("0.3.1"), backup, v.Revoked)
	})
	t.Run("a backup-key manifest revoking every key but the backup", func(t *testing.T) {
		v := verify(t, manifest("0.3.0", active.Fingerprint(), active2.Fingerprint(), backup.Fingerprint()), backup, nil)
		if len(v.Revoked) != 2 || v.IgnoredRevocations != 1 {
			t.Errorf("Revoked %q, ignored %d", v.Revoked, v.IgnoredRevocations)
		}
		verify(t, manifest("0.3.1"), backup, v.Revoked)
	})
	t.Run("revocations already persisted add nothing", func(t *testing.T) {
		persisted := []string{active.Fingerprint()}
		v := verify(t, manifest("0.3.0", active.Fingerprint()), backup, persisted)
		if v.Revoked != nil {
			t.Errorf("Revoked %q, want nil: nothing new to persist", v.Revoked)
		}
		v = verify(t, manifest("0.3.0", active2.Fingerprint()), backup, persisted)
		if want := []string{active.Fingerprint(), active2.Fingerprint()}; fmt.Sprint(v.Revoked) != fmt.Sprint(want) {
			t.Errorf("Revoked %q, want %q", v.Revoked, want)
		}
	})
	t.Run("a revocation of a key the app does not embed is persisted", func(t *testing.T) {
		gone := newTestSigner(t, "isshoni-update-0")
		v := verify(t, manifest("0.3.0", gone.Fingerprint()), backup, nil)
		if len(v.Revoked) != 1 || v.Revoked[0] != gone.Fingerprint() {
			t.Errorf("Revoked %q", v.Revoked)
		}
	})
	t.Run("applyRevocations refuses a list that leaves no trusted key", func(t *testing.T) {
		// Unreachable through a manifest, since a backup entry is always dropped: defence in depth.
		onlyActive := keys(active, active2)
		persisted := []string{active.Fingerprint()}
		all, dropped, err := applyRevocations(onlyActive, persisted, []string{active2.Fingerprint()})
		if err == nil || all != nil || dropped != 0 {
			t.Errorf("applyRevocations = %q, %d, %v; want a refusal and nothing to persist", all, dropped, err)
		}
		if len(persisted) != 1 {
			t.Error("applyRevocations changed its input")
		}
		// With the backup there, the same list leaves the backup trusted.
		if all, _, err := applyRevocations(embedded, persisted, []string{active2.Fingerprint(), backup.Fingerprint()}); err != nil || len(all) != 2 {
			t.Errorf("with the backup: %q, %v", all, err)
		}
	})
}

func TestVerifyManifestFailures(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	stranger := newTestSigner(t, "isshoni-update-7")
	embedded := keys(active, backup)
	good := encode(t, manifestFields("0.2.0"))

	tests := []struct {
		name string
		data []byte
		sig  []byte
		code string
	}{
		{"signed by an untrusted key", good, stranger.sign(good), CodeBadSignature},
		{"signature of other bytes", good, active.sign(append(bytes.Clone(good), ' ')), CodeBadSignature},
		{"no signature", good, nil, CodeBadSignature},
		{"signed, but not a manifest", []byte("hello\n"), active.sign([]byte("hello\n")), CodeBadManifest},
		{"signed, but schema 2", encode(t, with(manifestFields("0.2.0"), "schema", 2)), active.sign(encode(t, with(manifestFields("0.2.0"), "schema", 2))), CodeManifestUnsupported},
		{"signed, but above 64 KiB", pad(good, MaxManifestSize+1), active.sign(pad(good, MaxManifestSize+1)), CodeBadManifest},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, err := VerifyManifest(tt.data, tt.sig, embedded, nil)
			if v != nil || CodeOf(err) != tt.code {
				t.Errorf("VerifyManifest = %v, %v; want code %s", v, err, tt.code)
			}
		})
	}
}

func TestError(t *testing.T) {
	cause := errors.New("cause")
	err := fmt.Errorf("check: %w", &Error{Code: CodeNoArtifact, Err: cause})
	if CodeOf(err) != CodeNoArtifact || !errors.Is(err, cause) {
		t.Errorf("CodeOf = %q, errors.Is = %t", CodeOf(err), errors.Is(err, cause))
	}
	if got := err.Error(); got != "check: update: no_artifact: cause" {
		t.Errorf("Error() = %q", got)
	}
	if got := (&Error{Code: CodeNoKeys}).Error(); got != "update: no_keys" {
		t.Errorf("Error() without a cause = %q", got)
	}
	if CodeOf(cause) != "" || CodeOf(nil) != "" {
		t.Error("CodeOf of an error without *Error is not empty")
	}
}
