package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/mod/semver"
)

// The update manifest isshoni-desktop-update.json (06 §8.2), written by `desktoppkg manifest` in the unprivileged
// packaging job (06 §11.1) from the installer file, the tag and two committed files:
//
//	deploy/desktop/min-version    the update floor: one SemVer line (06 D21; raised only for security fixes)
//	deploy/keys/update_revoked    SHA256:… fingerprints the release revokes, one per line; blank lines and # comments
//	                              are skipped; empty normally (06 §8.12)
//
// The JSON has sorted keys, two-space indentation and LF line ends, so the same inputs give the same bytes on every
// OS; an empty revocation list is the exact line `  "revokedKeys": [],`, which check-draft.sh relies on (06 §11.3).
// The app's parser (internal/update's ParseManifest) reads the golden file of this command's test.

const (
	manifestName    = "isshoni-desktop-update.json"
	releasesURL     = "https://github.com/MoonWX/isshoni/releases"
	manifestExpiry  = 180 * 24 * time.Hour // 06 D21
	maxInstaller    = 256 << 20            // 06 §8.2
	maxRevokedKeys  = 64                   // internal/update.MaxRevokedKeys
	wireTimeLayout  = "2006-01-02T15:04:05.000Z07:00"
	minVersionFile  = "deploy/desktop/min-version"
	revokedKeysFile = "deploy/keys/update_revoked"
	updateSigners   = "deploy/keys/update_signers"
)

var (
	// semverRE is SemVer 2.0 without build metadata (semver.org's regular expression): what a tag holds after its v.
	semverRE = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)` +
		`(?:-((?:0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*)(?:\.(?:0|[1-9][0-9]*|[0-9]*[a-zA-Z-][0-9a-zA-Z-]*))*))?$`)
	rcRE          = regexp.MustCompile(`^rc\.(0|[1-9][0-9]*)$`)
	fingerprintRE = regexp.MustCompile(`^SHA256:[A-Za-z0-9+/]{43}$`)
	backupNameRE  = regexp.MustCompile(`^isshoni-update-backup-[0-9]+$`)
)

// runManifest is `desktoppkg manifest -version V -installer FILE [-date T] [-out FILE]`.
func runManifest(ctx context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("manifest", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	ver := fs.String("version", "", "the release's version: the tag, with or without its v (required)")
	installer := fs.String("installer", "", "the NSIS installer, isshoni-desktop_<version>_windows_amd64_setup.exe (required)")
	date := fs.String("date", "", "publishedAt: the tag commit's date, RFC 3339 (default: `git log -1 --format=%cI HEAD` in the root)")
	out := fs.String("out", "", "the file to write (default: <root>/dist-desktop/"+manifestName+")")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	switch {
	case fs.NArg() != 0:
		return fmt.Errorf("%w: manifest takes no arguments", errUsage)
	case *ver == "" || *installer == "":
		return fmt.Errorf("%w: manifest needs -version and -installer", errUsage)
	}

	dateText := *date
	if dateText == "" {
		var err error
		if dateText, err = commitDate(ctx, e.root); err != nil {
			return err
		}
	}
	published, err := time.Parse(time.RFC3339, dateText)
	if err != nil {
		return fmt.Errorf("%w: -date %q is not RFC 3339", errUsage, dateText)
	}
	in := manifestInput{version: strings.TrimPrefix(*ver, "v"), installer: *installer, published: published}
	if in.minVersion, err = readMinVersion(e.root); err != nil {
		return err
	}
	if in.revoked, err = readRevokedKeys(e.root); err != nil {
		return err
	}
	data, err := buildManifest(in)
	if err != nil {
		return err
	}

	path := *out
	if path == "" {
		path = filepath.Join(e.root, "dist-desktop", manifestName)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil { //nolint:gosec // G703: the folder the caller asked for
		return err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil { //nolint:gosec // G306, G703: a public release asset, where the caller asked
		return err
	}
	_, err = fmt.Fprintf(e.stdout, "wrote %s: version %s, channel %s, minVersion %s, %d revoked keys\n",
		path, in.version, channelFor(in.version), in.minVersion, len(in.revoked))
	return err
}

// manifestInput is what a manifest is made of.
type manifestInput struct {
	version    string    // canonical SemVer without v
	installer  string    // path of the installer file
	published  time.Time // the tag commit's date
	minVersion string    // deploy/desktop/min-version
	revoked    []string  // deploy/keys/update_revoked
}

// buildManifest returns the manifest's bytes (06 §8.2).
func buildManifest(in manifestInput) ([]byte, error) {
	if !semverRE.MatchString(in.version) {
		return nil, fmt.Errorf("version %q is not a SemVer version without build metadata", in.version)
	}
	channel := channelFor(in.version)
	if channel != "ci" && semver.Compare("v"+in.minVersion, "v"+in.version) > 0 {
		return nil, fmt.Errorf("%s is %s, above the release's version %s: every app, the updated ones too, would show "+
			"the update as required", minVersionFile, in.minVersion, in.version)
	}
	name := fmt.Sprintf("isshoni-desktop_%s_windows_amd64_setup.exe", in.version)
	if base := filepath.Base(in.installer); base != name {
		return nil, fmt.Errorf("the installer is %s, want %s for version %s (06 §2.1)", base, name, in.version)
	}
	size, sum, err := hashFile(in.installer)
	if err != nil {
		return nil, err
	}
	if size == 0 || size > maxInstaller {
		return nil, fmt.Errorf("the installer has %d bytes, want 1 to %d (06 §8.2)", size, maxInstaller)
	}
	if in.published.IsZero() {
		return nil, errors.New("no publishedAt date")
	}
	revoked := in.revoked
	if revoked == nil {
		revoked = []string{} // written as [], never null
	}
	fields := map[string]any{
		"schema":      1,
		"product":     "isshoni-desktop",
		"version":     in.version,
		"channel":     channel,
		"minVersion":  in.minVersion,
		"publishedAt": in.published.UTC().Format(wireTimeLayout),
		"expiresAt":   in.published.Add(manifestExpiry).UTC().Format(wireTimeLayout),
		"notesUrl":    releasesURL + "/tag/v" + in.version,
		"artifacts": []map[string]any{{
			"os":     "windows",
			"arch":   "amd64",
			"kind":   "nsis",
			"name":   name,
			"url":    releasesURL + "/download/v" + in.version + "/" + name,
			"size":   size,
			"sha256": sum,
		}},
		"revokedKeys": revoked,
	}
	// encoding/json writes map keys sorted; no HTML escaping, so the bytes are plain JSON.
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(fields); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// channelFor returns the channel of a release of version v (06 §8.2): stable for X.Y.Z, beta for X.Y.Z-rc.N, and ci
// for every other prerelease: the updater e2e's X.Y.Z-ci.N, and the release dry run's 0.0.0-dryrun.N, which is never
// published and which no release build of the app accepts.
func channelFor(v string) string {
	_, pre, ok := strings.Cut(v, "-")
	switch {
	case !ok:
		return "stable"
	case rcRE.MatchString(pre):
		return "beta"
	}
	return "ci"
}

// hashFile returns the size and the lowercase hex SHA-256 of a regular file.
func hashFile(path string) (int64, string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the installer the caller named
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil {
		return 0, "", err
	} else if !st.Mode().IsRegular() {
		return 0, "", fmt.Errorf("%s is not a regular file", path)
	}
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, maxInstaller+1))
	if err != nil {
		return 0, "", err
	}
	return n, hex.EncodeToString(h.Sum(nil)), nil
}

// commitDate returns the committer date of HEAD in root, as `git log -1 --format=%cI` prints it.
func commitDate(ctx context.Context, root string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", root, "log", "-1", "--format=%cI", "HEAD").Output()
	if err != nil {
		return "", fmt.Errorf("the tag commit's date from git: %w (pass -date)", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// readMinVersion reads deploy/desktop/min-version: one SemVer line without v or build metadata.
func readMinVersion(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(minVersionFile)))
	if err != nil {
		return "", err
	}
	text := strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n")
	if !semverRE.MatchString(text) {
		return "", fmt.Errorf("%s must be one SemVer line without v, such as 0.2.0; it holds %q", minVersionFile, text)
	}
	return text, nil
}

// readRevokedKeys reads deploy/keys/update_revoked and checks it against deploy/keys/update_signers when that exists:
// the backup key is never revoked (apps drop such an entry, 06 §8.1), and a revoked key is no longer in the file of
// the release that revokes it (06 §8.12).
func readRevokedKeys(root string) ([]string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(revokedKeysFile)))
	if err != nil {
		return nil, err
	}
	signers, err := readSignerNames(root)
	if err != nil {
		return nil, err
	}
	var keys []string
	sc := bufio.NewScanner(bytes.NewReader(data))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		switch name, inFile := signers[line]; {
		case !fingerprintRE.MatchString(line):
			return nil, fmt.Errorf("%s line %d: %q is not a fingerprint as `ssh-keygen -l` prints it (SHA256:…)",
				revokedKeysFile, n, line)
		case slices.Contains(keys, line):
			return nil, fmt.Errorf("%s line %d: %s is listed twice", revokedKeysFile, n, line)
		case inFile && backupNameRE.MatchString(name):
			return nil, fmt.Errorf("%s line %d: %s is the backup key %s, which apps never revoke; a release that "+
				"embeds a new backup replaces it (06 §8.1, §8.12)", revokedKeysFile, n, line, name)
		case inFile:
			return nil, fmt.Errorf("%s line %d: %s is %s, which %s still lists: drop its line in the release "+
				"that revokes it (06 §8.12)", revokedKeysFile, n, line, name, updateSigners)
		}
		keys = append(keys, line)
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(keys) > maxRevokedKeys {
		return nil, fmt.Errorf("%s lists %d keys, above the %d an app accepts", revokedKeysFile, len(keys), maxRevokedKeys)
	}
	return keys, nil
}

// readSignerNames returns the fingerprints of the keys in deploy/keys/update_signers with their names (comments), or
// an empty map while the file does not exist. `task lint:keys` checks the file's format; this reads only the keys.
func readSignerNames(root string) (map[string]string, error) {
	data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(updateSigners)))
	if errors.Is(err, os.ErrNotExist) {
		return map[string]string{}, nil
	}
	if err != nil {
		return nil, err
	}
	names := make(map[string]string)
	for n, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		i := strings.Index(line, "ssh-")
		if i < 0 {
			return nil, fmt.Errorf("%s line %d: no key", updateSigners, n+1)
		}
		pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(line[i:]))
		if err != nil {
			return nil, fmt.Errorf("%s line %d: %w", updateSigners, n+1, err)
		}
		names[ssh.FingerprintSHA256(pub)] = comment
	}
	return names, nil
}
