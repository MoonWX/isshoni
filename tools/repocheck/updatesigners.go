package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The desktop app's update keys (docs/m2/06-windows-release.md §8.1, §8.3, §11.5): deploy/keys/update_signers is
// their source of truth, an allowed-signers file with one line per key,
//
//	isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-1
//	isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-backup-1
//
// and internal/update/signers.go embeds a copy between its BEGIN and END lines, one {Name, Backup, Key} entry per
// line, in the file's order. Backup is true exactly when the line's comment matches ^isshoni-update-backup-[0-9]+$.
// `repocheck update-keys` (`task release:update-keys`) writes the copy; `repocheck keys` (`task lint:keys`) checks
// it, and requires exactly one backup line and at least one other line.
var (
	updateSigners = filepath.Join("deploy", "keys", "update_signers")
	signersGo     = filepath.Join("internal", "update", "signers.go")
)

const (
	updateLinePrefix = `isshoni-update namespaces="isshoni-update" `
	signersBegin     = "// BEGIN UPDATE SIGNERS"
	signersEnd       = "// END UPDATE SIGNERS"
)

var (
	updateActiveRE = regexp.MustCompile(`^isshoni-update-[0-9]+$`)
	updateBackupRE = regexp.MustCompile(`^isshoni-update-backup-[0-9]+$`)
)

// updateSigner is one line of deploy/keys/update_signers.
type updateSigner struct {
	signerKey
	backup bool
	key    string // the key in authorized_keys form, "ssh-ed25519 AAAA…", as ssh.MarshalAuthorizedKey writes it
}

// checkUpdateKeys checks deploy/keys/update_signers and the block of internal/update/signers.go. It returns the
// file's keys: nil while the file does not exist (the block must then embed no key), and a keyFile without keys when
// the file breaks a rule.
func checkUpdateKeys(root string) (file *keyFile, note string, err error) {
	haveFile, err := exists(filepath.Join(root, updateSigners))
	if err != nil {
		return nil, "", err
	}
	haveGo, err := exists(filepath.Join(root, signersGo))
	if err != nil {
		return nil, "", err
	}
	var block string
	if haveGo {
		if block, err = readSignersBlock(root); err != nil {
			return nil, "", err
		}
	}
	if !haveFile {
		note = "skipped the update keys: " + filepath.ToSlash(updateSigners) + " does not exist yet"
		if haveGo && block != renderSignersBlock(nil) {
			return nil, note, fmt.Errorf("%s embeds update keys, but %s does not exist: the block between %q and %q "+
				"must be %q until the file exists (06 §8.3)", filepath.ToSlash(signersGo), filepath.ToSlash(updateSigners),
				signersBegin, signersEnd, strings.TrimSpace(renderSignersBlock(nil)))
		}
		return nil, note, nil
	}

	data, err := os.ReadFile(filepath.Join(root, updateSigners))
	if err != nil {
		return nil, "", err
	}
	file = &keyFile{path: filepath.ToSlash(updateSigners), prefix: updatePrefix}
	keys, err := parseUpdateSigners(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if err != nil {
		return file, "", err
	}
	for _, k := range keys {
		file.keys = append(file.keys, k.signerKey)
	}
	if !haveGo {
		return file, "skipped the embedded update keys: " + filepath.ToSlash(signersGo) + " does not exist yet", nil
	}
	if want := renderSignersBlock(keys); block != want {
		return file, "", fmt.Errorf("the key block in %s differs from %s at line %d of the block: run "+
			"`task release:update-keys` to write it (06 §8.3)", filepath.ToSlash(signersGo),
			filepath.ToSlash(updateSigners), firstDiffLine(block, want))
	}
	return file, "", nil
}

// parseUpdateSigners reads deploy/keys/update_signers (LF line ends) and reports every broken rule at once: each key
// line is exactly `isshoni-update namespaces="isshoni-update" ssh-ed25519 <key> <name>` with the name
// isshoni-update-<n> or isshoni-update-backup-<n> (so it has only an option that OpenSSH 8.1 parses); no key or name
// twice; exactly one backup line and at least one other. Blank lines and lines starting with # are skipped.
func parseUpdateSigners(data string) ([]updateSigner, error) {
	path := filepath.ToSlash(updateSigners)
	if strings.Contains(data, "PRIVATE KEY") {
		return nil, fmt.Errorf("%s holds a PRIVATE key. Do not commit it: delete the file, and use the .pub files' "+
			"contents", path)
	}
	var (
		keys     []updateSigner
		problems []string
	)
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' {
			continue
		}
		k, err := parseUpdateSignerLine(path, i+1, line)
		if err != nil {
			problems = append(problems, fmt.Sprintf("line %d: %v", i+1, err))
			continue
		}
		for _, o := range keys {
			switch {
			case o.fingerprint == k.fingerprint:
				problems = append(problems, fmt.Sprintf("line %d: the same key as line %d", k.line, o.line))
			case o.name == k.name:
				problems = append(problems, fmt.Sprintf("line %d: the name %s is on line %d too", k.line, k.name, o.line))
			}
		}
		keys = append(keys, k)
	}
	if len(problems) == 0 {
		backups := 0
		for _, k := range keys {
			if k.backup {
				backups++
			}
		}
		switch {
		case backups != 1:
			problems = append(problems, fmt.Sprintf("it must hold exactly one backup key, a line whose name is "+
				"isshoni-update-backup-<n>; it holds %d (06 §8.1, §8.3)", backups))
		case len(keys) == 1:
			problems = append(problems, "it must hold at least one key besides the backup key, a line whose name is "+
				"isshoni-update-<n> (06 §8.3)")
		}
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("%s:\n  - %s\nEach line is: %sssh-ed25519 AAAA… isshoni-update-<n> "+
			"(or isshoni-update-backup-<n> for the backup key) (06 §8.3)", path, strings.Join(problems, "\n  - "),
			updateLinePrefix)
	}
	return keys, nil
}

// parseUpdateSignerLine parses one key line of deploy/keys/update_signers.
func parseUpdateSignerLine(path string, n int, line string) (updateSigner, error) {
	principals, rest, ok := splitPrincipals(line)
	if !ok {
		return updateSigner{}, errors.New("no key after the principals")
	}
	if principals != updatePrefix || strings.HasPrefix(line, `"`) {
		return updateSigner{}, fmt.Errorf("the principals are %q, want %s", principals, updatePrefix)
	}
	pub, comment, options, tail, err := ssh.ParseAuthorizedKey([]byte(rest))
	if err != nil {
		return updateSigner{}, fmt.Errorf("no public key that can be read: %w", err)
	}
	if strings.TrimSpace(string(tail)) != "" {
		return updateSigner{}, errors.New("more than one key")
	}
	if len(options) != 1 || options[0] != `namespaces="isshoni-update"` {
		bad := ""
		for _, o := range options {
			if !opensshOptionOK(o) {
				bad = fmt.Sprintf("; %s is not even an option that Windows 10's OpenSSH 8.1 parses (06 §7.2)", o)
				break
			}
		}
		return updateSigner{}, fmt.Errorf("the options are %q, want exactly namespaces=\"isshoni-update\"%s",
			strings.Join(options, ","), bad)
	}
	if pub.Type() != ssh.KeyAlgoED25519 {
		return updateSigner{}, fmt.Errorf("a %s key, want %s (06 §8.1)", pub.Type(), ssh.KeyAlgoED25519)
	}
	backup := updateBackupRE.MatchString(comment)
	if !backup && !updateActiveRE.MatchString(comment) {
		return updateSigner{}, fmt.Errorf("the name %q is neither isshoni-update-<n> nor isshoni-update-backup-<n>",
			comment)
	}
	return updateSigner{
		signerKey: signerKey{file: path, line: n, name: comment, fingerprint: ssh.FingerprintSHA256(pub)},
		backup:    backup,
		key:       strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))),
	}, nil
}

// renderSignersBlock returns the lines between the BEGIN and END lines of internal/update/signers.go for keys, as
// gofmt formats them inside the file's var block.
func renderSignersBlock(keys []updateSigner) string {
	if len(keys) == 0 {
		return "\tembeddedSigners = []signer{}"
	}
	var b strings.Builder
	b.WriteString("\tembeddedSigners = []signer{\n")
	for _, k := range keys {
		fmt.Fprintf(&b, "\t\t{\n\t\t\tName:   %s,\n\t\t\tBackup: %t,\n\t\t\tKey:    %s,\n\t\t},\n",
			strconv.Quote(k.name), k.backup, strconv.Quote(k.key))
	}
	b.WriteString("\t}")
	return b.String()
}

// readSignersBlock returns the lines between the BEGIN and END lines of internal/update/signers.go (LF line ends).
func readSignersBlock(root string) (string, error) {
	data, err := os.ReadFile(filepath.Join(root, signersGo))
	if err != nil {
		return "", err
	}
	lines, begin, end, err := signersBlockLines(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if err != nil {
		return "", err
	}
	return strings.Join(lines[begin+1:end], "\n"), nil
}

// signersBlockLines splits src into lines and finds its BEGIN and END lines.
func signersBlockLines(src string) (lines []string, begin, end int, err error) {
	lines = strings.Split(src, "\n")
	begin, end = -1, -1
	for i, line := range lines {
		t := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(t, signersBegin):
			if begin >= 0 {
				return nil, 0, 0, fmt.Errorf("%s line %d: a second %q line", filepath.ToSlash(signersGo), i+1, signersBegin)
			}
			begin = i
		case strings.HasPrefix(t, signersEnd):
			if end >= 0 {
				return nil, 0, 0, fmt.Errorf("%s line %d: a second %q line", filepath.ToSlash(signersGo), i+1, signersEnd)
			}
			end = i
		}
	}
	switch {
	case begin < 0:
		return nil, 0, 0, fmt.Errorf("%s has no %q line", filepath.ToSlash(signersGo), signersBegin)
	case end < 0:
		return nil, 0, 0, fmt.Errorf("%s has no %q line", filepath.ToSlash(signersGo), signersEnd)
	case end < begin:
		return nil, 0, 0, fmt.Errorf("%s line %d: %q comes before %q", filepath.ToSlash(signersGo), end+1, signersEnd,
			signersBegin)
	}
	return lines, begin, end, nil
}

// writeUpdateKeys is `repocheck update-keys`, run by `task release:update-keys`: it checks deploy/keys/update_signers,
// writes its keys into the block of internal/update/signers.go and prints each key's fingerprint and name, as the
// README.md table lists them. A file that breaks a rule leaves signers.go as it was.
func writeUpdateKeys(root string, stdout io.Writer) (note string, err error) {
	data, err := os.ReadFile(filepath.Join(root, updateSigners))
	if errors.Is(err, os.ErrNotExist) || err == nil && strings.TrimSpace(string(data)) == "" {
		return "", fmt.Errorf("%s is missing or empty: put the public halves of the update keys there first "+
			"(06 §8.3, §14; deploy/keys/README.md)", filepath.ToSlash(updateSigners))
	}
	if err != nil {
		return "", err
	}
	keys, err := parseUpdateSigners(strings.ReplaceAll(string(data), "\r\n", "\n"))
	if err != nil {
		return "", err
	}
	path := filepath.Join(root, signersGo)
	src, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	lines, begin, end, err := signersBlockLines(strings.ReplaceAll(string(src), "\r\n", "\n"))
	if err != nil {
		return "", err
	}
	out := append(append(append([]string{}, lines[:begin+1]...), renderSignersBlock(keys)), lines[end:]...)
	if err := os.WriteFile(path, []byte(strings.Join(out, "\n")), 0o644); err != nil { //nolint:gosec // G306: a source file of the repository
		return "", err
	}
	for _, k := range keys {
		kind := "active"
		if k.backup {
			kind = "backup"
		}
		if _, err := fmt.Fprintf(stdout, "repocheck update-keys: line %d: %s %s (%s)\n", k.line, k.fingerprint, k.name,
			kind); err != nil {
			return "", err
		}
	}
	return fmt.Sprintf("wrote %d keys into %s", len(keys), filepath.ToSlash(signersGo)), nil
}
