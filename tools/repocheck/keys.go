package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// The block in deploy/install.sh that carries the release signing keys (06 §4.3):
//
//	# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
//	ALLOWED_SIGNERS='isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-1
//	isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-backup-1'
//	# END ALLOWED SIGNERS
//
// The installer writes the value plus a final newline to a file for `ssh-keygen -Y verify`, so that output must equal
// deploy/keys/allowed_signers byte for byte.
const (
	keysBegin  = "# BEGIN ALLOWED SIGNERS"
	keysEnd    = "# END ALLOWED SIGNERS"
	keysPrefix = "ALLOWED_SIGNERS='"
)

var (
	installScript  = filepath.Join("deploy", "install.sh")
	allowedSigners = filepath.Join("deploy", "keys", "allowed_signers")
)

// checkKeys checks the signing keys' copies against their sources of truth and reports the problems of all checks
// at once:
//   - the release keys (M1 06 §10.3; checkReleaseKeys): the block embedded in deploy/install.sh equals
//     deploy/keys/allowed_signers byte for byte, and the file uses only options that OpenSSH 8.1 parses;
//   - the desktop app's update keys (M2 06 §8.3; checkUpdateKeys): deploy/keys/update_signers has the fixed line
//     format, exactly one backup line and at least one other, and the block in internal/update/signers.go equals
//     what `task release:update-keys` writes from it;
//   - no key is in both files (the update keys are keys of their own, M2 06 D3);
//   - once a key file holds keys, the "Release signing keys" section of README.md lists the fingerprint of each key
//     of the files that exist and no other one (checkReadmeFingerprints). A file of blank or comment lines only
//     fails: release.yml's `[ -s … ]` guard takes it for one with keys.
//
// While a key file does not exist yet, its checks are skipped with a note and no error (the embedded update block
// must then be empty). The notes are joined with "; ".
func checkKeys(root string) (note string, err error) {
	release, releaseNote, releaseErr := checkReleaseKeys(root)
	update, updateNote, updateErr := checkUpdateKeys(root)
	errs := []error{releaseErr, updateErr}

	var (
		files  []keyFile
		absent []string
	)
	for _, f := range []struct {
		file   *keyFile
		prefix string
	}{{release, releasePrefix}, {update, updatePrefix}} {
		switch {
		case f.file == nil:
			absent = append(absent, f.prefix)
		case len(f.file.keys) > 0:
			files = append(files, *f.file)
		}
	}
	if len(files) > 0 {
		errs = append(errs, checkReadmeFingerprints(root, files, absent))
	}
	if release != nil && update != nil {
		errs = append(errs, checkSeparateKeys(*release, *update))
	}

	var notes []string
	for _, n := range []string{releaseNote, updateNote} {
		if n != "" {
			notes = append(notes, n)
		}
	}
	return strings.Join(notes, "; "), errors.Join(errs...)
}

// The name prefixes of the release keys and of the update keys, which tell their rows in README.md apart.
const (
	releasePrefix = "isshoni-release"
	updatePrefix  = "isshoni-update"
)

// checkReleaseKeys checks deploy/keys/allowed_signers and its copy in deploy/install.sh. It returns the file's keys:
// nil while the file does not exist, and a keyFile without keys when it is empty or can't be read.
func checkReleaseKeys(root string) (file *keyFile, note string, err error) {
	haveScript, err := exists(filepath.Join(root, installScript))
	if err != nil {
		return nil, "", err
	}
	haveSigners, err := exists(filepath.Join(root, allowedSigners))
	if err != nil {
		return nil, "", err
	}
	switch {
	case !haveScript && !haveSigners:
		return nil, "skipped: " + filepath.ToSlash(installScript) + " and " + filepath.ToSlash(allowedSigners) +
			" do not exist yet", nil
	case !haveSigners:
		return nil, "skipped: " + filepath.ToSlash(allowedSigners) + " does not exist yet", nil
	}

	data, err := os.ReadFile(filepath.Join(root, allowedSigners))
	if err != nil {
		return nil, "", err
	}
	// Git for Windows checks text files out with CRLF line ends by default (core.autocrlf), while the committed
	// files, and so the installer that ships, have LF. Compare the content, not the checkout's line ends.
	signers := strings.ReplaceAll(string(data), "\r\n", "\n")

	var errs []error
	if haveScript {
		script, err := os.ReadFile(filepath.Join(root, installScript))
		if err != nil {
			return nil, "", err
		}
		errs = append(errs, compareBlock(strings.ReplaceAll(string(script), "\r\n", "\n"), signers))
	} else {
		note = "skipped the key block: " + filepath.ToSlash(installScript) + " does not exist yet"
	}
	file = &keyFile{path: filepath.ToSlash(allowedSigners), prefix: releasePrefix}
	// An empty file has no key whose fingerprint README.md could list (the block check still wants a final newline).
	// Any other file holds keys, or it is an error that it holds none.
	if signers != "" {
		keys, err := parseAllowedSigners(signers)
		switch {
		case err != nil:
			errs = append(errs, err)
		case len(keys) == 0:
			errs = append(errs, fmt.Errorf("%s has no key, only blank and comment lines", file.path))
		default:
			file.keys = keys
			if p := optionProblems(keys); len(p) > 0 {
				errs = append(errs, errors.New(strings.Join(p, "\n")))
			}
		}
	}
	return file, note, errors.Join(errs...)
}

// checkSeparateKeys fails when a key is both a release key and an update key.
func checkSeparateKeys(release, update keyFile) error {
	var problems []string
	for _, r := range release.keys {
		for _, u := range update.keys {
			if r.fingerprint == u.fingerprint {
				problems = append(problems, fmt.Sprintf("%s is both %s and %s: the update keys are keys of their own "+
					"(06 D3)", r.fingerprint, r, u))
			}
		}
	}
	if len(problems) > 0 {
		return errors.New(strings.Join(problems, "\n"))
	}
	return nil
}

// compareBlock compares the key block of script, the content of deploy/install.sh, with signers, the content of
// deploy/keys/allowed_signers (both with LF line ends).
func compareBlock(script, signers string) error {
	if strings.ContainsRune(signers, '\'') {
		return errors.New("deploy/keys/allowed_signers contains a single quote, which the shell block cannot hold")
	}
	value, err := embeddedSigners(script)
	if err != nil {
		return fmt.Errorf("deploy/install.sh: %w", err)
	}
	got := value + "\n"
	switch {
	case got == signers:
		return nil
	case value == signers:
		return errors.New("deploy/keys/allowed_signers must end with a newline")
	}
	return fmt.Errorf("the key block in deploy/install.sh differs from deploy/keys/allowed_signers at line %d: "+
		"copy the file between the %q and %q lines as %s…'", firstDiffLine(got, signers), keysBegin, keysEnd, keysPrefix)
}

// exists reports whether path exists. An error other than "does not exist" is returned.
func exists(path string) (bool, error) {
	_, err := os.Stat(path)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, fs.ErrNotExist):
		return false, nil
	}
	return false, err
}

// embeddedSigners returns the value of ALLOWED_SIGNERS between the BEGIN and END markers.
func embeddedSigners(script string) (string, error) {
	lines := strings.Split(script, "\n")
	begin, end := -1, -1
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, keysBegin):
			if begin >= 0 {
				return "", fmt.Errorf("line %d: a second %q line", i+1, keysBegin)
			}
			begin = i
		case strings.HasPrefix(line, keysEnd):
			if end >= 0 {
				return "", fmt.Errorf("line %d: a second %q line", i+1, keysEnd)
			}
			end = i
		}
	}
	switch {
	case begin < 0:
		return "", fmt.Errorf("no %q line", keysBegin)
	case end < 0:
		return "", fmt.Errorf("no %q line", keysEnd)
	case end < begin:
		return "", fmt.Errorf("line %d: %q comes before %q", end+1, keysEnd, keysBegin)
	}
	body := strings.Join(lines[begin+1:end], "\n")
	if !strings.HasPrefix(body, keysPrefix) || len(body) < len(keysPrefix)+1 || !strings.HasSuffix(body, "'") {
		return "", fmt.Errorf("line %d: the lines between the markers must be %s…' and nothing else", begin+2, keysPrefix)
	}
	value := body[len(keysPrefix) : len(body)-1]
	if strings.ContainsRune(value, '\'') {
		return "", fmt.Errorf("line %d: the key block has a stray single quote", begin+2)
	}
	return value, nil
}

// firstDiffLine returns the 1-based number of the first line where a and b differ.
func firstDiffLine(a, b string) int {
	la, lb := strings.Split(a, "\n"), strings.Split(b, "\n")
	for i := range min(len(la), len(lb)) {
		if la[i] != lb[i] {
			return i + 1
		}
	}
	return min(len(la), len(lb)) + 1
}
