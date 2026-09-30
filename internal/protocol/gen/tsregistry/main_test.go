package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/protocol"
)

func TestRunWritesFile(t *testing.T) {
	want, err := Generate(protocol.Registry)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "registry.gen.ts")
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-o", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, &stderr)
	}
	if stdout.Len() != 0 || stderr.Len() != 0 {
		t.Errorf("unexpected output: stdout %q, stderr %q", &stdout, &stderr)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Error("the file differs from Generate's output")
	}

	// A current file is left alone (its mtime stays), a stale one is rewritten.
	old := time.Now().Add(-time.Hour).Truncate(time.Second)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-o", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("second run: exit %d, stderr: %s", code, &stderr)
	}
	if fi, err := os.Stat(path); err != nil || !fi.ModTime().Equal(old) {
		t.Errorf("an unchanged file was rewritten (stat %v, err %v)", fi.ModTime(), err)
	}
	if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
		t.Fatal(err)
	}
	if code := run([]string{"-o", path}, &stdout, &stderr); code != 0 {
		t.Fatalf("third run: exit %d, stderr: %s", code, &stderr)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, want) {
		t.Error("a stale file was not rewritten")
	}
}

func TestRunStdout(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run(nil, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d, stderr: %s", code, &stderr)
	}
	if want, _ := Generate(protocol.Registry); !bytes.Equal(stdout.Bytes(), want) {
		t.Error("stdout differs from Generate's output")
	}
}

func TestRunErrors(t *testing.T) {
	dir := t.TempDir()
	for _, c := range []struct {
		name   string
		args   []string
		code   int
		stderr string
	}{
		{"unknown flag", []string{"-x"}, 2, "usage: tsregistry"},
		{"extra argument", []string{"-o", filepath.Join(dir, "a.ts"), "b.ts"}, 2, "usage: tsregistry"},
		{"help", []string{"-h"}, 0, "usage: tsregistry"},
		{"missing directory", []string{"-o", filepath.Join(dir, "no", "such", "dir.ts")}, 1, "tsregistry: write "},
		{"output is a directory", []string{"-o", dir}, 1, "tsregistry: read "},
	} {
		t.Run(c.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			if code := run(c.args, &stdout, &stderr); code != c.code {
				t.Errorf("exit %d, want %d (stderr %q)", code, c.code, &stderr)
			}
			if !strings.Contains(stderr.String(), c.stderr) {
				t.Errorf("stderr %q lacks %q", &stderr, c.stderr)
			}
			if stdout.Len() != 0 {
				t.Errorf("stdout %q, want nothing", &stdout)
			}
		})
	}
}
