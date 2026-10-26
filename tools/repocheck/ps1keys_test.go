package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestCheckKeysPS1 covers lint:keys' check of the key block in deploy/install-desktop.ps1 (06 §7.1, §16: "ps1
// block").
func TestCheckKeysPS1(t *testing.T) {
	const ps1 = "deploy/install-desktop.ps1"
	emptyPS1 := ps1With("")
	tests := []struct {
		name     string
		files    map[string]string
		wantNote string   // substring of the note
		wantErr  []string // substrings of the error; nil means no error
	}{
		{
			name:  "equal",
			files: keyTree(goodScript, signers),
		},
		{
			name:  "equal with CRLF line ends in the script",
			files: withFile(keyTree(goodScript, signers), ps1, crlf(ps1With(signers))),
		},
		{
			name:  "equal with comment and blank lines in the key file",
			files: withFile(keyTree(scriptWith("# release keys\n\n"+signers), "# release keys\n\n"+signers), "README.md", goodReadme),
		},
		{
			name:    "second key differs",
			files:   withFile(keyTree(goodScript, signers), ps1, ps1With(releaseKey.line+"\n"+otherKey.line+"\n")),
			wantErr: []string{"the key block in deploy/install-desktop.ps1 differs from deploy/keys/allowed_signers at line 2"},
		},
		{
			name:    "a key missing from the block",
			files:   withFile(keyTree(goodScript, signers), ps1, ps1With(releaseKey.line+"\n")),
			wantErr: []string{"the key block in deploy/install-desktop.ps1 differs from deploy/keys/allowed_signers at line 2"},
		},
		{
			name:    "an empty block",
			files:   withFile(keyTree(goodScript, signers), ps1, emptyPS1),
			wantErr: []string{"the key block in deploy/install-desktop.ps1 differs from deploy/keys/allowed_signers at line 1"},
		},
		{
			name:    "an extra key in the file: both blocks differ",
			files:   withFile(keyTree(goodScript, signers+otherKey.line+"\n"), ps1, ps1With(signers)),
			wantErr: []string{"deploy/install.sh differs from deploy/keys/allowed_signers at line 3", "deploy/install-desktop.ps1 differs from deploy/keys/allowed_signers at line 3"},
		},
		{
			name:     "no install-desktop.ps1",
			files:    withoutFile(keyTree(goodScript, signers), ps1),
			wantNote: "skipped the key block of deploy/install-desktop.ps1: it does not exist yet",
		},
		{
			name:     "no allowed_signers: an empty block passes",
			files:    map[string]string{"deploy/install.sh": scriptWith(""), ps1: emptyPS1},
			wantNote: "skipped: deploy/keys/allowed_signers does not exist yet",
		},
		{
			name:     "no allowed_signers and no install.sh: an empty block passes",
			files:    map[string]string{ps1: emptyPS1},
			wantNote: "do not exist yet",
		},
		{
			name:     "no allowed_signers: a block with keys fails",
			files:    map[string]string{"deploy/install.sh": scriptWith(""), ps1: ps1With(signers)},
			wantNote: "skipped: deploy/keys/allowed_signers does not exist yet",
			wantErr:  []string{"the key block in deploy/install-desktop.ps1 holds lines, but deploy/keys/allowed_signers does not exist"},
		},
		{
			name:     "no allowed_signers: a broken block fails",
			files:    map[string]string{ps1: strings.Replace(emptyPS1, keysEnd, "# END", 1)},
			wantNote: "do not exist yet",
			wantErr:  []string{`deploy/install-desktop.ps1: no "# END ALLOWED SIGNERS" line`},
		},
		{
			name:    "the here-string does not open on the line before BEGIN",
			files:   withFile(keyTree(goodScript, signers), ps1, strings.Replace(ps1With(signers), ps1BlockOpen, "$AllowedSigners = @\"", 1)),
			wantErr: []string{`the line before "# BEGIN ALLOWED SIGNERS" must be $AllowedSigners = @'`},
		},
		{
			name:    "the here-string does not close on the line after END",
			files:   withFile(keyTree(goodScript, signers), ps1, strings.Replace(ps1With(signers), keysEnd+"\n"+ps1BlockClose, keysEnd+"\n\n"+ps1BlockClose, 1)),
			wantErr: []string{`the line after "# END ALLOWED SIGNERS" must be '@`},
		},
		{
			name:    "two blocks",
			files:   withFile(keyTree(goodScript, signers), ps1, ps1With(signers)+ps1With(signers)),
			wantErr: []string{`deploy/install-desktop.ps1: line 17: a second "# BEGIN ALLOWED SIGNERS" line`},
		},
		{
			name:    "END before BEGIN",
			files:   withFile(keyTree(goodScript, signers), ps1, keysEnd+"\n"+ps1BlockClose+"\n"+ps1BlockOpen+"\n"+keysBegin+"\n"),
			wantErr: []string{`comes before "# BEGIN ALLOWED SIGNERS"`},
		},
		{
			name:    "a line in the block ends the here-string early",
			files:   withFile(keyTree(goodScript, signers), ps1, ps1With("'@\n"+signers)),
			wantErr: []string{"ends the here-string inside the key block"},
		},
		{
			name: "a key file line that would end the here-string",
			files: withFile(keyTree(scriptWith("'@x\n"+signers), "'@x\n"+signers), ps1,
				strings.Replace(ps1With(signers), keysBegin, keysBegin+"\n'@x", 1)),
			wantErr: []string{"deploy/keys/allowed_signers line 1 starts with '@, which would end the here-string"},
		},
		{
			name:    "a key file comment that is not ASCII",
			files:   keyTree(scriptWith("# rélease keys\n"+signers), "# rélease keys\n"+signers),
			wantErr: []string{"deploy/keys/allowed_signers line 1 has a character that is not printable ASCII"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCheckKeys(t, tt.files, tt.wantNote, tt.wantErr)
		})
	}
}

// TestCheckKeysPS1FinalNewline: a key file without its final newline is reported once, not once per block.
func TestCheckKeysPS1FinalNewline(t *testing.T) {
	_, err := checkKeys(writeTree(t, keyTree(goodScript, strings.TrimSuffix(signers, "\n"))))
	if err == nil {
		t.Fatal("no error")
	}
	if n := strings.Count(err.Error(), "must end with a newline"); n != 1 {
		t.Errorf("error %q reports the missing newline %d times, want once", err, n)
	}
	// Without install.sh the ps1 block reports it.
	_, err = checkKeys(writeTree(t, withoutFile(keyTree(goodScript, strings.TrimSuffix(signers, "\n")), "deploy/install.sh")))
	if err == nil || !strings.Contains(err.Error(), "deploy/keys/allowed_signers must end with a newline") {
		t.Errorf("without install.sh: %v", err)
	}
}

// TestCommittedReleaseKeyBlocks checks the repository's own key blocks: deploy/install-desktop.ps1's holds nothing
// while deploy/keys/allowed_signers does not exist, and both scripts' blocks equal the file once the owner's key PR
// adds it (`task release:keys`). The test job runs this on every Go change.
func TestCommittedReleaseKeyBlocks(t *testing.T) {
	if _, _, err := checkReleaseKeys(filepath.Join("..", "..")); err != nil {
		t.Errorf("the repository's release key blocks: %v", err)
	}
}

// TestWritePS1Keys runs ps1-keys on a copy of the real deploy/install-desktop.ps1.
func TestWritePS1Keys(t *testing.T) {
	real, err := os.ReadFile(filepath.Join("..", "..", "deploy", "install-desktop.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	// A CRLF checkout of the script is written back with LF line ends.
	files := withFile(keyTree(goodScript, signers), "deploy/install-desktop.ps1", crlf(string(bytes.ReplaceAll(real, []byte("\r\n"), []byte("\n")))))
	root := writeTree(t, files)
	if _, err := checkKeys(root); err == nil {
		t.Fatal("checkKeys passed before ps1-keys wrote the block")
	}

	note, err := writePS1Keys(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := "wrote deploy/keys/allowed_signers (2 keys) into deploy/install-desktop.ps1"; note != want {
		t.Errorf("note %q, want %q", note, want)
	}
	got, err := os.ReadFile(filepath.Join(root, "deploy", "install-desktop.ps1"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(got, []byte("\r")) {
		t.Error("the written script has CR characters")
	}
	want := strings.Replace(string(bytes.ReplaceAll(real, []byte("\r\n"), []byte("\n"))), keysEnd, signers+keysEnd, 1)
	if string(got) != want {
		t.Errorf("the written script differs from the original with the keys added:\n%s", got)
	}
	if note, err := checkKeys(root); err != nil {
		t.Errorf("checkKeys after ps1-keys: %v (note %q)", err, note)
	}
	// Running it again changes nothing.
	if _, err := writePS1Keys(root); err != nil {
		t.Fatal(err)
	}
	if again, _ := os.ReadFile(filepath.Join(root, "deploy", "install-desktop.ps1")); !bytes.Equal(again, got) {
		t.Error("a second ps1-keys changed the script")
	}
	// A rotation that removes a key replaces the block.
	if err := os.WriteFile(filepath.Join(root, "deploy", "keys", "allowed_signers"), []byte(releaseKey.line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := writePS1Keys(root); err != nil {
		t.Fatal(err)
	}
	rotated, _ := os.ReadFile(filepath.Join(root, "deploy", "install-desktop.ps1"))
	if strings.Contains(string(rotated), backupKey.key) || !strings.Contains(string(rotated), releaseKey.line+"\n"+keysEnd) {
		t.Errorf("after the rotation the block is not the file:\n%s", rotated)
	}
}

func TestWritePS1KeysRefusals(t *testing.T) {
	validBefore := strings.Replace(signers, `"isshoni-checksums" ssh`, `"isshoni-checksums",valid-before="20271231" ssh`, 1)
	for _, tt := range []struct {
		name  string
		files map[string]string
		want  string
	}{
		{"no allowed_signers", map[string]string{"deploy/install-desktop.ps1": ps1With("")}, "deploy/keys/allowed_signers is missing or empty"},
		{"an empty allowed_signers", map[string]string{"deploy/keys/allowed_signers": "", "deploy/install-desktop.ps1": ps1With("")}, "is missing or empty"},
		{"no final newline", map[string]string{"deploy/keys/allowed_signers": strings.TrimSuffix(signers, "\n"), "deploy/install-desktop.ps1": ps1With("")}, "must end with a newline"},
		{"a private key", map[string]string{"deploy/keys/allowed_signers": "-----BEGIN OPENSSH PRIVATE KEY-----\n", "deploy/install-desktop.ps1": ps1With("")}, "holds a PRIVATE key"},
		{"comments only", map[string]string{"deploy/keys/allowed_signers": "# later\n", "deploy/install-desktop.ps1": ps1With("")}, "has no key, only blank and comment lines"},
		{"a line that is no key", map[string]string{"deploy/keys/allowed_signers": "isshoni-release namespaces=\"isshoni-checksums\" ssh-ed25519 AAAAnotakey x\n", "deploy/install-desktop.ps1": ps1With("")}, "no public key that can be read"},
		{"valid-before", map[string]string{"deploy/keys/allowed_signers": validBefore, "deploy/install-desktop.ps1": ps1With("")}, "valid-before"},
		{"not ASCII", map[string]string{"deploy/keys/allowed_signers": "# é\n" + signers, "deploy/install-desktop.ps1": ps1With("")}, "not printable ASCII"},
		{"no install-desktop.ps1", map[string]string{"deploy/keys/allowed_signers": signers}, "install-desktop.ps1"},
		{"a script without the block", map[string]string{"deploy/keys/allowed_signers": signers, "deploy/install-desktop.ps1": "& { }\n"}, `deploy/install-desktop.ps1: no "# BEGIN ALLOWED SIGNERS" line`},
	} {
		t.Run(tt.name, func(t *testing.T) {
			root := writeTree(t, tt.files)
			before, _ := os.ReadFile(filepath.Join(root, "deploy", "install-desktop.ps1"))
			_, err := writePS1Keys(root)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Errorf("err = %v, want one containing %q", err, tt.want)
			}
			if after, _ := os.ReadFile(filepath.Join(root, "deploy", "install-desktop.ps1")); !bytes.Equal(after, before) {
				t.Error("a failed ps1-keys changed the script")
			}
		})
	}
}

func TestRunPS1Keys(t *testing.T) {
	root := writeTree(t, withFile(keyTree(goodScript, signers), "deploy/install-desktop.ps1", ps1With("")))
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-root", root, "keys"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "the key block in deploy/install-desktop.ps1 differs") {
		t.Errorf("keys before ps1-keys: exit %d, stderr %q", code, stderr.String())
	}
	stderr.Reset()
	if code := run([]string{"-root", root, "ps1-keys"}, &stdout, &stderr); code != 0 {
		t.Fatalf("ps1-keys: exit %d: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "repocheck ps1-keys: wrote deploy/keys/allowed_signers (2 keys) into deploy/install-desktop.ps1") {
		t.Errorf("stdout %q", stdout.String())
	}
	if code := run([]string{"-root", root, "keys"}, &stdout, &stderr); code != 0 {
		t.Errorf("keys after ps1-keys: exit %d: %s", code, stderr.String())
	}
	if code := run([]string{"-root", t.TempDir(), "ps1-keys"}, &stdout, &stderr); code != 1 ||
		!strings.Contains(stderr.String(), "repocheck ps1-keys: deploy/keys/allowed_signers is missing or empty") {
		t.Errorf("ps1-keys without the key file: exit %d, stderr %q", code, stderr.String())
	}
}
