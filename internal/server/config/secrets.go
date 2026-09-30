package config

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// secrets.json (§5.2, §5.3): the generated keys, created once at first start and changed only by a rotation.
//
//	{
//	  "format": 1,
//	  "created_at": "2026-09-29T10:00:00Z",
//	  "keys": {
//	    "session": {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"},
//	    "invite":  {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"},
//	    "resume":  {"id": "1", "key": "<base64url, 32 bytes>", "created_at": "2026-09-29T10:00:00Z"}
//	  },
//	  "vapid": {"public_key": "<base64url, 65 bytes>", "private_key": "<base64url, 32 bytes>", "created_at": "…"}
//	}
//
// Rotation contract (§5.2): rotation always restarts the server, so each consumer only compares its key's
// fingerprint with the last start's, at startup: a new session key deletes sessions and devices, a new invite key
// the outstanding links (03 §4.6), a new VAPID pair the push subscriptions (push.New); old resume tokens simply fail.

// KeyName names a generated secret in secrets.json.
type KeyName string

// The generated keys. What each is for is 03's and 01's business; 04 fixes the rotation contract (§5.2).
const (
	KeySession KeyName = "session" // 03: web sessions, device tokens and device codes
	KeyInvite  KeyName = "invite"  // 03: invite, setup and password-reset links
	KeyResume  KeyName = "resume"  // 01: resume tokens
)

// registeredKeys are the keys this build knows, in file order. A registered key missing from the file (added by a
// newer version) is generated and saved at startup; entries with other names are kept as they are (§5.2).
var registeredKeys = []KeyName{KeySession, KeyInvite, KeyResume}

// KeyNames returns the registered key names in file order. `isshoni admin rotate-secrets` rotates all of them and
// the VAPID pair (§5.3).
func KeyNames() []KeyName { return slices.Clone(registeredKeys) }

// ErrUnknownKey is wrapped by Rotate's error for a name that is not registered (the admin socket answers 400).
var ErrUnknownKey = errors.New("config: not a registered secret key")

const (
	secretsFormat     = 1  // the "format" this build writes and reads
	secretKeyBytes    = 32 // each registered key
	vapidPublicBytes  = 65 // uncompressed P-256 point
	vapidPrivateBytes = 32
)

// VAPIDKeys are the Web Push VAPID key pair (§14.1), both base64url without padding (the form of webpush-go's
// GenerateVAPIDKeys).
type VAPIDKeys struct {
	Public  string      // uncompressed P-256 point; safe to publish
	Private logx.Secret // 32-byte scalar
}

// RotateResult says what Rotate changed.
type RotateResult struct {
	Rotated []KeyName // in file order; empty, not nil, when only the VAPID pair was rotated
	VAPID   bool
}

// SecretStore holds the keys of secrets.json (§5.2). Its methods are safe for concurrent use. Consumers read their
// keys at startup; a rotation takes effect with the restart that follows it (§5.3).
type SecretStore struct {
	path string
	log  *slog.Logger

	mu  sync.RWMutex
	doc *secretsDoc
}

// OpenSecrets loads the secrets.json at path (Paths.Secrets), after PrepareDataDir:
//   - a missing file is created (0600, written atomically) with fresh keys from crypto/rand and a new VAPID pair;
//   - a registered key or the VAPID pair missing from the file is generated and saved; unknown names are kept;
//   - a file owned by another uid than the process's is an *OperatorError with ReasonSecretsOwner;
//   - a mode wider than 0600 is fixed to 0600 with a warning;
//   - a file that can't be parsed, or that holds an invalid key, is an *OperatorError with ReasonSecretsCorrupt. It is
//     never regenerated: new keys would sign everyone out, so the fix is to restore it from a backup.
//
// Both *OperatorErrors wrap ErrNeedsOperator (serve exits 78). Other errors (the file can't be read or written) are
// returned wrapped.
func OpenSecrets(path string, log *slog.Logger) (*SecretStore, error) {
	log = componentLogger(log)
	s := &SecretStore{path: path, log: log}
	now := secretsNow()
	fi, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		d, err := newSecretsDoc(now)
		if err != nil {
			return nil, err
		}
		if err := writeSecrets(path, d); err != nil {
			return nil, fmt.Errorf("config: creating %s: %w", path, err)
		}
		log.Info("created secrets.json with new keys", "path", path)
		s.doc = d
		return s, nil
	}
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return nil, secretsCorrupt(path, "is not a regular file", nil)
	}
	if unixPerms {
		if uid, ok := fileOwner(fi); ok && uid != geteuid() {
			return nil, &OperatorError{
				Reason: ReasonSecretsOwner, Path: path,
				Message: fmt.Sprintf("%s is owned by uid %d, but isshoni runs as uid %d", path, uid, geteuid()),
				Fix:     ownerFix(Host{}, path, false),
			}
		}
		if perm := fi.Mode().Perm(); perm&^0o600 != 0 {
			if err := os.Chmod(path, 0o600); err != nil {
				return nil, fmt.Errorf("config: making %s private (chmod 0600): %w", path, err)
			}
			log.Warn("secrets.json was open to other users; changed its mode to 0600", "path", path,
				"was", fmt.Sprintf("%#o", perm))
		}
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	d, err := parseSecrets(data)
	if err != nil {
		return nil, secretsCorrupt(path, err.Error(), err)
	}
	if added, err := d.addMissing(now); err != nil {
		return nil, err
	} else if len(added) > 0 {
		if err := writeSecrets(path, d); err != nil {
			return nil, fmt.Errorf("config: adding %v to %s: %w", added, path, err)
		}
		log.Info("added new keys to secrets.json", "path", path, "keys", added)
	}
	s.doc = d
	return s, nil
}

// InspectSecrets checks the secrets.json at path without changing anything: nil when it parses and every key in it
// is valid (missing keys are fine: the server adds them), an *OperatorError with ReasonSecretsCorrupt when it
// doesn't, other errors when it can't be read. For doctor's secrets check (§13.2) and restore validation (§12.4);
// the mode and owner are the caller's to check.
func InspectSecrets(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return secretsCorrupt(path, "is not a regular file", nil)
	}
	data, err := os.ReadFile(filepath.Clean(path))
	if err != nil {
		return fmt.Errorf("config: %w", err)
	}
	if _, err := parseSecrets(data); err != nil {
		return secretsCorrupt(path, err.Error(), err)
	}
	return nil
}

// secretsCorrupt is the *OperatorError for a secrets.json that can't be used; what follows the path.
func secretsCorrupt(path, what string, err error) error {
	return &OperatorError{
		Reason: ReasonSecretsCorrupt, Path: path, Err: err,
		Message: path + " " + what + ". isshoni never replaces it, because new keys would sign everyone out",
		Fix:     "restore it: isshoni admin restore --offline <backup>",
	}
}

// Key returns a copy of the 32-byte key name. It panics on an unregistered name.
func (s *SecretStore) Key(name KeyName) []byte {
	s.mu.RLock()
	defer s.mu.RUnlock()
	k, ok := s.mustDoc().key[name]
	if !ok {
		panic("config: SecretStore.Key: unregistered key name " + strconv.Quote(string(name)))
	}
	return bytes.Clone(k)
}

// KeyID returns the id of the current key name: "1", then "2" after a rotation, and so on. It panics on an
// unregistered name.
func (s *SecretStore) KeyID(name KeyName) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.mustDoc().id[name]
	if !ok {
		panic("config: SecretStore.KeyID: unregistered key name " + strconv.Quote(string(name)))
	}
	return id
}

// VAPID returns the VAPID key pair.
func (s *SecretStore) VAPID() VAPIDKeys {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mustDoc().vapid
}

// Rotate replaces the named keys (each gets the next id) and, when vapid is true, the VAPID pair, and writes the
// file atomically; the caller (the admin socket handler) then requests a restart, and the consumers purge what the
// old keys protected when the server starts again (§5.3). Nothing else in the file changes. On an error nothing is
// changed, in the file or in memory. A name that is not registered wraps ErrUnknownKey; no names and vapid false is
// a no-op.
func (s *SecretStore) Rotate(ctx context.Context, names []KeyName, vapid bool) (RotateResult, error) {
	if err := ctx.Err(); err != nil {
		return RotateResult{}, err
	}
	want := map[KeyName]bool{}
	for _, n := range names {
		if !slices.Contains(registeredKeys, n) {
			return RotateResult{}, fmt.Errorf("config: rotating %q: %w", n, ErrUnknownKey)
		}
		want[n] = true
	}
	res := RotateResult{Rotated: []KeyName{}, VAPID: vapid}
	for _, n := range registeredKeys {
		if want[n] {
			res.Rotated = append(res.Rotated, n)
		}
	}
	if len(res.Rotated) == 0 && !vapid {
		return res, nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.mustDoc().clone()
	now := secretsNow()
	for _, n := range res.Rotated {
		d.setKey(n, nextKeyID(d.id[n]), now)
	}
	if vapid {
		if err := d.setVAPID(now); err != nil {
			return RotateResult{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return RotateResult{}, err
	}
	if err := writeSecrets(s.path, d); err != nil {
		return RotateResult{}, fmt.Errorf("config: rotating secrets: %w", err)
	}
	s.doc = d
	s.log.Info("rotated secrets; they take effect when the server restarts", "keys", res.Rotated, "vapid", vapid)
	return res, nil
}

func (s *SecretStore) mustDoc() *secretsDoc {
	if s.doc == nil {
		panic("config: SecretStore used without OpenSecrets")
	}
	return s.doc
}

// nextKeyID is the id after id: "1" → "2". A non-numeric id (from a newer version) restarts at "1".
func nextKeyID(id string) string {
	n, err := strconv.Atoi(id)
	if err != nil || n < 1 {
		return "1"
	}
	return strconv.Itoa(n + 1)
}

// secretsNow is the created_at of new keys: UTC, whole seconds, as in §5.2.
func secretsNow() string { return time.Now().UTC().Truncate(time.Second).Format(time.RFC3339) }

// secretsDoc is secrets.json in memory. Every top-level field and every entry of "keys" is kept as raw JSON, so the
// ones this build doesn't know survive a rewrite (a file written by a newer version, then a downgrade); key and id
// hold the decoded registered keys.
type secretsDoc struct {
	fields map[string]json.RawMessage // top-level fields; "keys" is rebuilt from keys when writing
	keys   map[string]json.RawMessage // entries of "keys"
	key    map[KeyName][]byte
	id     map[KeyName]string
	vapid  VAPIDKeys
	// hasVAPID is false while a parsed file has no "vapid" (addMissing then generates it).
	hasVAPID bool
}

type keyJSON struct {
	ID        string `json:"id"`
	Key       string `json:"key"`
	CreatedAt string `json:"created_at"`
}

type vapidJSON struct {
	PublicKey  string `json:"public_key"`
	PrivateKey string `json:"private_key"` //nolint:gosec // G117: this is the secrets file itself
	CreatedAt  string `json:"created_at"`
}

// newSecretsDoc is the content of a new secrets.json: every registered key with id "1" and a VAPID pair.
func newSecretsDoc(now string) (*secretsDoc, error) {
	d := &secretsDoc{
		fields: map[string]json.RawMessage{
			"format":     json.RawMessage(strconv.Itoa(secretsFormat)),
			"created_at": mustJSON(now),
		},
		keys: map[string]json.RawMessage{},
		key:  map[KeyName][]byte{},
		id:   map[KeyName]string{},
	}
	for _, n := range registeredKeys {
		d.setKey(n, "1", now)
	}
	if err := d.setVAPID(now); err != nil {
		return nil, err
	}
	return d, nil
}

// parseSecrets decodes and checks a secrets.json. The error says what is wrong, for "<path> <error>".
func parseSecrets(data []byte) (*secretsDoc, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, fmt.Errorf("is not valid JSON (%w)", err)
	}
	if fields == nil {
		return nil, errors.New("is not a JSON object")
	}
	var format int
	if raw, ok := fields["format"]; !ok {
		return nil, errors.New(`has no "format"`)
	} else if err := json.Unmarshal(raw, &format); err != nil {
		return nil, errors.New(`has a "format" that is not a whole number`)
	}
	switch {
	case format > secretsFormat:
		return nil, fmt.Errorf("was written by a newer isshoni (format %d; this build reads format %d)", format, secretsFormat)
	case format != secretsFormat:
		return nil, fmt.Errorf("has an unknown format %d", format)
	}
	d := &secretsDoc{fields: fields, key: map[KeyName][]byte{}, id: map[KeyName]string{}}
	if raw, ok := fields["keys"]; !ok {
		return nil, errors.New(`has no "keys"`)
	} else if err := json.Unmarshal(raw, &d.keys); err != nil || d.keys == nil {
		return nil, errors.New(`has a "keys" that is not an object`)
	}
	for _, n := range registeredKeys {
		raw, ok := d.keys[string(n)]
		if !ok {
			continue
		}
		var e keyJSON
		if err := json.Unmarshal(raw, &e); err != nil {
			return nil, fmt.Errorf("has a keys.%s that is not a key entry", n)
		}
		if e.ID == "" {
			return nil, fmt.Errorf("has no keys.%s.id", n)
		}
		k, ok := decodeBase64URL(e.Key)
		if !ok || len(k) != secretKeyBytes {
			return nil, fmt.Errorf("has a keys.%s.key that is not %d bytes of base64url", n, secretKeyBytes)
		}
		d.key[n], d.id[n] = k, e.ID
	}
	if raw, ok := fields["vapid"]; ok {
		v, err := parseVAPID(raw)
		if err != nil {
			return nil, err
		}
		d.vapid, d.hasVAPID = v, true
	}
	return d, nil
}

// parseVAPID decodes and checks the "vapid" object: a valid P-256 private key whose public point is public_key.
func parseVAPID(raw json.RawMessage) (VAPIDKeys, error) {
	var v vapidJSON
	if err := json.Unmarshal(raw, &v); err != nil {
		return VAPIDKeys{}, errors.New(`has a "vapid" that is not an object`)
	}
	pub, ok := decodeBase64URL(v.PublicKey)
	if !ok || len(pub) != vapidPublicBytes {
		return VAPIDKeys{}, fmt.Errorf("has a vapid.public_key that is not %d bytes of base64url", vapidPublicBytes)
	}
	priv, ok := decodeBase64URL(v.PrivateKey)
	if !ok || len(priv) != vapidPrivateBytes {
		return VAPIDKeys{}, fmt.Errorf("has a vapid.private_key that is not %d bytes of base64url", vapidPrivateBytes)
	}
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil {
		return VAPIDKeys{}, errors.New("has a vapid.private_key that is not a P-256 key")
	}
	if !bytes.Equal(key.PublicKey().Bytes(), pub) {
		return VAPIDKeys{}, errors.New("has a vapid.public_key that does not belong to vapid.private_key")
	}
	return VAPIDKeys{
		Public:  base64.RawURLEncoding.EncodeToString(pub),
		Private: logx.Secret(base64.RawURLEncoding.EncodeToString(priv)),
	}, nil
}

// decodeBase64URL decodes base64url with or without padding.
func decodeBase64URL(s string) ([]byte, bool) {
	if b, err := base64.RawURLEncoding.DecodeString(s); err == nil {
		return b, true
	}
	b, err := base64.URLEncoding.DecodeString(s)
	return b, err == nil
}

// addMissing generates the registered keys and the VAPID pair that the file lacks, and returns their names.
func (d *secretsDoc) addMissing(now string) ([]string, error) {
	var added []string
	for _, n := range registeredKeys {
		if _, ok := d.key[n]; !ok {
			d.setKey(n, "1", now)
			added = append(added, string(n))
		}
	}
	if !d.hasVAPID {
		if err := d.setVAPID(now); err != nil {
			return nil, err
		}
		added = append(added, "vapid")
	}
	return added, nil
}

// setKey puts a fresh random key with id into d.
func (d *secretsDoc) setKey(name KeyName, id, now string) {
	k := make([]byte, secretKeyBytes)
	_, _ = rand.Read(k) // never fails: crypto/rand crashes the program instead
	d.keys[string(name)] = mustJSON(keyJSON{ID: id, Key: base64.RawURLEncoding.EncodeToString(k), CreatedAt: now})
	d.key[name], d.id[name] = k, id
}

// setVAPID puts a fresh VAPID pair into d, in the form of webpush-go's GenerateVAPIDKeys (§5.2): the uncompressed
// P-256 public point and the private scalar, base64url without padding.
func (d *secretsDoc) setVAPID(now string) error {
	key, err := ecdh.P256().GenerateKey(rand.Reader)
	if err != nil {
		return fmt.Errorf("config: generating the VAPID key pair: %w", err)
	}
	v := vapidJSON{
		PublicKey:  base64.RawURLEncoding.EncodeToString(key.PublicKey().Bytes()),
		PrivateKey: base64.RawURLEncoding.EncodeToString(key.Bytes()),
		CreatedAt:  now,
	}
	d.fields["vapid"] = mustJSON(v)
	d.vapid, d.hasVAPID = VAPIDKeys{Public: v.PublicKey, Private: logx.Secret(v.PrivateKey)}, true
	return nil
}

// clone copies d so that Rotate can change the copy and keep d when the write fails. Raw JSON and key bytes are
// never modified in place, so they are shared.
func (d *secretsDoc) clone() *secretsDoc {
	c := *d
	c.fields = maps.Clone(d.fields)
	c.keys = maps.Clone(d.keys)
	c.key = maps.Clone(d.key)
	c.id = maps.Clone(d.id)
	return &c
}

// encode renders d as secrets.json: the fields of §5.2 in their order, then other fields sorted by name; in "keys"
// the registered keys in file order, then other names sorted. Indented by two spaces, with a final newline.
func (d *secretsDoc) encode() ([]byte, error) {
	names := make([]string, len(registeredKeys))
	for i, n := range registeredKeys {
		names[i] = string(n)
	}
	fields := maps.Clone(d.fields)
	fields["keys"] = orderedObject(d.keys, names)
	top := orderedObject(fields, []string{"format", "created_at", "keys", "vapid"})
	var compact, out bytes.Buffer
	if err := json.Compact(&compact, top); err != nil {
		return nil, fmt.Errorf("config: encoding secrets.json: %w", err)
	}
	if err := json.Indent(&out, compact.Bytes(), "", "  "); err != nil {
		return nil, fmt.Errorf("config: encoding secrets.json: %w", err)
	}
	out.WriteByte('\n')
	return out.Bytes(), nil
}

// orderedObject writes fields as a JSON object: the names in first (those present), then the others sorted.
func orderedObject(fields map[string]json.RawMessage, first []string) json.RawMessage {
	var b bytes.Buffer
	b.WriteByte('{')
	add := func(name string) {
		if b.Len() > 1 {
			b.WriteByte(',')
		}
		b.Write(mustJSON(name))
		b.WriteByte(':')
		b.Write(fields[name])
	}
	for _, name := range first {
		if _, ok := fields[name]; ok {
			add(name)
		}
	}
	for _, name := range slices.Sorted(maps.Keys(fields)) {
		if !slices.Contains(first, name) {
			add(name)
		}
	}
	b.WriteByte('}')
	return b.Bytes()
}

// mustJSON marshals a value that can't fail to marshal (strings and the structs above).
func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic("config: " + err.Error())
	}
	return b
}

// writeSecrets writes d to path atomically.
func writeSecrets(path string, d *secretsDoc) error {
	data, err := d.encode()
	if err != nil {
		return err
	}
	return writeFileAtomic(path, data)
}

// Test seams for writeFileAtomic's failure cases.
var (
	syncFile   = (*os.File).Sync
	renameFile = os.Rename
)

// writeFileAtomic replaces path with data (§5.2): a temp file created with O_EXCL and mode 0600 in the same
// directory, fsync, rename over path, fsync of the directory. Until the rename, path is untouched; on an error the
// temp file is removed. The directory fsync is best effort (not every filesystem supports it), because the rename
// has happened by then.
func writeFileAtomic(path string, data []byte) (err error) {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	closed := false
	defer func() {
		if err != nil {
			if !closed {
				_ = f.Close()
			}
			_ = os.Remove(tmp)
		}
	}()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = syncFile(f); err != nil {
		return err
	}
	closed = true
	if err = f.Close(); err != nil {
		return err
	}
	if err = renameFile(tmp, path); err != nil {
		return err
	}
	if d, derr := os.Open(filepath.Clean(dir)); derr == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
