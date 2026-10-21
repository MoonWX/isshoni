package assets

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"
)

// The dev server (dev builds only, `task desktop:dev`): the page stays on the asset origin, and this handler takes
// the SPA from Vite's dev server instead of Dist, so Vite's live reload works in the app's window (03 D3, §5.3).
//
//   - A route (a path whose last segment has no dot, and that isn't one of Vite's /@… paths) gets Vite's
//     index.html with the bridge tag, the CSP and a per-request nonce on its inline scripts and styles: Vite's dev
//     pages start with an inline module script (React's refresh preamble) and inject <style> elements, and the
//     nonce lets exactly those run without 'unsafe-inline'. Vite's client reads the nonce from the
//     <meta property="csp-nonce"> tag added here.
//   - Every other GET or HEAD (modules, /@vite/client, public files) is proxied to the dev server, so it is
//     same-origin for the page; only the live-reload WebSocket goes to the dev server directly (connect-src).
//
// Release builds never set Options.DevServer (New refuses it without Options.Dev).

// devIndexLimit bounds the dev server's index.html.
const devIndexLimit = 4 << 20

type devServer struct {
	origin   string // "http://localhost:5173"
	wsOrigin string // "ws://localhost:5173"
	base     *url.URL
	client   *http.Client
	proxy    *httputil.ReverseProxy
}

func newDevServer(origin string) (*devServer, error) {
	scheme, host, err := parseOrigin(origin)
	if err != nil || scheme != "http" || !isLoopbackHost(host) {
		return nil, fmt.Errorf("assets: the dev server must be an http:// loopback origin, not %q", origin)
	}
	base := &url.URL{Scheme: "http", Host: host}
	d := &devServer{
		origin:   "http://" + host,
		wsOrigin: "ws://" + host,
		base:     base,
		client: &http.Client{
			Timeout:       10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
	d.proxy = &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(base)
			pr.Out.Host = base.Host
			// The page's origin means nothing to Vite; to it this is a same-origin request.
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("Referer")
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			slog.Default().Warn("desktop: dev server unreachable", "component", "assets", "err", err)
			plainError(w, r, http.StatusBadGateway)
		},
	}
	return d, nil
}

func (h *handler) serveDev(w http.ResponseWriter, r *http.Request, name string) {
	if name != "index.html" && (lastSegmentHasDot(name) || strings.HasPrefix(name, "@")) {
		h.dev.proxy.ServeHTTP(w, r)
		return
	}
	page, err := h.dev.index(r)
	if err != nil {
		slog.Default().Warn("desktop: dev server index.html", "component", "assets", "err", err)
		plainError(w, r, http.StatusBadGateway)
		return
	}
	nonce, err := newNonce()
	if err != nil {
		plainError(w, r, http.StatusInternalServerError)
		return
	}
	head, tail, err := rewriteIndex(page)
	if err != nil {
		slog.Default().Warn("desktop: dev server index.html", "component", "assets", "err", err)
		plainError(w, r, http.StatusBadGateway)
		return
	}
	head = append(addNonce(head, nonce), `<meta property="csp-nonce" nonce="`+nonce+`">`...)
	h.serveIndex(w, r, head, addNonce(tail, nonce), nonce)
}

// index fetches the dev server's index.html (Vite transforms it for /index.html).
func (d *devServer) index(r *http.Request) ([]byte, error) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, d.base.JoinPath("index.html").String(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/html")
	resp, err := d.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, devIndexLimit+1))
	if err != nil {
		return nil, err
	}
	if len(b) > devIndexLimit {
		return nil, errors.New("index.html too large")
	}
	return b, nil
}

func newNonce() (string, error) {
	b := make([]byte, 18)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// addNonce adds nonce="<nonce>" to every inline <script> (one without a src attribute) and every <style> start tag
// of page. Scripts with a src load from 'self' and keep their tags as they are.
func addNonce(page []byte, nonce string) []byte {
	attr := []byte(` nonce="` + nonce + `"`)
	lower := bytes.ToLower(page)
	var out bytes.Buffer
	out.Grow(len(page) + 256)
	last := 0
	for i := 0; i < len(lower); i++ {
		j := bytes.IndexByte(lower[i:], '<')
		if j < 0 {
			break
		}
		i += j
		for _, t := range nonceTags {
			end := i + len(t.open)
			if !bytes.HasPrefix(lower[i:], t.open) || end >= len(lower) || (lower[end] != '>' && !isHTMLSpace(lower[end])) {
				continue
			}
			if !t.inlineOnly || !hasSrc(lower[end:]) {
				out.Write(page[last:end])
				out.Write(attr)
				last = end
			}
			break
		}
	}
	out.Write(page[last:])
	return out.Bytes()
}

// nonceTags are the start tags addNonce looks at.
var nonceTags = []struct {
	open       []byte
	inlineOnly bool // only when the tag has no src attribute
}{
	{[]byte("<script"), true},
	{[]byte("<style"), false},
}

// hasSrc reports whether the rest of a start tag (lower case, up to its '>') has a src attribute.
func hasSrc(rest []byte) bool {
	end := bytes.IndexByte(rest, '>')
	if end < 0 {
		end = len(rest)
	}
	tag := rest[:end]
	for k := 0; k+4 <= len(tag); k++ {
		if isHTMLSpace(tag[k]) && bytes.HasPrefix(tag[k+1:], []byte("src")) {
			after := bytes.TrimLeft(tag[k+4:], " \t\r\n\f")
			if len(after) > 0 && after[0] == '=' {
				return true
			}
		}
	}
	return false
}
