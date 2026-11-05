//go:build (windows || (darwin && cgo)) && !isshoni_hw

package main

import (
	"io"
	"log/slog"
	"testing"
	"time"
)

// TestForeignTop: only the asset origin itself is home, with or without a path and its default port.
func TestForeignTop(t *testing.T) {
	const origin = "http://wails.localhost"
	for src, want := range map[string]bool{
		"http://wails.localhost/":                    false,
		"http://wails.localhost":                     false,
		"http://wails.localhost/r/lounge?x=1#y":      false,
		"HTTP://Wails.Localhost/desktop/setup":       false,
		"http://wails.localhost:80/":                 false,
		"":                                           true,
		"about:blank":                                true,
		"chrome-error://chromewebdata/":              true,
		"https://wails.localhost/":                   true,
		"http://wails.localhost:8080/":               true,
		"http://wails.localhost.evil.example/":       true,
		"http://evil.example/http://wails.localhost": true,
		"http://user@wails.localhost/":               true,
		"https://example.com/":                       true,
		"data:text/html,x":                           true,
		"file:///C:/Windows/win.ini":                 true,
		"http://wails.localhost%2F/":                 true,
	} {
		if got := foreignTop(src, origin); got != want {
			t.Errorf("foreignTop(%q) = %v, want %v", src, got, want)
		}
	}
	if foreignTop("wails://localhost/", "wails://localhost") {
		t.Error("the macOS asset origin is foreign to itself")
	}
}

// TestWhereReply: the page's answer carries the question's number; nothing else is an answer.
func TestWhereReply(t *testing.T) {
	if s := whereScript(7); s != "window.chrome&&chrome.webview&&chrome.webview.postMessage('isshoni:where:7')" {
		t.Fatalf("whereScript(7) = %s", s)
	}
	if n, ok := whereReply("isshoni:where:7"); !ok || n != 7 {
		t.Fatalf("whereReply = %d, %v", n, ok)
	}
	for _, msg := range []string{"", "isshoni:where:", "isshoni:where:x", "isshoni:where:-1", "wails:runtime:ready",
		"isshoni:where7"} {
		if _, ok := whereReply(msg); ok {
			t.Errorf("whereReply(%q) is an answer", msg)
		}
	}
}

type navRec struct {
	asked  []string
	homes  int
	timers []func()
	now    time.Time
}

func newTestNavGuard() (*navGuard, *navRec) {
	r := &navRec{now: time.Unix(1_700_000_000, 0)}
	g := newNavGuard("http://wails.localhost", slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.now = func() time.Time { return r.now }
	g.after = func(d time.Duration, f func()) {
		if d != whereTimeout {
			panic("unexpected timeout")
		}
		r.timers = append(r.timers, f)
	}
	g.attach(func(js string) { r.asked = append(r.asked, js) }, func() { r.homes++ })
	return g, r
}

// fire runs the timers armed so far.
func (r *navRec) fire() {
	ts := r.timers
	r.timers = nil
	for _, f := range ts {
		f()
	}
}

// TestNavGuard is W5's fallback in Go (03 §3.2, §5.5 layer 2): every completed navigation asks the page; an answer
// from the asset origin leaves it, one from elsewhere or silence once the runtime is up loads the start page, and
// Reload's choice follows the last answer.
func TestNavGuard(t *testing.T) {
	g, r := newTestNavGuard()

	// The first load: Wails runs the question only once its runtime is up, so the deadline starts there.
	g.navigated()
	if len(r.asked) != 1 || r.asked[0] != whereScript(1) || len(r.timers) != 0 {
		t.Fatalf("first navigation: asked %q, %d timers", r.asked, len(r.timers))
	}
	g.runtimeReady()
	if len(r.timers) != 1 {
		t.Fatalf("%d timers after the runtime came up", len(r.timers))
	}
	g.reported(1, "http://wails.localhost/")
	r.fire()
	if r.homes != 0 || !g.atHome() {
		t.Fatalf("the asset origin was sent home %d times (at home %v)", r.homes, g.atHome())
	}

	// A foreign page answers: home at once, and Reload would load the start page too.
	g.navigated()
	g.reported(2, "https://example.com/phish")
	if r.homes != 1 || g.atHome() {
		t.Fatalf("foreign answer: %d homes, at home %v", r.homes, g.atHome())
	}
	r.fire() // answered: nothing more
	if r.homes != 1 {
		t.Fatalf("an answered question timed out: %d homes", r.homes)
	}
	g.navigated()
	g.reported(3, "http://wails.localhost/")
	r.fire()
	if r.homes != 1 || !g.atHome() {
		t.Fatalf("back home: %d homes, at home %v", r.homes, g.atHome())
	}

	// An answer to a question never asked is ignored, even a foreign one.
	g.navigated()
	g.reported(99, "https://example.com/phish")
	if r.homes != 1 || !g.atHome() {
		t.Fatalf("an answer to no question: %d homes, at home %v", r.homes, g.atHome())
	}

	// No answer (chrome.webview taken away, an error page): home after the deadline.
	r.fire()
	if r.homes != 2 || g.atHome() {
		t.Fatalf("silence: %d homes, at home %v", r.homes, g.atHome())
	}

	// At most homeLimit per homeWindow.
	g.navigated()
	g.reported(5, "about:blank")
	g.navigated()
	g.reported(6, "about:blank")
	if r.homes != homeLimit {
		t.Fatalf("%d homes within a minute, want %d", r.homes, homeLimit)
	}
	r.now = r.now.Add(homeWindow)
	g.navigated()
	g.reported(7, "about:blank")
	if r.homes != homeLimit+1 {
		t.Fatalf("%d homes a minute later", r.homes)
	}

	// An older answer arriving after a newer one changes nothing.
	g.navigated()
	g.navigated()
	g.reported(9, "http://wails.localhost/")
	g.reported(8, "https://example.com/phish")
	g.reported(9, "https://example.com/phish")
	if r.homes != homeLimit+1 || !g.atHome() {
		t.Fatalf("a late answer: %d homes, at home %v", r.homes, g.atHome())
	}
}

// TestNavGuardUnattached: before attach nothing is asked and nothing panics.
func TestNavGuardUnattached(t *testing.T) {
	g := newNavGuard("http://wails.localhost", slog.New(slog.NewTextHandler(io.Discard, nil)))
	g.navigated()
	g.reported(1, "https://example.com/")
}
