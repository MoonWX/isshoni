package native

import (
	"bytes"
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// The OpenH264 helpers against httptest (04 §18.3; README WA51), with a test pin of Size and SHA256:
//
//	testdata/openh264/good.dll.bz2      a 20 000-byte stand-in for the DLL, bzip2 -9
//	testdata/openh264/tampered.dll.bz2  the same with one bit flipped (same size, another hash)
//	testdata/openh264/bomb.bz2          785 bytes that expand to 1 GiB of zeros (head -c 1073741824 /dev/zero | bzip2 -9)

// fixture returns a testdata file and the pin its decompressed bytes satisfy (size, hash, the real pin's file name).
func fixture(t *testing.T, name string) (body []byte, pin OpenH264Release) {
	t.Helper()
	body, err := os.ReadFile(filepath.Join("testdata", "openh264", name))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(body)))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	sum := sha256.Sum256(raw)
	return body, OpenH264Release{Version: OpenH264.Version, SHA256: hex.EncodeToString(sum[:]), Size: int64(len(raw)),
		FileName: OpenH264.FileName}
}

// serve answers every request with body (or with status when it isn't 200) and sets pin.URL.
func serve(t *testing.T, pin *OpenH264Release, status int, body []byte) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	pin.URL = srv.URL + "/" + pin.FileName + ".bz2"
}

// entries lists dir's names.
func entries(t *testing.T, dir string) []string {
	t.Helper()
	des, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, de := range des {
		names = append(names, de.Name())
	}
	return names
}

func wantEmpty(t *testing.T, dir string) {
	t.Helper()
	if names := entries(t, dir); len(names) != 0 {
		t.Errorf("the folder holds %v after a failed install; want nothing", names)
	}
}

// TestInstallOpenH264Good: a good fixture installs as <dir>/<FileName>, verifies, and nothing else stays.
func TestInstallOpenH264Good(t *testing.T) {
	body, pin := fixture(t, "good.dll.bz2")
	serve(t, &pin, http.StatusOK, body)
	dir := filepath.Join(t.TempDir(), "codecs") // created by the install
	path, err := installOpenH264(context.Background(), nil, dir, pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if path != filepath.Join(dir, pin.FileName) || !filepath.IsAbs(path) {
		t.Errorf("path %q", path)
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != pin.FileName {
		t.Errorf("folder %v, want only %s", names, pin.FileName)
	}
	if err := verifyOpenH264(path, pin, openFile); err != nil {
		t.Errorf("verify after install: %v", err)
	}
}

// TestInstallOpenH264Rejects: a wrong hash (a tampered file), a body one byte short and one byte long, a damaged
// stream, an HTTP error and a compressed body over 10 MiB each fail and leave the folder empty.
func TestInstallOpenH264Rejects(t *testing.T) {
	good, goodPin := fixture(t, "good.dll.bz2")
	tampered, tamperedPin := fixture(t, "tampered.dll.bz2")
	if tamperedPin.Size != goodPin.Size || tamperedPin.SHA256 == goodPin.SHA256 {
		t.Fatalf("the tampered fixture must have the good one's size and another hash")
	}
	// Repeated empty bzip2 streams: a valid body that decompresses to nothing, however long it is.
	emptyStream := []byte{0x42, 0x5a, 0x68, 0x39, 0x17, 0x72, 0x45, 0x38, 0x50, 0x90, 0, 0, 0, 0}
	huge := bytes.Repeat(emptyStream, (maxOpenH264Compressed+len(emptyStream))/len(emptyStream)+1)
	if len(huge) <= maxOpenH264Compressed {
		t.Fatal("the huge body is not over the limit")
	}
	damaged := append([]byte(nil), good...)
	damaged[len(damaged)/2] ^= 0xFF

	short := goodPin
	short.Size++ // the body is one byte short of the pin
	long := goodPin
	long.Size-- // the body is one byte longer than the pin
	hugePin := goodPin
	hugePin.Size = 20 << 20 // not what stops it: the compressed limit is

	cases := []struct {
		name   string
		pin    OpenH264Release
		status int
		body   []byte
		code   media.ErrorCode
		reason string
	}{
		{"tampered", goodPin, http.StatusOK, tampered, media.CodeDependency, reasonCorrupt},
		{"short", short, http.StatusOK, good, media.CodeDependency, reasonCorrupt},
		{"long", long, http.StatusOK, good, media.CodeDependency, reasonCorrupt},
		{"damaged", goodPin, http.StatusOK, damaged, media.CodeDependency, reasonCorrupt},
		{"not bzip2", goodPin, http.StatusOK, []byte("<html>a captive portal</html>"), media.CodeDependency, reasonCorrupt},
		{"http 404", goodPin, http.StatusNotFound, nil, media.CodeDependency, reasonDownload},
		{"over 10 MiB", hugePin, http.StatusOK, huge, media.CodeDependency, reasonCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pin := c.pin
			serve(t, &pin, c.status, c.body)
			dir := t.TempDir()
			path, err := installOpenH264(context.Background(), nil, dir, pin, nil)
			if path != "" {
				t.Errorf("path %q on failure", path)
			}
			detail := wantError(t, err, c.code, c.reason, "install_openh264")
			if c.name == "over 10 MiB" && !strings.Contains(detail, "10 MiB") {
				t.Errorf("detail %q: want the compressed limit", detail)
			}
			wantEmpty(t, dir)
		})
	}
}

// countingWriter counts the bytes written through it.
type countingWriter struct {
	w io.Writer
	n int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// TestInstallOpenH264Bomb: a few hundred bytes that expand to 1 GiB stop after at most Size + 1 bytes were written,
// the temp file is gone and the heap grows by less than 16 MB (04 §18.3). Not parallel: it reads the process's
// allocation counter.
func TestInstallOpenH264Bomb(t *testing.T) {
	bomb, err := os.ReadFile(filepath.Join("testdata", "openh264", "bomb.bz2"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bomb) > 4096 {
		t.Fatalf("the bomb fixture is %d bytes; want a few hundred", len(bomb))
	}
	pin := OpenH264 // the real Size: the limit a real install uses
	serve(t, &pin, http.StatusOK, bomb)
	dir := t.TempDir()
	var cw *countingWriter
	wrap := func(w io.Writer) io.Writer {
		cw = &countingWriter{w: w}
		return cw
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	path, err := installOpenH264(context.Background(), nil, dir, pin, wrap)
	runtime.ReadMemStats(&after)
	if path != "" {
		t.Errorf("path %q", path)
	}
	wantError(t, err, media.CodeDependency, reasonCorrupt, "install_openh264")
	if cw == nil {
		t.Fatal("the temp file was never written")
	}
	if cw.n > pin.Size+1 {
		t.Errorf("%d bytes written to the temp file; want at most Size + 1 = %d", cw.n, pin.Size+1)
	}
	if cw.n != pin.Size+1 {
		t.Logf("%d bytes written (Size + 1 = %d)", cw.n, pin.Size+1)
	}
	wantEmpty(t, dir)
	// Everything allocated during the install, the bzip2 block and the copy buffers included: an upper bound of the
	// heap's growth.
	if grew := after.TotalAlloc - before.TotalAlloc; grew >= 16<<20 {
		t.Errorf("the install allocated %d bytes; want less than 16 MB", grew)
	} else {
		t.Logf("the install allocated %d bytes", grew)
	}
}

// TestInstallOpenH264Context: a cancelled context is cancelled, a slow server ends as timeout, and both leave the
// folder empty.
func TestInstallOpenH264Context(t *testing.T) {
	good, pin := fixture(t, "good.dll.bz2")
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(good[:100])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		select { // stalls mid-body until the client gives up or the test ends
		case <-r.Context().Done():
		case <-release:
		}
	}))
	t.Cleanup(func() {
		close(release)
		srv.Close()
	})
	pin.URL = srv.URL + "/x.bz2"

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	_, err := installOpenH264(ctx, nil, dir, pin, nil)
	wantError(t, err, media.CodeTimeout, "", "install_openh264")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("%v does not unwrap to DeadlineExceeded", err)
	}
	wantEmpty(t, dir)

	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	_, err = installOpenH264(ctx2, nil, dir, pin, nil)
	wantError(t, err, media.CodeCancelled, "", "install_openh264")
	wantEmpty(t, dir)

	_, err = installOpenH264(context.Background(), nil, "", pin, nil)
	wantError(t, err, media.CodeInvalid, "", "install_openh264")
}

// TestInstallOpenH264CutOff: a body that breaks off before its Content-Length (the connection closed mid-transfer)
// is a failed download (trying again may help), while a body that arrives whole but holds a stream that ends early
// is corrupt; both leave the folder empty.
func TestInstallOpenH264CutOff(t *testing.T) {
	good, pin := fixture(t, "good.dll.bz2")
	half := good[:len(good)/2]
	cases := []struct {
		name          string
		contentLength bool
		reason        string
	}{
		{"cut off before Content-Length", true, reasonDownload},
		{"whole body, stream ends early", false, reasonCorrupt},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if c.contentLength {
					// More than is sent: the server closes the connection when the handler returns short.
					w.Header().Set("Content-Length", strconv.Itoa(len(good)))
				}
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(half)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			t.Cleanup(srv.Close)
			p := pin
			p.URL = srv.URL + "/x.bz2"
			dir := t.TempDir()
			path, err := installOpenH264(context.Background(), nil, dir, p, nil)
			if path != "" {
				t.Errorf("path %q on failure", path)
			}
			wantError(t, err, media.CodeDependency, c.reason, "install_openh264")
			wantEmpty(t, dir)
		})
	}
}

// TestInstallOpenH264ReplacesOld: a second install over an existing file replaces it.
func TestInstallOpenH264ReplacesOld(t *testing.T) {
	body, pin := fixture(t, "good.dll.bz2")
	serve(t, &pin, http.StatusOK, body)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, pin.FileName), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	path, err := installOpenH264(context.Background(), nil, dir, pin, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyOpenH264(path, pin, openFile); err != nil {
		t.Error(err)
	}
}

func openFile(p string) (io.ReadCloser, error) { return os.Open(p) }

// TestVerifyOpenH264: a good file passes; a file of the wrong size fails without being read; a wrong hash, a
// missing file and a folder fail.
func TestVerifyOpenH264(t *testing.T) {
	body, pin := fixture(t, "good.dll.bz2")
	raw, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good.dll")
	if err := os.WriteFile(good, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	opened := 0
	counting := func(p string) (io.ReadCloser, error) {
		opened++
		return os.Open(p)
	}
	if err := verifyOpenH264(good, pin, counting); err != nil {
		t.Errorf("good file: %v", err)
	}
	if opened != 1 {
		t.Errorf("the good file was opened %d times", opened)
	}

	opened = 0
	wrongSize := filepath.Join(dir, "short.dll")
	if err := os.WriteFile(wrongSize, raw[:len(raw)-1], 0o600); err != nil {
		t.Fatal(err)
	}
	wantError(t, verifyOpenH264(wrongSize, pin, counting), media.CodeDependency, reasonCorrupt, "verify_openh264")
	if opened != 0 {
		t.Error("a file of the wrong size was read; want a rejection from its size alone")
	}

	bad := append([]byte(nil), raw...)
	bad[0] ^= 1
	wrongHash := filepath.Join(dir, "bad.dll")
	if err := os.WriteFile(wrongHash, bad, 0o600); err != nil {
		t.Fatal(err)
	}
	wantError(t, verifyOpenH264(wrongHash, pin, openFile), media.CodeDependency, reasonCorrupt, "verify_openh264")
	wantError(t, verifyOpenH264(filepath.Join(dir, "none.dll"), pin, openFile), media.CodeDependency, tokenMissing,
		"verify_openh264")
	wantError(t, verifyOpenH264(dir, pin, openFile), media.CodeDependency, tokenMissing, "verify_openh264")
	// The exported form uses the real pin: the stand-in is not Cisco's DLL.
	wantError(t, VerifyOpenH264(good), media.CodeDependency, reasonCorrupt, "verify_openh264")
}

// TestRemoveOpenH264: the pinned file, another release's and a leftover temp file go; other files stay; a missing
// folder is fine.
func TestRemoveOpenH264(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{OpenH264.FileName, "openh264-2.5.0-win64.dll", ".openh264-123.tmp", "keep.txt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := RemoveOpenH264(dir); err != nil {
		t.Fatal(err)
	}
	if names := entries(t, dir); len(names) != 1 || names[0] != "keep.txt" {
		t.Errorf("folder %v, want only keep.txt", names)
	}
	if err := RemoveOpenH264(filepath.Join(dir, "missing")); err != nil {
		t.Errorf("a missing folder: %v", err)
	}
	wantError(t, RemoveOpenH264(""), media.CodeInvalid, "", "remove_openh264")
}

// TestSetOpenH264Path: the file is verified before the DLL sees it (a bad one makes no call), a good one goes in as
// an absolute path, "" goes as NULL, and the engine's failures map to dependency with their token.
func TestSetOpenH264Path(t *testing.T) {
	f := newFakeCaller(t)
	var got []string
	f.set(procSetOpenH264Path, func(args []uintptr) (uintptr, string) {
		if args[0] == 0 {
			got = append(got, "<NULL>")
			return fakeResult(imOK), ""
		}
		p := fakeCString(args[0])
		got = append(got, p)
		if strings.HasSuffix(p, "old.dll") {
			return fakeResult(imErrDependency), "version: openh264: OpenH264 2.5.0, want 2.7.0"
		}
		return fakeResult(imOK), ""
	})
	e := openFake(t, f, Options{})

	dir := t.TempDir()
	missing := filepath.Join(dir, OpenH264.FileName)
	wantError(t, e.SetOpenH264Path(missing), media.CodeDependency, tokenMissing, "set_openh264_path")
	if err := os.WriteFile(missing, []byte("not cisco's"), 0o600); err != nil {
		t.Fatal(err)
	}
	wantError(t, e.SetOpenH264Path(missing), media.CodeDependency, reasonCorrupt, "set_openh264_path")
	if f.count(procSetOpenH264Path) != 0 {
		t.Fatalf("an unverified file reached the DLL: %v", got)
	}

	if err := e.SetOpenH264Path(""); err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0] != "<NULL>" {
		t.Errorf("calls %v, want NULL", got)
	}

	// A file that passes VerifyOpenH264 needs the real DLL's bytes; with the pin swapped for a stand-in's, the path
	// that reaches the DLL is the absolute one, and the engine's version answer maps to dependency/version.
	body, pin := fixture(t, "good.dll.bz2")
	raw, err := io.ReadAll(bzip2.NewReader(bytes.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	saved := OpenH264
	t.Cleanup(func() { OpenH264 = saved })
	OpenH264 = pin
	for _, n := range []string{"new.dll", "old.dll"} {
		if err := os.WriteFile(filepath.Join(dir, n), raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Chdir(dir)
	if err := e.SetOpenH264Path("new.dll"); err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(dir, "new.dll"); len(got) != 2 || !samePath(got[1], want) {
		t.Errorf("calls %v, want the absolute %s", got, want)
	}
	wantError(t, e.SetOpenH264Path("old.dll"), media.CodeDependency, tokenVersion, "set_openh264_path")

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	calls := f.count(procSetOpenH264Path)
	wantError(t, e.SetOpenH264Path(""), media.CodeInternal, tokenNotInitialized, "set_openh264_path")
	if f.count(procSetOpenH264Path) != calls {
		t.Error("a call after Close reached the DLL")
	}
}

// samePath compares paths after resolving symlinks (macOS's temp folder is behind /var → /private/var).
func samePath(a, b string) bool {
	ra, errA := filepath.EvalSymlinks(a)
	rb, errB := filepath.EvalSymlinks(b)
	return errA == nil && errB == nil && ra == rb
}

// TestOpenH264PinMatchesEngine: the Go pin is the engine's (04 §12.5: "a unit test reads both"): the version equals
// codec_ver.h's OPENH264_MAJOR.MINOR.REVISION (the headers compiled into isshoni-media.dll) and openh264_pin.h's
// IM_OPENH264_VERSION, and the URL, file name, size and SHA-256 equal openh264_pin.h's, which openh264_test checks
// against Cisco's real download on CI windows.
func TestOpenH264PinMatchesEngine(t *testing.T) {
	root := filepath.Join("..", "..", "..", "native", "windows")
	ver, err := os.ReadFile(filepath.Join(root, "third_party", "openh264", "codec_ver.h"))
	if err != nil {
		t.Fatal(err)
	}
	define := func(name string) string {
		m := regexp.MustCompile(`(?m)^#define\s+` + name + `\s+\(?(\d+)\)?\s*$`).FindSubmatch(ver)
		if m == nil {
			t.Fatalf("codec_ver.h has no %s", name)
		}
		return string(m[1])
	}
	if hdr := define("OPENH264_MAJOR") + "." + define("OPENH264_MINOR") + "." + define("OPENH264_REVISION"); hdr != OpenH264.Version {
		t.Errorf("codec_ver.h says %s, the Go pin %s", hdr, OpenH264.Version)
	}

	pinH, err := os.ReadFile(filepath.Join(root, "src", "encode", "openh264_pin.h"))
	if err != nil {
		t.Fatal(err)
	}
	str := func(re string) string {
		m := regexp.MustCompile(re).FindSubmatch(pinH)
		if m == nil {
			t.Fatalf("openh264_pin.h: no match for %s", re)
		}
		return string(m[1])
	}
	checks := []struct{ name, engine, goPin string }{
		{"IM_OPENH264_VERSION", str(`#define IM_OPENH264_VERSION "([^"]+)"`), OpenH264.Version},
		{"kPinMajor.kPinMinor.kPinRevision", str(`kPinMajor = (\d+);`) + "." + str(`kPinMinor = (\d+);`) + "." +
			str(`kPinRevision = (\d+);`), OpenH264.Version},
		{"kOpenH264Url", str(`kOpenH264Url = "([^"]+)"`), OpenH264.URL},
		{"kOpenH264FileName", str(`kOpenH264FileName = "([^"]+)"`), OpenH264.FileName},
		{"kOpenH264Size", str(`kOpenH264Size = (\d+);`), strconv.FormatInt(OpenH264.Size, 10)},
		{"kOpenH264Sha256", str(`kOpenH264Sha256 = "([0-9a-f]+)"`), OpenH264.SHA256},
	}
	for _, c := range checks {
		if c.engine != c.goPin {
			t.Errorf("openh264_pin.h's %s is %q, the Go pin's %q", c.name, c.engine, c.goPin)
		}
	}
	if !strings.HasSuffix(OpenH264.URL, "/"+OpenH264.FileName+".bz2") || !strings.HasPrefix(OpenH264.URL, "https://") {
		t.Errorf("URL %q: want https and the file name", OpenH264.URL)
	}
	if !strings.Contains(OpenH264.FileName, OpenH264.Version) || len(OpenH264.SHA256) != 64 || OpenH264.Size <= 0 {
		t.Errorf("pin %v", OpenH264)
	}
}
