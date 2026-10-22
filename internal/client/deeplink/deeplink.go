// Package deeplink reads and writes isshoni:// links (docs/m2/02-device-auth.md §12) and normalizes server addresses
// (NormalizeOrigin):
//
//	isshoni://<host>[:<port>]/             server https://<host>[:<port>], its default room
//	isshoni://<host>[:<port>]/r/<roomId>   that server, room <roomId>
//
// A link names a server and a room and nothing else. Its query and fragment are dropped unread, so no version of a
// link can carry a credential, a code or a "start sharing" instruction into the app; the server is always https://
// (a dev build adds an http://localhost server through its address field, never through a link). What the app does
// with a link (02 §12.3: "Add server {host}?" before any network request, never starting a share) is
// 03-desktop-app's; the command line that delivers one (--open-url "%1") is 03's and 06's.
//
// testdata/links.json holds the cases the Go tests and the web's appLinkFor tests share (web/src/lib/appLink.ts
// builds the valid ones). The package imports only the standard library and golang.org/x/net/idna (02 §11.1).
package deeplink

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
)

// Link is an accepted isshoni:// link.
type Link struct {
	Origin      string // "https://watch.example.com"
	Host        string // ASCII host[:port], as in Origin
	HostUnicode string // the Unicode form when Host has xn-- labels; else equal to Host
	RoomID      string // "" = the server's default room
}

var (
	// ErrUnsupported is a well-formed link to a server whose path this version doesn't know ("This link isn't
	// supported by this version of the app").
	ErrUnsupported = errors.New("deeplink: unsupported link")
	// ErrInvalid is a link that isn't one: another scheme, no "//", userinfo, a bad host or port, more than MaxBytes,
	// invalid UTF-8, a space or a control character.
	ErrInvalid = errors.New("deeplink: invalid link")
)

// Scheme is the URL scheme of a link, as Build writes it. Parse accepts it in any case.
const Scheme = "isshoni"

// MaxRoomIDLen is the room-id rule's length (01 §4.1, protocol.MaxOpaqueIDLen): 1–64 of [A-Za-z0-9_-].
const MaxRoomIDLen = 64

// Parse reads an isshoni:// link (02 §12.1):
//   - the whole link: at most MaxBytes, valid UTF-8, no spaces or control characters (bidi controls included);
//   - the scheme "isshoni" in any case, then "//": "isshoni:watch.example.com" is refused;
//   - the query (from the first "?") and the fragment (from the first "#") are cut off and dropped without being
//     looked at; one debug line says that something was dropped, never what;
//   - the authority: no userinfo; the host as NormalizeOrigin canonicalizes it (a DNS name, IDN allowed, through
//     idna.Lookup.ToASCII, or an IP literal with IPv6 in brackets), nothing percent-decoded; a port of 1–65535
//     without leading zeros, with :443 dropped;
//   - the path: "" or "/" (the default room), or "/r/<roomId>" with an optional trailing "/", where roomId follows
//     the room-id rule (ValidRoomID). Any other path, "/r/" with an empty or invalid room id included, is
//     ErrUnsupported.
//
// The server is always https://. Errors wrap ErrInvalid or ErrUnsupported and name the rule that failed, never any
// part of the link, so a caller may log them; it logs the link itself at most by scheme and host (03 §13.1).
func Parse(raw string) (Link, error) {
	if rule := checkChars(raw); rule != "" {
		return Link{}, invalid(rule)
	}
	scheme, rest, ok := strings.Cut(raw, ":")
	if !ok || !equalFoldASCII(scheme, Scheme) {
		return Link{}, invalid("the scheme is not isshoni")
	}
	rest, ok = strings.CutPrefix(rest, "//")
	if !ok {
		return Link{}, invalid(`no "//" after the scheme`)
	}
	// The fragment, then the query: cut off here and never looked at (02 §12.1).
	rest, _, hasFragment := strings.Cut(rest, "#")
	rest, _, hasQuery := strings.Cut(rest, "?")

	auth, path := rest, ""
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		auth, path = rest[:i], rest[i:]
	}
	host, port, rule := parseAuthority(auth)
	if rule != "" {
		return Link{}, invalid(rule)
	}
	if port == "443" {
		port = ""
	}
	room, ok := parsePath(path)
	if !ok {
		return Link{}, fmt.Errorf("%w: the path is not / or /r/<roomId>", ErrUnsupported)
	}
	hostUnicode, ok := unicodeHost(host)
	if !ok {
		return Link{}, invalid("not a valid host name")
	}
	if hasQuery || hasFragment {
		slog.Debug("deep link: query or fragment dropped unread", slog.String("component", "deeplink"))
	}
	hp := joinHostPort(host, port)
	return Link{Origin: "https://" + hp, Host: hp, HostUnicode: joinHostPort(hostUnicode, port), RoomID: room}, nil
}

// Build returns the link for a server origin and a room (02 §12.1): "isshoni://<host>[:<port>]/" or
// "isshoni://<host>[:<port>]/r/<roomID>". The origin goes through NormalizeOrigin without loopback http: links are
// https only, so an origin that isn't https (or doesn't normalize) gives "". A roomID of "" or one that breaks the
// room-id rule gives the server's link without a room. Parse(Build(l.Origin, l.RoomID)) is l for every accepted l.
func Build(origin, roomID string) string {
	o, err := NormalizeOrigin(origin, false)
	if err != nil {
		return ""
	}
	link := Scheme + "://" + strings.TrimPrefix(o, "https://") + "/"
	if ValidRoomID(roomID) {
		link += "r/" + roomID
	}
	return link
}

// ValidRoomID reports whether id follows the room-id rule (01 §4.1): 1–64 of [A-Za-z0-9_-].
func ValidRoomID(id string) bool {
	if id == "" || len(id) > MaxRoomIDLen {
		return false
	}
	for i := range len(id) {
		switch c := id[i]; {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// parsePath reads a link's path: "" and "/" are the default room, "/r/<roomId>" and "/r/<roomId>/" that room.
func parsePath(p string) (room string, ok bool) {
	if p == "" || p == "/" {
		return "", true
	}
	id, found := strings.CutPrefix(p, "/r/")
	if !found {
		return "", false
	}
	id = strings.TrimSuffix(id, "/")
	return id, ValidRoomID(id)
}

func invalid(rule string) error { return fmt.Errorf("%w: %s", ErrInvalid, rule) }

// equalFoldASCII compares s with a lower-case ASCII word, ignoring the case of ASCII letters only: Unicode folding
// would let "\u017f" (long s) or the Kelvin sign stand in for a letter of a scheme.
func equalFoldASCII(s, lower string) bool {
	if len(s) != len(lower) {
		return false
	}
	for i := range len(s) {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != lower[i] {
			return false
		}
	}
	return true
}
