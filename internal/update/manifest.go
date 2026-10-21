// Package update is the desktop app's updater (docs/m2/06-windows-release.md §8, "06" below): it finds a newer
// release on GitHub Releases, checks its signed manifest, downloads the installer and hands it over to be run.
//
// The trust chain (06 §8.1): the app embeds two ssh-ed25519 public keys (signers.go), an active key and an offline
// backup key. A release publishes isshoni-desktop-update.json, which names the installer's URL, size and SHA-256,
// and its armored SSHSIG, isshoni-desktop-update.json.sig (sshsig.go). VerifyManifest checks the signature against
// the embedded keys minus the persisted revocations and then parses the manifest (manifest.go); a manifest signed
// by the backup key may revoke other keys, and nothing can revoke the backup key.
//
// Every SemVer parse and comparison goes through internal/version (Parse, Compare), so the updater and the app's
// "update the app" screen (M2 02 §13.2) agree on which version is newer.
//
// Imports: the standard library, golang.org/x/crypto/ssh, golang.org/x/sys/windows, internal/version and
// internal/logx; never internal/server (a depguard rule).
package update

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/version"
)

const (
	// ManifestName is the release asset of the manifest; the name is fixed, so that
	// releases/latest/download/isshoni-desktop-update.json works (06 §2.1).
	ManifestName = "isshoni-desktop-update.json"
	// SignatureName is the release asset of the manifest's armored SSHSIG (06 §8.3).
	SignatureName = ManifestName + ".sig"

	// Product is the manifest's product field.
	Product = "isshoni-desktop"
	// ManifestSchema is the manifest schema this app reads; another schema is CodeManifestUnsupported.
	ManifestSchema = 1

	// MaxManifestSize bounds the manifest (06 §8.2).
	MaxManifestSize = 64 << 10
	// MaxArtifactSize bounds an installer (06 §8.2, §8.6).
	MaxArtifactSize = 256 << 20
	// MaxRevokedKeys bounds a manifest's revokedKeys, so that what is persisted stays small (state.json is at most
	// 16 KiB, 06 §8.7). A real list holds one or two leaked keys.
	MaxRevokedKeys = 64

	// ReleasesURL is the releases page every manifest URL points into: notesUrl is <ReleasesURL>/tag/v<version>, and
	// an artifact's url is exactly <ReleasesURL>/download/v<version>/<name> (06 §8.2).
	ReleasesURL = "https://github.com/MoonWX/isshoni/releases"
)

// Channel is the release channel the app follows (03's setting), and the channel a manifest is published in.
type Channel string

const (
	ChannelStable Channel = "stable"
	ChannelBeta   Channel = "beta" // stable releases and -rc.N releases

	// channelCI is the channel of the updater e2e's test manifests (X.Y.Z-ci.N), which only updatetest builds accept
	// (06 §8.11), and of the release dry run's never-published 0.0.0-dryrun.N manifests.
	channelCI Channel = "ci"
)

// The error codes of the updater (06 §8.10): stable, snake_case, never renamed. Status.ErrorCode carries them, and 03
// maps each to desktop.update.errors.<code>.
const (
	CodeNetwork             = "network"
	CodeRateLimited         = "rate_limited"
	CodeBadSignature        = "bad_signature"
	CodeBadManifest         = "bad_manifest"
	CodeManifestUnsupported = "manifest_unsupported"
	CodeManifestExpired     = "manifest_expired"
	CodeWrongChannel        = "wrong_channel"
	CodeNoArtifact          = "no_artifact"
	CodeDownloadFailed      = "download_failed"
	CodeSizeMismatch        = "size_mismatch"
	CodeHashMismatch        = "hash_mismatch"
	CodeDisk                = "disk"
	CodeInstallFailed       = "install_failed"
	CodeNotInstalled        = "not_installed"

	// The codes of StateDisabled.
	CodeNoKeys     = "no_keys"     // the build embeds no update key
	CodeNotRelease = "not_release" // the running version is not Eligible
)

// Error is a failure of the updater with its code (06 §8.10). Err is the cause, for logs only: it may quote the
// manifest, never a URL with a query string.
type Error struct {
	Code string
	Err  error
}

func (e *Error) Error() string {
	if e.Err == nil {
		return "update: " + e.Code
	}
	return "update: " + e.Code + ": " + e.Err.Error()
}

func (e *Error) Unwrap() error { return e.Err }

// CodeOf returns the code of the first *Error in err's chain, or "" when it has none.
func CodeOf(err error) string {
	if e, ok := errors.AsType[*Error](err); ok {
		return e.Code
	}
	return ""
}

// updateTestBuild is true only in builds with the updatetest tag, whose hooks set it (06 §8.11): Eligible then also
// accepts X.Y.Z-ci.N, and a manifest of channel beta or ci is accepted whatever the app's channel. A release build
// has no code that sets it.
var updateTestBuild bool

// rules are the parts of the manifest checks that the updatetest hooks change.
type rules struct {
	releases  string // ReleasesURL; ISSHONI_UPDATE_TEST_BASE replaces it in updatetest builds (06 §8.11)
	testBuild bool   // updateTestBuild
}

func defaultRules() rules { return rules{releases: ReleasesURL, testBuild: updateTestBuild} }

// Manifest is a parsed isshoni-desktop-update.json (06 §8.2). Unknown fields are ignored: the bytes are signed, and
// Schema guards incompatible changes.
type Manifest struct {
	Schema      int        `json:"schema"`
	Product     string     `json:"product"`
	Version     string     `json:"version"` // canonical SemVer without "v", equal to the tag
	Channel     Channel    `json:"channel"`
	MinVersion  string     `json:"minVersion"` // the update floor (deploy/desktop/min-version)
	PublishedAt time.Time  `json:"publishedAt"`
	ExpiresAt   time.Time  `json:"expiresAt"`
	NotesURL    string     `json:"notesUrl"`
	Artifacts   []Artifact `json:"artifacts"`
	RevokedKeys []string   `json:"revokedKeys"` // SHA256:… fingerprints; honoured only from the backup key
}

// Artifact is one installer of a manifest, for one OS and architecture.
type Artifact struct {
	OS     string `json:"os"`   // GOOS: windows
	Arch   string `json:"arch"` // GOARCH: amd64
	Kind   string `json:"kind"` // nsis in M2 (M3: darwin-app)
	Name   string `json:"name"` // the release asset's name, also the file name in the updates folder
	URL    string `json:"url"`  // exactly <ReleasesURL>/download/v<version>/<name>
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"` // 64 lowercase hex digits
}

// artifactKinds is the installer kind the app installs on each OS. An entry of another kind is checked but never
// selected, so a later kind (M3's darwin-app) doesn't make an older app refuse the whole manifest.
var artifactKinds = map[string]string{"windows": "nsis"}

var (
	sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)
	tokenRE  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	nameRE   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,199}$`)
)

// ParseManifest parses and checks a manifest (06 §8.2). It does not check the signature (VerifyManifest does that
// first), the expiry (Expired), the version against the running one (Newer, Required) or the channel against the
// app's (CheckChannel). A schema other than 1 is CodeManifestUnsupported; any other fault is CodeBadManifest.
func ParseManifest(data []byte) (*Manifest, error) { return parseManifest(data, defaultRules()) }

func parseManifest(data []byte, r rules) (*Manifest, error) {
	m, err := decodeManifest(data, r)
	if err != nil {
		code := CodeBadManifest
		if errors.Is(err, errUnsupported) {
			code = CodeManifestUnsupported
		}
		return nil, &Error{Code: code, Err: err}
	}
	return m, nil
}

var errUnsupported = errors.New("manifest: unsupported schema")

func decodeManifest(data []byte, r rules) (*Manifest, error) {
	if len(data) > MaxManifestSize {
		return nil, fmt.Errorf("manifest: %d bytes, above the %d-byte limit", len(data), MaxManifestSize)
	}
	// The schema first, so that a later schema whose fields don't fit this struct is "unsupported", not "bad".
	var probe struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if string(probe.Schema) != "1" {
		return nil, fmt.Errorf("%w %s", errUnsupported, truncate(string(probe.Schema)))
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := m.check(r); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.RevokedKeys == nil {
		m.RevokedKeys = []string{}
	}
	return &m, nil
}

// check applies the field rules of 06 §8.2 to a decoded manifest.
func (m *Manifest) check(r rules) error {
	if m.Product != Product {
		return fmt.Errorf("product %q, want %q", truncate(m.Product), Product)
	}
	v, err := canonical(m.Version)
	if err != nil {
		return fmt.Errorf("version: %w", err)
	}
	if want := channelOf(v); m.Channel != want {
		return fmt.Errorf("channel %q for version %s, want %q", truncate(string(m.Channel)), m.Version, want)
	}
	if _, err := canonical(m.MinVersion); err != nil {
		return fmt.Errorf("minVersion: %w", err)
	}
	switch {
	case m.PublishedAt.IsZero():
		return errors.New("no publishedAt")
	case !m.ExpiresAt.After(m.PublishedAt):
		return errors.New("expiresAt is not after publishedAt")
	case m.NotesURL != r.releases+"/tag/v"+m.Version:
		return fmt.Errorf("notesUrl %q is not the release page %s/tag/v%s", truncate(m.NotesURL), r.releases, m.Version)
	}
	seen := make(map[[2]string]bool, len(m.Artifacts))
	for i, a := range m.Artifacts {
		if err := a.check(m.Version, r); err != nil {
			return fmt.Errorf("artifacts[%d]: %w", i, err)
		}
		if seen[[2]string{a.OS, a.Arch}] {
			return fmt.Errorf("artifacts[%d]: a second entry for %s/%s", i, a.OS, a.Arch)
		}
		seen[[2]string{a.OS, a.Arch}] = true
	}
	if len(m.RevokedKeys) > MaxRevokedKeys {
		return fmt.Errorf("revokedKeys: %d entries, above the limit of %d", len(m.RevokedKeys), MaxRevokedKeys)
	}
	for i, fp := range m.RevokedKeys {
		if !fingerprintRE.MatchString(fp) {
			return fmt.Errorf("revokedKeys[%d] %q is not a SHA256:… fingerprint", i, truncate(fp))
		}
	}
	return nil
}

// check applies the artifact rules of 06 §8.2 (and §2.1's installer name) to a in a manifest of version v.
func (a Artifact) check(v string, r rules) error {
	for _, f := range [...]struct{ name, value string }{{"os", a.OS}, {"arch", a.Arch}, {"kind", a.Kind}} {
		if !tokenRE.MatchString(f.value) {
			return fmt.Errorf("%s %q", f.name, truncate(f.value))
		}
	}
	if !nameRE.MatchString(a.Name) {
		return fmt.Errorf("name %q is not a plain file name", truncate(a.Name))
	}
	if a.Kind == "nsis" {
		if want := fmt.Sprintf("isshoni-desktop_%s_%s_%s_setup.exe", v, a.OS, a.Arch); a.Name != want {
			return fmt.Errorf("name %q, want %q", a.Name, want)
		}
	}
	if want := r.releases + "/download/v" + v + "/" + a.Name; a.URL != want {
		return fmt.Errorf("url %q, want %q", truncate(a.URL), want)
	}
	if a.Size <= 0 || a.Size > MaxArtifactSize {
		return fmt.Errorf("size %d is not in 1…%d", a.Size, MaxArtifactSize)
	}
	if !sha256RE.MatchString(a.SHA256) {
		return fmt.Errorf("sha256 %q is not 64 lowercase hex digits", truncate(a.SHA256))
	}
	return nil
}

// canonical parses a manifest version: SemVer through internal/version, without the leading "v" or build metadata
// that version.Parse would accept.
func canonical(s string) (version.SemVer, error) {
	if strings.HasPrefix(s, "v") || strings.Contains(s, "+") {
		return version.SemVer{}, fmt.Errorf("%q is not a canonical SemVer without \"v\" and build metadata", truncate(s))
	}
	v, err := version.Parse(s)
	if err != nil {
		return version.SemVer{}, err
	}
	return v, nil
}

// channelOf returns the channel a release of v is published in (06 §8.2): stable for X.Y.Z, beta for X.Y.Z-rc.N, and
// ci for every other prerelease (the updater e2e's X.Y.Z-ci.N and the release dry run's 0.0.0-dryrun.N, which no
// release build accepts).
func channelOf(v version.SemVer) Channel {
	switch {
	case len(v.Pre) == 0:
		return ChannelStable
	case numbered(v.Pre, "rc"):
		return ChannelBeta
	}
	return channelCI
}

// numbered reports whether the prerelease identifiers pre are exactly id.N for a number N.
func numbered(pre []string, id string) bool {
	if len(pre) != 2 || pre[0] != id || pre[1] == "" {
		return false
	}
	for i := range len(pre[1]) {
		if pre[1][i] < '0' || pre[1][i] > '9' {
			return false
		}
	}
	return true
}

// Eligible reports whether a build of this version may update itself (06 §8.5): the version parses
// (internal/version) and its prerelease part is empty or rc.N. Dev, CI, e2e and dry-run builds are not eligible;
// updatetest builds also accept ci.N (06 §8.11).
func Eligible(v string) bool { return eligible(v, updateTestBuild) }

func eligible(v string, testBuild bool) bool {
	sv, err := version.Parse(v)
	if err != nil {
		return false
	}
	return len(sv.Pre) == 0 || numbered(sv.Pre, "rc") || testBuild && numbered(sv.Pre, "ci")
}

// CheckChannel reports whether an app that follows the channel app accepts m (06 §8.4): a stable app takes only
// stable manifests, a beta app stable and beta ones; an updatetest build takes every channel, ci included, whatever
// its setting (06 §8.11). An unknown app channel counts as stable. A refusal is CodeWrongChannel.
func (m *Manifest) CheckChannel(app Channel) error { return m.checkChannel(app, updateTestBuild) }

func (m *Manifest) checkChannel(app Channel, testBuild bool) error {
	ok := testBuild || m.Channel == ChannelStable || m.Channel == ChannelBeta && app == ChannelBeta
	if !ok {
		return &Error{Code: CodeWrongChannel, Err: fmt.Errorf("a %s manifest for an app on channel %q", m.Channel, app)}
	}
	return nil
}

// Newer reports whether m offers a version above current, the running version (06 §8.5: "version ≤ current" is
// up to date). The comparison is internal/version's; an error means current is not SemVer.
func (m *Manifest) Newer(current string) (bool, error) {
	c, err := version.Compare(m.Version, current)
	if err != nil {
		return false, fmt.Errorf("update: %w", err)
	}
	return c > 0, nil
}

// Required reports whether current, the running version, is below the manifest's update floor, which makes the
// update required (06 §8.5). The comparison is internal/version's; an error means current is not SemVer.
func (m *Manifest) Required(current string) (bool, error) {
	c, err := version.Compare(current, m.MinVersion)
	if err != nil {
		return false, fmt.Errorf("update: %w", err)
	}
	return c < 0, nil
}

// Expired reports whether now is past the manifest's expiresAt (06 §8.1: a manifest past it is refused). At
// expiresAt itself the manifest is still valid. The local clock decides.
func (m *Manifest) Expired(now time.Time) bool { return now.After(m.ExpiresAt) }

// Artifact returns the installer for goos and goarch: the entry of that OS and architecture whose kind is the one the
// app installs there (nsis on Windows). ok is false when there is none (CodeNoArtifact).
func (m *Manifest) Artifact(goos, goarch string) (a Artifact, ok bool) {
	kind, ok := artifactKinds[goos]
	if !ok {
		return Artifact{}, false
	}
	for _, a := range m.Artifacts {
		if a.OS == goos && a.Arch == goarch && a.Kind == kind {
			return a, true
		}
	}
	return Artifact{}, false
}

// Verified is a manifest whose signature verified.
type Verified struct {
	*Manifest

	// Signer is the key that signed the manifest.
	Signer TrustedKey

	// Revoked is the revocation list to persist in state.json after this manifest (06 §8.7): the earlier list plus
	// the entries of RevokedKeys that it honours. It is nil when the manifest adds none, so that nothing is to be
	// persisted. It is set whether or not the manifest offers a newer version (06 §8.1).
	Revoked []string

	// IgnoredRevocations counts the entries of RevokedKeys that are not honoured: all of them in a manifest that a
	// key other than the backup signed (06 §8.1: the caller logs one INFO line with the count, and it is no error),
	// and in a backup-signed one each entry that names a backup key.
	IgnoredRevocations int
}

// VerifyManifest checks a manifest and its signature (06 §8.1–§8.3): sig must be an SSHSIG of data in the namespace
// isshoni-update by one of keys (the embedded keys, or ParseSigners' in an updatetest build) minus the persisted
// revocations revoked; then the manifest is parsed. When the backup key signed it, its revokedKeys are honoured
// (Verified.Revoked), except an entry naming a backup key, which is dropped. A revocation that would leave no key
// trusted is refused as a whole: the manifest is then CodeBadManifest. Errors are *Error with CodeBadSignature,
// CodeBadManifest or CodeManifestUnsupported.
func VerifyManifest(data, sig []byte, keys []TrustedKey, revoked []string) (*Verified, error) {
	return verifyManifest(data, sig, keys, revoked, defaultRules())
}

func verifyManifest(data, sig []byte, keys []TrustedKey, revoked []string, r rules) (*Verified, error) {
	if len(data) > MaxManifestSize {
		return nil, &Error{Code: CodeBadManifest,
			Err: fmt.Errorf("manifest: %d bytes, above the %d-byte limit", len(data), MaxManifestSize)}
	}
	signer, err := VerifySSHSIG(data, sig, Trusted(keys, revoked))
	if err != nil {
		return nil, err
	}
	m, err := parseManifest(data, r)
	if err != nil {
		return nil, err
	}
	v := &Verified{Manifest: m, Signer: signer}
	if !signer.Backup {
		v.IgnoredRevocations = len(m.RevokedKeys)
		return v, nil
	}
	all, dropped, err := applyRevocations(keys, revoked, m.RevokedKeys)
	if err != nil {
		return nil, &Error{Code: CodeBadManifest, Err: err}
	}
	v.IgnoredRevocations = dropped
	if len(all) != len(revoked) {
		v.Revoked = all
	}
	return v, nil
}

// applyRevocations returns persisted plus the entries of listed that a backup-signed manifest revokes, without
// duplicates, and the number of entries it dropped because they name a backup key of keys (06 §8.1). A result that
// would leave no key of keys trusted is refused as a whole with an error: through a manifest this can't happen,
// since a backup key is never revoked, so it is defence in depth.
func applyRevocations(keys []TrustedKey, persisted, listed []string) (all []string, dropped int, err error) {
	backup := make(map[string]bool)
	for _, k := range keys {
		if k.Backup {
			backup[k.Fingerprint()] = true
		}
	}
	all = slices.Clone(persisted)
	for _, fp := range listed {
		switch {
		case backup[fp]:
			dropped++
		case !slices.Contains(all, fp):
			all = append(all, fp)
		}
	}
	if len(Trusted(keys, all)) == 0 {
		return nil, 0, fmt.Errorf("manifest: revokedKeys would leave no trusted key; refused as a whole")
	}
	return all, dropped, nil
}
