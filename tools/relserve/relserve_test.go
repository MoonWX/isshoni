package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	testV1      = "0.0.1-ci.1"
	testV2      = "0.0.1-ci.2"
	testArchive = "isshoni_0.0.1-ci.1_linux_amd64.tar.gz"
)

// releaseTree writes a release directory with two versions in the main root and one in a mirror, and returns it.
func releaseTree(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	files := map[string]string{
		"download/v" + testV1 + "/" + testArchive:          "archive one",
		"download/v" + testV1 + "/checksums.txt":           "abc  " + testArchive + "\n",
		"download/v" + testV1 + "/checksums.txt.sig":       "signature",
		"download/v" + testV2 + "/install.sh":              "#!/bin/sh\n",
		"mirror/a/download/v" + testV1 + "/" + testArchive: "a swapped archive",
		"mirror/a/download/v" + testV1 + "/checksums.txt":  "abc  " + testArchive + "\n",
		"notes.txt": "outside every download directory",
	}
	for name, content := range files {
		path := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// testHandler returns the handler over a fresh release tree, and that tree.
func testHandler(t *testing.T, latest string) (http.Handler, string) {
	t.Helper()
	dir := releaseTree(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = root.Close() })
	return newHandler(root, latest, io.Discard), dir
}

// answer is what the handler sent for one request.
type answer struct {
	status int
	header http.Header
	length int64 // the Content-Length header, -1 when there is none
	body   string
}

func do(t *testing.T, h http.Handler, method, target string) answer {
	t.Helper()
	req := httptest.NewRequestWithContext(t.Context(), method, "https://relserve.test:8443"+target, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	res := rec.Result()
	defer func() { _ = res.Body.Close() }()
	return answer{status: res.StatusCode, header: res.Header, length: res.ContentLength, body: readAll(t, res.Body)}
}

func readAll(t *testing.T, r io.Reader) string {
	t.Helper()
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// githubLatest is the shape of the Location that github.com sends for /OWNER/REPO/releases/latest, an absolute
// https URL that ends in /releases/tag/v<version>. The installer takes the version from what follows "/tag/v"
// (06 §4.3), and relserve's release root stands for the ".../releases" part.
var githubLatest = regexp.MustCompile(`^https://[^/]+(/[^/?#]+)*/tag/v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$`)

func TestLatestRedirect(t *testing.T) {
	h, _ := testHandler(t, testV2)
	for _, tt := range []struct {
		name, target, want string
	}{
		{"main root", "/latest", "https://relserve.test:8443/tag/v" + testV2},
		{"mirror", "/mirror/a/latest", "https://relserve.test:8443/mirror/a/tag/v" + testV2},
		{"latest download", "/latest/download/install.sh", "https://relserve.test:8443/download/v" + testV2 + "/install.sh"},
		{"mirror latest download", "/mirror/a/latest/download/x.txt", "https://relserve.test:8443/mirror/a/download/v" + testV2 + "/x.txt"},
	} {
		for _, method := range []string{http.MethodGet, http.MethodHead} {
			t.Run(tt.name+" "+method, func(t *testing.T) {
				res := do(t, h, method, tt.target)
				if res.status != http.StatusFound {
					t.Fatalf("status = %d, want 302 as GitHub answers", res.status)
				}
				got := res.header.Get("Location")
				if got != tt.want {
					t.Fatalf("Location = %q, want %q", got, tt.want)
				}
				if strings.HasSuffix(tt.target, "/latest") && !githubLatest.MatchString(got) {
					t.Errorf("Location %q does not have the form of GitHub's /releases/latest redirect", got)
				}
			})
		}
	}
}

func TestLatestWithoutAVersion(t *testing.T) {
	h, _ := testHandler(t, "")
	for _, target := range []string{"/latest", "/latest/download/install.sh"} {
		if res := do(t, h, http.MethodGet, target); res.status != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404 without -latest", target, res.status)
		}
	}
}

func TestFilesServed(t *testing.T) {
	h, _ := testHandler(t, testV2)

	res := do(t, h, http.MethodGet, "/download/v"+testV1+"/"+testArchive)
	if res.status != http.StatusOK {
		t.Fatalf("GET archive = %d, want 200", res.status)
	}
	if got := res.body; got != "archive one" {
		t.Errorf("archive body = %q", got)
	}
	if got := res.header.Get("Content-Type"); got != "application/octet-stream" {
		t.Errorf("Content-Type = %q, want application/octet-stream", got)
	}

	res = do(t, h, http.MethodHead, "/download/v"+testV1+"/checksums.txt.sig")
	if res.status != http.StatusOK || res.length != int64(len("signature")) {
		t.Errorf("HEAD signature = %d with length %d, want 200 with length %d", res.status, res.length, len("signature"))
	}
	if got := res.body; got != "" {
		t.Errorf("HEAD has a body: %q", got)
	}

	res = do(t, h, http.MethodGet, "/mirror/a/download/v"+testV1+"/"+testArchive)
	if got := res.body; res.status != http.StatusOK || got != "a swapped archive" {
		t.Errorf("GET mirror archive = %d %q, want the mirror's own file", res.status, got)
	}

	// A mirror has only what its directory has: nothing falls back to the main root.
	res = do(t, h, http.MethodGet, "/mirror/a/download/v"+testV1+"/checksums.txt.sig")
	if res.status != http.StatusNotFound {
		t.Errorf("GET a file the mirror lacks = %d, want 404", res.status)
	}
}

func TestNotServed(t *testing.T) {
	h, dir := testHandler(t, testV2)
	// A link out of the tree must not be followed, and neither must one to a file inside a download directory
	// through a path that is not that file's URL.
	outside := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "download", "v"+testV1, "link")); err != nil {
		t.Skipf("no symbolic links here: %v", err)
	}

	for _, target := range []string{
		"/",
		"/notes.txt",
		"/download",
		"/download/",
		"/download/v" + testV1,
		"/download/v" + testV1 + "/",
		"/download/v" + testV1 + "/missing.txt",
		"/download/v9.9.9/" + testArchive,
		"/download/v" + testV1 + "/link",
		"/download/v" + testV1 + "/../v" + testV1 + "/checksums.txt",
		"/download/../notes.txt",
		"/download//v" + testV1 + "/checksums.txt",
		"/nomirror/download/v" + testV1 + "/checksums.txt",
		"/mirror/download/v" + testV1 + "/checksums.txt",
		"/tag/v9.9.9",
		"/tag/" + testV1,
		"/tag/vnot-a-version",
		"/mirror/a/tag/v" + testV2,
	} {
		if res := do(t, h, http.MethodGet, target); res.status != http.StatusNotFound {
			t.Errorf("GET %s = %d %q, want 404", target, res.status, res.body)
		}
	}

	res := do(t, h, http.MethodPost, "/download/v"+testV1+"/checksums.txt")
	if res.status != http.StatusMethodNotAllowed || res.header.Get("Allow") != "GET, HEAD" {
		t.Errorf("POST = %d (Allow %q), want 405 with Allow: GET, HEAD", res.status, res.header.Get("Allow"))
	}
}

func TestTagPage(t *testing.T) {
	h, _ := testHandler(t, testV2)
	// curl -fsSLI follows /latest to this page with HEAD and fails on anything but a success (06 §4.3).
	for _, method := range []string{http.MethodGet, http.MethodHead} {
		res := do(t, h, method, "/tag/v"+testV2)
		if res.status != http.StatusOK {
			t.Errorf("%s /tag/v%s = %d, want 200", method, testV2, res.status)
		}
	}
	if res := do(t, h, http.MethodGet, "/mirror/a/tag/v"+testV1); res.status != http.StatusOK {
		t.Errorf("GET the mirror's tag page = %d, want 200", res.status)
	}
}

func TestRequestLog(t *testing.T) {
	dir := releaseTree(t)
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	var log bytes.Buffer
	h := newHandler(root, testV2, &log)
	do(t, h, http.MethodGet, "/download/v"+testV1+"/checksums.txt")
	do(t, h, http.MethodHead, "/latest")
	do(t, h, http.MethodGet, "/nothing")
	do(t, h, http.MethodGet, "/nothing%0Arelserve:%20GET%20%22/forged%22%20200")
	want := `relserve: GET "/download/v` + testV1 + `/checksums.txt" 200` + "\n" +
		`relserve: HEAD "/latest" 302` + "\n" +
		`relserve: GET "/nothing" 404` + "\n" +
		// A line break in a path stays inside the quotes of its own line.
		`relserve: GET "/nothing\nrelserve: GET \"/forged\" 200" 404` + "\n"
	if log.String() != want {
		t.Errorf("request log =\n%s\nwant\n%s", log.String(), want)
	}
}

func TestAuthority(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ca, err := newAuthority([]string{"localhost", "host.containers.internal", "127.0.0.1", "::1"}, now)
	if err != nil {
		t.Fatal(err)
	}
	block, rest := pem.Decode(ca.certPEM)
	if block == nil || block.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		t.Fatalf("the CA output is not one PEM certificate")
	}
	caCert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if !caCert.IsCA || caCert.KeyUsage&x509.KeyUsageCertSign == 0 {
		t.Errorf("the CA certificate is not marked as a CA that may sign")
	}
	leaf, err := x509.ParseCertificate(ca.server.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	if leaf.IsCA {
		t.Errorf("the server certificate is a CA")
	}
	pool := x509.NewCertPool()
	pool.AddCert(caCert)
	for _, name := range []string{"localhost", "host.containers.internal", "127.0.0.1", "::1"} {
		_, err := leaf.Verify(x509.VerifyOptions{
			Roots: pool, DNSName: name, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		if err != nil {
			t.Errorf("the server certificate does not verify for %s: %v", name, err)
		}
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "example.com", CurrentTime: now}); err == nil {
		t.Errorf("the server certificate verifies for a name it was not made for")
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: "localhost", CurrentTime: now.Add(certLifetime + time.Hour)}); err == nil {
		t.Errorf("the server certificate still verifies after its lifetime")
	}
	if _, err := newAuthority(nil, now); err == nil {
		t.Errorf("newAuthority without hosts succeeded")
	}
}

// lockedBuffer is a bytes.Buffer that the server's goroutines and the test may use at the same time.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// TestRunServesOverTLS starts the whole program on a free port and reads a file the way the installer's downloader
// does: over HTTPS, trusting only the CA certificate that relserve wrote.
func TestRunServesOverTLS(t *testing.T) {
	dir := releaseTree(t)
	out := t.TempDir()
	caFile, readyFile := filepath.Join(out, "ca.pem"), filepath.Join(out, "addr")

	ctx, cancel := context.WithCancel(t.Context())
	var stdout, stderr lockedBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(ctx, []string{
			"-dir", dir, "-addr", "127.0.0.1:0", "-latest", testV2, "-ca-out", caFile, "-ready-file", readyFile,
		}, &stdout, &stderr)
	}()
	stopped := false
	stop := func() int {
		t.Helper()
		cancel()
		select {
		case code := <-done:
			stopped = true
			return code
		case <-time.After(10 * time.Second):
			t.Fatalf("relserve did not stop; stderr:\n%s", stderr.String())
			return -1
		}
	}
	defer func() {
		if !stopped {
			stop()
		}
	}()

	var addr string
	deadline := time.Now().Add(10 * time.Second)
	for {
		b, err := os.ReadFile(readyFile)
		if err == nil {
			addr = strings.TrimSpace(string(b))
			break
		}
		select {
		case code := <-done:
			stopped = true
			t.Fatalf("relserve exited with %d before it was ready; stderr:\n%s", code, stderr.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("no ready file after 10 s; stderr:\n%s", stderr.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if strings.HasSuffix(addr, ":0") || !strings.HasPrefix(addr, "127.0.0.1:") {
		t.Fatalf("ready file holds %q, want the bound 127.0.0.1:PORT", addr)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("%s holds no certificate", caFile)
	}
	trusting := &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}},
		// The installer reads the Location of /latest itself.
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	defer trusting.CloseIdleConnections()
	get := func(c *http.Client, target string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+addr+target, nil)
		if err != nil {
			t.Fatal(err)
		}
		return c.Do(req)
	}

	res, err := get(trusting, "/download/v"+testV1+"/"+testArchive)
	if err != nil {
		t.Fatalf("GET with the CA trusted: %v", err)
	}
	got := readAll(t, res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || got != "archive one" {
		t.Errorf("GET archive = %d %q, want 200 and the file", res.StatusCode, got)
	}
	if res.TLS == nil || res.TLS.Version < tls.VersionTLS12 {
		t.Errorf("the connection is not TLS 1.2 or newer")
	}

	res, err = get(trusting, "/latest")
	if err != nil {
		t.Fatalf("GET /latest: %v", err)
	}
	_ = res.Body.Close()
	if want := "https://" + addr + "/tag/v" + testV2; res.StatusCode != http.StatusFound || res.Header.Get("Location") != want {
		t.Errorf("GET /latest = %d to %q, want 302 to %q", res.StatusCode, res.Header.Get("Location"), want)
	}

	// Without the CA the certificate must not verify: the tests really depend on the trust they set up.
	empty := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}}}
	defer empty.CloseIdleConnections()
	res, err = get(empty, "/latest")
	if err == nil {
		_ = res.Body.Close()
		t.Fatalf("GET without the CA succeeded")
	}
	var unknown x509.UnknownAuthorityError
	if !errors.As(err, &unknown) {
		t.Errorf("GET without the CA failed with %v, want an unknown-authority error", err)
	}

	if !strings.Contains(stdout.String(), "https://"+addr) {
		t.Errorf("stdout does not name the address: %q", stdout.String())
	}
	if code := stop(); code != 0 {
		t.Errorf("exit code after a stop = %d, want 0; stderr:\n%s", code, stderr.String())
	}
	if !strings.Contains(stderr.String(), `relserve: GET "/download/v`+testV1+"/"+testArchive+`" 200`) {
		t.Errorf("the request log lacks the download; stderr:\n%s", stderr.String())
	}
}

func TestRunUsage(t *testing.T) {
	dir := releaseTree(t)
	ca := filepath.Join(t.TempDir(), "ca.pem")
	for _, tt := range []struct {
		name     string
		args     []string
		wantCode int
		wantErr  string
	}{
		{"no dir", []string{"-ca-out", ca}, 2, "missing -dir"},
		{"no ca-out", []string{"-dir", dir}, 2, "missing -ca-out"},
		{"latest with v", []string{"-dir", dir, "-ca-out", ca, "-latest", "v1.2.3"}, 2, "is not a version"},
		{"latest not semver", []string{"-dir", dir, "-ca-out", ca, "-latest", "1.2"}, 2, "is not a version"},
		{"no hosts", []string{"-dir", dir, "-ca-out", ca, "-hosts", " , "}, 2, "names no host"},
		{"extra argument", []string{"-dir", dir, "-ca-out", ca, "extra"}, 2, "unexpected argument"},
		{"unknown flag", []string{"-nope"}, 2, "flag provided but not defined"},
		{"help", []string{"-h"}, 0, "usage: relserve"},
		{"missing directory", []string{"-dir", filepath.Join(dir, "nope"), "-ca-out", ca}, 1, "-dir:"},
		{"address in use by nothing valid", []string{"-dir", dir, "-ca-out", ca, "-addr", "256.0.0.1:1"}, 1, "-addr:"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := run(t.Context(), tt.args, &stdout, &stderr)
			if code != tt.wantCode {
				t.Errorf("exit code = %d, want %d; stderr:\n%s", code, tt.wantCode, stderr.String())
			}
			if !strings.Contains(stderr.String(), tt.wantErr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantErr)
			}
		})
	}
}

func TestValidVersion(t *testing.T) {
	for v, want := range map[string]bool{
		"0.1.0": true, "0.0.1-ci.2": true, "1.2.3-rc.1": true,
		"": false, "v0.1.0": false, "1.2": false, "1.2.3+build": false, "latest": false, "1.2.3/..": false,
	} {
		if got := validVersion(v); got != want {
			t.Errorf("validVersion(%q) = %v, want %v", v, got, want)
		}
	}
}
