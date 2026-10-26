package httpapi

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
)

// The desktop install scripts (M2 06 §7.4, D12): GET /install-desktop.ps1 serves deploy/install-desktop.ps1 with its
// prefill block written for this server, and /install-desktop.sh answers 404 until M3. The router registers both
// explicitly, so the SPA fallback can never send index.html to `irm … | iex` or `curl … | sh`.

// DesktopScripts is what the server needs to serve the desktop install scripts.
type DesktopScripts struct {
	PS1     []byte // deploy.InstallDesktopPS1
	Origin  string // config.Site.Origin, e.g. "https://watch.example.com"
	Version string // internal/version.Version()
}

const (
	installPS1Path = "/install-desktop.ps1"
	installShPath  = "/install-desktop.sh"

	// The marker lines of the prefill block, as deploy/install-desktop.ps1 and its Pester test spell them: a line
	// that starts with one of them is that marker (the opening one carries a comment after it).
	prefillOpen  = "# >>> isshoni-prefill"
	prefillClose = "# <<< isshoni-prefill"

	// installShBody is the text of /install-desktop.sh until M3 brings a desktop app for macOS and Linux.
	installShBody = "Not available yet.\n"
)

// releaseVersion is a release or release-candidate version (M2 06 §7.4): the versions whose desktop installer the
// project publishes. Any other version (a dev, CI or e2e build) leaves $IsshoniVersion empty, and the script then
// installs the latest release. Go's $ matches only at the end of the text.
var releaseVersion = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$`)

// RenderInstallPS1 rewrites the prefill block: exactly one "# >>> isshoni-prefill" line and one
// "# <<< isshoni-prefill" line after it, else an error.
//
// Everything from the opening marker line to the closing one becomes
//
//	# >>> isshoni-prefill (written by the isshoni server at https://watch.example.com)
//	$IsshoniServer  = 'https://watch.example.com'
//	$IsshoniVersion = '0.2.0'
//	# <<< isshoni-prefill
//
// and every other byte of tmpl stays as it is. $IsshoniServer is origin with every ' doubled (PowerShell's escape in
// a single-quoted string), kept only when origin is ASCII without control characters and either https:// or http://
// with a loopback host (a dev server); otherwise it is empty, the opening line names no server, and the app asks for
// one. ASCII matters beyond the script's own rule: PowerShell also ends a single-quoted string at the typographic
// quotes U+2018–U+201B. $IsshoniVersion is version when it is a release or release-candidate version
// (^[0-9]+\.[0-9]+\.[0-9]+(-rc\.[0-9]+)?$), else empty.
func RenderInstallPS1(tmpl []byte, origin, version string) ([]byte, error) {
	start, end, err := prefillBlock(tmpl)
	if err != nil {
		return nil, err
	}
	server := installPS1Server(origin)
	if !releaseVersion.MatchString(version) {
		version = ""
	}
	var b bytes.Buffer
	b.Grow(len(tmpl) + 2*len(origin) + 128)
	b.Write(tmpl[:start])
	b.WriteString(prefillOpen)
	if server != "" {
		// origin, not server: a comment needs no escape, and origin is ASCII without control characters here.
		b.WriteString(" (written by the isshoni server at " + origin + ")\n")
	} else {
		b.WriteString(" (written by an isshoni server)\n")
	}
	b.WriteString("$IsshoniServer  = '" + server + "'\n")
	b.WriteString("$IsshoniVersion = '" + version + "'\n")
	b.WriteString(prefillClose + "\n")
	b.Write(tmpl[end:])
	return b.Bytes(), nil
}

// prefillBlock finds the prefill block of tmpl: start is the offset of its opening marker line, end the offset just
// past its closing marker line and that line's newline.
func prefillBlock(tmpl []byte) (start, end int, err error) {
	opens, closes := 0, 0
	start, end = -1, -1
	for at := 0; at < len(tmpl); {
		next := len(tmpl)
		if i := bytes.IndexByte(tmpl[at:], '\n'); i >= 0 {
			next = at + i + 1
		}
		line := tmpl[at:next]
		switch {
		case bytes.HasPrefix(line, []byte(prefillOpen)):
			opens++
			start = at
		case bytes.HasPrefix(line, []byte(prefillClose)):
			closes++
			end = next
		}
		at = next
	}
	switch {
	case opens == 0:
		return 0, 0, fmt.Errorf("httpapi: install-desktop.ps1 has no %q line", prefillOpen)
	case opens > 1:
		return 0, 0, fmt.Errorf("httpapi: install-desktop.ps1 has %d %q lines, want one", opens, prefillOpen)
	case closes == 0:
		return 0, 0, fmt.Errorf("httpapi: install-desktop.ps1 has no %q line", prefillClose)
	case closes > 1:
		return 0, 0, fmt.Errorf("httpapi: install-desktop.ps1 has %d %q lines, want one", closes, prefillClose)
	case end <= start:
		return 0, 0, fmt.Errorf("httpapi: install-desktop.ps1 has its %q line before its %q line", prefillClose, prefillOpen)
	}
	return start, end, nil
}

// installPS1Server is the value of $IsshoniServer for origin: origin with every ' doubled when the script may carry
// it (see RenderInstallPS1), else "".
func installPS1Server(origin string) string {
	if !usableInstallServer(origin) {
		return ""
	}
	return strings.ReplaceAll(origin, "'", "''")
}

// usableInstallServer reports whether origin may go into the script: ASCII without control characters, and https://
// or http:// on a loopback host. The scheme is matched in lower case, as the script's own check (-cmatch
// '^https?://…') does before it passes /SERVER= to the installer.
func usableInstallServer(origin string) bool {
	if origin == "" {
		return false
	}
	for i := range len(origin) {
		if c := origin[i]; c < 0x20 || c >= 0x7f { // C0 controls, DEL, and every byte of a non-ASCII character
			return false
		}
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	switch {
	case strings.HasPrefix(origin, "https://"):
		return true
	case strings.HasPrefix(origin, "http://"):
		return isLoopbackHostname(u.Hostname())
	default:
		return false
	}
}

// isLoopbackHostname reports localhost (any case) and loopback address literals (127.0.0.0/8, ::1).
func isLoopbackHostname(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	a, err := netip.ParseAddr(host)
	return err == nil && a.IsLoopback()
}

// installPS1 serves the rendered script. Origin and version are fixed for the process (M1 04 §6.2), so the body,
// its ETag and its length are computed once, when the router is built.
type installPS1 struct {
	body []byte
	etag string // the first 16 hex characters of the body's SHA-256, quoted, as the SPA's (04 §9.5)
}

// newInstallPS1 renders d's script. An origin the script can't carry is left out with one warning, here at startup:
// the script then installs the app without a server, and the app asks for one.
func newInstallPS1(d *DesktopScripts, log *slog.Logger) (*installPS1, error) {
	if d.PS1 == nil {
		return nil, errors.New("httpapi: DesktopScripts.PS1 is nil")
	}
	body, err := RenderInstallPS1(d.PS1, d.Origin, d.Version)
	if err != nil {
		return nil, err
	}
	if d.Origin != "" && installPS1Server(d.Origin) == "" {
		log.Warn("install-desktop.ps1 is served without this server's address: the public origin is not ASCII, "+
			"has a control character, or is http:// on a host that is not loopback, so the app will ask for the "+
			"server after the install", slog.String("origin", d.Origin))
	}
	return &installPS1{body: body, etag: etagOf(body)}, nil
}

// ServeHTTP answers GET and HEAD (the mux's GET pattern): 200 with the script as UTF-8 text (Windows PowerShell
// decodes text without a charset as ISO-8859-1; the script is ASCII anyway), revalidated on every fetch by its
// ETag, with Content-Length. http.ServeContent also answers If-None-Match with 304 and leaves the body out of HEAD.
// The security headers step has set the rest (nosniff, the deny-all policy). No auth: the script says nothing that
// /api/v1/info doesn't.
func (h *installPS1) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	hd := w.Header()
	hd.Set("Content-Type", "text/plain; charset=utf-8")
	hd.Set("Cache-Control", cacheNoCache)
	hd.Set("ETag", h.etag)
	http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(h.body))
}

// serveInstallSh answers /install-desktop.sh, for every method: 404 with a short text until M3. With curl's -f the
// pipe gets nothing; without it, sh fails on the one line, which runs nothing.
func serveInstallSh(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "text/plain; charset=utf-8")
	h.Set("Cache-Control", cacheNoStore)
	h.Set("Content-Length", strconv.Itoa(len(installShBody)))
	w.WriteHeader(http.StatusNotFound)
	if r.Method != http.MethodHead {
		_, _ = io.WriteString(w, installShBody) // a failed write means the client is gone
	}
}

// registerDesktopScripts adds the desktop install script routes to the mux (NewRouter): GET /install-desktop.ps1
// pre-filled from d, or a plain 404 without it (tests that don't set RouterOptions.Desktop) and for a d that can't
// be rendered, which the wiring has refused at startup already; and /install-desktop.sh.
func (rt *Router) registerDesktopScripts(d *DesktopScripts) {
	ps1 := http.Handler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		plainError(w, r, http.StatusNotFound)
	}))
	if d != nil {
		h, err := newInstallPS1(d, rt.log)
		if err != nil {
			rt.log.Error("install-desktop.ps1 is not served", slog.String("route", "GET "+installPS1Path), logx.Err(err))
		} else {
			ps1 = h
		}
	}
	rt.mux.Handle("GET "+installPS1Path, ps1)
	rt.mux.Handle(installShPath, http.HandlerFunc(serveInstallSh))
}
