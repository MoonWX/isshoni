package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fetchFixture is a repository with native/ffmpeg files, a lock.json for a zip built in the test, and a TLS server
// that serves the zip as GitHub serves a release asset (through a redirect to another path).
type fetchFixture struct {
	root     string
	zip      []byte
	source   []byte
	server   *httptest.Server
	requests atomic.Int32
	fail     atomic.Int32 // the next requests that get a 503
}

// buildZip lays out a build as native/ffmpeg/build.sh does; mutate may change the files before they are zipped.
func buildZip(t *testing.T, mutate func(files map[string]string)) []byte {
	t.Helper()
	cfg, err := os.ReadFile(filepath.Join("testdata", "ffmpeg-config-ok.json"))
	if err != nil {
		t.Fatal(err)
	}
	top := "ffmpeg-9.0.2-r1-win-x64/"
	files := map[string]string{
		"bin/avcodec-63.dll":                    "avcodec dll",
		"bin/avutil-61.dll":                     "avutil dll",
		"pdb/avcodec-63.pdb":                    "pdb",
		"pdb/avutil-61.pdb":                     "pdb",
		"lib/avcodec.lib":                       "lib",
		"lib/avutil.lib":                        "lib",
		"include/libavcodec/avcodec.h":          "/* avcodec.h */",
		"include/libavutil/avutil.h":            "/* avutil.h */",
		"include/libavutil/hwcontext_d3d11va.h": "/* hwcontext_d3d11va.h */",
		"licenses/ffmpeg/COPYING.LGPLv2.1":      "LGPL",
		"licenses/ffmpeg/LICENSE.md":            "license",
		"licenses/ffmpeg/README-LGPL.txt":       "readme",
		"licenses/libvpl/LICENSE":               "MIT",
		"licenses/nv-codec-headers/LICENSE":     "MIT",
		"licenses/amf/LICENSE.txt":              "MIT",
		"ffmpeg-config.json":                    string(cfg),
		"build-manifest.json":                   "{}",
	}
	if mutate != nil {
		mutate(files)
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		if !strings.HasPrefix(name, "/") && !strings.HasPrefix(name, "..") && !strings.HasPrefix(name, "other/") {
			name = top + name
		}
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func hexSHA256(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newFetchFixture writes the repository (the committed native/ffmpeg files and a lock.json for zipData) and starts
// the server.
func newFetchFixture(t *testing.T, zipData []byte) *fetchFixture {
	t.Helper()
	f := &fetchFixture{root: t.TempDir(), zip: zipData, source: []byte("source tar")}
	copyTree(t, filepath.Join(repoRoot(t), "native", "ffmpeg"), filepath.Join(f.root, "native", "ffmpeg"),
		"pins.json", "build.sh", "ffcheck.c")
	inputs, err := inputsSHA256(f.root)
	if err != nil {
		t.Fatal(err)
	}
	lock := sampleLock(inputs)
	lock = strings.Replace(lock, strings.Repeat("1", 64), hexSHA256(zipData), 1)
	lock = strings.Replace(lock, strings.Repeat("2", 64), hexSHA256(f.source), 1)
	lock = strings.Replace(lock, strings.Repeat("3", 64), hexSHA256([]byte("avcodec dll")), 1)
	lock = strings.Replace(lock, strings.Repeat("4", 64), hexSHA256([]byte("avutil dll")), 1)
	writeFile(t, filepath.Join(f.root, lockPath), lock)

	mux := http.NewServeMux()
	mux.HandleFunc("/download/deps-ffmpeg-9.0.2-r1/{asset}", func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if f.fail.Load() > 0 {
			f.fail.Add(-1)
			http.Error(w, "busy", http.StatusServiceUnavailable)
			return
		}
		// GitHub redirects a release asset to its storage host.
		http.Redirect(w, r, "/storage/"+r.PathValue("asset"), http.StatusFound) //nolint:gosec // G710: the test server redirects to itself
	})
	mux.HandleFunc("/storage/{asset}", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("asset") {
		case "ffmpeg-9.0.2-r1-win-x64.zip":
			_, _ = w.Write(f.zip)
		case "ffmpeg-9.0.2-r1-source.tar":
			_, _ = w.Write(f.source)
		default:
			http.NotFound(w, r)
		}
	})
	mux.HandleFunc("/plain/{asset}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "http://"+r.Host+"/storage/"+r.PathValue("asset"), http.StatusFound) //nolint:gosec // G710: the plain-HTTP redirect under test
	})
	f.server = httptest.NewTLSServer(mux)
	t.Cleanup(f.server.Close)
	return f
}

// fetch runs ffmpeg-fetch against the fixture's server with no waiting between attempts.
func (f *fetchFixture) fetch(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	client := f.server.Client()
	client.CheckRedirect = newHTTPClient().CheckRedirect
	defer client.CloseIdleConnections()
	var out, errOut bytes.Buffer
	e := &env{root: f.root, stdout: &out, stderr: &errOut, client: client,
		sleep: func(context.Context, time.Duration) error { return nil }}
	err := runFFmpegFetch(context.Background(), e, append([]string{"-base", f.server.URL + "/download"}, args...))
	code = 0
	if err != nil {
		errOut.WriteString(err.Error())
		code = 1
	}
	return code, out.String(), errOut.String()
}

func TestFetch(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	out := filepath.Join(t.TempDir(), "ffmpeg")
	code, stdout, stderr := f.fetch(t, "-out", out, "-source")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	root := filepath.Join(out, "ffmpeg-9.0.2-r1-win-x64")
	if strings.TrimSpace(stdout) != root {
		t.Errorf("printed %q, want %q", stdout, root)
	}
	for _, rel := range []string{"bin/avcodec-63.dll", "include/libavcodec/avcodec.h", "licenses/amf/LICENSE.txt"} {
		if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
			t.Error(err)
		}
	}
	if data, err := os.ReadFile(filepath.Join(out, "ffmpeg-9.0.2-r1-source.tar")); err != nil || string(data) != "source tar" {
		t.Errorf("source archive: %q, %v", data, err)
	}
	if n := f.requests.Load(); n != 2 {
		t.Errorf("%d requests, want 2", n)
	}
	assertNoTemp(t, out)

	// A second run keeps the verified zip and unpacks it again.
	if code, _, stderr = f.fetch(t, "-out", out); code != 0 || !strings.Contains(stderr, "already there") {
		t.Errorf("second run: exit %d, %q", code, stderr)
	}
	if n := f.requests.Load(); n != 2 {
		t.Errorf("the second run downloaded again: %d requests", n)
	}
}

func TestFetchDefaultOut(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	if code, stdout, stderr := f.fetch(t); code != 0 || strings.TrimSpace(stdout) != filepath.Join(f.root, ".deps", "ffmpeg", "ffmpeg-9.0.2-r1-win-x64") {
		t.Errorf("exit %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}

func TestFetchRetries(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	f.fail.Store(2)
	out := t.TempDir()
	if code, _, stderr := f.fetch(t, "-out", out); code != 0 || strings.Count(stderr, "trying again") != 2 {
		t.Errorf("two 503s then success: exit %d, %q", code, stderr)
	}
	f.fail.Store(3)
	if err := os.RemoveAll(out); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.fetch(t, "-out", out); code != 1 || !strings.Contains(stderr, "503") {
		t.Errorf("three 503s: exit %d, %q", code, stderr)
	}
}

func TestFetchLocalZip(t *testing.T) {
	zipData := buildZip(t, nil)
	f := newFetchFixture(t, zipData)
	local := filepath.Join(t.TempDir(), "copy.zip")
	writeFile(t, local, string(zipData))
	out := t.TempDir()
	if code, _, stderr := f.fetch(t, "-out", out, "-zip", local); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if n := f.requests.Load(); n != 0 {
		t.Errorf("-zip made %d requests", n)
	}
	writeFile(t, local, string(zipData)+"x")
	if code, _, stderr := f.fetch(t, "-out", out, "-zip", local); code != 1 || !strings.Contains(stderr, "but lock.json has") {
		t.Errorf("a changed local zip: exit %d, %q", code, stderr)
	}
}

func TestFetchRefuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(files map[string]string)
		want   string
	}{
		{"changed DLL", func(m map[string]string) { m["bin/avutil-61.dll"] = "patched" }, "avutil-61.dll: SHA-256"},
		{"third DLL", func(m map[string]string) { m["bin/x264.dll"] = "x" }, "bin/ holds"},
		{"other major", func(m map[string]string) {
			m["bin/avcodec-62.dll"] = m["bin/avcodec-63.dll"]
			delete(m, "bin/avcodec-63.dll")
		}, "lock.json names avcodec-63.dll"},
		{"GPL build", func(m map[string]string) {
			m["ffmpeg-config.json"] = strings.Replace(m["ffmpeg-config.json"], "--disable-gpl", "--enable-gpl", 1)
		}, "contains --enable-gpl"},
		{"config majors", func(m map[string]string) {
			m["ffmpeg-config.json"] = strings.Replace(m["ffmpeg-config.json"], `"avutilMajor": 61`, `"avutilMajor": 60`, 1)
		}, "majors 63 and 60"},
		{"no headers", func(m map[string]string) { delete(m, "include/libavcodec/avcodec.h") }, "no include/libavcodec/avcodec.h"},
		{"no license", func(m map[string]string) { delete(m, "licenses/ffmpeg/COPYING.LGPLv2.1") }, "no licenses/ffmpeg/COPYING.LGPLv2.1"},
		{"entry outside the folder", func(m map[string]string) { m["other/x"] = "x" }, `"other/x" is not a file under`},
		{"parent entry", func(m map[string]string) { m["../evil"] = "x" }, `"../evil" is not a file under`},
		{"absolute entry", func(m map[string]string) { m["/evil"] = "x" }, `"/evil" is not a file under`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFetchFixture(t, buildZip(t, tc.mutate))
			out := t.TempDir()
			code, stdout, stderr := f.fetch(t, "-out", out)
			if code != 1 || !strings.Contains(stderr, tc.want) || stdout != "" {
				t.Errorf("exit %d, stdout %q, stderr %q; want exit 1 and %q", code, stdout, stderr, tc.want)
			}
			if _, err := os.Stat(filepath.Join(out, "ffmpeg-9.0.2-r1-win-x64")); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a refused build was unpacked: %v", err)
			}
			assertNoTemp(t, out)
		})
	}
}

func TestFetchHashMismatch(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	f.zip = append([]byte(nil), f.zip...)
	f.zip[len(f.zip)-1] ^= 1
	out := t.TempDir()
	if code, _, stderr := f.fetch(t, "-out", out); code != 1 || !strings.Contains(stderr, "but lock.json has") {
		t.Errorf("exit %d, %q", code, stderr)
	}
	if n := f.requests.Load(); n != 1 {
		t.Errorf("a hash mismatch was retried: %d requests", n)
	}
	entries, err := os.ReadDir(out)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("left behind: %v", entries)
	}
}

func TestFetchRefusesPlainHTTPRedirect(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	client := f.server.Client()
	client.CheckRedirect = newHTTPClient().CheckRedirect
	defer client.CloseIdleConnections()
	root, err := os.OpenRoot(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	_, err = download(context.Background(), client, f.server.URL+"/plain/ffmpeg-9.0.2-r1-win-x64.zip", root, "x.zip",
		hexSHA256(f.zip))
	if err == nil || !strings.Contains(err.Error(), "refusing a redirect to http://") {
		t.Errorf("error %v, want a refused redirect", err)
	}
}

func TestFetchNeedsCurrentLock(t *testing.T) {
	f := newFetchFixture(t, buildZip(t, nil))
	writeFile(t, filepath.Join(f.root, "native", "ffmpeg", "ffcheck.c"), "/* changed */\n")
	if code, _, stderr := f.fetch(t, "-out", t.TempDir()); code != 1 || !strings.Contains(stderr, errStaleLock.Error()) {
		t.Errorf("stale lock: exit %d, %q", code, stderr)
	}
	if err := os.Remove(filepath.Join(f.root, lockPath)); err != nil {
		t.Fatal(err)
	}
	if code, _, stderr := f.fetch(t, "-out", t.TempDir()); code != 1 || !strings.Contains(stderr, errNoLock.Error()) {
		t.Errorf("no lock: exit %d, %q", code, stderr)
	}
	if n := f.requests.Load(); n != 0 {
		t.Errorf("%d requests without a current lock", n)
	}
}

// assertNoTemp fails when unpackBuild left a temporary folder or a .part file in dir.
func assertNoTemp(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") || strings.HasSuffix(e.Name(), ".part") {
			t.Errorf("left behind: %s", e.Name())
		}
	}
}
