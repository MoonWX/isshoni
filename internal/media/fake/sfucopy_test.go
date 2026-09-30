package fake

import (
	"bytes"
	"flag"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/pion/rtp/codecs"
)

// The fake tests check the fake video with the SFU's own H.264 code (02 §17: "the decodable SPS/PPS and slice
// headers parse with the SFU's parser"). That code is unexported in internal/server/sfu, so sfuh264_copy_test.go
// holds a verbatim copy of what the tests call (sfuCopyRoots) and everything of the SFU's h264.go that it uses: the
// top-level declarations it names and, of the methods, only those whose names it mentions. TestSFUParserCopy fails
// as soon as the copy differs from the SFU's current source; -update rewrites it.
//
// sfuh264_copy_test.go is therefore a generated shared file: a slice that changes parseSPS, isKeyframeStart,
// profileLevelID or anything they use in internal/server/sfu/h264.go runs
//
//	go test ./internal/media/fake -run TestSFUParserCopy -update
//
// and commits the output. Other changes to h264.go (a new function, or a new method on spsInfo or bitReader that
// the roots don't call) leave the copy as it is.

var updateSFUCopy = flag.Bool("update", false, "rewrite "+sfuCopyFile+" from the SFU's h264.go")

const (
	sfuH264File = "../../server/sfu/h264.go"
	sfuCopyFile = "sfuh264_copy_test.go"
)

// sfuCopyRoots are the SFU functions and methods the fake tests call. A method that only an interface calls (a
// String that fmt calls, say) is not reached by name, so a root must name it if the copy needs it.
var sfuCopyRoots = []string{"parseSPS", "isKeyframeStart", "profileLevelID"}

func TestSFUParserCopy(t *testing.T) {
	src, err := os.ReadFile(sfuH264File)
	if err != nil {
		t.Fatal(err)
	}
	want, err := sfuCopy(src, sfuCopyRoots)
	if err != nil {
		t.Fatal(err)
	}
	if *updateSFUCopy {
		if err := os.WriteFile(sfuCopyFile, want, 0o644); err != nil { //nolint:gosec // G306: a source file
			t.Fatal(err)
		}
		return
	}
	got, err := os.ReadFile(sfuCopyFile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("internal/media/fake/%s differs from internal/server/sfu/h264.go: regenerate it with "+
			"go test ./internal/media/fake -run TestSFUParserCopy -update and commit it; it is a generated shared file "+
			"(docs/m1/README.md, Shared files and their owners)", sfuCopyFile)
	}
}

// TestSFUCopyScope: the copy holds what the roots reach and nothing else, so a new function, or a new method on a
// type the roots use, leaves the copy (and so internal/media/fake) as it is.
func TestSFUCopyScope(t *testing.T) {
	const src = `package sfu

import (
	"errors"
	"fmt"
	"strings"
)

// reader is used.
type reader struct{ b []byte }

func (r *reader) used() int { return len(r.b) }

func (r *reader) unused() string { return strings.ToUpper(string(r.b)) }

var errShort = errors.New("short")

func root(b []byte) (int, error) {
	r := reader{b}
	if r.used() == 0 {
		return 0, fmt.Errorf("root: %w", errShort)
	}
	return r.used(), nil
}

func other() {}
`
	for _, tc := range []struct {
		roots     []string
		want, not []string
	}{
		{
			[]string{"root"},
			[]string{"type reader struct", "func (r *reader) used()", "var errShort", "func root(", `"errors"`, `"fmt"`},
			[]string{"unused", "other", `"strings"`},
		},
		{
			[]string{"root", "unused"},
			[]string{"func (r *reader) used()", "func (r *reader) unused()", `"strings"`},
			[]string{"other"},
		},
		{
			[]string{"unused"},
			[]string{"type reader struct", "func (r *reader) unused()", `"strings"`},
			[]string{"func (r *reader) used()", "root", `"errors"`},
		},
	} {
		got, err := sfuCopy([]byte(src), tc.roots)
		if err != nil {
			t.Fatalf("%v: %v", tc.roots, err)
		}
		for _, w := range tc.want {
			if !bytes.Contains(got, []byte(w)) {
				t.Errorf("%v: the copy lacks %s:\n%s", tc.roots, w, got)
			}
		}
		for _, n := range tc.not {
			if bytes.Contains(got, []byte(n)) {
				t.Errorf("%v: the copy has %s:\n%s", tc.roots, n, got)
			}
		}
	}
	if _, err := sfuCopy([]byte(src), []string{"missing"}); err == nil {
		t.Error("a missing root: no error")
	}
}

// TestSFUParsesSPS: every SPS the fake writes (each profile it accepts, sizes with and without cropping, low and
// high levels, Decodable's level floor) parses with the SFU's parseSPS to the profile, level and display size it
// was made for, and a keyframe's first RTP payload (SPS and PPS in a STAP-A) is a keyframe start for the SFU.
// TestDecodableStream does the same for Decodable streams, frame by frame.
func TestSFUParsesSPS(t *testing.T) {
	checkPayloads := func(name string, sps []byte) {
		t.Helper()
		au := slices.Concat(startCode[:], sps, startCode[:], buildPPS(), startCode[:], []byte{nalIDR, 0x88, 0x80})
		var pay codecs.H264Payloader
		if payloads := pay.Payload(1200, au); len(payloads) != 2 || !isKeyframeStart(payloads[0]) ||
			isKeyframeStart(payloads[1]) {
			t.Errorf("%s: RTP payloads %d, keyframe start not only on the first", name, len(payloads))
		}
	}
	for _, prof := range []string{"6400", "42e0", "4200", "4d00", "640c"} {
		p, err := parseProfile(prof)
		if err != nil {
			t.Fatal(err)
		}
		for _, l := range []VideoLayer{
			{RID: "f", Width: 1920, Height: 1080, FPS: 60}, {RID: "q", Width: 640, Height: 360, FPS: 15},
			{RID: "h", Width: 1280, Height: 720, FPS: 30}, {RID: "f", Width: 2, Height: 2, FPS: 1},
			{RID: "f", Width: 7680, Height: 4320, FPS: 60}, {RID: "q", Width: 322, Height: 178, FPS: 240},
		} {
			sps, err := buildSPS(p, l, 0)
			if err != nil {
				t.Fatal(err)
			}
			info, err := parseSPS(sps)
			if err != nil {
				t.Fatalf("%s %dx%d: the SFU's parseSPS: %v", prof, l.Width, l.Height, err)
			}
			level, _ := levelFor((l.Width+15)/16, (l.Height+15)/16, l.FPS, 0)
			if want := fmt.Sprintf("%s%02x", prof, level); info.profileLevelID() != want || info.width != l.Width ||
				info.height != l.Height {
				t.Errorf("%s %dx%d@%d: the SFU reads %s %dx%d, want %s", prof, l.Width, l.Height, l.FPS,
					info.profileLevelID(), info.width, info.height, want)
			}
			checkPayloads(fmt.Sprintf("%s %dx%d", prof, l.Width, l.Height), sps)
		}
	}

	// Decodable: level 3.0 at least, so the default q layer (level 1.2 by size and rate) declares 42e01e.
	p, err := parseProfile(DecodableProfile)
	if err != nil {
		t.Fatal(err)
	}
	def := DefaultDecodableLayers()
	for _, tc := range []struct {
		l    VideoLayer
		want string
	}{
		{def[0], "42e01e"}, {def[1], "42e01e"},
		{VideoLayer{RID: "h", Width: 1280, Height: 720, FPS: 30}, "42e01f"},
	} {
		sps, err := buildSPS(p, tc.l, decodableMinLevel)
		if err != nil {
			t.Fatal(err)
		}
		info, err := parseSPS(sps)
		if err != nil {
			t.Fatalf("decodable %dx%d: the SFU's parseSPS: %v", tc.l.Width, tc.l.Height, err)
		}
		if info.profileLevelID() != tc.want || info.width != tc.l.Width || info.height != tc.l.Height {
			t.Errorf("decodable %dx%d@%d: the SFU reads %s %dx%d, want %s", tc.l.Width, tc.l.Height, tc.l.FPS,
				info.profileLevelID(), info.width, info.height, tc.want)
		}
		checkPayloads(fmt.Sprintf("decodable %dx%d", tc.l.Width, tc.l.Height), sps)
	}
}

// sfuCopy returns the source of the copy: the declarations of src that roots reach, verbatim with their comments and
// in source order, and the imports they use. A reached declaration reaches the top-level declarations it names and
// the methods whose names it mentions (every method of that name, whatever its receiver: the source is not
// type-checked); a type does not bring its other methods. A method brings its receiver type.
func sfuCopy(src []byte, roots []string) ([]byte, error) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "h264.go", src, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		return nil, err
	}
	byName := map[string]int{}    // top-level name → declaration
	methods := map[string][]int{} // method name → the methods of that name
	for i, d := range f.Decls {
		switch d := d.(type) {
		case *ast.FuncDecl:
			if d.Recv == nil {
				byName[d.Name.Name] = i
			} else {
				methods[d.Name.Name] = append(methods[d.Name.Name], i)
			}
		case *ast.GenDecl:
			for _, s := range d.Specs {
				switch s := s.(type) {
				case *ast.TypeSpec:
					byName[s.Name.Name] = i
				case *ast.ValueSpec:
					for _, n := range s.Names {
						byName[n.Name] = i
					}
				}
			}
		}
	}
	need := map[int]bool{}
	pkgs := map[string]bool{} // package names used as selector bases
	var visit func(i int)
	visit = func(i int) {
		if need[i] {
			return
		}
		need[i] = true
		var own *ast.Ident // a function's own name, which reaches nothing
		if fd, ok := f.Decls[i].(*ast.FuncDecl); ok {
			own = fd.Name
		}
		ast.Inspect(f.Decls[i], func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.SelectorExpr:
				if id, ok := n.X.(*ast.Ident); ok {
					pkgs[id.Name] = true
				}
			case *ast.Ident:
				if n == own {
					break
				}
				if j, ok := byName[n.Name]; ok {
					visit(j)
				}
				for _, j := range methods[n.Name] {
					visit(j)
				}
			}
			return true
		})
	}
	for _, r := range roots {
		i, ok := byName[r]
		switch {
		case ok:
			visit(i)
		case len(methods[r]) > 0:
			for _, j := range methods[r] {
				visit(j)
			}
		default:
			return nil, fmt.Errorf("the SFU's h264.go has no %s", r)
		}
	}

	var imports []string
	for _, spec := range f.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil {
			return nil, err
		}
		name := path[strings.LastIndex(path, "/")+1:]
		if spec.Name != nil {
			name = spec.Name.Name
		}
		if pkgs[name] {
			imports = append(imports, spec.Path.Value)
		}
	}
	var b bytes.Buffer
	b.WriteString("// Code generated by TestSFUParserCopy from internal/server/sfu/h264.go; DO NOT EDIT.\n\n")
	fmt.Fprintf(&b, "// A verbatim copy of %s and what they use (sfucopy_test.go).\n\n", strings.Join(roots, ", "))
	b.WriteString("package fake\n\nimport (\n")
	for _, imp := range imports {
		b.WriteString("\t" + imp + "\n")
	}
	b.WriteString(")\n")
	for _, i := range slices.Sorted(maps.Keys(need)) {
		start := f.Decls[i].Pos()
		switch d := f.Decls[i].(type) {
		case *ast.FuncDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
		case *ast.GenDecl:
			if d.Doc != nil {
				start = d.Doc.Pos()
			}
		}
		from, to := fset.Position(start).Offset, fset.Position(f.Decls[i].End()).Offset
		if nl := bytes.IndexByte(src[to:], '\n'); nl >= 0 { // keep a trailing line comment
			to += nl
		}
		b.WriteString("\n")
		b.Write(src[from:to])
		b.WriteString("\n")
	}
	return format.Source(b.Bytes())
}
