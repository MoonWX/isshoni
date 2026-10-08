package config

import (
	"bytes"
	"context"
	"crypto/ecdh"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

// newSecrets opens a new secrets.json in a fresh directory.
func newSecrets(t *testing.T) (*SecretStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "secrets.json")
	s, err := OpenSecrets(path, nil)
	if err != nil {
		t.Fatalf("OpenSecrets: %v", err)
	}
	return s, path
}

func readBytes(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path) //nolint:gosec // the test's own temp dir
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// editSecrets decodes the file at path into generic JSON, lets edit change it and writes it back (0600).
func editSecrets(t *testing.T, path string, edit func(doc map[string]any)) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readBytes(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	edit(doc)
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func keysOf(doc map[string]any) map[string]any { return doc["keys"].(map[string]any) }

// snapshot is everything a SecretStore hands out.
type snapshot struct {
	keys  map[KeyName]string
	ids   map[KeyName]string
	vapid VAPIDKeys
}

func snap(s *SecretStore) snapshot {
	sn := snapshot{keys: map[KeyName]string{}, ids: map[KeyName]string{}, vapid: s.VAPID()}
	for _, n := range KeyNames() {
		sn.keys[n] = string(s.Key(n))
		sn.ids[n] = s.KeyID(n)
	}
	return sn
}

func (a snapshot) equal(b snapshot) bool {
	return fmt.Sprint(a.keys) == fmt.Sprint(b.keys) && fmt.Sprint(a.ids) == fmt.Sprint(b.ids) &&
		a.vapid.Public == b.vapid.Public && a.vapid.Private.Reveal() == b.vapid.Private.Reveal()
}

// dirNames lists the directory that holds path.
func dirNames(t *testing.T, path string) []string {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func assertValidVAPID(t *testing.T, v VAPIDKeys) {
	t.Helper()
	pub, err := base64.RawURLEncoding.DecodeString(v.Public)
	if err != nil || len(pub) != 65 || pub[0] != 4 {
		t.Fatalf("VAPID public key %q is not an uncompressed P-256 point in unpadded base64url", v.Public)
	}
	priv, err := base64.RawURLEncoding.DecodeString(v.Private.Reveal())
	if err != nil || len(priv) != 32 {
		t.Fatal("VAPID private key is not 32 bytes of unpadded base64url")
	}
	key, err := ecdh.P256().NewPrivateKey(priv)
	if err != nil || !bytes.Equal(key.PublicKey().Bytes(), pub) {
		t.Fatal("VAPID public key does not belong to the private key")
	}
}

// First start: a 0600 file with every registered key (id "1", 32 random bytes) and a VAPID pair, in the §5.2 format.
func TestSecretsCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "secrets.json")
	log, buf := captureLog()
	s, err := OpenSecrets(path, log)
	if err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o600)
	seen := map[string]bool{}
	for _, n := range []KeyName{KeySession, KeyInvite, KeyResume} {
		k := s.Key(n)
		if len(k) != 32 || bytes.Equal(k, make([]byte, 32)) {
			t.Errorf("Key(%s) = %x", n, k)
		}
		if seen[string(k)] {
			t.Errorf("Key(%s) repeats another key", n)
		}
		seen[string(k)] = true
		if id := s.KeyID(n); id != "1" {
			t.Errorf("KeyID(%s) = %q, want 1", n, id)
		}
	}
	assertValidVAPID(t, s.VAPID())

	data := readBytes(t, path)
	var doc struct {
		Format    int    `json:"format"`
		CreatedAt string `json:"created_at"`
		Keys      map[string]struct {
			ID, Key   string
			CreatedAt string `json:"created_at"`
		} `json:"keys"`
		VAPID map[string]string `json:"vapid"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Format != 1 || len(doc.Keys) != 3 {
		t.Errorf("format %d with %d keys", doc.Format, len(doc.Keys))
	}
	stamps := []string{doc.CreatedAt, doc.VAPID["created_at"]}
	for n, e := range doc.Keys {
		stamps = append(stamps, e.CreatedAt)
		if k, _ := base64.RawURLEncoding.DecodeString(e.Key); !bytes.Equal(k, s.Key(KeyName(n))) {
			t.Errorf("keys.%s.key in the file differs from Key", n)
		}
	}
	for _, ts := range stamps {
		if tm, err := time.Parse(time.RFC3339, ts); err != nil || !strings.HasSuffix(ts, "Z") || tm.Nanosecond() != 0 {
			t.Errorf("created_at %q is not RFC 3339 UTC in whole seconds", ts)
		}
	}
	if doc.VAPID["public_key"] != s.VAPID().Public || doc.VAPID["private_key"] != s.VAPID().Private.Reveal() {
		t.Error("vapid in the file differs from VAPID()")
	}
	// The order of §5.2, keys in file order, two-space indent.
	order := []string{`"format": 1`, `"created_at"`, `"keys"`, `"session"`, `"invite"`, `"resume"`, `"vapid"`, `"public_key"`}
	last := -1
	for _, s := range order {
		i := bytes.Index(data, []byte(s))
		if i <= last {
			t.Errorf("%s is out of order in:\n%s", s, data)
		}
		last = i
	}
	if !bytes.HasPrefix(data, []byte("{\n  \"format\": 1,\n")) || !bytes.HasSuffix(data, []byte("}\n")) {
		t.Errorf("unexpected layout:\n%s", data)
	}
	if names := dirNames(t, path); !slices.Equal(names, []string{"secrets.json"}) {
		t.Errorf("directory holds %v", names)
	}
	if !strings.Contains(buf.String(), "created secrets.json") {
		t.Errorf("no log line:\n%s", buf.String())
	}
	assertNoKeyMaterial(t, buf.String(), s)
}

// assertNoKeyMaterial fails if text contains any key of s, raw or encoded.
func assertNoKeyMaterial(t *testing.T, text string, s *SecretStore) {
	t.Helper()
	for _, n := range KeyNames() {
		k := s.Key(n)
		for _, enc := range []string{base64.RawURLEncoding.EncodeToString(k), base64.StdEncoding.EncodeToString(k), fmt.Sprintf("%x", k)} {
			if strings.Contains(text, enc) {
				t.Errorf("key %s appears in %q", n, text)
			}
		}
	}
	if strings.Contains(text, s.VAPID().Private.Reveal()) {
		t.Errorf("the VAPID private key appears in %q", text)
	}
}

// A restart keeps the keys and does not rewrite the file.
func TestSecretsReopen(t *testing.T) {
	s1, path := newSecrets(t)
	before := readBytes(t, path)
	log, buf := captureLog()
	s2, err := OpenSecrets(path, log)
	if err != nil {
		t.Fatal(err)
	}
	if !snap(s1).equal(snap(s2)) {
		t.Error("keys changed across a restart")
	}
	if !bytes.Equal(before, readBytes(t, path)) {
		t.Error("the file was rewritten")
	}
	if buf.Len() != 0 {
		t.Errorf("unexpected log:\n%s", buf.String())
	}
}

// A registered key or the VAPID pair missing from the file is generated and saved; unknown names and fields are kept.
func TestSecretsAddsMissing(t *testing.T) {
	s1, path := newSecrets(t)
	editSecrets(t, path, func(doc map[string]any) {
		delete(keysOf(doc), "resume")
		delete(doc, "vapid")
		keysOf(doc)["future"] = map[string]any{"id": "k7", "key": "not checked", "algo": "x"}
		doc["future_field"] = []any{1.0, "two"}
	})
	log, buf := captureLog()
	s2, err := OpenSecrets(path, log)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []KeyName{KeySession, KeyInvite} {
		if !bytes.Equal(s1.Key(n), s2.Key(n)) || s2.KeyID(n) != "1" {
			t.Errorf("%s changed", n)
		}
	}
	if bytes.Equal(s1.Key(KeyResume), s2.Key(KeyResume)) || len(s2.Key(KeyResume)) != 32 || s2.KeyID(KeyResume) != "1" {
		t.Error("resume was not regenerated")
	}
	assertValidVAPID(t, s2.VAPID())
	if s2.VAPID().Public == s1.VAPID().Public {
		t.Error("the VAPID pair was not regenerated")
	}
	if !strings.Contains(buf.String(), "added new keys") || !strings.Contains(buf.String(), "resume vapid") {
		t.Errorf("no log line:\n%s", buf.String())
	}
	s3, err := OpenSecrets(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !snap(s2).equal(snap(s3)) {
		t.Error("the added keys were not saved")
	}
	assertUnknownKept(t, path)
}

// assertUnknownKept checks that the unknown entries of TestSecretsAddsMissing are in the file unchanged.
func assertUnknownKept(t *testing.T, path string) {
	t.Helper()
	var doc map[string]any
	if err := json.Unmarshal(readBytes(t, path), &doc); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprint(keysOf(doc)["future"]); got != "map[algo:x id:k7 key:not checked]" {
		t.Errorf("keys.future = %s", got)
	}
	if got := fmt.Sprint(doc["future_field"]); got != "[1 two]" {
		t.Errorf("future_field = %s", got)
	}
}

func TestSecretsPaddedBase64(t *testing.T) {
	s1, path := newSecrets(t)
	editSecrets(t, path, func(doc map[string]any) {
		e := keysOf(doc)["session"].(map[string]any)
		e["key"] = base64.URLEncoding.EncodeToString(s1.Key(KeySession))
	})
	s2, err := OpenSecrets(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(s2.Key(KeySession), s1.Key(KeySession)) {
		t.Error("padded key decoded differently")
	}
}

func TestSecretsWideModeFixed(t *testing.T) {
	skipOnWindows(t, "Unix modes")
	for _, mode := range []os.FileMode{0o644, 0o640, 0o700, 0o606} {
		_, path := newSecrets(t)
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		log, buf := captureLog()
		if _, err := OpenSecrets(path, log); err != nil {
			t.Fatalf("%#o: %v", mode, err)
		}
		assertMode(t, path, 0o600)
		if !strings.Contains(buf.String(), "changed its mode to 0600") || !strings.Contains(buf.String(), fmt.Sprintf("was=%#o", mode)) {
			t.Errorf("%#o: no warning:\n%s", mode, buf.String())
		}
	}
	// Narrower is left alone.
	_, path := newSecrets(t)
	if err := os.Chmod(path, 0o400); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenSecrets(path, nil); err != nil {
		t.Fatal(err)
	}
	assertMode(t, path, 0o400)
}

// A secrets.json owned by another uid is a refusal (exit 78) with a chown fix; the file is not touched.
func TestSecretsWrongOwner(t *testing.T) {
	skipOnWindows(t, "Unix owners")
	_, path := newSecrets(t)
	if err := os.Chmod(path, 0o644); err != nil { //nolint:gosec // a wide mode is not fixed for a foreign file
		t.Fatal(err)
	}
	before := readBytes(t, path)
	defer func(f func() int) { geteuid = f }(geteuid)
	real := os.Geteuid()
	geteuid = func() int { return real + 1 }

	_, err := OpenSecrets(path, nil)
	if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonSecretsOwner {
		t.Fatalf("OpenSecrets = %v, want %s", err, ReasonSecretsOwner)
	}
	for _, s := range []string{
		fmt.Sprintf("%s is owned by uid %d, but isshoni runs as uid %d", path, real, real+1),
		// OpenSecrets looks at the real machine, which may be a container (TestOwnerFix covers both forms).
		"fix: " + ownerFix(Host{}, path, false), "sudo chown ",
	} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q:\n%v", s, err)
		}
	}
	if !bytes.Equal(before, readBytes(t, path)) {
		t.Error("the file changed")
	}
	assertMode(t, path, 0o644)

	// `sudo isshoni serve` on a systemd install: the fix is to run as the file's owner, not to give the file to root
	// (the next `systemctl start isshoni` would fail the same way).
	t.Run("root outside a container", func(t *testing.T) {
		if real == 0 {
			t.Skip("the test's file must belong to a user other than root")
		}
		if (Host{}).Container() != ContainerNone {
			t.Skip("OpenSecrets looks at the real machine, which is a container here")
		}
		geteuid = func() int { return 0 }
		_, err := OpenSecrets(path, nil)
		if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonSecretsOwner {
			t.Fatalf("OpenSecrets = %v, want %s", err, ReasonSecretsOwner)
		}
		msg := err.Error()
		for _, s := range []string{"fix: run isshoni as the file's owner ", "sudo systemctl start isshoni", "isshoni serve)"} {
			if !strings.Contains(msg, s) {
				t.Errorf("error lacks %q:\n%s", s, msg)
			}
		}
		if strings.Contains(msg, "chown") {
			t.Errorf("the fix gives the file to root:\n%s", msg)
		}
		if !bytes.Equal(before, readBytes(t, path)) {
			t.Error("the file changed")
		}
	})
}

func TestSecretsOwnerFix(t *testing.T) {
	defer func(e, u, g func() int) { geteuid, getuid, getgid = e, u, g }(geteuid, getuid, getgid)
	geteuid = func() int { return 0 }
	getuid = func() int { return 0 }
	getgid = func() int { return 0 }
	const path = "/var/lib/isshoni/secrets.json"
	docker := fakeHost(t)
	writeHostFile(t, docker, "/.dockerenv", "")

	// Root outside a container, a file of an account without a name: sudo -u takes the uid as '#uid'.
	if got, want := secretsOwnerFix(fakeHost(t), path, 3999999),
		"run isshoni as the file's owner 3999999: sudo systemctl start isshoni (or sudo -u '#3999999' isshoni serve)"; got != want {
		t.Errorf("root fix = %q, want %q", got, want)
	}
	// Root in a container (compose.host.yaml runs as 0:0): give the file to the process.
	if got, want := secretsOwnerFix(docker, path, 65532), ownerFix(docker, path, false); got != want {
		t.Errorf("container fix = %q, want %q", got, want)
	}
	// Not root: give the file to the process, whoever owns it.
	geteuid = func() int { return 998 }
	getuid = func() int { return 998 }
	getgid = func() int { return 998 }
	for _, owner := range []int{0, 1000} {
		if got, want := secretsOwnerFix(fakeHost(t), path, owner), ownerFix(fakeHost(t), path, false); got != want ||
			!strings.Contains(got, "sudo chown ") {
			t.Errorf("owner %d: fix = %q, want %q", owner, got, want)
		}
	}
}

// validSecrets returns the content of a valid secrets.json as generic JSON.
func validSecrets(t *testing.T) map[string]any {
	t.Helper()
	d, err := newSecretsDoc(secretsNow())
	if err != nil {
		t.Fatal(err)
	}
	data, err := d.encode()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func corruptCases(t *testing.T) map[string]string {
	t.Helper()
	with := func(edit func(doc map[string]any)) string {
		doc := validSecrets(t)
		edit(doc)
		data, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	other := validSecrets(t)["vapid"].(map[string]any)
	return map[string]string{
		"empty":             "",
		"truncated":         with(func(map[string]any) {})[:40],
		"null":              "null",
		"array":             "[]",
		"no format":         with(func(doc map[string]any) { delete(doc, "format") }),
		"format string":     with(func(doc map[string]any) { doc["format"] = "1" }),
		"format 2":          with(func(doc map[string]any) { doc["format"] = 2 }),
		"format 0":          with(func(doc map[string]any) { doc["format"] = 0 }),
		"no keys":           with(func(doc map[string]any) { delete(doc, "keys") }),
		"keys array":        with(func(doc map[string]any) { doc["keys"] = []any{} }),
		"keys null":         with(func(doc map[string]any) { doc["keys"] = nil }),
		"entry string":      with(func(doc map[string]any) { keysOf(doc)["session"] = "abc" }),
		"entry null":        with(func(doc map[string]any) { keysOf(doc)["invite"] = nil }),
		"no id":             with(func(doc map[string]any) { delete(keysOf(doc)["resume"].(map[string]any), "id") }),
		"short key":         with(func(doc map[string]any) { keysOf(doc)["session"].(map[string]any)["key"] = "AAAAAAAAAAAAAAAAAAAAAA" }),
		"key not base64url": with(func(doc map[string]any) { keysOf(doc)["session"].(map[string]any)["key"] = strings.Repeat("+/", 22) }),
		"vapid string":      with(func(doc map[string]any) { doc["vapid"] = "x" }),
		"vapid short":       with(func(doc map[string]any) { doc["vapid"].(map[string]any)["public_key"] = "BAAA" }),
		"vapid mismatch": with(func(doc map[string]any) {
			doc["vapid"].(map[string]any)["public_key"] = other["public_key"]
		}),
		"vapid zero scalar": with(func(doc map[string]any) {
			doc["vapid"].(map[string]any)["private_key"] = base64.RawURLEncoding.EncodeToString(make([]byte, 32))
		}),
	}
}

// A corrupt secrets.json is a refusal (exit 78, secrets_corrupt) with the restore fix; it is never regenerated.
func TestSecretsCorrupt(t *testing.T) {
	for name, content := range corruptCases(t) {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "secrets.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := OpenSecrets(path, nil)
			assertCorrupt(t, err, path)
			if got := string(readBytes(t, path)); got != content {
				t.Error("the file changed")
			}
			if names := dirNames(t, path); !slices.Equal(names, []string{"secrets.json"}) {
				t.Errorf("directory holds %v", names)
			}
			assertCorrupt(t, InspectSecrets(path), path)
		})
	}
	t.Run("directory", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		_, err := OpenSecrets(path, nil)
		assertCorrupt(t, err, path)
		assertCorrupt(t, InspectSecrets(path), path)
	})
	t.Run("newer format message", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "secrets.json")
		if err := os.WriteFile(path, []byte(corruptCases(t)["format 2"]), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := OpenSecrets(path, nil); err == nil || !strings.Contains(err.Error(), "written by a newer isshoni (format 2") {
			t.Errorf("OpenSecrets = %v", err)
		}
	})
}

func assertCorrupt(t *testing.T, err error, path string) {
	t.Helper()
	if !errors.Is(err, ErrNeedsOperator) || ReasonOf(err) != ReasonSecretsCorrupt {
		t.Fatalf("error = %v, want %s", err, ReasonSecretsCorrupt)
	}
	for _, s := range []string{path + " ", "never replaces it", "fix: restore it: isshoni admin restore --offline <backup>"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("error lacks %q:\n%v", s, err)
		}
	}
}

func TestInspectSecrets(t *testing.T) {
	_, path := newSecrets(t)
	if err := InspectSecrets(path); err != nil {
		t.Errorf("valid file: %v", err)
	}
	editSecrets(t, path, func(doc map[string]any) { delete(keysOf(doc), "invite"); delete(doc, "vapid") })
	before := readBytes(t, path)
	if err := InspectSecrets(path); err != nil {
		t.Errorf("file with missing keys: %v", err)
	}
	if !bytes.Equal(before, readBytes(t, path)) {
		t.Error("InspectSecrets changed the file")
	}
	err := InspectSecrets(filepath.Join(t.TempDir(), "secrets.json"))
	if !errors.Is(err, fs.ErrNotExist) || errors.Is(err, ErrNeedsOperator) {
		t.Errorf("missing file: %v", err)
	}
}

// Rotate changes exactly the chosen keys (each gets the next id), writes them, and keeps everything else.
func TestSecretsRotate(t *testing.T) {
	_, path := newSecrets(t)
	editSecrets(t, path, func(doc map[string]any) {
		keysOf(doc)["future"] = map[string]any{"id": "k7", "key": "not checked", "algo": "x"}
		doc["future_field"] = []any{1.0, "two"}
	})
	s, err := OpenSecrets(path, nil)
	if err != nil {
		t.Fatal(err)
	}
	steps := []struct {
		names       []KeyName
		vapid       bool
		wantRotated []KeyName
	}{
		{[]KeyName{KeySession}, false, []KeyName{KeySession}},
		{[]KeyName{KeyResume, KeyInvite, KeyResume}, true, []KeyName{KeyInvite, KeyResume}},
		{nil, true, []KeyName{}},
		{KeyNames(), true, KeyNames()},
	}
	for i, st := range steps {
		before := snap(s)
		log, buf := captureLog()
		s.log = componentLogger(log)
		res, err := s.Rotate(t.Context(), st.names, st.vapid)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if !slices.Equal(res.Rotated, st.wantRotated) || res.Rotated == nil || res.VAPID != st.vapid {
			t.Errorf("step %d: result %+v, want %v and vapid %v", i, res, st.wantRotated, st.vapid)
		}
		after := snap(s)
		for _, n := range KeyNames() {
			rotated := slices.Contains(st.wantRotated, n)
			if changed := before.keys[n] != after.keys[n]; changed != rotated {
				t.Errorf("step %d: %s changed = %v, want %v", i, n, changed, rotated)
			}
			wantID := before.ids[n]
			if rotated {
				wantID = nextKeyID(wantID)
			}
			if after.ids[n] != wantID || len(after.keys[n]) != 32 {
				t.Errorf("step %d: KeyID(%s) = %q, want %q", i, n, after.ids[n], wantID)
			}
		}
		if changed := before.vapid.Public != after.vapid.Public; changed != st.vapid {
			t.Errorf("step %d: VAPID changed = %v, want %v", i, changed, st.vapid)
		}
		assertValidVAPID(t, after.vapid)
		reopened, err := OpenSecrets(path, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !after.equal(snap(reopened)) {
			t.Errorf("step %d: the file differs from memory", i)
		}
		assertMode(t, path, 0o600)
		if !strings.Contains(buf.String(), "rotated secrets") {
			t.Errorf("step %d: no log line:\n%s", i, buf.String())
		}
		assertNoKeyMaterial(t, buf.String(), s)
	}
	if got := s.KeyID(KeySession); got != "3" {
		t.Errorf("KeyID(session) after two rotations = %q", got)
	}
	assertUnknownKept(t, path)
	if names := dirNames(t, path); !slices.Equal(names, []string{"secrets.json"}) {
		t.Errorf("directory holds %v", names)
	}
}

// Rotate refuses unknown names and a cancelled context, and does nothing for an empty request; the file stays.
func TestSecretsRotateNoChange(t *testing.T) {
	s, path := newSecrets(t)
	before, file := snap(s), readBytes(t, path)
	check := func(what string) {
		t.Helper()
		if !before.equal(snap(s)) || !bytes.Equal(file, readBytes(t, path)) {
			t.Errorf("%s changed the keys", what)
		}
	}
	if _, err := s.Rotate(t.Context(), []KeyName{KeySession, "bogus"}, true); !errors.Is(err, ErrUnknownKey) ||
		!strings.Contains(err.Error(), `"bogus"`) {
		t.Errorf("unknown name: %v", err)
	}
	check("an unknown name")
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.Rotate(ctx, KeyNames(), true); !errors.Is(err, context.Canceled) {
		t.Errorf("cancelled: %v", err)
	}
	check("a cancelled context")
	res, err := s.Rotate(t.Context(), nil, false)
	if err != nil || res.Rotated == nil || len(res.Rotated) != 0 || res.VAPID {
		t.Errorf("empty request: %+v, %v", res, err)
	}
	check("an empty request")
}

// The atomic write leaves the old file, and the keys in memory, on a failure before the rename; no temp file stays.
func TestSecretsAtomicWriteFailure(t *testing.T) {
	boom := errors.New("simulated failure")
	defer func(s func(*os.File) error, r func(string, string) error) { syncFile, renameFile = s, r }(syncFile, renameFile)
	for _, fail := range []string{"sync", "rename"} {
		t.Run(fail, func(t *testing.T) {
			syncFile, renameFile = (*os.File).Sync, os.Rename
			s, path := newSecrets(t)
			before, file := snap(s), readBytes(t, path)
			if fail == "sync" {
				syncFile = func(*os.File) error { return boom }
			} else {
				renameFile = func(string, string) error { return boom }
			}
			if _, err := s.Rotate(t.Context(), KeyNames(), true); !errors.Is(err, boom) {
				t.Fatalf("Rotate = %v", err)
			}
			if !bytes.Equal(file, readBytes(t, path)) {
				t.Error("the file changed")
			}
			if !before.equal(snap(s)) {
				t.Error("the keys in memory changed")
			}
			if names := dirNames(t, path); !slices.Equal(names, []string{"secrets.json"}) {
				t.Errorf("directory holds %v", names)
			}

			// The first start fails the same way and leaves nothing behind.
			fresh := filepath.Join(t.TempDir(), "secrets.json")
			if _, err := OpenSecrets(fresh, nil); !errors.Is(err, boom) {
				t.Fatalf("OpenSecrets = %v", err)
			}
			if names := dirNames(t, fresh); len(names) != 0 {
				t.Errorf("directory holds %v", names)
			}
		})
	}
}

func TestSecretsAccessors(t *testing.T) {
	s, _ := newSecrets(t)
	k := s.Key(KeySession)
	k[0] ^= 0xff
	if bytes.Equal(k, s.Key(KeySession)) {
		t.Error("Key returns the store's own slice")
	}
	for name, f := range map[string]func(){
		"Key":            func() { s.Key("bogus") },
		"KeyID":          func() { s.KeyID("bogus") },
		"zero Key":       func() { (&SecretStore{}).Key(KeySession) },
		"zero VAPID":     func() { (&SecretStore{}).VAPID() },
		"zero KeyID":     func() { (&SecretStore{}).KeyID(KeySession) },
		"zero Rotate(…)": func() { _, _ = (&SecretStore{}).Rotate(context.Background(), KeyNames(), false) },
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s did not panic", name)
				}
			}()
			f()
		}()
	}
	v := s.VAPID()
	for _, out := range []string{fmt.Sprint(v), fmt.Sprintf("%+v", v), fmt.Sprintf("%#v", v)} {
		if strings.Contains(out, v.Private.Reveal()) {
			t.Errorf("the VAPID private key is printed: %s", out)
		}
	}
	if names := KeyNames(); !slices.Equal(names, []KeyName{KeySession, KeyInvite, KeyResume}) {
		t.Errorf("KeyNames() = %v", names)
	}
	KeyNames()[0] = "changed"
	if KeyNames()[0] != KeySession {
		t.Error("KeyNames returns the registry itself")
	}
}

func TestNextKeyID(t *testing.T) {
	for in, want := range map[string]string{"1": "2", "9": "10", "41": "42", "": "1", "0": "1", "-3": "1", "k7": "1"} {
		if got := nextKeyID(in); got != want {
			t.Errorf("nextKeyID(%q) = %q, want %q", in, got, want)
		}
	}
}

// Readers never see a torn state while rotations run (go test -race).
func TestSecretsConcurrent(t *testing.T) {
	s, _ := newSecrets(t)
	var wg sync.WaitGroup
	stop := make(chan struct{})
	for range 4 {
		wg.Go(func() {
			for {
				select {
				case <-stop:
					return
				default:
				}
				for _, n := range KeyNames() {
					if len(s.Key(n)) != 32 || s.KeyID(n) == "" {
						t.Error("torn read")
						return
					}
				}
				_ = s.VAPID()
			}
		})
	}
	for range 5 {
		if _, err := s.Rotate(t.Context(), KeyNames(), true); err != nil {
			t.Error(err)
		}
	}
	close(stop)
	wg.Wait()
	if got := s.KeyID(KeyResume); got != "6" {
		t.Errorf("KeyID after 5 rotations = %q", got)
	}
}
