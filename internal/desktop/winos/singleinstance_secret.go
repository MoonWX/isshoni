package winos

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
)

// The per-user instance secret (03 §13.3, §14.1; README §3 item 118): the part of the single-instance names that
// another account can't know. It lives in the data root, %LOCALAPPDATA%\isshoni (per user, per PC; the e2e build's
// ISSHONI_E2E_DATA_DIR replaces the folder above it, so each test data root gets its own names), in a file that only
// the user and SYSTEM may open. Its value is never logged.
const (
	instanceSecretFile  = "instance.secret"
	instanceSecretBytes = 16
	// secretReadLimit bounds what is read of the file: a valid one is 32 bytes.
	secretReadLimit = 256
	// secretAttempts bounds instanceSecret's loop: a read, and a creation or regeneration followed by a read, with
	// room for a racing launch that regenerated it meanwhile.
	secretAttempts = 4
)

// secretFiles is the OS's side of the secret's files.
type secretFiles struct {
	// createNew creates path for writing; it fails (fs.ErrExist) when path exists. Windows creates it with
	// CREATE_NEW and a security descriptor of owner the user, DACL the user and SYSTEM only; elsewhere O_EXCL, 0600.
	createNew func(path string) (*os.File, error)
	// publish makes the complete file tmp appear at path unless path exists (fs.ErrExist then): MoveFileExW without
	// MOVEFILE_REPLACE_EXISTING on Windows, a hard link and the temp file's removal elsewhere.
	publish func(tmp, path string) error
	// replace moves tmp over path atomically (os.Rename).
	replace func(tmp, path string) error
}

// errSecretMalformed: the file isn't 32 lowercase hex digits. The value itself is never in an error.
var errSecretMalformed = errors.New("winos: the single-instance secret is malformed")

// instanceSecret returns the user's instance secret from root (the data root), creating it on the first launch. Two
// launches that create it at once end with one value: each writes a complete temp file and publishes it only where
// none exists, and the one that finds the name taken reads the other's. A file that can't be read or isn't a valid
// secret is regenerated (a temp file renamed over it) with a warning that names neither the old value nor the new
// one; the loop then reads the file again, so racing regenerations settle on the last one written.
func instanceSecret(root string, files secretFiles, log *slog.Logger) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("winos: the data root is not an absolute path")
	}
	if err := os.MkdirAll(root, 0o700); err != nil { //nolint:gosec // G703: the app's own data root
		return "", fmt.Errorf("winos: the data root: %w", err)
	}
	path := filepath.Join(root, instanceSecretFile)
	for range secretAttempts {
		secret, err := readSecret(path)
		switch {
		case err == nil:
			return secret, nil
		case errors.Is(err, fs.ErrNotExist):
			// The first launch: publish a new secret unless another launch just did (then read that one).
			if err := writeSecret(path, files, files.publish); err != nil && !errors.Is(err, fs.ErrExist) {
				return "", err
			}
		default:
			log.Warn("winos: the single-instance secret can't be used; a new one replaces it "+
				"(an instance started with the old one isn't found)", "err", err)
			if err := writeSecret(path, files, files.replace); err != nil {
				return "", err
			}
		}
	}
	return "", errors.New("winos: the single-instance secret keeps changing")
}

// readSecret reads and checks the secret's file. Its errors never hold the file's content.
func readSecret(path string) (string, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the data root's own file
	if err != nil {
		return "", fmt.Errorf("winos: the single-instance secret: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, secretReadLimit))
	if err != nil {
		return "", fmt.Errorf("winos: the single-instance secret: %w", err)
	}
	secret := strings.TrimSpace(string(data))
	if !validSecret(secret) {
		return "", errSecretMalformed
	}
	return secret, nil
}

// validSecret reports whether s is 32 lowercase hex digits, as writeSecret writes them.
func validSecret(s string) bool {
	if len(s) != 2*instanceSecretBytes {
		return false
	}
	for _, c := range s {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

// writeSecret writes a new secret to a temp file beside path (created by files.createNew, so with the user-only
// security) and puts it at path with put (files.publish or files.replace). The temp file is removed on failure.
func writeSecret(path string, files secretFiles, put func(tmp, path string) error) error {
	b := make([]byte, instanceSecretBytes+4)
	if _, err := rand.Read(b); err != nil {
		return fmt.Errorf("winos: the single-instance secret: %w", err)
	}
	tmp := path + "." + hex.EncodeToString(b[instanceSecretBytes:]) + ".tmp"
	f, err := files.createNew(tmp)
	if err != nil {
		return fmt.Errorf("winos: the single-instance secret's file: %w", err)
	}
	_, err = f.WriteString(hex.EncodeToString(b[:instanceSecretBytes]))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = put(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp) //nolint:gosec // G703: a temp file of the app's own data root
		return fmt.Errorf("winos: the single-instance secret's file: %w", err)
	}
	return nil
}
