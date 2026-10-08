package protocol

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// parsePackage parses the package's non-test Go files.
func parsePackage(t *testing.T) []*ast.File {
	t.Helper()
	return parseDir(t, ".")
}

// parseDir parses the non-test Go files of dir.
func parseDir(t *testing.T, dir string) []*ast.File {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(dir, "*.go"))
	if err != nil {
		t.Fatal(err)
	}
	var files []*ast.File
	fset := token.NewFileSet()
	for _, p := range paths {
		if strings.HasSuffix(p, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, p, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, f)
	}
	return files
}

// TestStdlibOnly: internal/protocol imports only the standard library (01 D12, §15.1), so the Go clients and the TS
// generator can use it.
func TestStdlibOnly(t *testing.T) {
	for _, f := range parsePackage(t) {
		for _, imp := range f.Imports {
			path, _ := strconv.Unquote(imp.Path.Value)
			if first, _, _ := strings.Cut(path, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s, which is not in the standard library", f.Name.Name, path)
			}
		}
	}
}

// TestNoAccidentalTSUnions: tygo v0.2.21 (enum_style union) turns a const group of 2 or more exported constants
// into a TS union type when it finds no explicit type prefix, naming it after the longest capitalized prefix the
// names share (detectEnumGroup, findCommonPrefix). Untyped groups must therefore not share a prefix; RIDHigh/RIDLow
// (type RID = "f" | "q") is the one accepted case.
func TestNoAccidentalTSUnions(t *testing.T) {
	allowed := map[string]bool{"RID": true}
	for _, f := range parsePackage(t) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST || len(gd.Specs) < 2 {
				continue
			}
			var names []string
			typed := false
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				if vs.Type != nil {
					typed = true
				}
				if vs.Names[0].IsExported() {
					names = append(names, vs.Names[0].Name)
				}
			}
			if typed || len(names) < 2 {
				continue // typed groups are checked by TestEnumConstants
			}
			prefix := ""
			for n := len(names[0]); n > 0 && prefix == ""; n-- {
				if c := names[0][0]; c < 'A' || c > 'Z' {
					break
				}
				if !slices.ContainsFunc(names[1:], func(s string) bool { return !strings.HasPrefix(s, names[0][:n]) }) {
					prefix = names[0][:n]
				}
			}
			if prefix != "" && !allowed[prefix] {
				t.Errorf("const group %v: tygo would emit a TS union type %q; use single-line consts", names, prefix)
			}
		}
	}
}

// TestEmbeddedStructsExtend: an embedded struct is inlined in JSON, so tygo must emit "extends" for it, which needs
// the tag tstype:",extends" (without it tygo v0.2.21 emits a nested property).
func TestEmbeddedStructsExtend(t *testing.T) {
	for _, typ := range reachableStructs() {
		for i := range typ.NumField() {
			sf := typ.Field(i)
			if sf.Anonymous && sf.Tag.Get("json") == "" && sf.Tag.Get("tstype") != ",extends" {
				t.Errorf("%v embeds %v without the tag tstype:\",extends\"", typ, sf.Type)
			}
		}
	}
}

// holdsAny reports whether a field's type expression has a Go any (or interface{}) in it: any, map[string]any,
// []any, ... It does not look into a nested struct type, whose fields are fields of their own.
func holdsAny(expr ast.Expr) bool {
	found := false
	ast.Inspect(expr, func(n ast.Node) bool {
		switch x := n.(type) {
		case *ast.StructType:
			return false
		case *ast.Ident:
			found = found || x.Name == "any"
		case *ast.InterfaceType:
			found = found || x.Methods == nil || len(x.Methods.List) == 0
		}
		return !found
	})
	return found
}

// TestNoTSAny: tygo v0.2.21 turns a Go any into a TypeScript any, which switches type checking off for everything
// the value touches. So every struct field whose type has an any in it carries a tstype tag that names a checked
// type instead: unknown (the Go-only registry Spec), or an index signature of unknown values (Params). tygo reads
// the tag's value up to its first comma as the type and the rest as options, so the type itself has no comma
// (Record<string, unknown> would be cut at it).
//
// The rule holds for both packages tygo reads (01 §14.4), so the test also parses the sources of the REST DTOs in
// internal/protocol/api, where later slices add most types.
func TestNoTSAny(t *testing.T) {
	// The fields known today, as a floor: a check that finds fewer no longer sees them.
	for _, pkg := range []struct {
		dir   string
		floor int
	}{
		{".", 3},   // Spec.Payload, Spec.Result, Error.Params
		{"api", 4}, // the Params of Error, DoctorCheck and Alert, and AuditEntry.Detail
	} {
		checked := checkNoTSAny(t, pkg.dir)
		if checked < pkg.floor {
			t.Errorf("%s: found %d fields holding an any, want at least %d: the check no longer sees them",
				pkg.dir, checked, pkg.floor)
		}
	}
}

// checkNoTSAny runs TestNoTSAny's check on the non-test sources of dir and returns the number of fields it looked
// at.
func checkNoTSAny(t *testing.T, dir string) int {
	t.Helper()
	anyWord := regexp.MustCompile(`\bany\b`)
	options := []string{"required", "readonly"}
	checked := 0
	for _, f := range parseDir(t, dir) {
		typeName := f.Name.Name // the package, then the enclosing type declaration, for the messages
		ast.Inspect(f, func(n ast.Node) bool {
			if ts, ok := n.(*ast.TypeSpec); ok {
				typeName = f.Name.Name + "." + ts.Name.Name
			}
			st, ok := n.(*ast.StructType)
			if !ok {
				return true
			}
			for _, field := range st.Fields.List {
				if !holdsAny(field.Type) {
					continue
				}
				checked++
				name := typeName + ".(embedded)"
				if len(field.Names) > 0 {
					name = typeName + "." + field.Names[0].Name
				}
				tag := ""
				if field.Tag != nil {
					raw, _ := strconv.Unquote(field.Tag.Value)
					tag = reflect.StructTag(raw).Get("tstype")
				}
				tsType, opts, _ := strings.Cut(tag, ",")
				switch {
				case tsType == "":
					t.Errorf("%s holds an any and has no tstype tag: tygo would emit a TypeScript any", name)
				case anyWord.MatchString(tsType):
					t.Errorf("%s has the tstype %q, which is still any", name, tsType)
				}
				for _, o := range strings.Split(opts, ",") {
					if o != "" && !slices.Contains(options, o) {
						t.Errorf("%s: tstype %q has a comma in its type; tygo reads %q as an option", name, tag, o)
					}
				}
			}
			return true
		})
	}
	return checked
}

// typedConst is one constant declared with an explicit type.
type typedConst struct {
	typ, name, value string
	grouped          bool // declared in a parenthesized const group
	group            int  // which group (per file and position)
}

func typedConsts(t *testing.T) []typedConst {
	var out []typedConst
	group := 0
	for _, f := range parsePackage(t) {
		for _, d := range f.Decls {
			gd, ok := d.(*ast.GenDecl)
			if !ok || gd.Tok != token.CONST {
				continue
			}
			group++
			for _, spec := range gd.Specs {
				vs := spec.(*ast.ValueSpec)
				ident, ok := vs.Type.(*ast.Ident)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					value := ""
					if i < len(vs.Values) {
						if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
							value, _ = strconv.Unquote(lit.Value)
						}
					}
					out = append(out, typedConst{ident.Name, name.Name, value, gd.Lparen.IsValid(), group})
				}
			}
		}
	}
	return out
}

// TestEnumConstants checks the enum rules that tygo's union generation relies on (01 §14.4) and that the Valid
// methods cover exactly the declared constants: one const group per enum type with at least 2 members, every name
// prefixed by the type name. CodecKey and AuthScheme use single-line consts so they stay open strings in TS.
func TestEnumConstants(t *testing.T) {
	valid := map[string]func(string) bool{
		"Role":              func(s string) bool { return Role(s).Valid() },
		"ClientKind":        func(s string) bool { return ClientKind(s).Valid() },
		"ClientOS":          func(s string) bool { return ClientOS(s).Valid() },
		"Feature":           func(s string) bool { return Feature(s).Valid() },
		"TrackKind":         func(s string) bool { return TrackKind(s).Valid() },
		"PCKind":            func(s string) bool { return PCKind(s).Valid() },
		"VideoLayer":        func(s string) bool { return VideoLayer(s).Valid() },
		"AudioState":        func(s string) bool { return AudioState(s).Valid() },
		"ShareKind":         func(s string) bool { return ShareKind(s).Valid() },
		"Preset":            func(s string) bool { return Preset(s).Valid() },
		"ShareStatus":       func(s string) bool { return ShareStatus(s).Valid() },
		"ParticipantStatus": func(s string) bool { return ParticipantStatus(s).Valid() },
		"ConnectionStatus":  func(s string) bool { return ConnectionStatus(s).Valid() },
		"RoomEventKind":     func(s string) bool { return RoomEventKind(s).Valid() },
		"EndReason":         func(s string) bool { return EndReason(s).Valid() },
		"StatusReason":      func(s string) bool { return StatusReason(s).Valid() },
		"HintReason":        func(s string) bool { return HintReason(s).Valid() },
		"RestartMode":       func(s string) bool { return RestartMode(s).Valid() },
		"RestartReason":     func(s string) bool { return RestartReason(s).Valid() },
		"Topic":             func(s string) bool { return Topic(s).Valid() },
		"ShutdownReason":    func(s string) bool { return ShutdownReason(s).Valid() },
		"ErrorScope":        func(s string) bool { return ErrorScope(s).Valid() },
		"ErrorCode":         func(s string) bool { return ErrorCode(s).Valid() },
		"MessageType":       func(s string) bool { _, ok := Lookup(MessageType(s), DirClientToServer|DirServerToClient); return ok },
	}
	singleLine := map[string]bool{"CodecKey": true, "AuthScheme": true}
	notEnums := map[string]bool{"CloseCode": true, "Direction": true, "MsgKind": true} // numbers, not TS string unions

	byType := map[string][]typedConst{}
	for _, c := range typedConsts(t) {
		byType[c.typ] = append(byType[c.typ], c)
	}
	for typ, consts := range byType {
		switch {
		case singleLine[typ]:
			for _, c := range consts {
				if c.grouped {
					t.Errorf("%s must be a single-line const (tygo would make %s a closed union)", c.name, typ)
				}
			}
			continue
		case notEnums[typ]:
			continue
		}
		isValid, ok := valid[typ]
		if !ok {
			t.Errorf("enum type %s has no entry in this test (add its Valid method)", typ)
			continue
		}
		if len(consts) < 2 {
			t.Errorf("%s has %d constants; tygo needs at least 2 for a union", typ, len(consts))
		}
		for _, c := range consts {
			if !c.grouped || c.group != consts[0].group {
				t.Errorf("%s: all %s constants belong in one const group", c.name, typ)
			}
			if !strings.HasPrefix(c.name, typ) || len(c.name) == len(typ) {
				t.Errorf("%s: an enum constant is named %s<Value>", c.name, typ)
			}
			if c.value == "" || !isValid(c.value) {
				t.Errorf("%s = %q is not accepted by %s.Valid", c.name, c.value, typ)
			}
		}
		if isValid("") || isValid("zz_not_a_value") {
			t.Errorf("%s.Valid accepts values that are not constants", typ)
		}
		// Valid accepts nothing beyond the constants: count the accepted values among all string constants.
		var declared []string
		for _, c := range consts {
			declared = append(declared, c.value)
		}
		for other, cs := range byType {
			for _, c := range cs {
				if other != typ && isValid(c.value) && !slices.Contains(declared, c.value) {
					t.Errorf("%s.Valid accepts %q (a %s), which is not one of its constants", typ, c.value, other)
				}
			}
		}
	}
	// The MessageType constants are exactly the Registry's types, and the ErrorCode constants exactly ErrorCodes().
	var types, codes []string
	for _, c := range byType["MessageType"] {
		types = append(types, c.value)
	}
	for _, c := range byType["ErrorCode"] {
		codes = append(codes, c.value)
	}
	var regTypes, catCodes []string
	for _, s := range Registry {
		if !slices.Contains(regTypes, string(s.Type)) {
			regTypes = append(regTypes, string(s.Type))
		}
	}
	for _, c := range ErrorCodes() {
		catCodes = append(catCodes, string(c))
	}
	for _, l := range [][]string{types, codes, regTypes, catCodes} {
		slices.Sort(l)
	}
	if !slices.Equal(types, regTypes) {
		t.Errorf("MessageType constants %v\nRegistry types         %v", types, regTypes)
	}
	if !slices.Equal(codes, catCodes) {
		t.Errorf("ErrorCode constants %v\nErrorCodes()        %v", codes, catCodes)
	}
}
