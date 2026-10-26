package i18n

import (
	"bytes"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/MoonWX/isshoni/web"
)

const fixture = `{
  "desktop": {
    "native": {
      "hello": "Hello {{name}}",
      "spaced": "Hi {{ name }}, {{name}} again",
      "two": "{{a}} and {{b}}",
      "open": "Unclosed {{name",
      "watching": "{{count}} watching",
      "questions_one": "1 question waiting",
      "questions_other": "{{count}} questions waiting"
    },
    "audio": {"other": "not for Go"}
  },
  "common": {"x": "not desktop"}
}`

func load(t *testing.T, data string) (*Catalog, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	c, err := Load([]byte(data), slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return c, &buf
}

func TestInterpolation(t *testing.T) {
	c, _ := load(t, fixture)
	for _, tc := range []struct {
		key  string
		args []any
		want string
	}{
		{"desktop.native.hello", []any{"name", "Alex"}, "Hello Alex"},
		{"desktop.native.hello", nil, "Hello {{name}}"},                      // no value: the placeholder stays
		{"desktop.native.hello", []any{"name"}, "Hello {{name}}"},            // a name without a value
		{"desktop.native.hello", []any{7, "x", "name", "A"}, "Hello A"},      // a non-string name is skipped
		{"desktop.native.spaced", []any{"name", "Sam"}, "Hi Sam, Sam again"}, // spaces inside the braces
		{"desktop.native.two", []any{"a", 1, "b", 2.5}, "1 and 2.5"},
		{"desktop.native.two", []any{"b", "B"}, "{{a}} and B"},
		{"desktop.native.open", []any{"name", "x"}, "Unclosed {{name"},
		{"desktop.native.watching", []any{"count", 3}, "3 watching"}, // no plural forms: the key itself
	} {
		if got := c.T(tc.key, tc.args...); got != tc.want {
			t.Errorf("T(%s, %v) = %q, want %q", tc.key, tc.args, got, tc.want)
		}
	}
}

func TestPlurals(t *testing.T) {
	c, _ := load(t, fixture)
	for _, tc := range []struct {
		count any
		want  string
	}{
		{1, "1 question waiting"},
		{int64(1), "1 question waiting"},
		{uint8(1), "1 question waiting"},
		{1.0, "1 question waiting"},
		{0, "0 questions waiting"},
		{2, "2 questions waiting"},
		{uint64(21), "21 questions waiting"},
		{-1, "-1 questions waiting"},
	} {
		if got := c.T("desktop.native.questions", "count", tc.count); got != tc.want {
			t.Errorf("count %v (%T): %q, want %q", tc.count, tc.count, got, tc.want)
		}
	}
	if !c.Has("desktop.native.questions") || !c.Has("desktop.native.hello") || c.Has("desktop.native.nope") {
		t.Error("Has")
	}
	// Without a count the base key is asked for, and there is none.
	if got := c.T("desktop.native.questions"); got != "desktop.native.questions" {
		t.Errorf("without count: %q", got)
	}
}

func TestMissingKeyWarnsOnce(t *testing.T) {
	c, buf := load(t, fixture)
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			if got := c.T("desktop.native.missing", "count", 2); got != "desktop.native.missing" {
				t.Errorf("missing key: %q", got)
			}
		})
	}
	wg.Wait()
	if got := c.T("desktop.audio.other"); got != "desktop.audio.other" {
		t.Errorf("a key outside desktop.native: %q", got)
	}
	if got := c.T("common.x"); got != "common.x" {
		t.Errorf("a key outside desktop: %q", got)
	}
	out := buf.String()
	if n := strings.Count(out, "key=desktop.native.missing"); n != 1 {
		t.Errorf("%d WARNs for one missing key:\n%s", n, out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "component=i18n") {
		t.Errorf("log: %s", out)
	}
	if c.Keys() != 7 {
		t.Errorf("Keys = %d, want the 7 desktop.native messages", c.Keys())
	}
}

func TestNilCatalog(t *testing.T) {
	var c *Catalog
	if got := c.T("desktop.native.tray.open"); got != "desktop.native.tray.open" {
		t.Errorf("nil T = %q", got)
	}
	if c.Has("desktop.native.tray.open") || c.Keys() != 0 {
		t.Error("nil Has/Keys")
	}
}

func TestLoadRejects(t *testing.T) {
	for _, data := range []string{
		``,
		`[]`,
		`{"desktop": {"native": {"a": 1}}}`,
		`{"desktop": {"native": {"a": ["x"]}}}`,
		`{"desktop": {"audio": {"a": "x"}}}`, // nothing for Go
		`{"desktop": {"native": {"a": "x"}`,
	} {
		if _, err := Load([]byte(data), nil); err == nil {
			t.Errorf("Load(%q) succeeded", data)
		}
	}
}

// TestDesktopCatalog: the app's real catalog (web.DesktopCatalog(), desktop.en.json) loads, and every
// T("desktop.native.…") literal in Go code under internal/desktop and cmd/isshoni-desktop names a message it has
// (03 §18.1). Keys built at run time aren't followed.
func TestDesktopCatalog(t *testing.T) {
	c, err := Load(web.DesktopCatalog(), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"desktop.native.tray.open", "desktop.native.audio.voiceAppsHeard",
		"desktop.native.audio.questionsWaiting", "desktop.native.toast.micPrompt",
		"desktop.native.toast.browserMicPrompt", "desktop.native.toast.keepOut", "desktop.native.toast.keepBrowserOut",
		"desktop.native.toast.include", "desktop.native.toast.voiceAppsHeard"} {
		if !c.Has(key) {
			t.Errorf("the catalog has no %s (05 §14.8, 03 §18.1)", key)
		}
	}
	if got := c.T("desktop.native.audio.questionsWaiting", "count", 3); got != "3 questions waiting" {
		t.Errorf("questionsWaiting 3: %q", got)
	}

	used := regexp.MustCompile(`\bT\(\s*"(desktop\.native\.[A-Za-z0-9_.]+)"`)
	self, err := filepath.Abs(".")
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, root := range []string{"..", filepath.Join("..", "..", "..", "cmd", "isshoni-desktop")} {
		err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if d.Name() == "testdata" {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") {
				return nil
			}
			// This package's tests name missing keys on purpose.
			if abs, _ := filepath.Abs(filepath.Dir(path)); abs == self && strings.HasSuffix(path, "_test.go") {
				return nil
			}
			src, err := os.ReadFile(path) //nolint:gosec // G122: this repository's own source tree
			if err != nil {
				return err
			}
			for _, m := range used.FindAllSubmatch(src, -1) {
				found++
				if key := string(m[1]); !c.Has(key) {
					t.Errorf("%s: T(%q) is not in web/src/i18n/lazy/desktop.en.json", path, key)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("%d desktop.native keys used in Go", found)
}
