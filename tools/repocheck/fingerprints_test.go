package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// withFile returns files with name set to content.
func withFile(files map[string]string, name, content string) map[string]string {
	files[name] = content
	return files
}

// withoutFile returns files without name.
func withoutFile(files map[string]string, name string) map[string]string {
	delete(files, name)
	return files
}

// TestCheckKeysReadme covers the cross-check of deploy/keys/allowed_signers with the "Release signing keys" section of
// README.md (06 §9.4, §10.3).
func TestCheckKeysReadme(t *testing.T) {
	notCreatedReadme := readmeWith("not created yet", "not created yet") // the README as F05 wrote it
	// A rotation: the old key is marked valid-before, a new one, without a comment, is added.
	rotated := strings.Replace(releaseKey.line, `"isshoni-checksums"`, `"isshoni-checksums",valid-before="20271231"`, 1) +
		"\n" + backupKey.line + "\n" + `isshoni-release namespaces="isshoni-checksums" ` + otherKey.key + "\n"
	rotatedReadme := strings.Replace(goodReadme, "| `isshoni-release-backup-1`",
		"| (new) | signs every release from 2028 | "+otherKey.fingerprint+" |\n| `isshoni-release-backup-1`", 1)
	staleRow := "| `isshoni-release-0` | replaced | " + otherKey.fingerprint + " |\n"
	mistyped := releaseKey.fingerprint[:len(releaseKey.fingerprint)-1]
	noKey := releaseKey.line + "\n" +
		`isshoni-release namespaces="isshoni-checksums" ssh-ed25519 AAAAnotakey isshoni-release-backup-1` + "\n"
	wrongType := releaseKey.line + "\n" + strings.Replace(backupKey.line, "ssh-ed25519 ", "ssh-rsa ", 1) + "\n"

	tests := []struct {
		name     string
		files    map[string]string
		wantNote string   // substring of the note
		wantErr  []string // substrings of the error; nil means no error
	}{
		{
			name: "no key file yet: skipped",
			files: map[string]string{
				"deploy/install.sh": scriptWith(""),
				"README.md":         notCreatedReadme,
			},
			wantNote: "skipped: deploy/keys/allowed_signers does not exist yet",
		},
		{
			name:  "every fingerprint listed",
			files: keyTree(goodScript, signers),
		},
		{
			name:  "every fingerprint listed, README.md with CRLF line ends",
			files: withFile(keyTree(goodScript, signers), "README.md", crlf(goodReadme)),
		},
		{
			name:  "fingerprints in a level-3 subsection",
			files: withFile(keyTree(goodScript, signers), "README.md", strings.Replace(goodReadme, "| Key |", "### Fingerprints\n\n| Key |", 1)),
		},
		{
			name: "a heading in a code block does not end the section",
			files: withFile(keyTree(goodScript, signers), "README.md", strings.Replace(goodReadme, "## Release signing keys\n",
				"## Release signing keys\n\n```sh\n## Privacy\n# BEGIN ALLOWED SIGNERS\n```\n", 1)),
		},
		{
			name:  "a closing #s after the heading",
			files: withFile(keyTree(goodScript, signers), "README.md", strings.Replace(goodReadme, "## Release signing keys", "## Release signing keys ##", 1)),
		},
		{
			name:  "rotation: a valid-before option and a key without a comment",
			files: withFile(keyTree(scriptWith(rotated), rotated), "README.md", rotatedReadme),
		},
		{
			name:  "rotation: the new key's fingerprint is missing",
			files: keyTree(scriptWith(rotated), rotated),
			wantErr: []string{"it does not list " + otherKey.fingerprint +
				", the fingerprint of the key on deploy/keys/allowed_signers line 3"},
		},
		{
			name:  "one fingerprint missing",
			files: withFile(keyTree(goodScript, signers), "README.md", readmeWith(releaseKey.fingerprint, "")),
			wantErr: []string{
				`the "Release signing keys" section of README.md does not match deploy/keys/allowed_signers`,
				"it does not list " + backupKey.fingerprint +
					", the fingerprint of isshoni-release-backup-1 (deploy/keys/allowed_signers line 2)",
				"deploy/keys/README.md step 2",
			},
		},
		{
			name: "an extra, stale fingerprint",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(goodReadme, "| `isshoni-release-backup-1`", staleRow+"| `isshoni-release-backup-1`", 1)),
			wantErr: []string{"README.md:16 lists " + otherKey.fingerprint +
				", which is not the fingerprint of any key in deploy/keys/allowed_signers"},
		},
		{
			name:  "a mistyped fingerprint",
			files: withFile(keyTree(goodScript, signers), "README.md", readmeWith(mistyped, backupKey.fingerprint)),
			wantErr: []string{
				"README.md:15 lists " + mistyped + ", which is not the fingerprint of any key",
				"it does not list " + releaseKey.fingerprint,
			},
		},
		{
			name:  "a 'not created yet' row left",
			files: withFile(keyTree(goodScript, signers), "README.md", readmeWith(releaseKey.fingerprint, "not created yet")),
			wantErr: []string{
				"README.md:16 still says a key is not created yet: | `isshoni-release-backup-1` |",
				"it does not list " + backupKey.fingerprint,
			},
		},
		{
			name:    "a 'Not created yet' row next to the fingerprint",
			files:   withFile(keyTree(goodScript, signers), "README.md", readmeWith(releaseKey.fingerprint, backupKey.fingerprint+" (Not created yet)")),
			wantErr: []string{"README.md:16 still says a key is not created yet"},
		},
		{
			name:  "the README as F05 wrote it",
			files: withFile(keyTree(goodScript, signers), "README.md", notCreatedReadme),
			wantErr: []string{
				"README.md:15 still says a key is not created yet",
				"README.md:16 still says a key is not created yet",
				"it does not list " + releaseKey.fingerprint,
				"it does not list " + backupKey.fingerprint,
			},
		},
		{
			name:  "the fingerprints in each other's rows",
			files: withFile(keyTree(goodScript, signers), "README.md", readmeWith(backupKey.fingerprint, releaseKey.fingerprint)),
			wantErr: []string{
				"README.md:15 names isshoni-release-1 but lists " + backupKey.fingerprint +
					", the fingerprint of isshoni-release-backup-1 (deploy/keys/allowed_signers line 2)",
				"README.md:16 names isshoni-release-backup-1 but lists " + releaseKey.fingerprint,
			},
		},
		{
			name: "a fingerprint only in an HTML comment",
			files: withFile(keyTree(goodScript, signers), "README.md",
				readmeWith("<!-- "+releaseKey.fingerprint+" -->", backupKey.fingerprint)),
			wantErr: []string{"it does not list " + releaseKey.fingerprint},
		},
		{
			name: "a fingerprint only in the next section",
			files: withFile(keyTree(goodScript, signers), "README.md",
				readmeWith("", backupKey.fingerprint)+"\n"+releaseKey.fingerprint+"\n"),
			wantErr: []string{"it does not list " + releaseKey.fingerprint},
		},
		{
			// "```ssh-keygen -lf```" is inline code, no fence that would hide the "## Privacy" heading (CommonMark 4.5).
			name: "a fingerprint only after inline code and the next heading",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(readmeWith(releaseKey.fingerprint, "TBD"), "## Privacy\n",
					"```ssh-keygen -lf``` prints them.\n\n## Privacy\n\n"+backupKey.fingerprint+"\n", 1)),
			wantErr: []string{"it does not list " + backupKey.fingerprint},
		},
		{
			name: "a fingerprint only after the next heading, a setext one",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(readmeWith(releaseKey.fingerprint, "TBD"), "## Privacy\n",
					"Privacy\n-------\n\n"+backupKey.fingerprint+"\n", 1)),
			wantErr: []string{"it does not list " + backupKey.fingerprint},
		},
		{
			name: "the section under a setext heading",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(goodReadme, "## Release signing keys\n", "Release signing keys\n--------------------\n", 1)),
		},
		{
			name: "README.md without the section",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(goodReadme, "## Release signing keys", "## Signing", 1)),
			wantErr: []string{`README.md has no "## Release signing keys" section`},
		},
		{
			name: "the section as a level-3 heading",
			files: withFile(keyTree(goodScript, signers), "README.md",
				strings.Replace(goodReadme, "## Release signing keys", "### Release signing keys", 1)),
			wantErr: []string{`README.md has no "## Release signing keys" section`},
		},
		{
			name:    "no README.md",
			files:   withoutFile(keyTree(goodScript, signers), "README.md"),
			wantErr: []string{`there is no README.md, whose "## Release signing keys" section must list`},
		},
		{
			// release.yml's `[ -s allowed_signers ]` guard takes a file of blank lines for one with keys.
			name:    "a blank key file",
			files:   withFile(keyTree(scriptWith("\n"), "\n"), "README.md", notCreatedReadme),
			wantErr: []string{"deploy/keys/allowed_signers has no key, only blank and comment lines"},
		},
		{
			name: "an empty key file: README.md is not checked",
			files: map[string]string{
				"deploy/keys/allowed_signers": "",
				"README.md":                   notCreatedReadme,
			},
			wantNote: "skipped the key block: deploy/install.sh does not exist yet",
		},
		{
			name:    "an empty key file: the block check wants a final newline",
			files:   withFile(keyTree(scriptWith(""), ""), "README.md", notCreatedReadme),
			wantErr: []string{"deploy/keys/allowed_signers must end with a newline"},
		},
		{
			name:    "a key file with comments only",
			files:   keyTree(scriptWith("# the keys come later\n"), "# the keys come later\n"),
			wantErr: []string{"deploy/keys/allowed_signers has no key, only blank and comment lines"},
		},
		{
			name:    "a line without a key",
			files:   keyTree(scriptWith(noKey), noKey),
			wantErr: []string{"deploy/keys/allowed_signers line 2: no public key that can be read"},
		},
		{
			name:    "a key type that does not match the key",
			files:   keyTree(scriptWith(wrongType), wrongType),
			wantErr: []string{"deploy/keys/allowed_signers line 2: no public key that can be read", "key type mismatch"},
		},
		{
			name: "no install.sh: README.md is still checked",
			files: map[string]string{
				"deploy/keys/allowed_signers": signers,
				"README.md":                   readmeWith(releaseKey.fingerprint, ""),
			},
			wantNote: "skipped the key block: deploy/install.sh does not exist yet",
			wantErr:  []string{"it does not list " + backupKey.fingerprint},
		},
		{
			name: "no install.sh, README.md complete",
			files: map[string]string{
				"deploy/keys/allowed_signers": signers,
				"README.md":                   goodReadme,
			},
			wantNote: "skipped the key block: deploy/install.sh does not exist yet",
		},
		{
			name: "the block and README.md both wrong: both reported",
			files: withFile(keyTree(strings.Replace(goodScript, backupKey.key, otherKey.key, 1), signers), "README.md",
				readmeWith(releaseKey.fingerprint, "")),
			wantErr: []string{
				"differs from deploy/keys/allowed_signers at line 2",
				"it does not list " + backupKey.fingerprint,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assertCheckKeys(t, tt.files, tt.wantNote, tt.wantErr)
		})
	}
}

func TestParseAllowedSigners(t *testing.T) {
	data := "# release keys\n\n" +
		"  " + releaseKey.line + "  \r\n" + // surrounding blanks and a CR
		`"isshoni-release,other" namespaces="isshoni-checksums" ` + backupKey.key + " " + backupKey.name + "\n" +
		"isshoni-release\t" + otherKey.key + "\n" // no options, no comment, a tab
	keys, err := parseAllowedSigners(data)
	if err != nil {
		t.Fatal(err)
	}
	want := []signerKey{
		{line: 3, name: releaseKey.name, fingerprint: releaseKey.fingerprint},
		{line: 4, name: backupKey.name, fingerprint: backupKey.fingerprint},
		{line: 5, name: "", fingerprint: otherKey.fingerprint},
	}
	if len(keys) != len(want) {
		t.Fatalf("parseAllowedSigners = %+v, want %+v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("key %d = %+v, want %+v", i, keys[i], want[i])
		}
	}

	for _, bad := range []string{
		"isshoni-release\n",                         // principals only
		`"isshoni-release ` + releaseKey.key + "\n", // unterminated quote
		releaseKey.key + "\n",                       // no principals: the key type is taken for them
	} {
		if _, err := parseAllowedSigners(bad); err == nil {
			t.Errorf("parseAllowedSigners(%q): no error", bad)
		}
	}
}

// TestFingerprintLikeSSHKeygen checks that the fingerprints are the ones `ssh-keygen -l` prints: against a value
// that ssh-keygen printed for releaseKey, and, where ssh-keygen is installed, against ssh-keygen itself.
func TestFingerprintLikeSSHKeygen(t *testing.T) {
	keys, err := parseAllowedSigners(signers + otherKey.line + "\n")
	if err != nil {
		t.Fatal(err)
	}
	// `ssh-keygen -l -f` of releaseKey's public half printed this (OpenSSH 10.3).
	if got, want := keys[0].fingerprint, "SHA256:BCB7VLPy6hZg4DSe4YgT/AaisS0LX9Sr1baP8JC0vYA"; got != want {
		t.Errorf("releaseKey's fingerprint = %s, want %s", got, want)
	}

	sshKeygen, err := exec.LookPath("ssh-keygen")
	if err != nil {
		t.Skip("ssh-keygen is not installed")
	}
	for i, k := range []testKey{releaseKey, backupKey, otherKey} {
		path := filepath.Join(t.TempDir(), "key.pub")
		if err := os.WriteFile(path, []byte(k.key+" "+k.name+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := exec.CommandContext(context.Background(), sshKeygen, "-l", "-f", path).Output()
		if err != nil {
			t.Fatalf("ssh-keygen -l -f: %v", err)
		}
		// "256 SHA256:… isshoni-release-1 (ED25519)"
		fields := strings.Fields(string(out))
		if len(fields) < 2 {
			t.Fatalf("ssh-keygen -l printed %q", out)
		}
		if keys[i].fingerprint != fields[1] {
			t.Errorf("%s: fingerprint %s, ssh-keygen prints %s", k.name, keys[i].fingerprint, fields[1])
		}
	}
}

func TestMarkdownSection(t *testing.T) {
	const title = "Keys"
	tests := []struct {
		name string
		doc  string
		want []string // "n:text" of each line of the section; nil means no section
	}{
		{
			name: "up to the next level-2 heading",
			doc:  "# T\n## Keys\na\n### Sub\nb\n## Next\nc\n",
			want: []string{"3:a", "4:### Sub", "5:b"},
		},
		{
			name: "up to a level-1 heading",
			doc:  "## Keys\na\n# Other\nb\n",
			want: []string{"2:a"},
		},
		{
			name: "to the end of the document",
			doc:  "## Keys\na",
			want: []string{"2:a"},
		},
		{
			name: "the heading ignoring case, with a closing sequence and up to three spaces of indent",
			doc:  "   ## keys ##\na",
			want: []string{"2:a"},
		},
		{
			name: "no heading: four spaces of indent, no space after the #s, or another level",
			doc:  "    ## Keys\n##Keys\n### Keys\n# Keys\n",
		},
		{
			name: "the first of two sections",
			doc:  "## Keys\na\n## Keys\nb\n",
			want: []string{"2:a"},
		},
		{
			name: "headings in code blocks do not count",
			doc:  "```\n## Keys\n```\n## Keys\n~~~sh\n## Next\n```\n~~~~\na\n## Next\n",
			want: []string{"5:~~~sh", "6:## Next", "7:```", "8:~~~~", "9:a"},
		},
		{
			name: "a longer closing fence closes; one with text after it does not",
			doc:  "## Keys\n```\n``` x\n## Next\n`````\n## Next\n",
			want: []string{"2:```", "3:``` x", "4:## Next", "5:`````"},
		},
		{
			name: "HTML comments are left out, on one line and over several",
			doc:  "<!--\n## Keys\n-->\n## Keys\na <!-- b --> c <!-- d\n## Next\ne --> f\n",
			want: []string{"5:a  c ", "6:", "7: f", "8:"},
		},
		{
			name: "the code block in a comment is no code block",
			doc:  "## Keys\n<!-- ```\n-->\n## Next\n",
			want: []string{"2:", "3:"},
		},
		{
			// A backtick fence's info string has no backtick (CommonMark 4.5); a tilde fence's may have one.
			name: "inline code at the start of a line is no fence",
			doc:  "## Keys\n```x``` y\na\n~~~ `z`\n## Next\n~~~\nb\n## Next\nc\n",
			want: []string{"2:```x``` y", "3:a", "4:~~~ `z`", "5:## Next", "6:~~~", "7:b"},
		},
		{
			name: "up to a setext heading, without the paragraph it underlines",
			doc:  "## Keys\na\n\nNext\nsection\n  ----  \nb\n",
			want: []string{"2:a", "3:"},
		},
		{
			name: "up to a level-1 setext heading",
			doc:  "## Keys\na\n\n   Next\n=\nb\n",
			want: []string{"2:a", "3:"},
		},
		{
			// With no blank line between them, a and Next are one paragraph, the heading's text.
			name: "a section whose only paragraph is the next heading's text",
			doc:  "## Keys\na\nNext\n----\nb\n",
			want: []string{},
		},
		{
			name: "the section under a setext heading",
			doc:  "Keys\n===\nb\n\nKeys\n---\na\n## Next\n",
			want: []string{"7:a"},
		},
		{
			name: "no setext heading after a table row, a list item, a block quote, HTML, a break, a blank or indented code",
			doc: "## Keys\n| a |\n---\n- b\n---\n> c\nlazy\n---\n<p>d</p>\n---\n***\n---\n\n---\n" +
				"    code\n---\n\t~~~\n---\ne\n## Next\n",
			want: []string{"2:| a |", "3:---", "4:- b", "5:---", "6:> c", "7:lazy", "8:---", "9:<p>d</p>", "10:---",
				"11:***", "12:---", "13:", "14:---", "15:    code", "16:---", "17:\t~~~", "18:---", "19:e"},
		},
		{
			name: "an underline in a code block makes no heading",
			doc:  "## Keys\n```\na\n---\n```\nb",
			want: []string{"2:```", "3:a", "4:---", "5:```", "6:b"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lines, ok := markdownSection(tt.doc, title)
			if ok != (tt.want != nil) {
				t.Fatalf("markdownSection found = %v, want %v", ok, tt.want != nil)
			}
			var got []string
			for _, l := range lines {
				got = append(got, strconv.Itoa(l.n)+":"+l.text)
			}
			if strings.Join(got, "\n") != strings.Join(tt.want, "\n") {
				t.Errorf("markdownSection lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(tt.want, "\n"))
			}
		})
	}
}

func TestContainsWord(t *testing.T) {
	tests := []struct {
		text string
		want bool
	}{
		{"| `isshoni-release-1` | x |", true},
		{"isshoni-release-1", true},
		{"isshoni-release-10", false},
		{"my-isshoni-release-1", false},
		{"isshoni-release-10 and isshoni-release-1.", true},
		{"isshoni-release", false},
	}
	for _, tt := range tests {
		if got := containsWord(tt.text, "isshoni-release-1"); got != tt.want {
			t.Errorf("containsWord(%q) = %v, want %v", tt.text, got, tt.want)
		}
	}
}
