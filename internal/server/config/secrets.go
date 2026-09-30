package config

import (
	"context"
	"errors"
	"log/slog"

	"github.com/MoonWX/isshoni/internal/logx"
)

// secrets.json (§5.2, §5.3). This file declares the whole API; the data-directory and secrets slice (S25) fills it
// in. Until then OpenSecrets fails with ErrSecretsNotImplemented, so no *SecretStore exists.

// ErrSecretsNotImplemented is returned by OpenSecrets until the secrets slice lands.
var ErrSecretsNotImplemented = errors.New("config: secrets.json is not implemented in this build yet")

// KeyName names a generated secret in secrets.json.
type KeyName string

// The generated keys. What each is for is 03's and 01's business; 04 fixes the rotation contract (§5.2).
const (
	KeySession KeyName = "session" // 03: web sessions, device tokens and device codes
	KeyInvite  KeyName = "invite"  // 03: invite, setup and password-reset links
	KeyResume  KeyName = "resume"  // 01: resume tokens
)

// VAPIDKeys are the Web Push VAPID key pair (§14.1).
type VAPIDKeys struct {
	Public  string      // base64url uncompressed P-256 point; safe to publish
	Private logx.Secret // base64url, 32 bytes
}

// RotateResult says what Rotate changed.
type RotateResult struct {
	Rotated []KeyName
	VAPID   bool
}

// SecretStore holds the keys of secrets.json.
type SecretStore struct{}

// OpenSecrets loads path, creating it (0600) with fresh keys on first start and adding registered keys that are
// missing. A corrupt file or a wrong owner is an error that a restart can't fix (exit 78, §5.2).
func OpenSecrets(path string, log *slog.Logger) (*SecretStore, error) {
	_, _ = path, log
	return nil, ErrSecretsNotImplemented
}

// Key returns the 32-byte key name. It panics on an unregistered name.
func (s *SecretStore) Key(name KeyName) []byte {
	panic("config: SecretStore.Key(" + string(name) + "): " + ErrSecretsNotImplemented.Error())
}

// KeyID returns the id of the current key name ("1", then "2" after a rotation, …).
func (s *SecretStore) KeyID(name KeyName) string {
	panic("config: SecretStore.KeyID(" + string(name) + "): " + ErrSecretsNotImplemented.Error())
}

// VAPID returns the VAPID key pair.
func (s *SecretStore) VAPID() VAPIDKeys {
	panic("config: SecretStore.VAPID: " + ErrSecretsNotImplemented.Error())
}

// Rotate replaces the named keys (and the VAPID pair when vapid is true) and writes the file atomically; the caller
// (the admin socket handler) then requests a restart (§5.3).
func (s *SecretStore) Rotate(ctx context.Context, names []KeyName, vapid bool) (RotateResult, error) {
	_, _, _ = ctx, names, vapid
	return RotateResult{}, ErrSecretsNotImplemented
}
