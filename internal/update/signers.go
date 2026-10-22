package update

import (
	"bytes"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// signer is one update key built into the app (06 §8.1, §8.3).
type signer struct {
	Name   string // the key's comment in deploy/keys/update_signers, such as isshoni-update-1
	Backup bool   // true exactly when Name matches ^isshoni-update-backup-[0-9]+$
	Key    string // the public key in authorized_keys form: "ssh-ed25519 AAAA…"
}

// The embedded keys: the block between the BEGIN and END lines is written from deploy/keys/update_signers by
// `task release:update-keys` and checked equal to it by `task lint:keys` (tools/repocheck), one entry per key line of
// the file, in its order. An app built with an empty block reports StateDisabled with CodeNoKeys.
var (
	// BEGIN UPDATE SIGNERS (generated from deploy/keys/update_signers by `task release:update-keys`; do not edit here)
	embeddedSigners = []signer{}
	// END UPDATE SIGNERS
)

// backupNameRE matches the comment of the backup key's line (06 §8.3).
var backupNameRE = regexp.MustCompile(`^isshoni-update-backup-[0-9]+$`)

// IsBackupName reports whether name, the comment of an update_signers line, names a backup key:
// isshoni-update-backup-<n> (06 §8.3).
func IsBackupName(name string) bool { return backupNameRE.MatchString(name) }

// EmbeddedSigners returns the update keys built into this binary. An empty list means a build without keys
// (StateDisabled, CodeNoKeys). An error means a hand-edited block that `task lint:keys` would have refused.
func EmbeddedSigners() ([]TrustedKey, error) {
	keys, err := trustedKeys(embeddedSigners)
	if err != nil {
		return nil, fmt.Errorf("update: embedded signers: %w", err)
	}
	return keys, nil
}

// trustedKeys parses the keys of signers and checks their names: every key is ssh-ed25519, Backup is set exactly
// for a backup name, and no key or name is there twice.
func trustedKeys(signers []signer) ([]TrustedKey, error) {
	keys := make([]TrustedKey, 0, len(signers))
	for _, s := range signers {
		pub, _, _, rest, err := ssh.ParseAuthorizedKey([]byte(s.Key))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", s.Name, err)
		}
		if len(bytes.TrimSpace(rest)) != 0 {
			return nil, fmt.Errorf("%s: more than one key", s.Name)
		}
		k := TrustedKey{Name: s.Name, Backup: s.Backup, Key: pub}
		if err := checkKey(k, keys); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// checkKey checks k, the next key of a list after keys.
func checkKey(k TrustedKey, keys []TrustedKey) error {
	switch {
	case k.Name == "":
		return errors.New("a key without a name")
	case k.Key.Type() != ssh.KeyAlgoED25519:
		return fmt.Errorf("%s: a %s key, want %s", k.Name, k.Key.Type(), ssh.KeyAlgoED25519)
	case k.Backup != IsBackupName(k.Name):
		return fmt.Errorf("%s: Backup is %t, but it is set exactly for names isshoni-update-backup-<n>", k.Name, k.Backup)
	}
	for _, o := range keys {
		switch {
		case o.Name == k.Name:
			return fmt.Errorf("%s: the name is there twice", k.Name)
		case bytes.Equal(o.Key.Marshal(), k.Key.Marshal()):
			return fmt.Errorf("%s: the same key as %s", k.Name, o.Name)
		}
	}
	return nil
}

// ParseSigners reads an allowed-signers file in the format of deploy/keys/update_signers (06 §8.3), as the updater
// e2e's ISSHONI_UPDATE_TEST_SIGNERS names it (06 §8.11):
//
//	isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-1
//	isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAA… isshoni-update-backup-1
//
// Blank lines and lines starting with # are skipped. Every other line names the principal isshoni-update, may carry
// only the namespaces option (which then lists isshoni-update), and holds an ssh-ed25519 key with a comment, its
// name; Backup follows the same comment rule as the embedded keys (IsBackupName). CRLF line ends are accepted.
func ParseSigners(data []byte) ([]TrustedKey, error) {
	var keys []TrustedKey
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		k, err := parseSignerLine(line)
		if err == nil {
			err = checkKey(k, keys)
		}
		if err != nil {
			return nil, fmt.Errorf("update: signers line %d: %w", i+1, err)
		}
		keys = append(keys, k)
	}
	return keys, nil
}

// parseSignerLine parses one non-blank allowed-signers line: principals, options, key, comment.
func parseSignerLine(line string) (TrustedKey, error) {
	principals, rest, ok := cutPrincipals(line)
	if !ok {
		return TrustedKey{}, errors.New("no key after the principals")
	}
	if !hasPattern(principals, Namespace) {
		return TrustedKey{}, fmt.Errorf("principals %q do not name %s", principals, Namespace)
	}
	pub, comment, options, tail, err := ssh.ParseAuthorizedKey([]byte(rest))
	if err != nil {
		return TrustedKey{}, err
	}
	if len(bytes.TrimSpace(tail)) != 0 {
		return TrustedKey{}, errors.New("more than one key")
	}
	for _, opt := range options {
		name, value, _ := strings.Cut(opt, "=")
		if !strings.EqualFold(name, "namespaces") {
			return TrustedKey{}, fmt.Errorf("option %q: only namespaces is allowed", name)
		}
		value, ok := unquote(value)
		if !ok || !hasPattern(value, Namespace) {
			return TrustedKey{}, fmt.Errorf("namespaces %s do not list %s", value, Namespace)
		}
	}
	return TrustedKey{Name: comment, Backup: IsBackupName(comment), Key: pub}, nil
}

// cutPrincipals splits an allowed-signers line into its principals field and what follows. As in OpenSSH, the field
// is a run of characters up to a space or tab, or a double-quoted string.
func cutPrincipals(line string) (principals, rest string, ok bool) {
	if strings.HasPrefix(line, `"`) {
		end := strings.IndexByte(line[1:], '"')
		if end < 0 {
			return "", "", false
		}
		principals, rest = line[1:end+1], line[end+2:]
	} else {
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return "", "", false
		}
		principals, rest = line[:i], line[i:]
	}
	rest = strings.TrimLeft(rest, " \t")
	return principals, rest, rest != ""
}

// hasPattern reports whether the comma-separated list holds want itself. OpenSSH's wildcards are not expanded: a
// pattern such as * does not count.
func hasPattern(list, want string) bool {
	for p := range strings.SplitSeq(list, ",") {
		if p == want {
			return true
		}
	}
	return false
}

// unquote removes the double quotes around an option value.
func unquote(v string) (string, bool) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' {
		return v, false
	}
	return v[1 : len(v)-1], true
}
