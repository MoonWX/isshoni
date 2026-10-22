package update

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// The committed vectors in testdata/sshsig were made once with OpenSSH's ssh-keygen and an ephemeral key whose
// private half was deleted right after (06 §16):
//
//	ssh-keygen -t ed25519 -N '' -C isshoni-update-test-vector -f vec
//	ssh-keygen -Y sign -f vec -n isshoni-update vector.msg                       # vector.msg.sig
//	ssh-keygen -Y sign -f vec -n isshoni-update -O hashalg=sha256 vector.msg     # vector.msg.sha256.sig
//	ssh-keygen -Y sign -f vec -n isshoni-checksums vector.msg                    # vector.msg.checksums-namespace.sig
//
// vector.pub is vec.pub. The key is no update key and signs nothing else.
const vectorDir = "testdata/sshsig"

// readText reads a committed text file with LF line ends: a Git for Windows checkout (core.autocrlf) has CRLF.
func readText(t testing.TB, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(name)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
}

// vector returns the committed message, its signature and the key that made it.
func vector(t testing.TB) (msg, sig []byte, key TrustedKey) {
	t.Helper()
	pub, comment, _, _, err := ssh.ParseAuthorizedKey(readText(t, filepath.Join(vectorDir, "vector.pub")))
	if err != nil {
		t.Fatal(err)
	}
	return readText(t, filepath.Join(vectorDir, "vector.msg")), readText(t, filepath.Join(vectorDir, "vector.msg.sig")),
		TrustedKey{Name: comment, Backup: IsBackupName(comment), Key: pub}
}

// testSigner is a throwaway ed25519 key derived from a fixed seed; it is no update key.
type testSigner struct {
	TrustedKey
	priv ed25519.PrivateKey
}

func newTestSigner(t testing.TB, name string) testSigner {
	t.Helper()
	seed := sha256.Sum256([]byte("update test key " + name))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub, err := ssh.NewPublicKey(priv.Public())
	if err != nil {
		t.Fatal(err)
	}
	return testSigner{TrustedKey: TrustedKey{Name: name, Backup: IsBackupName(name), Key: pub}, priv: priv}
}

// authorizedKey returns the key as an authorized_keys line without the newline.
func (s testSigner) authorizedKey() string {
	return strings.TrimSuffix(string(ssh.MarshalAuthorizedKey(s.Key)), "\n")
}

// sign returns an armored SSHSIG of msg in the namespace isshoni-update with hash sha512, as ssh-keygen -Y sign makes.
func (s testSigner) sign(msg []byte) []byte { return armor(s.blob(msg, Namespace, "sha512")) }

// blob returns an SSHSIG blob of msg with any namespace and hash algorithm, signed correctly for them.
func (s testSigner) blob(msg []byte, namespace, hashAlg string) []byte {
	var digest []byte
	switch hashAlg {
	case "sha256":
		sum := sha256.Sum256(msg)
		digest = sum[:]
	default:
		sum := sha512.Sum512(msg)
		digest = sum[:]
	}
	sig := &sshsig{publicKey: s.Key.Marshal(), namespace: namespace, hashAlg: hashAlg}
	var data []byte
	data = append(data, sshsigMagic...)
	data = appendString(data, []byte(namespace))
	data = appendString(data, nil)
	data = appendString(data, []byte(hashAlg))
	data = appendString(data, digest)
	sig.signature = ssh.Marshal(struct {
		Format string
		Blob   []byte
	}{ssh.KeyAlgoED25519, ed25519.Sign(s.priv, data)})
	return sig.marshal()
}

// armor wraps a blob as ssh-keygen does: the BEGIN line, base64 in lines of 70 characters, the END line, a newline.
func armor(blob []byte) []byte {
	b64 := base64.StdEncoding.EncodeToString(blob)
	var b strings.Builder
	b.WriteString(armorBegin + "\n")
	for len(b64) > 70 {
		b.WriteString(b64[:70] + "\n")
		b64 = b64[70:]
	}
	b.WriteString(b64 + "\n" + armorEnd + "\n")
	return []byte(b.String())
}

func keys(signers ...testSigner) []TrustedKey {
	out := make([]TrustedKey, len(signers))
	for i, s := range signers {
		out[i] = s.TrustedKey
	}
	return out
}

// wantBadSignature fails the test unless err is an *Error with CodeBadSignature whose message contains want.
func wantBadSignature(t *testing.T, err error, want string) {
	t.Helper()
	switch {
	case err == nil:
		t.Fatalf("verified, want a bad_signature error containing %q", want)
	case CodeOf(err) != CodeBadSignature:
		t.Fatalf("error %v has code %q, want %q", err, CodeOf(err), CodeBadSignature)
	case !strings.Contains(err.Error(), want):
		t.Fatalf("error %q does not contain %q", err, want)
	}
}

func TestVerifySSHSIGVector(t *testing.T) {
	msg, sig, key := vector(t)
	other := newTestSigner(t, "isshoni-update-1")
	got, err := VerifySSHSIG(msg, sig, []TrustedKey{other.TrustedKey, key})
	if err != nil {
		t.Fatalf("the committed vector does not verify: %v", err)
	}
	if got.Name != "isshoni-update-test-vector" || got.Backup || !bytes.Equal(got.Key.Marshal(), key.Key.Marshal()) {
		t.Errorf("matched %+v, want the vector's key", got)
	}
	if fp := got.Fingerprint(); fp != "SHA256:UyAkBREPYR/Ye3cAoZoJbn5aw/noCD4TRY2qVdtvRWA" {
		t.Errorf("fingerprint %s, want the one ssh-keygen printed", fp)
	}

	t.Run("CRLF armor", func(t *testing.T) {
		if _, err := VerifySSHSIG(msg, bytes.ReplaceAll(sig, []byte("\n"), []byte("\r\n")), []TrustedKey{key}); err != nil {
			t.Errorf("CRLF armor: %v", err)
		}
	})
	t.Run("sha256 made by ssh-keygen", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, readText(t, filepath.Join(vectorDir, "vector.msg.sha256.sig")), []TrustedKey{key})
		wantBadSignature(t, err, `hash algorithm "sha256", want "sha512"`)
	})
	t.Run("checksums namespace made by ssh-keygen", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, readText(t, filepath.Join(vectorDir, "vector.msg.checksums-namespace.sig")),
			[]TrustedKey{key})
		wantBadSignature(t, err, `namespace "isshoni-checksums", want "isshoni-update"`)
	})
	t.Run("not trusted", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, sig, []TrustedKey{other.TrustedKey})
		wantBadSignature(t, err, "not trusted")
	})
	t.Run("no keys", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, sig, nil)
		wantBadSignature(t, err, "not trusted")
	})
	t.Run("revoked", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, sig, Trusted([]TrustedKey{key}, []string{key.Fingerprint()}))
		wantBadSignature(t, err, "not trusted")
	})
}

// TestVerifySSHSIGFlippedBytes flips every byte of the message and of the signature's blob in turn: none verifies.
func TestVerifySSHSIGFlippedBytes(t *testing.T) {
	msg, sig, key := vector(t)
	trusted := []TrustedKey{key}
	for i := range msg {
		m := bytes.Clone(msg)
		m[i] ^= 0x01
		if _, err := VerifySSHSIG(m, sig, trusted); CodeOf(err) != CodeBadSignature {
			t.Errorf("message byte %d flipped: err = %v, want bad_signature", i, err)
		}
	}
	blob, err := dearmor(sig)
	if err != nil {
		t.Fatal(err)
	}
	for i := range blob {
		for _, mask := range []byte{0x01, 0x80} {
			b := bytes.Clone(blob)
			b[i] ^= mask
			if _, err := VerifySSHSIG(msg, armor(b), trusted); CodeOf(err) != CodeBadSignature {
				t.Errorf("blob byte %d ^ %#x: err = %v, want bad_signature", i, mask, err)
			}
		}
	}
}

func TestVerifySSHSIGFailures(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	stranger := newTestSigner(t, "isshoni-update-9")
	msg := []byte(`{"schema": 1}` + "\n")
	good := active.sign(msg)
	goodBlob := active.blob(msg, Namespace, "sha512")
	trusted := keys(active, backup)

	// withSignature returns goodBlob with its signature field replaced.
	withSignature := func(sig []byte) []byte {
		s, err := parseSSHSIG(goodBlob)
		if err != nil {
			t.Fatal(err)
		}
		s.signature = sig
		return s.marshal()
	}
	parsedGood, err := parseSSHSIG(goodBlob)
	if err != nil {
		t.Fatal(err)
	}
	strangerSig, err := parseSSHSIG(stranger.blob(msg, Namespace, "sha512"))
	if err != nil {
		t.Fatal(err)
	}
	version2 := bytes.Clone(goodBlob)
	binary.BigEndian.PutUint32(version2[len(sshsigMagic):], 2)
	body := strings.TrimSuffix(strings.TrimPrefix(string(good), armorBegin+"\n"), armorEnd+"\n")

	tests := []struct {
		name string
		sig  []byte
		keys []TrustedKey
		want string // substring of the error
	}{
		{"wrong namespace", armor(active.blob(msg, "isshoni-checksums", "sha512")), trusted, `namespace "isshoni-checksums"`},
		{"empty namespace", armor(active.blob(msg, "", "sha512")), trusted, `namespace ""`},
		{"sha256 instead of sha512", armor(active.blob(msg, Namespace, "sha256")), trusted, `hash algorithm "sha256"`},
		{"untrusted key", stranger.sign(msg), trusted, "not trusted"},
		{"revoked key", good, Trusted(trusted, []string{active.Fingerprint()}), "not trusted"},
		{"signature of another message", active.sign([]byte("other")), trusted, "signature did not verify"},
		{"signature by another key under the trusted key's name", armor(withSignature(strangerSig.signature)), trusted, "signature did not verify"},
		{"trailing bytes after the blob", armor(append(bytes.Clone(goodBlob), 0)), trusted, "1 trailing bytes"},
		{"trailing bytes after the signature", armor(withSignature(append(bytes.Clone(parsedGood.signature), 0))), trusted, "trailing bytes after the signature"},
		{"another signature format", armor(withSignature(ssh.Marshal(struct {
			Format string
			Blob   []byte
		}{"rsa-sha2-512", bytes.Repeat([]byte{1}, 64)}))), trusted, "sshsig"},
		{"version 2", armor(version2), trusted, "version 2, want 1"},
		{"no magic", armor(goodBlob[1:]), trusted, "no SSHSIG magic"},
		{"truncated", armor(goodBlob[:len(goodBlob)-1]), trusted, "past the end"},
		{"truncated in the version", armor(goodBlob[:8]), trusted, "truncated before the version"},
		{"empty", nil, trusted, "no \"-----BEGIN SSH SIGNATURE-----\" line"},
		{"no armor", goodBlob, trusted, "BEGIN SSH SIGNATURE"},
		{"no END line", []byte(strings.Replace(string(good), armorEnd, "", 1)), trusted, "END SSH SIGNATURE"},
		{"END without a newline before it", []byte(armorBegin + "\n" + strings.TrimSuffix(body, "\n") + armorEnd + "\n"), trusted, "END SSH SIGNATURE"},
		{"wrong label", []byte(strings.ReplaceAll(string(good), "SSH SIGNATURE", "SSH SIG")), trusted, "BEGIN SSH SIGNATURE"},
		{"text before BEGIN", append([]byte("x\n"), good...), trusted, "BEGIN SSH SIGNATURE"},
		{"blank line before BEGIN", append([]byte("\n"), good...), trusted, "BEGIN SSH SIGNATURE"},
		{"text after END", append(bytes.Clone(good), "x\n"...), trusted, "END SSH SIGNATURE"},
		{"a second signature after END", append(bytes.Clone(good), good...), trusted, "sshsig"},
		{"space in the body", []byte(strings.Replace(string(good), "\n", "\n ", 2)), trusted, "byte 0x20 in the base64 body"},
		{"non-base64 byte in the body", []byte(strings.Replace(string(good), "\n", "\n*", 2)), trusted, "byte 0x2a"},
		{"base64 with bits set after the data", armorString("QR=="), trusted, "base64"}, // QQ== is "A"
		{"base64 without padding", armorString("QQ"), trusted, "base64"},
		{"base64 that decodes to no blob", armorString("QQ=="), trusted, "no SSHSIG magic"},
		{"oversize input", append(bytes.Clone(good), bytes.Repeat([]byte("\n"), MaxSignatureSize+1-len(good))...), trusted, "above the 8192-byte limit"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := VerifySSHSIG(msg, tt.sig, tt.keys)
			wantBadSignature(t, err, tt.want)
		})
	}

	t.Run("exactly the size limit", func(t *testing.T) {
		sig := append(bytes.Clone(good), bytes.Repeat([]byte("\n"), MaxSignatureSize-len(good))...)
		if _, err := VerifySSHSIG(msg, sig, trusted); err != nil {
			t.Errorf("a signature of exactly %d bytes: %v", MaxSignatureSize, err)
		}
	})
	t.Run("errors.As", func(t *testing.T) {
		_, err := VerifySSHSIG(msg, nil, trusted)
		var e *Error
		if !errors.As(err, &e) || e.Code != CodeBadSignature || e.Err == nil {
			t.Errorf("err = %#v, want *Error with code bad_signature and a cause", err)
		}
	})
}

// armorString armors an already base64-encoded body.
func armorString(b64 string) []byte {
	return []byte(armorBegin + "\n" + b64 + "\n" + armorEnd + "\n")
}

func TestVerifySSHSIGBackup(t *testing.T) {
	active := newTestSigner(t, "isshoni-update-1")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	msg := []byte("manifest")
	trusted := keys(active, backup)

	got, err := VerifySSHSIG(msg, backup.sign(msg), trusted)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "isshoni-update-backup-1" || !got.Backup {
		t.Errorf("matched %s (backup %t), want the backup key", got.Name, got.Backup)
	}
	got, err = VerifySSHSIG(msg, active.sign(msg), trusted)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "isshoni-update-1" || got.Backup {
		t.Errorf("matched %s (backup %t), want the active key", got.Name, got.Backup)
	}

	// A persisted revocation that names the backup key is ignored (06 §8.3 step 3).
	if _, err := VerifySSHSIG(msg, backup.sign(msg), Trusted(trusted, []string{backup.Fingerprint()})); err != nil {
		t.Errorf("a persisted revocation of the backup key stopped it verifying: %v", err)
	}
	// The same revocation of the active key does stop it.
	_, err = VerifySSHSIG(msg, active.sign(msg), Trusted(trusted, []string{active.Fingerprint(), backup.Fingerprint()}))
	wantBadSignature(t, err, "not trusted")
}

func TestTrusted(t *testing.T) {
	a := newTestSigner(t, "isshoni-update-1")
	b := newTestSigner(t, "isshoni-update-2")
	backup := newTestSigner(t, "isshoni-update-backup-1")
	all := keys(a, b, backup)
	names := func(ks []TrustedKey) string {
		var n []string
		for _, k := range ks {
			n = append(n, k.Name)
		}
		return strings.Join(n, ",")
	}
	tests := []struct {
		revoked []string
		want    string
	}{
		{nil, "isshoni-update-1,isshoni-update-2,isshoni-update-backup-1"},
		{[]string{a.Fingerprint()}, "isshoni-update-2,isshoni-update-backup-1"},
		{[]string{a.Fingerprint(), b.Fingerprint(), backup.Fingerprint()}, "isshoni-update-backup-1"},
		{[]string{"SHA256:not-a-key", ""}, "isshoni-update-1,isshoni-update-2,isshoni-update-backup-1"},
	}
	for _, tt := range tests {
		if got := names(Trusted(all, tt.revoked)); got != tt.want {
			t.Errorf("Trusted(…, %q) = %s, want %s", tt.revoked, got, tt.want)
		}
	}
	if got := Trusted(all, []string{a.Fingerprint()}); len(all) != 3 || len(got) != 2 {
		t.Error("Trusted changed its input")
	}
}

func TestParseSSHSIGRoundTrip(t *testing.T) {
	_, sig, _ := vector(t)
	blob, err := dearmor(sig)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseSSHSIG(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(parsed.marshal(), blob) {
		t.Error("marshal(parseSSHSIG(blob)) differs from blob")
	}
	if parsed.namespace != Namespace || parsed.hashAlg != "sha512" || len(parsed.reserved) != 0 {
		t.Errorf("parsed %q, %q, reserved %q", parsed.namespace, parsed.hashAlg, parsed.reserved)
	}
}

// TestVerifySSHSIGWithSSHKeygen signs with the ssh-keygen on the PATH, when there is one, and checks that the
// verifier takes its signatures, with keys read by ParseSigners from an update_signers-format file (06 §16).
func TestVerifySSHSIGWithSSHKeygen(t *testing.T) {
	keygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("no ssh-keygen on the PATH")
	}
	dir := t.TempDir()
	msgPath := filepath.Join(dir, ManifestName)
	msg := readText(t, filepath.Join(vectorDir, "vector.msg"))
	msg = append(msg, "\x00\xff binary tail\r\n"...)
	if err := os.WriteFile(msgPath, msg, 0o600); err != nil {
		t.Fatal(err)
	}
	var signers strings.Builder
	sign := func(name, namespace string) []byte {
		t.Helper()
		key := filepath.Join(dir, name)
		if _, err := os.Stat(key); err != nil {
			run(t, keygen, "-q", "-t", "ed25519", "-N", "", "-C", name, "-f", key)
			pub, err := os.ReadFile(key + ".pub")
			if err != nil {
				t.Fatal(err)
			}
			signers.WriteString(`isshoni-update namespaces="isshoni-update" ` + strings.TrimSpace(string(pub)) + "\n")
		}
		_ = os.Remove(msgPath + ".sig")
		run(t, keygen, "-Y", "sign", "-f", key, "-n", namespace, msgPath)
		sig, err := os.ReadFile(msgPath + ".sig")
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}

	activeSig := sign("isshoni-update-1", Namespace)
	backupSig := sign("isshoni-update-backup-1", Namespace)
	otherNamespace := sign("isshoni-update-1", "isshoni-checksums")
	trusted, err := ParseSigners([]byte(signers.String()))
	if err != nil {
		t.Fatal(err)
	}
	if len(trusted) != 2 {
		t.Fatalf("ParseSigners read %d keys, want 2", len(trusted))
	}

	if k, err := VerifySSHSIG(msg, activeSig, trusted); err != nil || k.Name != "isshoni-update-1" || k.Backup {
		t.Errorf("ssh-keygen's signature by the active key: key %s (backup %t), err %v", k.Name, k.Backup, err)
	}
	if k, err := VerifySSHSIG(msg, backupSig, trusted); err != nil || k.Name != "isshoni-update-backup-1" || !k.Backup {
		t.Errorf("ssh-keygen's signature by the backup key: key %s (backup %t), err %v", k.Name, k.Backup, err)
	}
	_, err = VerifySSHSIG(msg, otherNamespace, trusted)
	wantBadSignature(t, err, `namespace "isshoni-checksums"`)
	_, err = VerifySSHSIG(append(bytes.Clone(msg), '\n'), activeSig, trusted)
	wantBadSignature(t, err, "signature did not verify")
}

// run runs ssh-keygen. On Windows, where the inbox OpenSSH may refuse a key file in a temporary folder whose
// permissions it doesn't like, a failure skips the test; elsewhere it fails it.
func run(t *testing.T, name string, args ...string) {
	t.Helper()
	cmd := exec.CommandContext(t.Context(), name, args...) //nolint:gosec // the test runs ssh-keygen with fixed arguments
	if out, err := cmd.CombinedOutput(); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
		}
		t.Fatalf("%s %s: %v\n%s", name, strings.Join(args, " "), err, out)
	}
}

// FuzzSSHSIG feeds arbitrary signatures to the verifier, with the committed vector's message and key (seeds in
// testdata/fuzz/FuzzSSHSIG). An input that starts with the BEGIN line is taken as an armored signature; any other
// input is a blob that the target armors itself, so the fuzzer can change the blob's fields directly. Nothing panics;
// every error has the bad_signature code; whatever parses marshals back to the same blob; and only a signature by the
// vector's key verifies.
func FuzzSSHSIG(f *testing.F) {
	msg, sig, key := vector(f)
	blob, err := dearmor(sig)
	if err != nil {
		f.Fatal(err)
	}
	f.Add(sig)
	f.Add(blob)
	f.Add([]byte(armorBegin + "\n" + armorEnd + "\n"))
	trusted := []TrustedKey{key}
	f.Fuzz(func(t *testing.T, data []byte) {
		armored := data
		if !bytes.HasPrefix(data, []byte(armorBegin)) {
			armored = armor(data)
		}
		got, err := VerifySSHSIG(msg, armored, trusted)
		switch {
		case err != nil && CodeOf(err) != CodeBadSignature:
			t.Fatalf("error %v without the bad_signature code", err)
		case err == nil && !bytes.Equal(got.Key.Marshal(), key.Key.Marshal()):
			t.Fatalf("verified with %s, not the vector's key", got.Name)
		}
		blob, err := dearmor(armored)
		if err != nil {
			return
		}
		if parsed, err := parseSSHSIG(blob); err == nil && !bytes.Equal(parsed.marshal(), blob) {
			t.Fatalf("marshal(parseSSHSIG(blob)) differs from blob %x", blob)
		}
	})
}
