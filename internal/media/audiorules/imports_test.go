package audiorules

import (
	"go/parser"
	"go/token"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// thisPackage is the one non-standard import allowed, in the external example tests only.
const thisPackage = "github.com/MoonWX/isshoni/internal/media/audiorules"

// Acceptance: standard library only (01 §4's import table: internal/media/audiorules imports nothing of this
// repository and no third-party module). depguard enforces the repository half; this test covers both halves,
// test files included, on every OS the tests run on.
func TestStandardLibraryOnly(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Fatal("no Go files found in the package directory")
	}
	fset := token.NewFileSet()
	for _, name := range files {
		f, err := parser.ParseFile(fset, name, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, imp := range f.Imports {
			path, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if path == thisPackage && f.Name.Name == "audiorules_test" {
				continue
			}
			// Standard library paths have no dot in their first element (cmd/go's rule for "std").
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %q: this package imports only the standard library", name, path)
			}
		}
	}
}
