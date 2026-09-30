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

// checkKeys compares the embedded key block with deploy/keys/allowed_signers. While either file does not exist yet,
// it returns a note and no error.
func checkKeys(root string) (note string, err error) {
	var missing []string
	for _, name := range []string{installScript, allowedSigners} {
		_, err := os.Stat(filepath.Join(root, name))
		switch {
		case errors.Is(err, fs.ErrNotExist):
			missing = append(missing, filepath.ToSlash(name))
		case err != nil:
			return "", err
		}
	}
	switch len(missing) {
	case 1:
		return "skipped: " + missing[0] + " does not exist yet", nil
	case 2:
		return "skipped: " + missing[0] + " and " + missing[1] + " do not exist yet", nil
	}

	script, err := os.ReadFile(filepath.Join(root, installScript))
	if err != nil {
		return "", err
	}
	want, err := os.ReadFile(filepath.Join(root, allowedSigners))
	if err != nil {
		return "", err
	}
	if strings.ContainsRune(string(want), '\'') {
		return "", errors.New("deploy/keys/allowed_signers contains a single quote, which the shell block cannot hold")
	}
	value, err := embeddedSigners(string(script))
	if err != nil {
		return "", fmt.Errorf("deploy/install.sh: %w", err)
	}
	got := value + "\n"
	switch {
	case got == string(want):
		return "", nil
	case value == string(want):
		return "", errors.New("deploy/keys/allowed_signers must end with a newline")
	}
	return "", fmt.Errorf("the key block in deploy/install.sh differs from deploy/keys/allowed_signers at line %d: "+
		"copy the file between the %q and %q lines as %s…'", firstDiffLine(got, string(want)), keysBegin, keysEnd, keysPrefix)
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
