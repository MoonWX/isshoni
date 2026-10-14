package doctor_test

import (
	"bufio"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/MoonWX/isshoni/internal/server/ops/doctor"
)

// TestDocsAnchors holds the project site to the anchor contract of docs/m1/06-deploy-and-ci.md §10.4: the server, the
// installer and the web client link to fixed sections of the site, and every one of them must exist.
//
//   - /troubleshooting#doctor-<id> for every id of doctor.CheckIDs() (doctor's text output, the admin dashboard);
//   - /troubleshooting#ct-<code> for every code in web/src/conntest/codes.json (the connection test);
//   - /install/vps#<provider> for every CloudProvider constant of internal/protocol/api, collected with go/parser
//     (doctor's firewall hint, the wizard's fix text);
//   - the fixed pages and anchors that the installer, the release notes and the product name.
//
// It lives here because no other slice of its group touches this package, and tools/ is a module that can't import
// internal/ packages. It runs in CI's test-go job and in the site job, so a docs-only or codes-only change runs it too.
//
// A section counts only with an explicit anchor, `## Title {#anchor}`: VitePress's own slug follows the title's
// words, and a reworded title must not move a link that released programs carry. The page may have more anchors
// than the product links to; a code that the product no longer uses keeps its section, for older releases.
func TestDocsAnchors(t *testing.T) {
	root := repoRoot(t)
	troubleshooting := pageAnchors(t, root, "docs/troubleshooting.md")
	vps := pageAnchors(t, root, "docs/install/vps.md")

	t.Run("doctor", func(t *testing.T) {
		ids := doctor.CheckIDs()
		if len(ids) == 0 {
			t.Fatal("doctor.CheckIDs() is empty")
		}
		for _, id := range ids {
			requireAnchor(t, troubleshooting, "doctor-"+id,
				"`isshoni doctor` links to it after a warning or failure of the check "+strconv.Quote(id))
		}
	})

	t.Run("connection test", func(t *testing.T) {
		for _, code := range conntestCodes(t, root) {
			requireAnchor(t, troubleshooting, "ct-"+code,
				"the connection test links to it for the result "+strconv.Quote(code))
		}
	})

	t.Run("providers", func(t *testing.T) {
		for _, provider := range cloudProviders(t, root) {
			requireAnchor(t, vps, provider,
				"doctor's firewall hint and the wizard's fix text link to it for the provider "+strconv.Quote(provider))
		}
	})

	// The links of the anchor table's last row (06 §10.4), and the pages that install.sh, the compose files, the
	// release notes (.goreleaser.yaml), `isshoni config example` and the web app's about page name.
	t.Run("fixed", func(t *testing.T) {
		for _, page := range []string{
			"docs/index.md",
			"docs/install/index.md",
			"docs/install/docker.md",
			"docs/install/tls.md",
			"docs/install/vps.md",
			"docs/install/reverse-proxy.md",
			"docs/troubleshooting.md",
			"docs/security.md",
			"docs/privacy.md",
			"docs/code-signing.md",
			"docs/reference/config.md",
		} {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(page))); err != nil {
				t.Errorf("%s: %v: the product links to this page (06 §10.4)", page, err)
			}
		}
		requireAnchor(t, pageAnchors(t, root, "docs/install/reverse-proxy.md"), "docker",
			"install.sh and the Docker docs link to /install/reverse-proxy#docker")
		security := pageAnchors(t, root, "docs/security.md")
		requireAnchor(t, security, "verify", "install.sh and the release notes link to /security#verify")
		requireAnchor(t, security, "keys", "the release notes link to /security#keys")
	})
}

// anchors are a page's explicit heading anchors, with the line of each.
type anchors struct {
	page  string
	lines map[string]int
}

// requireAnchor fails t when the page has no heading with {#id}.
func requireAnchor(t *testing.T, a anchors, id, why string) {
	t.Helper()
	if _, ok := a.lines[id]; !ok {
		t.Errorf("%s has no heading with {#%s}: %s (docs/m1/06-deploy-and-ci.md §10.4). Add a section like "+
			"`### Title {#%s}`", a.page, id, why, id)
	}
}

// repoRoot is the repository's root: four levels above this package, where go.mod is.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", "..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod in %s, which should be the repository's root: %v", root, err)
	}
	return root
}

var (
	// A fence opens and closes a code block: three or more backticks or tildes, indented by at most three spaces.
	fenceRE = regexp.MustCompile("^ {0,3}(`{3,}|~{3,})")
	// An ATX heading that ends with an explicit anchor, VitePress's `## Title {#anchor}`.
	headingAnchorRE = regexp.MustCompile(`^ {0,3}#{1,6}[ \t]+.*\{#([^}\s]+)\}[ \t]*$`)
	// What the product's links can carry as an anchor: lower-case words with - and _.
	anchorRE = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
)

// pageAnchors reads the explicit heading anchors of a markdown page. Lines in the front matter, in fenced code
// blocks and in HTML comments are not headings. A page with the same anchor twice fails t: a link would land on
// the first one only.
func pageAnchors(t *testing.T, root, page string) anchors {
	t.Helper()
	f, err := os.Open(filepath.Join(root, filepath.FromSlash(page)))
	if err != nil {
		t.Fatalf("%s: %v: the product links into this page (docs/m1/06-deploy-and-ci.md §10.4)", page, err)
	}
	defer func() { _ = f.Close() }()

	a := anchors{page: page, lines: map[string]int{}}
	var (
		n           int
		fence       string // the open fence, "" outside a code block
		frontMatter bool
		comment     bool
	)
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		switch {
		case n == 1 && line == "---":
			frontMatter = true
			continue
		case frontMatter:
			if line == "---" {
				frontMatter = false
			}
			continue
		case fence != "":
			if m := fenceRE.FindStringSubmatch(line); m != nil && m[1][0] == fence[0] && len(m[1]) >= len(fence) &&
				strings.TrimSpace(strings.TrimLeft(strings.TrimSpace(line), m[1][:1])) == "" {
				fence = ""
			}
			continue
		case comment:
			if strings.Contains(line, "-->") {
				comment = false
			}
			continue
		}
		if m := fenceRE.FindStringSubmatch(line); m != nil {
			fence = m[1]
			continue
		}
		if i := strings.Index(line, "<!--"); i >= 0 && !strings.Contains(line[i:], "-->") {
			comment = true
			continue
		}
		m := headingAnchorRE.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		id := m[1]
		if first, dup := a.lines[id]; dup {
			t.Errorf("%s:%d: the anchor {#%s} is on line %d already", page, n, id, first)
			continue
		}
		a.lines[id] = n
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("%s: %v", page, err)
	}
	if fence != "" {
		t.Errorf("%s: a code block opened with %s is never closed, which hides every heading after it", page, fence)
	}
	return a
}

// conntestCodes are the connection test's result codes, from the list the web client exports for this test.
func conntestCodes(t *testing.T, root string) []string {
	t.Helper()
	const file = "web/src/conntest/codes.json"
	b, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(file)))
	if err != nil {
		t.Fatal(err)
	}
	var codes []string
	if err := json.Unmarshal(b, &codes); err != nil {
		t.Fatalf("%s is not a JSON array of strings: %v", file, err)
	}
	if len(codes) == 0 {
		t.Fatalf("%s lists no code", file)
	}
	for _, c := range codes {
		if !anchorRE.MatchString(c) {
			t.Errorf("%s: %q can't be part of an anchor (lower-case letters, digits, - and _)", file, c)
		}
	}
	return codes
}

// cloudProviders are the values of the CloudProvider constants in the Go files of internal/protocol/api (04 §13.3;
// today conntest.go), read with go/parser rather than from a list kept by hand.
func cloudProviders(t *testing.T, root string) []string {
	t.Helper()
	const dir = "internal/protocol/api"
	fset := token.NewFileSet()
	files, err := fs.Glob(os.DirFS(filepath.Join(root, dir)), "*.go")
	if err != nil {
		t.Fatal(err)
	}
	var providers []string
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(root, dir, name)
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			// In a const group a spec without a type and values repeats the ones before it.
			var typ ast.Expr
			var values []ast.Expr
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				if vs.Type != nil || len(vs.Values) > 0 {
					typ, values = vs.Type, vs.Values
				}
				if ident, ok := typ.(*ast.Ident); !ok || ident.Name != "CloudProvider" {
					continue
				}
				for i, name := range vs.Names {
					if i >= len(values) {
						t.Fatalf("%s: %s has no value of its own", fset.Position(name.Pos()), name.Name)
					}
					lit, ok := values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: %s is not a string literal; this test reads the values as written",
							fset.Position(name.Pos()), name.Name)
					}
					v, err := strconv.Unquote(lit.Value)
					if err != nil {
						t.Fatal(err)
					}
					if !anchorRE.MatchString(v) {
						t.Errorf("%s: %s = %q can't be an anchor", fset.Position(name.Pos()), name.Name, v)
					}
					providers = append(providers, v)
				}
			}
		}
	}
	if len(providers) == 0 {
		t.Fatalf("no CloudProvider constant in %s/*.go: the anchor test needs updating for where they are now", dir)
	}
	slices.Sort(providers)
	return slices.Compact(providers)
}
