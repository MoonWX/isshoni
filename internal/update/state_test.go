package update

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// state.json (06 §8.7): versioned, at most 16 KiB, written atomically.

func testState(t *testing.T) *stateFile {
	t.Helper()
	return &stateFile{
		V: stateVersion,
		Pending: &pendingRec{Version: "0.2.1", File: "isshoni-desktop_0.2.1_windows_amd64_setup.exe", Size: 15204352,
			SHA256: testSHA256, ExpiresAt: wireTime{time.Date(2027, 6, 1, 10, 0, 0, 0, time.UTC)}, Attempts: 1,
			Required: true, NotesURL: ReleasesURL + "/tag/v0.2.1"},
		RevokedKeys: []string{newTestSigner(t, "isshoni-update-1").Fingerprint()},
		LastCheckAt: wireTime{time.Date(2026, 12, 5, 9, 12, 0, 0, time.UTC)},
	}
}

func TestStateRoundTrip(t *testing.T) {
	want := testState(t)
	data, err := marshalState(want)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{`"v":1`, `"expiresAt":"2027-06-01T10:00:00.000Z"`, `"attempts":1`,
		`"lastCheckAt":"2026-12-05T09:12:00.000Z"`, `"revokedKeys":["SHA256:`} {
		if !strings.Contains(string(data), field) {
			t.Errorf("state.json %s lacks %s", data, field)
		}
	}
	got, dropped, err := parseState(data)
	if err != nil || dropped != "" {
		t.Fatalf("parseState = %v, dropped %q", err, dropped)
	}
	if !got.Pending.ExpiresAt.Equal(want.Pending.ExpiresAt.Time) || !got.LastCheckAt.Equal(want.LastCheckAt.Time) {
		t.Errorf("times %v %v, want %v %v", got.Pending.ExpiresAt, got.LastCheckAt, want.Pending.ExpiresAt, want.LastCheckAt)
	}
	got.Pending.ExpiresAt, got.LastCheckAt = want.Pending.ExpiresAt, want.LastCheckAt
	if *got.Pending != *want.Pending || !slices.Equal(got.RevokedKeys, want.RevokedKeys) || got.V != stateVersion {
		t.Errorf("parsed %+v / %+v, want %+v / %+v", got, got.Pending, want, want.Pending)
	}

	// The empty state: no pending, an empty revocation list, no lastCheckAt.
	data, err = marshalState(emptyState())
	if err != nil || string(data) != `{"v":1,"pending":null,"revokedKeys":[]}`+"\n" {
		t.Errorf("the empty state = %s, %v", data, err)
	}
}

func TestParseState(t *testing.T) {
	fp := newTestSigner(t, "isshoni-update-1").Fingerprint()
	good := func(edit func(m map[string]any)) []byte {
		data, err := marshalState(testState(t))
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(data, &m); err != nil {
			t.Fatal(err)
		}
		edit(m)
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	pending := func(kv ...any) func(m map[string]any) {
		return func(m map[string]any) {
			p := m["pending"].(map[string]any)
			for i := 0; i < len(kv); i += 2 {
				p[kv[i].(string)] = kv[i+1]
			}
		}
	}
	for _, tt := range []struct {
		name        string
		data        []byte
		err         bool
		dropped     bool
		pending     bool
		revoked     []string
		lastCheckAt bool
	}{
		{name: "good", data: good(func(map[string]any) {}), pending: true, revoked: []string{fp}, lastCheckAt: true},
		{name: "unknown fields", data: good(func(m map[string]any) { m["later"] = 1 }), pending: true, revoked: []string{fp},
			lastCheckAt: true},
		{name: "schema 2 keeps the revocations", data: good(func(m map[string]any) { m["v"] = 2 }), dropped: true,
			revoked: []string{fp}},
		{name: "no v", data: good(func(m map[string]any) { delete(m, "v") }), dropped: true, revoked: []string{fp}},
		{name: "a path as file", data: good(pending("file", `..\isshoni.exe`)), dropped: true, revoked: []string{fp},
			lastCheckAt: true},
		{name: "state.json as file", data: good(pending("file", stateName)), dropped: true, revoked: []string{fp},
			lastCheckAt: true},
		{name: "a bad version", data: good(pending("version", "v0.2.1")), dropped: true, revoked: []string{fp},
			lastCheckAt: true},
		{name: "a bad sum", data: good(pending("sha256", "ABC")), dropped: true, revoked: []string{fp}, lastCheckAt: true},
		{name: "a size of 0", data: good(pending("size", 0)), dropped: true, revoked: []string{fp}, lastCheckAt: true},
		{name: "negative attempts", data: good(pending("attempts", -1)), dropped: true, revoked: []string{fp},
			lastCheckAt: true},
		{name: "no expiresAt", data: good(func(m map[string]any) { delete(m["pending"].(map[string]any), "expiresAt") }),
			dropped: true, revoked: []string{fp}, lastCheckAt: true},
		{name: "a bad fingerprint", data: good(func(m map[string]any) { m["revokedKeys"] = []any{"MD5:x", fp, fp, 7} }),
			dropped: true, pending: true, revoked: []string{fp}, lastCheckAt: true},
		{name: "a bad lastCheckAt", data: good(func(m map[string]any) { m["lastCheckAt"] = "yesterday" }), pending: true,
			revoked: []string{fp}},
		{name: "not JSON", data: []byte("{"), err: true},
		{name: "above 16 KiB", data: append([]byte(`{"v":1}`), make([]byte, maxStateSize)...), err: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			st, dropped, err := parseState(tt.data)
			if (err != nil) != tt.err {
				t.Fatalf("parseState = %v", err)
			}
			if err != nil {
				return
			}
			if (dropped != "") != tt.dropped || (st.Pending != nil) != tt.pending || !slices.Equal(st.RevokedKeys, tt.revoked) ||
				st.LastCheckAt.IsZero() == tt.lastCheckAt {
				t.Errorf("parseState = %+v, dropped %q", st, dropped)
			}
		})
	}
}

func TestMarshalStateLimit(t *testing.T) {
	st := emptyState()
	for range 400 {
		st.RevokedKeys = append(st.RevokedKeys, newTestSigner(t, "isshoni-update-1").Fingerprint())
	}
	if _, err := marshalState(st); err == nil {
		t.Error("a state above 16 KiB was written")
	}
	st.RevokedKeys = st.RevokedKeys[:MaxRevokedKeys]
	if _, err := marshalState(st); err != nil {
		t.Errorf("a state with %d revocations: %v", MaxRevokedKeys, err)
	}
}

// The atomic write: a temp file in the same folder, renamed over state.json; a failure leaves the old file whole and
// no temp file behind.
func TestWriteAtomic(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "updates")
	if err := writeAtomic(dir, stateName, []byte("old\n"), os.Rename); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		t.Fatalf("the folder: %v", err)
	}
	var renamed [2]string
	crash := errors.New("crash before the rename")
	err := writeAtomic(dir, stateName, []byte("new\n"), func(oldpath, newpath string) error {
		renamed = [2]string{oldpath, newpath}
		data, err := os.ReadFile(oldpath)
		if err != nil || string(data) != "new\n" {
			t.Errorf("the temp file holds %q, %v before the rename", data, err)
		}
		return crash
	})
	if !errors.Is(err, crash) {
		t.Fatalf("writeAtomic = %v", err)
	}
	if filepath.Dir(renamed[0]) != dir || renamed[1] != filepath.Join(dir, stateName) ||
		!strings.HasPrefix(filepath.Base(renamed[0]), stateName+".") {
		t.Errorf("renamed %q", renamed)
	}
	if data, err := os.ReadFile(filepath.Join(dir, stateName)); err != nil || string(data) != "old\n" {
		t.Errorf("state.json = %q, %v after a failed write, want the old content", data, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the folder holds %v, want state.json only", entries)
	}
	if err := writeAtomic(dir, stateName, []byte("new\n"), os.Rename); err != nil {
		t.Fatal(err)
	}
	if data, err := os.ReadFile(filepath.Join(dir, stateName)); err != nil || string(data) != "new\n" {
		t.Errorf("state.json = %q, %v", data, err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("the folder holds %v, want state.json only", entries)
	}
}

// The Updater's state: a broken state.json counts as empty and is replaced at the next write; one of another schema
// keeps its revocations.
func TestUpdaterState(t *testing.T) {
	for _, tt := range []struct {
		name    string
		content string
		revoked int
	}{
		{"broken", "{", 0},
		{"schema 2", `{"v":2,"revokedKeys":["` + newTestSigner(t, "isshoni-update-1").Fingerprint() + `"],"pending":{"x":1}}`, 1},
	} {
		t.Run(tt.name, func(t *testing.T) {
			data := t.TempDir()
			dir := filepath.Join(data, "updates")
			if err := os.MkdirAll(dir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, stateName), []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			logs := &logRecorder{}
			u, err := newUpdater(Options{CurrentVersion: "0.2.0", DataDir: data, Logger: logs.logger()},
				env{signers: func() ([]TrustedKey, error) { return keys(newTestSigner(t, "isshoni-update-1")), nil },
					ep: defaultEndpoints(), sys: system{policy: func() bool { return false }, rename: os.Rename}})
			if err != nil {
				t.Fatal(err)
			}
			if st := u.state(); st.Pending != nil || len(st.RevokedKeys) != tt.revoked {
				t.Errorf("state %+v", st)
			}
			if len(logs.lines()) != 1 {
				t.Errorf("logs %v, want one WARN", logs.lines())
			}
			if err := u.updateState(func(st *stateFile) { st.LastCheckAt = wireTime{testEpoch} }); err != nil {
				t.Fatal(err)
			}
			got, dropped, err := parseState(mustRead(t, filepath.Join(dir, stateName)))
			if err != nil || dropped != "" || got.V != stateVersion || len(got.RevokedKeys) != tt.revoked {
				t.Errorf("rewritten state %+v, %q, %v", got, dropped, err)
			}
		})
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestVerifyFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(path, []byte("abc"), 0o600); err != nil {
		t.Fatal(err)
	}
	const sum = "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad" // SHA-256("abc")
	if err := verifyFile(path, 3, sum); err != nil {
		t.Errorf("verifyFile: %v", err)
	}
	for _, tt := range []struct {
		size int64
		sum  string
	}{{2, sum}, {4, sum}, {3, strings.Repeat("0", 64)}} {
		if err := verifyFile(path, tt.size, tt.sum); err == nil {
			t.Errorf("verifyFile(%d, %s) succeeded", tt.size, tt.sum)
		}
	}
	if err := verifyFile(path+"x", 3, sum); err == nil {
		t.Error("verifyFile of a missing file succeeded")
	}
}
