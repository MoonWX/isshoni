package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// The block in deploy/install-desktop.ps1 that carries the release signing keys (docs/m2/06-windows-release.md §7.1,
// §7.2): a single-quoted here-string whose first and last lines are the markers that deploy/install.sh uses,
//
//	$AllowedSigners = @'
//	# BEGIN ALLOWED SIGNERS (generated from deploy/keys/allowed_signers; do not edit here)
//	isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-1
//	isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAA… isshoni-release-backup-1
//	# END ALLOWED SIGNERS
//	'@
//
// The lines between the markers are deploy/keys/allowed_signers, line for line (the script hands its key lines to
// `ssh-keygen -Y verify`); while that file does not exist there are none, and the script, holding no key, installs
// nothing. `repocheck keys` (`task lint:keys`) checks the block, `repocheck ps1-keys` writes it.
const (
	ps1BlockOpen  = "$AllowedSigners = @'"
	ps1BlockClose = "'@"
)

var desktopScript = filepath.Join("deploy", "install-desktop.ps1")

// ps1KeyBlock splits script (LF line ends) into lines and finds the key block's marker lines. It fails unless there
// is exactly one BEGIN and one END line after it, the here-string opens on the line before BEGIN and closes on the
// line after END, and no line in between would close it early.
func ps1KeyBlock(script string) (lines []string, begin, end int, err error) {
	lines = strings.Split(script, "\n")
	begin, end = -1, -1
	for i, line := range lines {
		switch {
		case strings.HasPrefix(line, keysBegin):
			if begin >= 0 {
				return nil, 0, 0, fmt.Errorf("line %d: a second %q line", i+1, keysBegin)
			}
			begin = i
		case strings.HasPrefix(line, keysEnd):
			if end >= 0 {
				return nil, 0, 0, fmt.Errorf("line %d: a second %q line", i+1, keysEnd)
			}
			end = i
		}
	}
	switch {
	case begin < 0:
		return nil, 0, 0, fmt.Errorf("no %q line", keysBegin)
	case end < 0:
		return nil, 0, 0, fmt.Errorf("no %q line", keysEnd)
	case end < begin:
		return nil, 0, 0, fmt.Errorf("line %d: %q comes before %q", end+1, keysEnd, keysBegin)
	case begin == 0 || lines[begin-1] != ps1BlockOpen:
		return nil, 0, 0, fmt.Errorf("line %d: the line before %q must be %s", begin, keysBegin, ps1BlockOpen)
	case end+1 >= len(lines) || lines[end+1] != ps1BlockClose:
		return nil, 0, 0, fmt.Errorf("line %d: the line after %q must be %s", end+2, keysEnd, ps1BlockClose)
	}
	for i := begin + 1; i < end; i++ {
		if strings.HasPrefix(lines[i], ps1BlockClose) {
			return nil, 0, 0, fmt.Errorf("line %d: a line starting with %s ends the here-string inside the key block",
				i+1, ps1BlockClose)
		}
	}
	return lines, begin, end, nil
}

// ps1Value returns what the lines between the markers hold as a file: each line with its newline.
func ps1Value(lines []string, begin, end int) string {
	if end == begin+1 {
		return ""
	}
	return strings.Join(lines[begin+1:end], "\n") + "\n"
}

// ps1SignersProblem returns why signers, the content of deploy/keys/allowed_signers (LF line ends), cannot go into
// the key block, or "".
func ps1SignersProblem(signers string) string {
	path := filepath.ToSlash(allowedSigners)
	for i, line := range strings.Split(signers, "\n") {
		if strings.HasPrefix(line, ps1BlockClose) {
			return fmt.Sprintf("%s line %d starts with %s, which would end the here-string of %s's key block", path,
				i+1, ps1BlockClose, filepath.ToSlash(desktopScript))
		}
		for _, r := range line {
			if r > 0x7e || r < 0x20 && r != '\t' {
				return fmt.Sprintf("%s line %d has a character that is not printable ASCII, which %s (ASCII only, "+
					"06 §7.1) cannot hold", path, i+1, filepath.ToSlash(desktopScript))
			}
		}
	}
	return ""
}

// comparePS1Block compares the key block of script, the content of deploy/install-desktop.ps1, with signers, the
// content of deploy/keys/allowed_signers (both with LF line ends).
func comparePS1Block(script, signers string) error {
	ps1 := filepath.ToSlash(desktopScript)
	if p := ps1SignersProblem(signers); p != "" {
		return errors.New(p)
	}
	lines, begin, end, err := ps1KeyBlock(script)
	if err != nil {
		return fmt.Errorf("%s: %w", ps1, err)
	}
	got := ps1Value(lines, begin, end)
	switch got {
	case signers:
		return nil
	case signers + "\n":
		return errors.New("deploy/keys/allowed_signers must end with a newline")
	}
	return fmt.Errorf("the key block in %s differs from deploy/keys/allowed_signers at line %d: copy the file between "+
		"the %q and %q lines (`go -C tools run ./repocheck -root .. ps1-keys` writes it)", ps1,
		firstDiffLine(got, signers), keysBegin, keysEnd)
}

// checkPS1Unkeyed checks the key block of deploy/install-desktop.ps1 while deploy/keys/allowed_signers does not
// exist: it must hold no line, as `task release:keys` writes the keys only from that file. Without the script there
// is nothing to check.
func checkPS1Unkeyed(root string) error {
	data, err := os.ReadFile(filepath.Join(root, desktopScript))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	ps1 := filepath.ToSlash(desktopScript)
	_, begin, end, err := ps1KeyBlock(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if err != nil {
		return fmt.Errorf("%s: %w", ps1, err)
	}
	if end > begin+1 {
		return fmt.Errorf("the key block in %s holds lines, but %s does not exist: it must stay empty until that "+
			"file exists (06 §7.1)", ps1, filepath.ToSlash(allowedSigners))
	}
	return nil
}

// writePS1Keys is `repocheck ps1-keys`, the step of `task release:keys` for deploy/install-desktop.ps1: it checks
// deploy/keys/allowed_signers as `repocheck keys` does and writes it into the key block of the script, with LF line
// ends. A key file that breaks a rule leaves the script as it was.
func writePS1Keys(root string) (note string, err error) {
	path := filepath.ToSlash(allowedSigners)
	data, err := os.ReadFile(filepath.Join(root, allowedSigners))
	if errors.Is(err, os.ErrNotExist) || err == nil && len(data) == 0 {
		return "", fmt.Errorf("%s is missing or empty: put the public halves of the release keys there first "+
			"(deploy/keys/README.md)", path)
	}
	if err != nil {
		return "", err
	}
	signers := strings.ReplaceAll(string(data), "\r\n", "\n")
	if strings.Contains(signers, "PRIVATE KEY") {
		return "", fmt.Errorf("%s holds a PRIVATE key. Do not commit it: delete the file, and use the .pub files' "+
			"contents", path)
	}
	if !strings.HasSuffix(signers, "\n") {
		return "", fmt.Errorf("%s must end with a newline", path)
	}
	if p := ps1SignersProblem(signers); p != "" {
		return "", errors.New(p)
	}
	keys, err := parseAllowedSigners(signers)
	switch {
	case err != nil:
		return "", err
	case len(keys) == 0:
		return "", fmt.Errorf("%s has no key, only blank and comment lines", path)
	}
	if p := optionProblems(keys); len(p) > 0 {
		return "", errors.New(strings.Join(p, "\n"))
	}

	target := filepath.Join(root, desktopScript)
	src, err := os.ReadFile(target)
	if err != nil {
		return "", err
	}
	lines, begin, end, err := ps1KeyBlock(strings.ReplaceAll(string(src), "\r\n", "\n"))
	if err != nil {
		return "", fmt.Errorf("%s: %w", filepath.ToSlash(desktopScript), err)
	}
	out := append(append(append([]string{}, lines[:begin+1]...),
		strings.Split(strings.TrimSuffix(signers, "\n"), "\n")...), lines[end:]...)
	if err := os.WriteFile(target, []byte(strings.Join(out, "\n")), 0o644); err != nil { //nolint:gosec // G306: a source file of the repository
		return "", err
	}
	return fmt.Sprintf("wrote %s (%d keys) into %s", path, len(keys), filepath.ToSlash(desktopScript)), nil
}
