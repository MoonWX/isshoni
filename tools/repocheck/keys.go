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

// checkKeys checks the copies of the release keys against deploy/keys/allowed_signers, their source of truth
// (06 §10.3), and reports the problems of both checks at once:
//   - the block embedded in deploy/install.sh must equal the file byte for byte;
//   - once the file is non-empty, it must hold at least one key, and the "Release signing keys" section of README.md
//     must list the fingerprint of each of them and no other one (checkReadmeFingerprints). A file of blank or
//     comment lines only fails: release.yml's `[ -s … ]` guard takes it for one with keys.
//
// While deploy/keys/allowed_signers does not exist yet, it returns a note and no error. While deploy/install.sh does
// not, it notes that the block was not checked.
func checkKeys(root string) (note string, err error) {
	haveScript, err := exists(filepath.Join(root, installScript))
	if err != nil {
		return "", err
	}
	haveSigners, err := exists(filepath.Join(root, allowedSigners))
	if err != nil {
		return "", err
	}
	switch {
	case !haveScript && !haveSigners:
		return "skipped: " + filepath.ToSlash(installScript) + " and " + filepath.ToSlash(allowedSigners) +
			" do not exist yet", nil
	case !haveSigners:
		return "skipped: " + filepath.ToSlash(allowedSigners) + " does not exist yet", nil
	}

	data, err := os.ReadFile(filepath.Join(root, allowedSigners))
	if err != nil {
		return "", err
	}
	// Git for Windows checks text files out with CRLF line ends by default (core.autocrlf), while the committed
	// files, and so the installer that ships, have LF. Compare the content, not the checkout's line ends.
	signers := strings.ReplaceAll(string(data), "\r\n", "\n")

	var blockErr, readmeErr error
	if haveScript {
		script, err := os.ReadFile(filepath.Join(root, installScript))
		if err != nil {
			return "", err
		}
		blockErr = compareBlock(strings.ReplaceAll(string(script), "\r\n", "\n"), signers)
	} else {
		note = "skipped the key block: " + filepath.ToSlash(installScript) + " does not exist yet"
	}
	// An empty file has no key whose fingerprint README.md could list (the block check still wants a final newline).
	// Any other file holds keys, or checkReadmeFingerprints reports that it holds none.
	if signers != "" {
		readmeErr = checkReadmeFingerprints(root, signers)
	}
	return note, errors.Join(blockErr, readmeErr)
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
