package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/goleak"
)

// TestMain fails the tests when a goroutine outlives them (the clock server, the measurement's ticker).
func TestMain(m *testing.M) {
	goleak.VerifyTestMain(m)
}

func TestUsage(t *testing.T) {
	for _, tc := range []struct {
		args []string
		code int
		out  string // in stdout or stderr
	}{
		{nil, 2, "usage: g2g serve"},
		{[]string{"help"}, 0, "usage: g2g serve"},
		{[]string{"frobnicate"}, 2, `unknown command "frobnicate"`},
		{[]string{"version"}, 0, "g2g "},
		{[]string{"serve", "-h"}, 0, "-port"},
		{[]string{"serve", "-port", "70000"}, 2, "is not a port"},
		{[]string{"serve", "extra"}, 2, `unexpected argument "extra"`},
		{[]string{"measure", "-h"}, 0, "-server"},
		{[]string{"measure", "-bogus"}, 2, "flag provided but not defined"},
		{[]string{"measure", "-run", "fast"}, 2, `-run "fast" is not one of gate, nosound, nocuts`},
		{[]string{"measure", "-d", "500ms"}, 2, "shorter than a second"},
		{[]string{"measure", "-server", "isshoni.example"}, 2, "-server must be the server's URL"},
	} {
		var stdout, stderr bytes.Buffer
		code := run(context.Background(), tc.args, &stdout, &stderr)
		if code != tc.code || !strings.Contains(stdout.String()+stderr.String(), tc.out) {
			t.Errorf("g2g %v: exit %d, output %q; want %d and %q", tc.args, code, stdout.String()+stderr.String(), tc.code,
				tc.out)
		}
	}
}

// TestMeasureNeedsWindows: elsewhere, measure says why it can't run, and the result is inconclusive.
func TestMeasureNeedsWindows(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("measure captures the screen on Windows")
	}
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"measure"}, io.Discard, &stderr); code != exitInconclusive ||
		!strings.Contains(stderr.String(), "runs on Windows only") {
		t.Errorf("exit %d, stderr %q", code, stderr.String())
	}
}

// TestMeasureOutputFirst: a result file that can't be created fails before anything is measured.
func TestMeasureOutputFirst(t *testing.T) {
	old := openCapturer
	defer func() { openCapturer = old }()
	opened := false
	openCapturer = func() (capturer, error) { opened = true; return nil, errNoScreen }
	out := filepath.Join(t.TempDir(), "missing", "result.json")
	var stderr bytes.Buffer
	if code := run(context.Background(), []string{"measure", "-o", out}, io.Discard, &stderr); code != exitInconclusive ||
		opened || !strings.Contains(stderr.String(), "result.json") {
		t.Errorf("exit %d, capture opened %v, stderr %q; want 2 before the capture", code, opened, stderr.String())
	}
	// A run that can't measure leaves no result file behind.
	out = filepath.Join(t.TempDir(), "result.json")
	if code := run(context.Background(), []string{"measure", "-o", out}, io.Discard, io.Discard); code != exitInconclusive {
		t.Errorf("exit %d, want 2", code)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the result file is there after a run that could not measure: %v", err)
	}
}

var errNoScreen = errors.New("no screen")

// syncBuffer is a bytes.Buffer that the server goroutine and the test can share.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestServe: `g2g serve` serves the clock page on 127.0.0.1 and stops with exit 0 when interrupted.
func TestServe(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve", "-port", "0"}, &stdout, &stderr) }()
	addr := regexp.MustCompile(`http://(127\.0\.0\.1:\d+)/`)
	var base string
	for deadline := time.Now().Add(10 * time.Second); base == "" && time.Now().Before(deadline); {
		if m := addr.FindStringSubmatch(stdout.String()); m != nil {
			base = "http://" + m[1]
			break
		}
		select {
		case code := <-done:
			t.Fatalf("serve exited with %d: %s", code, stderr.String())
		case <-time.After(10 * time.Millisecond):
		}
	}
	if base == "" {
		t.Fatalf("serve printed no address: %q", stdout.String())
	}
	get := func(method, path string) (int, http.Header, []byte) {
		req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		b, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, res.Header, b
	}
	for _, path := range []string{"/", "/clock.html", "/?cuts=0"} {
		code, header, body := get(http.MethodGet, path)
		if code != http.StatusOK || !bytes.Equal(body, clockPage) {
			t.Errorf("GET %s: %d, %d bytes; want 200 and the clock page", path, code, len(body))
		}
		for h, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-store",
			"Content-Security-Policy": clockCSP,
			"Content-Length":          strconv.Itoa(len(clockPage)),
		} {
			if got := header.Get(h); got != want {
				t.Errorf("GET %s: %s %q, want %q", path, h, got, want)
			}
		}
	}
	for path, want := range map[string]int{"/favicon.ico": 404, "/web/clock.html": 404} {
		if code, _, _ := get(http.MethodGet, path); code != want {
			t.Errorf("GET %s: %d, want %d", path, code, want)
		}
	}
	if code, _, _ := get(http.MethodPost, "/"); code != http.StatusMethodNotAllowed {
		t.Errorf("POST /: %d, want 405", code)
	}
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case code := <-done:
		if code != exitOK {
			t.Errorf("serve exited with %d after the interrupt, want 0: %s", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not stop")
	}
}

// TestServePortInUse: a port that is taken is an error, exit 2.
func TestServePortInUse(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var stdout syncBuffer
	done := make(chan int, 1)
	go func() { done <- run(ctx, []string{"serve", "-port", "0"}, &stdout, io.Discard) }()
	var port string
	for deadline := time.Now().Add(10 * time.Second); port == "" && time.Now().Before(deadline); {
		if m := regexp.MustCompile(`127\.0\.0\.1:(\d+)`).FindStringSubmatch(stdout.String()); m != nil {
			port = m[1]
		}
		time.Sleep(10 * time.Millisecond)
	}
	var stderr bytes.Buffer
	if code := run(ctx, []string{"serve", "-port", port}, io.Discard, &stderr); code != exitInconclusive {
		t.Errorf("a second serve on port %s exited with %d, want 2: %s", port, code, stderr.String())
	}
	cancel()
	<-done
}
