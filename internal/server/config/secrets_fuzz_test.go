package config

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// FuzzParseSecrets feeds arbitrary files to the secrets.json parser. It must never panic; an accepted file must
// encode to a file that parses back to the same keys, and that encoding must be stable.
func FuzzParseSecrets(f *testing.F) {
	d, err := newSecretsDoc("2026-09-29T10:00:00Z")
	if err != nil {
		f.Fatal(err)
	}
	valid, err := d.encode()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add(bytes.Replace(valid, []byte(`"resume"`), []byte(`"future"`), 1))
	f.Add([]byte(`{"format":1,"keys":{}}`))
	f.Add([]byte(`{"format":1,"keys":{"x":{"id":"1"}},"extra":[1,{"a":null}]}`))
	f.Add([]byte(`{"format":2}`))
	f.Add([]byte(`null`))
	f.Add([]byte(``))
	f.Fuzz(func(t *testing.T, data []byte) {
		d, err := parseSecrets(data)
		if err != nil {
			if err.Error() == "" {
				t.Fatal("empty error")
			}
			return
		}
		if _, err := d.addMissing("2026-09-29T10:00:00Z"); err != nil {
			t.Fatal(err)
		}
		out, err := d.encode()
		if err != nil {
			t.Fatalf("encode of an accepted file: %v\n%q", err, data)
		}
		back, err := parseSecrets(out)
		if err != nil {
			t.Fatalf("re-parse: %v\n%s", err, out)
		}
		for _, n := range registeredKeys {
			if !bytes.Equal(back.key[n], d.key[n]) || back.id[n] != d.id[n] {
				t.Fatalf("key %s changed in a round trip", n)
			}
		}
		if back.vapid != d.vapid {
			t.Fatal("VAPID changed in a round trip")
		}
		again, err := back.encode()
		if err != nil || !bytes.Equal(again, out) {
			t.Fatalf("unstable encoding:\n%s\n%s", out, again)
		}
	})
}

// FuzzParseMountPoints feeds arbitrary mount tables to the parser: no panic, and every mount point is absolute and
// clean.
func FuzzParseMountPoints(f *testing.F) {
	entries, err := os.ReadDir(filepath.Join("testdata", "mountinfo"))
	if err != nil {
		f.Fatal(err)
	}
	for _, e := range entries {
		data, err := os.ReadFile(filepath.Join("testdata", "mountinfo", e.Name()))
		if err != nil {
			f.Fatal(err)
		}
		f.Add(data)
	}
	f.Add([]byte("1 2 3:4 / /a\\040b\\ rw - x y z\n5 6 7:8 / /\\777\\0 rw\n"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, mp := range parseMountPoints(data) {
			if !strings.HasPrefix(mp, "/") || filepath.ToSlash(filepath.Clean(mp)) != mp {
				t.Fatalf("mount point %q is not absolute and clean", mp)
			}
		}
	})
}
