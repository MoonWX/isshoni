//go:build windows || (darwin && cgo)

package main

import (
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The navigation guard is what Go does of 03 §5.5's layer 2 at the pinned Wails (W5's fallback, 03 §3.2): beta.34
// registers no NewWindowRequested handler and drops NavigationStarting's arguments, so Go can cancel neither a
// navigation nor a new window. New windows are the CSP sandbox's (internal/desktop/assets, §5.3), a script's
// navigation is the SPA's navigate listener (links.ts); what Go can still do is notice that the main window's top
// level left the asset origin and send it back to the start page.
//
// After every WebViewNavigationCompleted (Windows) the guard asks the page where it is: whereScript posts
// "isshoni:where:<n>", and the reply's top-level source is WebView2's own (CoreWebView2.Source, Wails'
// OriginInfo.TopOrigin), which page script can't forge. A reply from another origin, or no reply within whereTimeout
// once the runtime is up (a page that took chrome.webview away, an error page), loads the start page with
// WebviewWindow.SetURL("/"): a navigation the browser starts, which page script can't block. Wails' Reload can't do
// that: at beta.34 it is location.reload() of whatever page is showing. Pages that keep leaving are sent back at most
// homeLimit times per homeWindow.

const (
	// whereMessage prefixes the page's answer, "isshoni:where:<n>" (Wails passes messages without its "wails:"
	// prefix to Options.RawMessageHandler).
	whereMessage = "isshoni:where:"
	// whereTimeout is how long a question may go unanswered once Wails' runtime is up in the page.
	whereTimeout = 5 * time.Second
	homeLimit    = 3
	homeWindow   = time.Minute
)

// whereScript is the question n: the page posts whereMessage+n through WebView2's own channel, if it has one.
func whereScript(n uint64) string {
	return "window.chrome&&chrome.webview&&chrome.webview.postMessage('" + whereMessage +
		strconv.FormatUint(n, 10) + "')"
}

// whereReply parses an answer to whereScript; ok is false for any other message.
func whereReply(msg string) (n uint64, ok bool) {
	rest, found := strings.CutPrefix(msg, whereMessage)
	if !found {
		return 0, false
	}
	n, err := strconv.ParseUint(rest, 10, 64)
	return n, err == nil
}

// foreignTop reports whether src, a top-level document's URL, is not on origin (a bare scheme://host, compared as
// text after lower-casing and dropping a default port). "", about:blank, chrome-error:// pages and anything that
// doesn't parse are foreign.
func foreignTop(src, origin string) bool {
	u, err := url.Parse(src)
	if err != nil || u.Opaque != "" || u.User != nil || u.Host == "" {
		return true
	}
	scheme, host := strings.ToLower(u.Scheme), strings.ToLower(u.Host)
	if p := u.Port(); (scheme == "http" && p == "80") || (scheme == "https" && p == "443") {
		host = strings.TrimSuffix(host, ":"+p)
	}
	return scheme+"://"+host != origin
}

// navGuard asks the main window where it is after each navigation and sends it home from anywhere else. Its methods
// may be called from any goroutine.
type navGuard struct {
	origin string
	log    *slog.Logger
	now    func() time.Time
	after  func(time.Duration, func()) // time.AfterFunc without the timer

	mu       sync.Mutex
	ask      func(js string) // runs js in the main window (Wails' ExecJS, queued until the runtime is ready)
	home     func()          // loads the start page (SetURL("/"))
	ready    bool            // Wails' runtime has been up in the window
	asked    uint64          // the last question
	answered uint64          // the last question answered
	away     bool            // the last answer (or silence) said the window isn't on the asset origin
	homes    []time.Time     // when the guard sent the window home, within homeWindow
	limited  bool            // the limit was logged
}

// newNavGuard is a guard for the asset origin; attach gives it its window.
func newNavGuard(origin string, log *slog.Logger) *navGuard {
	return &navGuard{origin: origin, log: log, now: time.Now,
		after: func(d time.Duration, f func()) { time.AfterFunc(d, f) }}
}

// attach gives the guard the window's ExecJS and the way home.
func (g *navGuard) attach(ask func(js string), home func()) {
	g.mu.Lock()
	g.ask, g.home = ask, home
	g.mu.Unlock()
}

// navigated is a completed navigation of the main window: the guard asks where the window is now.
func (g *navGuard) navigated() {
	g.mu.Lock()
	g.asked++
	n, ready, ask := g.asked, g.ready, g.ask
	g.mu.Unlock()
	if ask == nil {
		return
	}
	ask(whereScript(n))
	if ready {
		g.arm(n)
	}
}

// runtimeReady is Wails' runtime up in the window (WindowRuntimeReady): a question still open, which Wails ran only
// now if it was the first, gets its deadline.
func (g *navGuard) runtimeReady() {
	g.mu.Lock()
	g.ready = true
	n, open := g.asked, g.answered < g.asked
	g.mu.Unlock()
	if open {
		g.arm(n)
	}
}

// arm sends the window home if question n is still unanswered after whereTimeout.
func (g *navGuard) arm(n uint64) {
	g.after(whereTimeout, func() {
		g.mu.Lock()
		silent := g.answered < n
		if silent {
			g.away = true
		}
		g.mu.Unlock()
		if silent {
			g.leave("no answer", "")
		}
	})
}

// reported is the answer to question n with the browser's top-level source.
func (g *navGuard) reported(n uint64, top string) {
	away := foreignTop(top, g.origin)
	g.mu.Lock()
	if n <= g.asked && n > g.answered {
		g.answered = n
	}
	g.away = away
	g.mu.Unlock()
	if away {
		g.leave("left the asset origin", top)
	}
}

// atHome reports whether the window was on the asset origin at the last answer (true before the first).
func (g *navGuard) atHome() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return !g.away
}

// leave sends the window home, at most homeLimit times per homeWindow. The log names the host only: a path can hold
// a room id (03 §17).
func (g *navGuard) leave(reason, top string) {
	now := g.now()
	g.mu.Lock()
	keep := g.homes[:0]
	for _, t := range g.homes {
		if now.Sub(t) < homeWindow {
			keep = append(keep, t)
		}
	}
	g.homes = keep
	if len(g.homes) >= homeLimit {
		logIt := !g.limited
		g.limited = true
		g.mu.Unlock()
		if logIt {
			g.log.Error("desktop: the window keeps leaving the app's pages; not sending it back again for now",
				"host", hostOf(top))
		}
		return
	}
	g.homes = append(g.homes, now)
	g.limited = false
	home := g.home
	g.mu.Unlock()
	g.log.Warn("desktop: the window isn't showing the app's pages; loading the start page", "reason", reason,
		"host", hostOf(top))
	if home != nil {
		home()
	}
}

// hostOf is the host of a URL for the log, or its scheme when it has none.
func hostOf(s string) string {
	u, err := url.Parse(s)
	switch {
	case err != nil:
		return "(unparsable)"
	case u.Host != "":
		return u.Host
	case u.Scheme != "":
		return u.Scheme + ":"
	default:
		return "(none)"
	}
}
