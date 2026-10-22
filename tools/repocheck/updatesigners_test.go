package main

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

// Throwaway update keys, derived in the test from fixed seeds. They are no update key and sign nothing.
var (
	updateKey1      = newUpdateKey("isshoni-update-1")
	updateKey2      = newUpdateKey("isshoni-update-2")
	updateBackupKey = newUpdateKey("isshoni-update-backup-1")

	goodUpdateSigners = updateKey1.line + "\n" + updateBackupKey.line + "\n"
)

// newUpdateKey returns a test key whose line is an update_signers line.
func newUpdateKey(name string) testKey {
	k := newTestKey(name)
	k.line = updateLinePrefix + k.key + " " + name
	return k
}

// signersGoWith returns a minimal internal/update/signers.go whose block is body.
func signersGoWith(body string) string {
	return "package update\n\ntype signer struct {\n\tName   string\n\tBackup bool\n\tKey    string\n}\n\n" +
		"var (\n\t" + signersBegin + " (generated from deploy/keys/update_signers; do not edit here)\n" + body + "\n\t" +
		signersEnd + "\n)\n"
}

// blockFor returns the block that update-keys writes for keys (each with its backup flag).
func blockFor(keys ...testKey) string {
	var signers []updateSigner
	for _, k := range keys {
		signers = append(signers, updateSigner{
			signerKey: signerKey{name: k.name}, backup: updateBackupRE.MatchString(k.name), key: k.key,
		})
	}
	return renderSignersBlock(signers)
}

// readmeWithUpdateKeys returns goodReadme with rows for the update keys, whose Fingerprint cells are cells.
func readmeWithUpdateKeys(cells ...[2]string) string {
	var rows strings.Builder
	for _, c := range cells {
		rows.WriteString("| `" + c[0] + "` | signs the desktop app's update manifests | " + c[1] + " |\n")
	}
	return strings.Replace(goodReadme, "\n\n```sh", "\n"+strings.TrimSuffix(rows.String(), "\n")+"\n\n```sh", 1)
}

// updateTree is keyTree plus deploy/keys/update_signers, the matching signers.go and a README.md that lists every
// key's fingerprint.
func updateTree(updateSignersFile string, keys ...testKey) map[string]string {
	files := keyTree(goodScript, signers)
	files["deploy/keys/update_signers"] = updateSignersFile
	files["internal/update/signers.go"] = signersGoWith(blockFor(keys...))
	var cells [][2]string
	for _, k := range keys {
		cells = append(cells, [2]string{k.name, k.fingerprint})
	}
	files["README.md"] = readmeWithUpdateKeys(cells...)
	return files
}

func ecdsaUpdateLine(t *testing.T) string {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub, err := ssh.NewPublicKey(&priv.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return updateLinePrefix + strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pub))) + " isshoni-update-2"
}

// TestCheckKeysUpdateSigners covers 06 §16's repocheck rows for update_signers and the signers.go block.
func TestCheckKeysUpdateSigners(t *testing.T) {
	// good returns a fresh tree each time: withFile changes the map it is given.
	good := func() map[string]string { return updateTree(goodUpdateSigners, updateKey1, updateBackupKey) }
	three := updateKey1.line + "\n" + updateKey2.line + "\n" + updateBackupKey.line + "\n"
	twoActive := updateKey1.line + "\n" + updateKey2.line + "\n"
	twoBackups := updateKey1.line + "\n" + updateBackupKey.line + "\n" +
		strings.Replace(updateKey2.line, "isshoni-update-2", "isshoni-update-backup-2", 1) + "\n"
	line1 := func(old, repl string) string {
		return strings.Replace(updateKey1.line, old, repl, 1) + "\n" + updateBackupKey.line + "\n"
	}

	tests := []struct {
		name     string
		files    map[string]string
		wantNote string
		wantErr  []string
	}{
		{
			name:     "no update_signers yet, an empty block: skipped",
			files:    withFile(keyTree(goodScript, signers), "internal/update/signers.go", signersGoWith("\tembeddedSigners = []signer{}")),
			wantNote: noUpdateKeysNote,
		},
		{
			name: "no update_signers yet, but the block embeds keys",
			files: withFile(keyTree(goodScript, signers), "internal/update/signers.go",
				signersGoWith(blockFor(updateKey1, updateBackupKey))),
			wantErr: []string{"internal/update/signers.go embeds update keys, but deploy/keys/update_signers does not exist"},
		},
		{
			name:  "an active and a backup key, the block and README.md matching",
			files: good(),
		},
		{
			name:  "two active keys during a rotation",
			files: updateTree(three, updateKey1, updateKey2, updateBackupKey),
		},
		{
			name: "CRLF line ends in update_signers, signers.go and README.md",
			files: withFile(withFile(withFile(updateTree(goodUpdateSigners, updateKey1, updateBackupKey),
				"deploy/keys/update_signers", crlf(goodUpdateSigners)),
				"internal/update/signers.go", crlf(signersGoWith(blockFor(updateKey1, updateBackupKey)))),
				"README.md", crlf(readmeWithUpdateKeys([2]string{"isshoni-update-1", updateKey1.fingerprint},
					[2]string{"isshoni-update-backup-1", updateBackupKey.fingerprint}))),
		},
		{
			name:  "comments and blank lines in update_signers",
			files: withFile(good(), "deploy/keys/update_signers", "# update keys (06 §8.3)\n\n"+goodUpdateSigners+"\n"),
		},
		{
			name:     "no signers.go: the block is not checked",
			files:    withoutFile(updateTree(goodUpdateSigners, updateKey1, updateBackupKey), "internal/update/signers.go"),
			wantNote: "skipped the embedded update keys: internal/update/signers.go does not exist yet",
		},
		{
			name:    "a drifted block: another key",
			files:   withFile(good(), "internal/update/signers.go", signersGoWith(blockFor(updateKey2, updateBackupKey))),
			wantErr: []string{"the key block in internal/update/signers.go differs from deploy/keys/update_signers at line 3 of the block", "task release:update-keys"},
		},
		{
			name: "a drifted block: Backup flipped",
			files: withFile(good(), "internal/update/signers.go", signersGoWith(strings.Replace(
				blockFor(updateKey1, updateBackupKey), "Backup: true", "Backup: false", 1))),
			wantErr: []string{"differs from deploy/keys/update_signers at line 9 of the block"},
		},
		{
			name:    "a drifted block: the order",
			files:   withFile(good(), "internal/update/signers.go", signersGoWith(blockFor(updateBackupKey, updateKey1))),
			wantErr: []string{"differs from deploy/keys/update_signers"},
		},
		{
			name:    "a drifted block: a key missing",
			files:   withFile(good(), "internal/update/signers.go", signersGoWith(blockFor(updateKey1))),
			wantErr: []string{"differs from deploy/keys/update_signers"},
		},
		{
			name:    "a drifted block: empty",
			files:   withFile(good(), "internal/update/signers.go", signersGoWith("\tembeddedSigners = []signer{}")),
			wantErr: []string{"differs from deploy/keys/update_signers at line 1 of the block"},
		},
		{
			name:    "signers.go without the END line",
			files:   withFile(good(), "internal/update/signers.go", strings.Replace(signersGoWith(blockFor(updateKey1, updateBackupKey)), signersEnd, "", 1)),
			wantErr: []string{`internal/update/signers.go has no "// END UPDATE SIGNERS" line`},
		},
		{
			name:    "signers.go with two BEGIN lines",
			files:   withFile(good(), "internal/update/signers.go", signersGoWith(blockFor(updateKey1, updateBackupKey))+"\t"+signersBegin+"\n"),
			wantErr: []string{`a second "// BEGIN UPDATE SIGNERS" line`},
		},
		{
			name:    "zero backup lines",
			files:   updateTree(twoActive, updateKey1, updateKey2),
			wantErr: []string{"deploy/keys/update_signers:", "it must hold exactly one backup key, a line whose name is isshoni-update-backup-<n>; it holds 0"},
		},
		{
			name:    "two backup lines",
			files:   withFile(good(), "deploy/keys/update_signers", twoBackups),
			wantErr: []string{"it must hold exactly one backup key", "it holds 2"},
		},
		{
			name:    "only the backup key",
			files:   withFile(good(), "deploy/keys/update_signers", updateBackupKey.line+"\n"),
			wantErr: []string{"it must hold at least one key besides the backup key"},
		},
		{
			name:    "an empty update_signers",
			files:   withFile(good(), "deploy/keys/update_signers", ""),
			wantErr: []string{"it holds 0"},
		},
		{
			name:    "comments only",
			files:   withFile(good(), "deploy/keys/update_signers", "# the keys come later\n"),
			wantErr: []string{"it holds 0"},
		},
		{
			name:    "the release principal",
			files:   withFile(good(), "deploy/keys/update_signers", line1("isshoni-update namespaces", "isshoni-release namespaces")),
			wantErr: []string{`line 1: the principals are "isshoni-release", want isshoni-update`},
		},
		{
			name:    "quoted principals",
			files:   withFile(good(), "deploy/keys/update_signers", line1("isshoni-update namespaces", `"isshoni-update" namespaces`)),
			wantErr: []string{`line 1: the principals are "isshoni-update", want isshoni-update`},
		},
		{
			name:    "the checksums namespace",
			files:   withFile(good(), "deploy/keys/update_signers", line1(`"isshoni-update" ssh`, `"isshoni-checksums" ssh`)),
			wantErr: []string{`line 1: the options are "namespaces=\"isshoni-checksums\"", want exactly namespaces="isshoni-update"`},
		},
		{
			name:    "no namespaces option",
			files:   withFile(good(), "deploy/keys/update_signers", line1(`namespaces="isshoni-update" `, "")),
			wantErr: []string{`line 1: the options are "", want exactly`},
		},
		{
			name:    "a valid-before option (OpenSSH 8.1)",
			files:   withFile(good(), "deploy/keys/update_signers", line1(`"isshoni-update" ssh`, `"isshoni-update",valid-before="20271231" ssh`)),
			wantErr: []string{`valid-before="20271231" is not even an option that Windows 10's OpenSSH 8.1 parses`},
		},
		{
			name:    "cert-authority",
			files:   withFile(good(), "deploy/keys/update_signers", line1("namespaces=", "cert-authority,namespaces=")),
			wantErr: []string{`line 1: the options are "cert-authority,namespaces=\"isshoni-update\""`},
		},
		{
			name:    "an ecdsa key",
			files:   withFile(good(), "deploy/keys/update_signers", ecdsaUpdateLine(t)+"\n"+goodUpdateSigners),
			wantErr: []string{"line 1: a ecdsa-sha2-nistp256 key, want ssh-ed25519"},
		},
		{
			name:    "a name of another pattern",
			files:   withFile(good(), "deploy/keys/update_signers", line1("isshoni-update-1", "isshoni-updte-backup-1")),
			wantErr: []string{`line 1: the name "isshoni-updte-backup-1" is neither isshoni-update-<n> nor isshoni-update-backup-<n>`},
		},
		{
			name:    "no name",
			files:   withFile(good(), "deploy/keys/update_signers", line1(" isshoni-update-1", "")),
			wantErr: []string{`line 1: the name "" is neither`},
		},
		{
			name:    "not a key",
			files:   withFile(good(), "deploy/keys/update_signers", line1(updateKey1.key, "ssh-ed25519 AAAAnotakey")),
			wantErr: []string{"line 1: no public key that can be read"},
		},
		{
			name:    "the same key twice",
			files:   withFile(good(), "deploy/keys/update_signers", goodUpdateSigners+strings.Replace(updateKey1.line, "isshoni-update-1", "isshoni-update-3", 1)+"\n"),
			wantErr: []string{"line 3: the same key as line 1"},
		},
		{
			name:    "the same name twice",
			files:   withFile(good(), "deploy/keys/update_signers", goodUpdateSigners+strings.Replace(updateKey2.line, "isshoni-update-2", "isshoni-update-1", 1)+"\n"),
			wantErr: []string{"line 3: the name isshoni-update-1 is on line 1 too"},
		},
		{
			name:    "a private key",
			files:   withFile(good(), "deploy/keys/update_signers", "-----BEGIN OPENSSH PRIVATE KEY-----\nb3Blbg==\n-----END OPENSSH PRIVATE KEY-----\n"),
			wantErr: []string{"deploy/keys/update_signers holds a PRIVATE key"},
		},
		{
			name:  "README.md lacks the update keys",
			files: withFile(updateTree(goodUpdateSigners, updateKey1, updateBackupKey), "README.md", goodReadme),
			wantErr: []string{
				`does not match deploy/keys/allowed_signers and deploy/keys/update_signers`,
				"it does not list " + updateKey1.fingerprint + ", the fingerprint of isshoni-update-1 (deploy/keys/update_signers line 1)",
				"it does not list " + updateBackupKey.fingerprint + ", the fingerprint of isshoni-update-backup-1 (deploy/keys/update_signers line 2)",
			},
		},
		{
			name: "README.md lists a fingerprint of neither file",
			files: withFile(good(), "README.md", readmeWithUpdateKeys([2]string{"isshoni-update-1", updateKey1.fingerprint},
				[2]string{"isshoni-update-backup-1", updateBackupKey.fingerprint}, [2]string{"isshoni-update-0", updateKey2.fingerprint})),
			wantErr: []string{"lists " + updateKey2.fingerprint + ", which is not the fingerprint of any key in " +
				"deploy/keys/allowed_signers or deploy/keys/update_signers"},
		},
		{
			name: "README.md swaps the update keys' fingerprints",
			files: withFile(good(), "README.md", readmeWithUpdateKeys([2]string{"isshoni-update-1", updateBackupKey.fingerprint},
				[2]string{"isshoni-update-backup-1", updateKey1.fingerprint})),
			wantErr: []string{"names isshoni-update-1 but lists " + updateBackupKey.fingerprint},
		},
		{
			name: "update rows 'not created yet' while update_signers does not exist",
			files: withFile(withFile(keyTree(goodScript, signers), "internal/update/signers.go", signersGoWith("\tembeddedSigners = []signer{}")),
				"README.md", readmeWithUpdateKeys([2]string{"isshoni-update-1", "not created yet"},
					[2]string{"isshoni-update-backup-1", "not created yet"})),
			wantNote: noUpdateKeysNote,
		},
		{
			name: "update rows 'not created yet' once update_signers exists",
			files: withFile(good(), "README.md", readmeWithUpdateKeys([2]string{"isshoni-update-1", updateKey1.fingerprint},
				[2]string{"isshoni-update-backup-1", "not created yet"})),
			wantErr: []string{"still says a key is not created yet: | `isshoni-update-backup-1`", "it does not list " + updateBackupKey.fingerprint},
		},
		{
			name: "only update_signers: the release rows may say 'not created yet'",
			files: map[string]string{
				"deploy/keys/update_signers": goodUpdateSigners,
				"internal/update/signers.go": signersGoWith(blockFor(updateKey1, updateBackupKey)),
				"README.md": strings.NewReplacer(releaseKey.fingerprint, "not created yet", backupKey.fingerprint, "not created yet").
					Replace(readmeWithUpdateKeys([2]string{"isshoni-update-1", updateKey1.fingerprint},
						[2]string{"isshoni-update-backup-1", updateBackupKey.fingerprint})),
			},
			wantNote: "skipped: deploy/install.sh and deploy/keys/allowed_signers do not exist yet",
		},
		{
			name: "only update_signers: README.md must list the update keys",
			files: map[string]string{
				"deploy/keys/update_signers": goodUpdateSigners,
				"internal/update/signers.go": signersGoWith(blockFor(updateKey1, updateBackupKey)),
				"README.md":                  readmeWith("not created yet", "not created yet"),
			},
			wantNote: "skipped: deploy/install.sh and deploy/keys/allowed_signers do not exist yet",
			wantErr:  []string{"does not match deploy/keys/update_signers:", "it does not list " + updateKey1.fingerprint},
		},
		{
			name: "a release key is also an update key",
			files: withFile(withFile(withFile(good(), "deploy/keys/update_signers",
				updateLinePrefix+releaseKey.key+" isshoni-update-1\n"+updateBackupKey.line+"\n"),
				"internal/update/signers.go", signersGoWith(blockFor(testKey{name: "isshoni-update-1", key: releaseKey.key}, updateBackupKey))),
				"README.md", readmeWithUpdateKeys([2]string{"isshoni-update-1", releaseKey.fingerprint},
					[2]string{"isshoni-update-backup-1", updateBackupKey.fingerprint})),
			wantErr: []string{releaseKey.fingerprint + " is both isshoni-release-1 (deploy/keys/allowed_signers line 1) and " +
				"isshoni-update-1 (deploy/keys/update_signers line 1): the update keys are keys of their own"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCheckKeys(t, tt.files, tt.wantNote, tt.wantErr)
		})
	}
}

// TestWriteUpdateKeys runs update-keys on a copy of the real internal/update/signers.go.
func TestWriteUpdateKeys(t *testing.T) {
	real, err := os.ReadFile(filepath.Join("..", "..", "internal", "update", "signers.go"))
	if err != nil {
		t.Fatal(err)
	}
	real = bytes.ReplaceAll(real, []byte("\r\n"), []byte("\n"))
	files := withFile(updateTree(three(), updateKey1, updateKey2, updateBackupKey), "internal/update/signers.go", string(real))
	root := writeTree(t, files)

	var stdout bytes.Buffer
	note, err := writeUpdateKeys(root, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if note != "wrote 3 keys into internal/update/signers.go" {
		t.Errorf("note %q", note)
	}
	for _, want := range []string{
		"repocheck update-keys: line 1: " + updateKey1.fingerprint + " isshoni-update-1 (active)\n",
		"repocheck update-keys: line 2: " + updateKey2.fingerprint + " isshoni-update-2 (active)\n",
		"repocheck update-keys: line 3: " + updateBackupKey.fingerprint + " isshoni-update-backup-1 (backup)\n",
	} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("stdout %q does not contain %q", stdout.String(), want)
		}
	}
	got, err := os.ReadFile(filepath.Join(root, "internal", "update", "signers.go"))
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := format.Source(got)
	if err != nil {
		t.Fatalf("the written signers.go does not parse: %v\n%s", err, got)
	}
	if !bytes.Equal(formatted, got) {
		t.Errorf("the written signers.go is not gofmt-clean:\n%s", got)
	}
	if !strings.Contains(string(got), `Name:   "isshoni-update-backup-1",`+"\n\t\t\tBackup: true,") {
		t.Errorf("no backup entry:\n%s", got)
	}
	// Outside the block the file is unchanged.
	gotLines, b1, e1, err := signersBlockLines(string(got))
	if err != nil {
		t.Fatal(err)
	}
	realLines, b2, e2, err := signersBlockLines(string(real))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(gotLines[:b1+1], "\n") != strings.Join(realLines[:b2+1], "\n") ||
		strings.Join(gotLines[e1:], "\n") != strings.Join(realLines[e2:], "\n") {
		t.Error("update-keys changed signers.go outside the block")
	}
	// lint:keys now passes.
	if note, err := checkKeys(root); err != nil {
		t.Errorf("checkKeys after update-keys: %v (note %q)", err, note)
	}

	// A file that breaks a rule leaves signers.go as it was.
	if err := os.WriteFile(filepath.Join(root, "deploy", "keys", "update_signers"), []byte(updateKey1.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writeUpdateKeys(root, &stdout); err == nil || !strings.Contains(err.Error(), "exactly one backup key") {
		t.Errorf("update-keys with no backup line: %v", err)
	}
	if after, _ := os.ReadFile(filepath.Join(root, "internal", "update", "signers.go")); !bytes.Equal(after, got) {
		t.Error("a failed update-keys changed signers.go")
	}
}

// TestCommittedUpdateKeys checks the repository's own deploy/keys/update_signers and internal/update/signers.go: the
// block embeds no key while the file does not exist, and exactly the file's keys once the owner's key PR (06 §14) adds
// it. The test job runs this on every Go change, so a hand-edited block fails there too, not only in lint:keys.
func TestCommittedUpdateKeys(t *testing.T) {
	if _, _, err := checkUpdateKeys(filepath.Join("..", "..")); err != nil {
		t.Errorf("the repository's update keys: %v", err)
	}
}

func three() string {
	return updateKey1.line + "\n" + updateKey2.line + "\n" + updateBackupKey.line + "\n"
}

func TestWriteUpdateKeysRefusals(t *testing.T) {
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no update_signers", map[string]string{"internal/update/signers.go": signersGoWith("\tembeddedSigners = []signer{}")}, "deploy/keys/update_signers is missing or empty"},
		{"an empty update_signers", map[string]string{"deploy/keys/update_signers": "\n", "internal/update/signers.go": signersGoWith("\tembeddedSigners = []signer{}")}, "is missing or empty"},
		{"no signers.go", map[string]string{"deploy/keys/update_signers": goodUpdateSigners}, "signers.go"},
		{"signers.go without markers", map[string]string{"deploy/keys/update_signers": goodUpdateSigners, "internal/update/signers.go": "package update\n"}, `has no "// BEGIN UPDATE SIGNERS" line`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := writeUpdateKeys(writeTree(t, tt.files), &bytes.Buffer{})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
		})
	}
}

func TestRunUpdateKeys(t *testing.T) {
	files := updateTree(goodUpdateSigners, updateKey1, updateBackupKey)
	files["internal/update/signers.go"] = signersGoWith("\tembeddedSigners = []signer{}")
	root := writeTree(t, files)
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", root, "update-keys"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "repocheck update-keys: wrote 2 keys into internal/update/signers.go") {
		t.Errorf("stdout %q", stdout.String())
	}
	stdout.Reset()
	if code := run([]string{"-root", root, "keys"}, &stdout, &stderr); code != 0 {
		t.Errorf("keys after update-keys: exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"-root", t.TempDir(), "update-keys"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "repocheck update-keys: deploy/keys/update_signers is missing or empty") {
		t.Errorf("update-keys without the file: exit %d, stderr %q", code, stderr.String())
	}
}
