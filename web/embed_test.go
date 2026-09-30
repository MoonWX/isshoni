package web

import (
	"io/fs"
	"path"
	"strings"
	"testing"
)

// TestDistRooted checks that Dist is rooted at dist/ and embeds the committed keep file, which exists both on a fresh
// clone and after a web build (the build keeps it).
func TestDistRooted(t *testing.T) {
	d := Dist()
	if _, err := fs.Stat(d, ".gitkeep"); err != nil {
		t.Fatalf("Dist() has no .gitkeep at its root: %v", err)
	}
	if _, err := fs.Stat(d, "dist"); err == nil {
		t.Fatal(`Dist() contains "dist": it is not rooted at dist/`)
	}
}

// TestDistBuilt checks the layout 04 serves (05 §17.1) when the SPA has been built into dist/.
func TestDistBuilt(t *testing.T) {
	d := Dist()
	if _, err := fs.Stat(d, "index.html"); err != nil {
		t.Skip("web/dist has no build (run task build:web)")
	}
	for _, name := range []string{"index.html", "version.json", "boot-check.js", "robots.txt"} {
		if _, err := fs.Stat(d, name); err != nil {
			t.Errorf("built dist/ has no %s: %v", name, err)
		}
	}
	assets, err := fs.ReadDir(d, "assets")
	if err != nil || len(assets) == 0 {
		t.Fatalf("built dist/ has no assets/: %v", err)
	}
	// Precompressed siblings: the entry chunk is always over the 1 KiB threshold, and each sibling has its original.
	var br, gz int
	for _, e := range assets {
		name := e.Name()
		for _, ext := range []string{".br", ".gz"} {
			orig, ok := strings.CutSuffix(name, ext)
			if !ok {
				continue
			}
			if _, err := fs.Stat(d, path.Join("assets", orig)); err != nil {
				t.Errorf("assets/%s has no original: %v", name, err)
			}
			if ext == ".br" {
				br++
			} else {
				gz++
			}
		}
	}
	if br == 0 || gz == 0 {
		t.Errorf("built dist/assets has %d .br and %d .gz files, want at least one of each", br, gz)
	}
}
