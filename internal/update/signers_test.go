package update

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func TestIsBackupName(t *testing.T) {
	for name, want := range map[string]bool{
		"isshoni-update-backup-1":   true,
		"isshoni-update-backup-12":  true,
		"isshoni-update-1":          false,
		"isshoni-update-backup-":    false,
		"isshoni-update-backup-1a":  false,
		"isshoni-update-backup-1 x": false,
		"xisshoni-update-backup-1":  false,
		"isshoni-release-backup-1":  false,
		"":                          false,
	} {
		if got := IsBackupName(name); got != want {
			t.Errorf("IsBackupName(%q) = %t, want %t", name, got, want)
		}
	}
}

// TestEmbeddedSigners checks the block that `task release:update-keys` writes: it parses, and once it holds keys,
// it holds exactly one backup key and at least one other (the rule `task lint:keys` applies to update_signers).
func TestEmbeddedSigners(t *testing.T) {
	keys, err := EmbeddedSigners()
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) == 0 {
		t.Log("the build embeds no update key yet (deploy/keys/update_signers does not exist)")
		return
	}
	backups := 0
	for _, k := range keys {
		if k.Backup {
			backups++
		}
	}
	if backups != 1 || len(keys) < 2 {
		t.Errorf("%d keys with %d backups, want exactly one backup and at least one other key", len(keys), backups)
	}
}

func TestTrustedKeys(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	good := []signer{
		{Name: active.Name, Backup: false, Key: active.authorizedKey()},
		{Name: backup.Name, Backup: true, Key: backup.authorizedKey()},
	}
	keys, err := trustedKeys(good)
	if err != nil {
		t.Fatal(err)
	}
	if len(keys) != 2 || keys[0].Backup || !keys[1].Backup || keys[1].Fingerprint() != backup.Fingerprint() {
		t.Errorf("trustedKeys = %+v", keys)
	}

	tests := []struct {
		name    string
		signers []signer
		want    string
	}{
		{"backup flag on an active name", []signer{{Name: active.Name, Backup: true, Key: active.authorizedKey()}}, "Backup is true"},
		{"backup flag missing", []signer{{Name: backup.Name, Backup: false, Key: backup.authorizedKey()}}, "Backup is false"},
		{"no name", []signer{{Key: active.authorizedKey()}}, "without a name"},
		{"not a key", []signer{{Name: active.Name, Key: "ssh-ed25519 AAAAnotakey"}}, "isshoni-update-1"},
		{"two keys in one entry", []signer{{Name: active.Name, Key: active.authorizedKey() + "\n" + backup.authorizedKey()}}, "more than one key"},
		{"the same key twice", []signer{good[0], {Name: "isshoni-update-2", Key: active.authorizedKey()}}, "the same key as isshoni-update-1"},
		{"the same name twice", []signer{good[0], {Name: active.Name, Key: backup.authorizedKey()}}, "the name is there twice"},
		{"not ed25519", []signer{{Name: active.Name, Key: ecdsaKey(t)}}, "a ecdsa-sha2-nistp256 key"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := trustedKeys(tt.signers)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

// ecdsaKey returns a fresh ECDSA public key in authorized_keys form.
func ecdsaKey(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub)))
}

func TestParseSigners(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	line := func(s testSigner) string {
		return `isshoni-update namespaces="isshoni-update" ` + s.authorizedKey() + " " + s.Name
	}
	file := "# update keys\n\n" + line(active) + "\n" + line(backup) + "\n"

	for name, data := range map[string]string{
		"LF":                     file,
		"CRLF":                   strings.ReplaceAll(file, "\n", "\r\n"),
		"no final newline":       strings.TrimSuffix(file, "\n"),
		"quoted principals":      strings.Replace(file, "isshoni-update namespaces", `"isshoni-update,other" namespaces`, 1),
		"no namespaces option":   strings.Replace(file, `namespaces="isshoni-update" `, "", 1),
		"two namespaces":         strings.Replace(file, `"isshoni-update" ssh`, `"isshoni-checksums,isshoni-update" ssh`, 1),
		"upper-case option name": strings.Replace(file, "namespaces=", "NAMESPACES=", 1),
	} {
		t.Run(name, func(t *testing.T) {
			keys, err := ParseSigners([]byte(data))
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 2 {
				t.Fatalf("%d keys, want 2", len(keys))
			}
			if keys[0].Name != "isshoni-update-1" || keys[0].Backup || keys[0].Fingerprint() != active.Fingerprint() {
				t.Errorf("first key %s (backup %t)", keys[0].Name, keys[0].Backup)
			}
			if keys[1].Name != "isshoni-update-backup-1" || !keys[1].Backup || keys[1].Fingerprint() != backup.Fingerprint() {
				t.Errorf("second key %s (backup %t)", keys[1].Name, keys[1].Backup)
			}
		})
	}

	if keys, err := ParseSigners([]byte("# nothing yet\n\n")); err != nil || len(keys) != 0 {
		t.Errorf("a file without keys: %d keys, err %v", len(keys), err)
	}

	bad := []struct {
		name, data, want string
	}{
		{"other principal", strings.Replace(line(active), "isshoni-update namespaces", "isshoni-release namespaces", 1), "do not name isshoni-update"},
		{"wildcard principal", strings.Replace(line(active), "isshoni-update namespaces", "* namespaces", 1), "do not name isshoni-update"},
		{"other namespace", strings.Replace(line(active), `"isshoni-update" ssh`, `"isshoni-checksums" ssh`, 1), "do not list isshoni-update"},
		{"unquoted namespaces", strings.Replace(line(active), `"isshoni-update" ssh`, `isshoni-update ssh`, 1), "line 1"},
		{"valid-before", strings.Replace(line(active), `"isshoni-update" ssh`, `"isshoni-update",valid-before="20271231" ssh`, 1), `option "valid-before"`},
		{"cert-authority", strings.Replace(line(active), `namespaces`, `cert-authority,namespaces`, 1), `option "cert-authority"`},
		{"no comment", strings.TrimSuffix(line(active), " isshoni-update-1"), "without a name"},
		{"no key", "isshoni-update", "no key after the principals"},
		{"garbage key", `isshoni-update namespaces="isshoni-update" ssh-ed25519 AAAAnotakey isshoni-update-1`, "line 1"},
		{"ecdsa key", `isshoni-update ` + ecdsaKey(t) + " isshoni-update-1", "a ecdsa-sha2-nistp256 key"},
		{"duplicate key", line(active) + "\n" + strings.Replace(line(active), "isshoni-update-1", "isshoni-update-2", 1), "line 2: isshoni-update-2: the same key as isshoni-update-1"},
		{"duplicate name", line(active) + "\n" + strings.Replace(line(backup), "isshoni-update-backup-1", "isshoni-update-1", 1), "line 2: isshoni-update-1: the name is there twice"},
	}
	for _, tt := range bad {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseSigners([]byte(tt.data))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}
