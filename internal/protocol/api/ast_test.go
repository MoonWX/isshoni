package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// The helpers in this file read Go source with go/parser, so tests can check what reflection can't see: the
// package's declared constants, struct types and imports.

// parseSources parses the non-test .go files of dir, in file-name order. It returns nil when dir has none.
func parseSources(t *testing.T, dir string) []*ast.File {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("read %s: %v", dir, err)
	}
	fset := token.NewFileSet()
	var files []*ast.File
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files = append(files, f)
	}
	return files
}

// stringConst is one constant declared with an explicit string literal value.
type stringConst struct {
	Name  string
	Type  string // the declared type's name, "" when untyped
	Value string
}

// stringConsts lists the string constants of files in declaration order.
func stringConsts(t *testing.T, files []*ast.File) []stringConst {
	t.Helper()
	var out []stringConst
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			for _, s := range gd.Specs {
				vs := s.(*ast.ValueSpec)
				if len(vs.Values) != len(vs.Names) {
					continue
				}
				typ := ""
				if id, ok := vs.Type.(*ast.Ident); ok {
					typ = id.Name
				}
				for i, n := range vs.Names {
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatalf("const %s: %v", n.Name, err)
					}
					out = append(out, stringConst{Name: n.Name, Type: typ, Value: v})
				}
			}
		}
	}
	return out
}

// exportedStructs lists the names of the exported struct types declared in files, sorted.
func exportedStructs(files []*ast.File) []string {
	var out []string
	for _, f := range files {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.TYPE {
				continue
			}
			for _, s := range gd.Specs {
				ts := s.(*ast.TypeSpec)
				if _, ok := ts.Type.(*ast.StructType); ok && ts.Name.IsExported() {
					out = append(out, ts.Name.Name)
				}
			}
		}
	}
	slices.Sort(out)
	return out
}

// importPaths lists the import paths of files, sorted and without duplicates.
func importPaths(t *testing.T, files []*ast.File) []string {
	t.Helper()
	var out []string
	for _, f := range files {
		for _, imp := range f.Imports {
			p, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatalf("import %s: %v", imp.Path.Value, err)
			}
			out = append(out, p)
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}
