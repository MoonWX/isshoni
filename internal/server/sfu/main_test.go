package sfu

import (
	"flag"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// update rewrites the golden files in testdata/golden: go test ./internal/server/sfu -run Golden -update
var update = flag.Bool("update", false, "rewrite golden files")

// TestMain fails the run when goroutines outlive the tests. Pion's goroutines get 2 s to settle after Close
// (02 §17); goleak's own retries stop after about 0.5 s.
func TestMain(m *testing.M) {
	code := m.Run()
	if code == 0 {
		err := goleak.Find()
		for deadline := time.Now().Add(2 * time.Second); err != nil && time.Now().Before(deadline); {
			time.Sleep(100 * time.Millisecond)
			err = goleak.Find()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "goleak: %v\n", err)
			code = 1
		}
	}
	os.Exit(code)
}

// readSDP reads an SDP fixture from testdata and returns it with CRLF line endings, whatever the file uses.
func readSDP(t testing.TB, name string) string {
	t.Helper()
	b, err := fs.ReadFile(os.DirFS("testdata"), name)
	if err != nil {
		t.Fatal(err)
	}
	return crlf(string(b))
}

// crlf normalizes line endings to CRLF, as SDP wants.
func crlf(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n")
}

// golden compares got with testdata/golden/name, or rewrites the file with -update.
func golden(t *testing.T, name, got string) {
	t.Helper()
	if *update {
		if err := os.WriteFile(filepath.Join("testdata", "golden", name), []byte(got), 0o600); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := fs.ReadFile(os.DirFS("testdata"), "golden/"+name)
	if err != nil {
		t.Fatalf("%v (run with -update to create it)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from the golden file (run with -update after checking the change):\n--- got\n%s\n--- want\n%s",
			name, got, want)
	}
}
