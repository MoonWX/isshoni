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
//   - the fixed pages and anchors that the installer, the release notes and the product name;
//   - the Windows app's anchors of docs/m2/06-windows-release.md §17 ("Site anchors"), which the app, its installer,
//     install-desktop.ps1 and the server's Download page link to (desktopAnchors);
//   - every link with an anchor into the site that the product's sources carry (cmd/, internal/, web/src/, deploy/,
//     .goreleaser.yaml), and every link with an anchor between the site's own pages.
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
			"docs/download.md",
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

	// The Windows app's anchors (docs/m2/06-windows-release.md §13 and §17's "Site anchors"): the app (03), the
	// server's Download page, the installer, install-desktop.ps1 and the release notes link to them.
	t.Run("desktop", func(t *testing.T) {
		pages := map[string]anchors{}
		for _, a := range desktopAnchors {
			page, ok := pages[a.page]
			if !ok {
				page = pageAnchors(t, root, a.page)
				pages[a.page] = page
			}
			requireAnchor(t, page, a.anchor, a.why)
		}
	})

	// Every link into the site that the product's sources carry, whether or not a list above names it: a new link
	// to a section that doesn't exist fails here rather than in a friend's browser.
	t.Run("product links", func(t *testing.T) {
		links := productSiteLinks(t, root)
		if len(links) == 0 {
			t.Fatal("found no link into the project site in the product's sources: productSiteLinks needs updating")
		}
		checkLinks(t, root, links, "the product links to it")
	})

	// The site's own links between its pages, so a page never sends a reader to a section that isn't there.
	t.Run("site links", func(t *testing.T) {
		checkLinks(t, root, docsSiteLinks(t, root), "a page of the site links to it")
	})
}

// desktopAnchors are the Windows app's fixed sections of the site (docs/m2/06-windows-release.md §17, "Site
// anchors"), and the ones docs/m2/README.md's WA57 and WA85 rows add: /troubleshooting#desktop-linking (the app's
// linking texts, 03 §8.1, §9) and #desktop-sound (the sound check's "Learn more", 03 §8.4).
var desktopAnchors = []struct{ page, anchor, why string }{
	{"docs/download.md", "windows", "the installer's welcome page and Apps & features' help link open /download#windows"},
	{"docs/download.md", "windows-powershell", "install-desktop.ps1's header and the Download page link to it"},
	{"docs/download.md", "windows-smartscreen", "the Download page and the release notes link to it"},
	{"docs/download.md", "windows-smart-app-control", "the Download page and install-desktop.ps1 link to it"},
	{"docs/download.md", "windows-verify", "install-desktop.ps1 links to it when it can't check a download"},
	{"docs/download.md", "windows-updates", "the app's update texts link to it"},
	{"docs/download.md", "windows-uninstall", "06 §17's site anchors"},
	{"docs/download.md", "windows-firewall", "06 §17's site anchors"},
	{"docs/download.md", "windows-webview2", "06 §17's site anchors"},
	{"docs/download.md", "ffmpeg-source", "the LGPL notice of the app (06 §9.1) points to the FFmpeg source"},
	{"docs/download.md", "macos", "06 §13: the macOS app is coming"},
	{"docs/download.md", "linux", "06 §13: the Linux agent is coming"},
	{"docs/privacy.md", "desktop", "the installer's update option and the app's About link to /privacy#desktop"},
	{"docs/privacy.md", "linked-apps", "the Devices page links to /privacy#linked-apps"},
	{"docs/security.md", "verify-desktop", "install-desktop.ps1 links to it after a bad signature"},
	{"docs/security.md", "linked-apps", "the app's linking texts link to /security#linked-apps"},
	{"docs/troubleshooting.md", "desktop-update-failed", "06 §17's site anchors"},
	{"docs/troubleshooting.md", "desktop-smart-app-control", "the app's blocked_by_policy text links to it"},
	{"docs/troubleshooting.md", "desktop-webview2", "the app's and the installer's WebView2 messages link to it"},
	{"docs/troubleshooting.md", "desktop-openssh", "06 §17's site anchors"},
	{"docs/troubleshooting.md", "desktop-windows-n", "the app's codec banner and its N-edition fix link to it"},
	{"docs/troubleshooting.md", "desktop-media-feature-pack", "the Download page and the app's engine errors link to it"},
	{"docs/troubleshooting.md", "desktop-gpu-driver", "the Download page and the app's driver_too_old text link to it"},
	{"docs/troubleshooting.md", "desktop-fullscreen-games", "06 §17's site anchors"},
	{"docs/troubleshooting.md", "desktop-crash-dumps", "06 §17's site anchors"},
	{"docs/troubleshooting.md", "desktop-windows10-border", "06 §17's site anchors"},
	{"docs/troubleshooting.md", "desktop-linking", "the app's \"Can't reach the server\" texts link to it"},
	{"docs/troubleshooting.md", "desktop-sound", "the app's sound check links to it (\"Learn more\")"},
}

// siteLink is a link into a page of the project site with an anchor, and where it was found.
type siteLink struct {
	page   string // the page's file, such as docs/troubleshooting.md
	anchor string
	from   string // file:line
}

// checkLinks fails t for every link whose anchor its page doesn't have, or whose page doesn't exist.
func checkLinks(t *testing.T, root string, links []siteLink, why string) {
	t.Helper()
	pages := map[string]*anchors{}
	for _, l := range links {
		a, ok := pages[l.page]
		if !ok {
			if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(l.page))); err != nil {
				t.Errorf("%s links to %s#%s, but there is no %s", l.from, l.page, l.anchor, l.page)
				pages[l.page] = nil
				continue
			}
			p := pageAnchors(t, root, l.page)
			a = &p
			pages[l.page] = a
		}
		if a == nil {
			continue
		}
		requireAnchor(t, *a, l.anchor, why+" ("+l.from+")")
	}
}

var (
	// A link into the site in the product's sources: a page path, then #anchor. The page path comes after a slash, a
	// brace or a quote, as in https://moonwx.github.io/isshoni/troubleshooting#<anchor>, ${DOCS_URL}download#<anchor>,
	// "$site/download#<anchor>", "${SITE}/privacy#<anchor>" or "troubleshooting#<anchor>". An anchor that ends in - or
	// _ is the start of one built at run time (#doctor-<id>, #ct-<code>), which the subtests above check.
	productLinkRE = regexp.MustCompile(`(?:^|[/}"'` + "`" + `])(troubleshooting|download|privacy|security|code-signing|` +
		`install(?:/[a-z-]+)?|guide|reference/[a-z]+)/?#([a-z0-9](?:[a-z0-9_-]*[a-z0-9])?)(?:[^a-z0-9_$-]|$)`)
	// The engine errors' troubleshooting sections, given as `anchor: '<id>'` (web/src/share/native/engineErrors.ts).
	engineAnchorRE = regexp.MustCompile(`\banchor:\s*'([a-z0-9][a-z0-9_-]*)'`)
	// A markdown link into the site from one of its pages: ](/page#anchor), ](/page/#anchor) or ](#anchor).
	docsLinkRE = regexp.MustCompile(`\]\((/[a-z0-9/_-]*)?#([a-z0-9][a-z0-9_-]*)\)`)
)

// productSourceExts are the kinds of files that can carry a link into the site.
var productSourceExts = map[string]bool{
	".go": true, ".ts": true, ".tsx": true, ".js": true, ".mjs": true, ".json": true, ".ps1": true, ".nsi": true,
	".nsh": true, ".sh": true, ".yaml": true, ".yml": true, ".txtar": true, ".txt": true,
}

// productSiteLinks are the links into the site that the product's sources carry: the server, the web client, the
// desktop app, the installers and scripts, and the release notes.
func productSiteLinks(t *testing.T, root string) []siteLink {
	t.Helper()
	var links []siteLink
	scan := func(rel string) {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			from := rel + ":" + strconv.Itoa(i+1)
			for _, m := range productLinkRE.FindAllStringSubmatch(line, -1) {
				links = append(links, siteLink{page: sitePage(m[1]), anchor: m[2], from: from})
			}
			if strings.HasPrefix(rel, "web/src/") {
				for _, m := range engineAnchorRE.FindAllStringSubmatch(line, -1) {
					links = append(links, siteLink{page: "docs/troubleshooting.md", anchor: m[1], from: from})
				}
			}
		}
	}
	files := []string{".goreleaser.yaml"} // slash-separated, relative to root
	for _, dir := range []string{"cmd", "internal", "web/src", "deploy"} {
		err := filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if name := d.Name(); name == "node_modules" || name == "dist" || strings.HasPrefix(name, ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if !productSourceExts[filepath.Ext(path)] {
				return nil
			}
			rel, err := filepath.Rel(root, path)
			if err != nil {
				return err
			}
			files = append(files, filepath.ToSlash(rel))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, rel := range files {
		scan(rel)
	}
	return links
}

// docsSiteLinks are the links with an anchor between the site's pages (docs/, without the design documents that
// the site leaves out: PLAN.md, m1/, m2/).
func docsSiteLinks(t *testing.T, root string) []siteLink {
	t.Helper()
	var (
		links []siteLink
		pages []string // slash-separated, relative to root
	)
	docs := filepath.Join(root, "docs")
	err := filepath.WalkDir(docs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if d.IsDir() {
			switch rel {
			case "docs/m1", "docs/m2", "docs/node_modules", "docs/public", "docs/scripts":
				return filepath.SkipDir
			}
			if strings.HasPrefix(d.Name(), ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if filepath.Ext(path) == ".md" && rel != "docs/PLAN.md" {
			pages = append(pages, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range pages {
		data, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			for _, m := range docsLinkRE.FindAllStringSubmatch(line, -1) {
				page := rel
				if m[1] != "" {
					page = sitePage(strings.TrimPrefix(m[1], "/"))
				}
				links = append(links, siteLink{page: page, anchor: m[2], from: rel + ":" + strconv.Itoa(i+1)})
			}
		}
	}
	return links
}

// sitePage is the markdown file of a page of the site, from its path without the base: "troubleshooting" →
// docs/troubleshooting.md, "install/" and "install" → docs/install/index.md, "guide/" → docs/guide/index.md.
func sitePage(path string) string {
	path = strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/")
	switch path {
	case "", "index":
		return "docs/index.md"
	case "install", "guide":
		return "docs/" + path + "/index.md"
	}
	return "docs/" + path + ".md"
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
