package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

// The root README.md publishes the release keys' fingerprints in its "Release signing keys" section, so that a user
// can compare them with what `ssh-keygen -Y verify` prints (06 §9.4, §10.3). README F05 wrote the section with a
// "not created yet" placeholder in each key's row; the owner replaces the placeholders with the fingerprints in the
// pull request that adds deploy/keys/allowed_signers (deploy/keys/README.md step 2). From then on the section must
// list the fingerprint of every key in the file and no other one.
const (
	readmeFile    = "README.md"
	readmeSection = "Release signing keys" // the heading text of a level-2 (##) heading
	notCreated    = "not created"          // README F05's placeholder ("not created yet"), matched ignoring case
)

// fingerprintRE matches a SHA256 fingerprint as `ssh-keygen -l` prints it: "SHA256:" and the unpadded base64 of the
// SHA-256 of the key blob. It takes the whole base64 run, so a mistyped or truncated fingerprint shows up as one that
// matches no key.
var fingerprintRE = regexp.MustCompile(`SHA256:[A-Za-z0-9+/]+`)

// signerKey is one key of deploy/keys/allowed_signers.
type signerKey struct {
	line        int    // 1-based line number in the file
	name        string // the key's comment, such as isshoni-release-1; empty when the line has none
	fingerprint string // as `ssh-keygen -l` prints it: SHA256:…
}

func (k signerKey) String() string {
	if k.name == "" {
		return fmt.Sprintf("the key on %s line %d", filepath.ToSlash(allowedSigners), k.line)
	}
	return fmt.Sprintf("%s (%s line %d)", k.name, filepath.ToSlash(allowedSigners), k.line)
}

// checkReadmeFingerprints cross-checks the "Release signing keys" section of README.md with signers, the content of
// deploy/keys/allowed_signers (LF line ends). It reports every problem at once:
//   - a key of the file whose fingerprint the section does not list;
//   - a fingerprint in the section that is no key's (a stale, mistyped or rotated-out one);
//   - a line of the section that names one key (its comment) but lists another key's fingerprint;
//   - a line of the section that still says a key is "not created yet".
//
// Only what a reader sees counts: HTML comments are left out, and the section ends at the next heading of level 1 or
// 2 outside a fenced code block.
func checkReadmeFingerprints(root, signers string) error {
	keys, err := parseAllowedSigners(signers)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return fmt.Errorf("%s has no key, only blank and comment lines", filepath.ToSlash(allowedSigners))
	}
	data, err := os.ReadFile(filepath.Join(root, readmeFile))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("there is no %s, whose %q section must list the fingerprints of the keys in %s "+
			"(deploy/keys/README.md step 2)", readmeFile, "## "+readmeSection, filepath.ToSlash(allowedSigners))
	}
	if err != nil {
		return err
	}
	// A Git for Windows checkout has CRLF line ends (core.autocrlf), as in checkKeys.
	section, ok := markdownSection(strings.ReplaceAll(string(data), "\r\n", "\n"), readmeSection)
	if !ok {
		return fmt.Errorf("%s has no %q section, which lists the fingerprints of the keys in %s "+
			"(06 §9.4; deploy/keys/README.md step 2)", readmeFile, "## "+readmeSection, filepath.ToSlash(allowedSigners))
	}

	byFingerprint := make(map[string]signerKey, len(keys))
	for _, k := range keys {
		if _, dup := byFingerprint[k.fingerprint]; !dup {
			byFingerprint[k.fingerprint] = k
		}
	}
	listed := make(map[string]bool, len(keys))
	var problems []string
	for _, l := range section {
		if strings.Contains(strings.ToLower(l.text), notCreated) {
			problems = append(problems, fmt.Sprintf("%s:%d still says a key is not created yet: %s",
				readmeFile, l.n, strings.TrimSpace(l.text)))
		}
		named := keysNamedIn(l.text, keys)
		for _, fp := range fingerprintRE.FindAllString(l.text, -1) {
			k, ok := byFingerprint[fp]
			if !ok {
				problems = append(problems, fmt.Sprintf("%s:%d lists %s, which is not the fingerprint of any key in %s",
					readmeFile, l.n, fp, filepath.ToSlash(allowedSigners)))
				continue
			}
			listed[fp] = true
			if len(named) == 1 && named[0].fingerprint != fp {
				problems = append(problems, fmt.Sprintf("%s:%d names %s but lists %s, the fingerprint of %s",
					readmeFile, l.n, named[0].name, fp, k))
			}
		}
	}
	for _, k := range keys {
		if !listed[k.fingerprint] {
			problems = append(problems, fmt.Sprintf("it does not list %s, the fingerprint of %s", k.fingerprint, k))
			listed[k.fingerprint] = true // report a key that is in the file twice once
		}
	}
	if len(problems) > 0 {
		return fmt.Errorf("the %q section of %s does not match %s:\n  - %s\n"+
			"Put each key's fingerprint, as `task release:keys` or `ssh-keygen -lf` prints it, into the key's row of "+
			"the table, and remove any other (deploy/keys/README.md step 2)",
			readmeSection, readmeFile, filepath.ToSlash(allowedSigners), strings.Join(problems, "\n  - "))
	}
	return nil
}

// parseAllowedSigners returns the keys of an allowed-signers file (ssh-keygen(1), "ALLOWED SIGNERS"): one key per
// line as principals, optional options, the key and an optional comment; blank lines and lines starting with # are
// skipped. Each key's fingerprint is the one `ssh-keygen -l` prints.
func parseAllowedSigners(data string) ([]signerKey, error) {
	var keys []signerKey
	for i, line := range strings.Split(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		rest, ok := cutPrincipals(line)
		if !ok {
			return nil, fmt.Errorf("%s line %d: no key after the principals", filepath.ToSlash(allowedSigners), i+1)
		}
		// What follows the principals has the authorized_keys format: [options] keytype base64 [comment].
		pub, comment, _, _, err := ssh.ParseAuthorizedKey([]byte(rest))
		if err != nil {
			return nil, fmt.Errorf("%s line %d: no public key that can be read: %w", filepath.ToSlash(allowedSigners), i+1, err)
		}
		keys = append(keys, signerKey{line: i + 1, name: comment, fingerprint: ssh.FingerprintSHA256(pub)})
	}
	return keys, nil
}

// cutPrincipals returns what follows the principals field of a non-blank allowed-signers line. As in OpenSSH, the
// field is a run of characters up to a space or tab, or a double-quoted string.
func cutPrincipals(line string) (rest string, ok bool) {
	if strings.HasPrefix(line, `"`) {
		end := strings.IndexByte(line[1:], '"')
		if end < 0 {
			return "", false
		}
		rest = line[end+2:]
	} else {
		i := strings.IndexAny(line, " \t")
		if i < 0 {
			return "", false
		}
		rest = line[i:]
	}
	rest = strings.TrimLeft(rest, " \t")
	return rest, rest != ""
}

// keysNamedIn returns the keys whose name (the comment of their allowed-signers line) text contains as a whole word.
func keysNamedIn(text string, keys []signerKey) []signerKey {
	var named []signerKey
	for _, k := range keys {
		if k.name != "" && containsWord(text, k.name) {
			named = append(named, k)
		}
	}
	return named
}

// containsWord reports whether word occurs in text with no letter, digit, '-' or '_' right before or after it, so
// that isshoni-release-1 is not found in isshoni-release-10.
func containsWord(text, word string) bool {
	for i := 0; ; {
		j := strings.Index(text[i:], word)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(word)
		if (start == 0 || !isWordByte(text[start-1])) && (end == len(text) || !isWordByte(text[end])) {
			return true
		}
		i = start + 1
	}
}

func isWordByte(b byte) bool {
	return 'a' <= b && b <= 'z' || 'A' <= b && b <= 'Z' || '0' <= b && b <= '9' || b == '-' || b == '_'
}

// docLine is a line of a Markdown document.
type docLine struct {
	n    int    // 1-based line number
	text string // the line without the parts in HTML comments
}

// markdownSection returns the lines of the first level-2 section of doc (LF line ends) whose heading text equals
// title, ignoring case: the lines after the heading, up to the next ATX heading of level 1 or 2. Headings in fenced
// code blocks and in HTML comments do not count, and the parts of lines in HTML comments are left out.
func markdownSection(doc, title string) (lines []docLine, found bool) {
	var (
		fence     string // the fence that opened the code block the scan is in, or ""
		inComment bool
		inSection bool
	)
	for i, text := range strings.Split(doc, "\n") {
		if fence != "" {
			if run, after := fenceRun(text); run != "" && run[0] == fence[0] && len(run) >= len(fence) &&
				strings.TrimSpace(after) == "" {
				fence = ""
			}
		} else {
			text, inComment = stripHTMLComments(text, inComment)
			if run, _ := fenceRun(text); run != "" {
				fence = run
			} else if level, heading := atxHeading(text); level == 1 || level == 2 {
				if inSection {
					return lines, true
				}
				if level == 2 && strings.EqualFold(heading, title) {
					inSection, found = true, true
					continue
				}
			}
		}
		if inSection {
			lines = append(lines, docLine{n: i + 1, text: text})
		}
	}
	return lines, found
}

// stripHTMLComments removes the parts of line that are in HTML comments. inComment says whether the line starts in
// a comment that an earlier line opened; the result says whether the next line does.
func stripHTMLComments(line string, inComment bool) (string, bool) {
	var b strings.Builder
	for {
		if inComment {
			end := strings.Index(line, "-->")
			if end < 0 {
				return b.String(), true
			}
			line = line[end+len("-->"):]
		}
		start := strings.Index(line, "<!--")
		if start < 0 {
			b.WriteString(line)
			return b.String(), false
		}
		b.WriteString(line[:start])
		line, inComment = line[start+len("<!--"):], true
	}
}

// fenceRun returns the run of three or more backticks or tildes that starts line (after at most three spaces), as a
// code fence does, and the rest of the line after it; run is "" when the line is no fence.
func fenceRun(line string) (run, after string) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 || t == "" || (t[0] != '`' && t[0] != '~') {
		return "", ""
	}
	n := 0
	for n < len(t) && t[n] == t[0] {
		n++
	}
	if n < 3 {
		return "", ""
	}
	return t[:n], t[n:]
}

// atxHeading returns the level (1–6) and text of an ATX heading line ("## Text", optionally closed by #s), or level 0
// when the line is no heading.
func atxHeading(line string) (level int, text string) {
	t := strings.TrimLeft(line, " ")
	if len(line)-len(t) > 3 {
		return 0, ""
	}
	for level < len(t) && t[level] == '#' {
		level++
	}
	if level == 0 || level > 6 {
		return 0, ""
	}
	rest := t[level:]
	if rest != "" && rest[0] != ' ' && rest[0] != '\t' {
		return 0, ""
	}
	rest = strings.TrimSpace(rest)
	// The closing sequence: #s after a space, or a heading of #s only.
	if open := strings.TrimRight(rest, "#"); open == "" || strings.HasSuffix(open, " ") || strings.HasSuffix(open, "\t") {
		rest = strings.TrimSpace(open)
	}
	return level, rest
}
