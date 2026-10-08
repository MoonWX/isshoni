package ops

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"golang.org/x/mod/semver"

	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/server/config"
)

// readFeed returns the release feed fixture: GitHub's "list releases" answer for an invented history of the
// project, with everything the parser has to cope with (testdata/releases/feed.json).
func readFeed(t testing.TB) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "releases", "feed.json"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// feedTransport is the network of the release tests: an http.RoundTripper that records every request that would
// have gone out and answers from a function. Nothing is sent anywhere.
type feedTransport struct {
	mu       sync.Mutex
	requests []*http.Request
	times    []time.Time
	answer   func(req *http.Request) feedAnswer // nil: 200 with an empty list
}

// feedAnswer is what the feed says to one request.
type feedAnswer struct {
	status   int
	etag     string
	body     string
	encoding string // the Content-Encoding of body; "" sends none
	err      error  // no answer at all: the transport's error
	// silent: the server accepts the request and never answers. stalled: it sends the header and then no body.
	// Both last until the request is cancelled.
	silent, stalled bool
}

func (f *feedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	f.mu.Lock()
	f.requests = append(f.requests, req.Clone(context.WithoutCancel(req.Context())))
	f.times = append(f.times, time.Now())
	answer := f.answer
	f.mu.Unlock()
	a := feedAnswer{status: http.StatusOK, body: "[]"}
	if answer != nil {
		a = answer(req)
	}
	switch {
	case a.err != nil:
		return nil, a.err
	case a.silent:
		<-req.Context().Done()
		return nil, req.Context().Err()
	}
	h := http.Header{"Content-Type": {"application/json; charset=utf-8"}}
	if a.etag != "" {
		h.Set("ETag", a.etag)
	}
	if a.encoding != "" {
		h.Set("Content-Encoding", a.encoding)
	}
	res := &http.Response{
		StatusCode: a.status, Status: fmt.Sprintf("%d %s", a.status, http.StatusText(a.status)),
		Proto: "HTTP/1.1", ProtoMajor: 1, ProtoMinor: 1,
		Header: h, Body: io.NopCloser(strings.NewReader(a.body)), Request: req,
	}
	if a.stalled {
		res.Body = stalledBody{ctx: req.Context()}
	}
	return res, nil
}

func (f *feedTransport) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.requests)
}

func (f *feedTransport) last() *http.Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests[len(f.requests)-1]
}

func (f *feedTransport) set(answer func(req *http.Request) feedAnswer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answer = answer
}

// answerWith returns a feedTransport answer: 200 with body and etag, or 304 when the request names that etag.
func answerWith(etag, body string) func(*http.Request) feedAnswer {
	return func(req *http.Request) feedAnswer {
		if etag != "" && req.Header.Get("If-None-Match") == etag {
			return feedAnswer{status: http.StatusNotModified, etag: etag}
		}
		return feedAnswer{status: http.StatusOK, etag: etag, body: body}
	}
}

// answerStatus returns a feedTransport answer with a fixed status and body.
func answerStatus(status int, body string) func(*http.Request) feedAnswer {
	return func(*http.Request) feedAnswer { return feedAnswer{status: status, body: body} }
}

// answerEncoded returns a feedTransport answer: 200 with a body that is said to be in a content encoding.
func answerEncoded(encoding string, body []byte) func(*http.Request) feedAnswer {
	return func(*http.Request) feedAnswer {
		return feedAnswer{status: http.StatusOK, body: string(body), encoding: encoding}
	}
}

// gzipped compresses data as a server does for Content-Encoding: gzip. gzip.NoCompression gives a body that is as
// large on the wire as it is decoded.
func gzipped(t testing.TB, data []byte, level int) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, err := gzip.NewWriterLevel(&buf, level)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// readFeedBody reads body as the body of a 200 answer with a Content-Encoding ("" sends none), as check does.
func readFeedBody(encoding string, body []byte) ([]knownRelease, error) {
	res := &http.Response{StatusCode: http.StatusOK, Header: http.Header{}, Body: io.NopCloser(bytes.NewReader(body))}
	if encoding != "" {
		res.Header.Set("Content-Encoding", encoding)
	}
	return readReleaseFeed(res)
}

// paddedFeed returns a feed of exactly size bytes: one release, v9.9.9, and white space.
func paddedFeed(size int) []byte {
	const head, tail = `[{"tag_name":"v9.9.9"}`, `]`
	return []byte(head + strings.Repeat(" ", size-len(head)-len(tail)) + tail)
}

// fullShapeFeed returns a "list releases" answer the size of a real one: every release and every asset as GitHub
// describes them, with the author and the uploader in full, which is most of the bytes. The asset names are those
// of 06 §3, then invented ones. It is indented, as GitHub sends it to some clients. The newest release is
// v0.<releases>.0, and the fourth newest is a security release.
func fullShapeFeed(t testing.TB, releases, assets, notesBytes int) []byte {
	t.Helper()
	const repo = "https://api.github.com/repos/MoonWX/isshoni"
	bot := map[string]any{
		"login": "github-actions[bot]", "id": 41898282, "node_id": "MDM6Qm90NDE4OTgyODI=",
		"avatar_url": "https://avatars.githubusercontent.com/in/15368?v=4", "gravatar_id": "",
		"url":                 "https://api.github.com/users/github-actions%5Bbot%5D",
		"html_url":            "https://github.com/apps/github-actions",
		"followers_url":       "https://api.github.com/users/github-actions%5Bbot%5D/followers",
		"following_url":       "https://api.github.com/users/github-actions%5Bbot%5D/following{/other_user}",
		"gists_url":           "https://api.github.com/users/github-actions%5Bbot%5D/gists{/gist_id}",
		"starred_url":         "https://api.github.com/users/github-actions%5Bbot%5D/starred{/owner}{/repo}",
		"subscriptions_url":   "https://api.github.com/users/github-actions%5Bbot%5D/subscriptions",
		"organizations_url":   "https://api.github.com/users/github-actions%5Bbot%5D/orgs",
		"repos_url":           "https://api.github.com/users/github-actions%5Bbot%5D/repos",
		"events_url":          "https://api.github.com/users/github-actions%5Bbot%5D/events{/privacy}",
		"received_events_url": "https://api.github.com/users/github-actions%5Bbot%5D/received_events",
		"type":                "Bot", "user_view_type": "public", "site_admin": false,
	}
	names := func(v string) []string {
		out := []string{
			"install.sh", "compose.yaml", "compose.host.yaml",
			"checksums.txt", "checksums.txt.sig", "checksums.txt.sigstore.json",
			"isshoni_" + v + "_amd64.deb", "isshoni_" + v + "_arm64.deb",
			"isshoni-" + v + "-1.x86_64.rpm", "isshoni-" + v + "-1.aarch64.rpm",
		}
		for _, archive := range []string{
			"linux_amd64.tar.gz", "linux_arm64.tar.gz", "darwin_amd64.tar.gz", "darwin_arm64.tar.gz",
			"windows_amd64.zip", "windows_arm64.zip",
		} {
			out = append(out, "isshoni_"+v+"_"+archive, "isshoni_"+v+"_"+archive+".sbom.json")
		}
		for i := len(out); i < assets; i++ {
			out = append(out, "isshoni-desktop_"+v+"_"+strconv.Itoa(i)+".zip")
		}
		return out[:assets]
	}
	// Release notes that compress like prose, not like one repeated line.
	random := rand.New(rand.NewPCG(1, 2)) //nolint:gosec // G404: the same text in every run, not a secret
	words := strings.Fields("share screen viewer room invite bitrate layer fixes adds the a of for when with admin " +
		"dashboard transfer certificate update simulcast keyframe reconnect push audio focus thumbnail doctor")
	notes := func() string {
		var b strings.Builder
		b.WriteString("## What's new\r\n\r\n")
		for b.Len() < notesBytes {
			b.WriteString("- ")
			for range 6 + random.IntN(10) {
				b.WriteString(words[random.IntN(len(words))] + " ")
			}
			b.WriteString("(#" + strconv.Itoa(random.IntN(3000)) + ")\r\n")
		}
		return b.String()
	}

	feed := make([]map[string]any, 0, releases)
	for i := range releases {
		v := "0." + strconv.Itoa(releases-i) + ".0"
		id := 300000000 - i*1000
		stamp := time.Date(2027, 6, 1, 9, 0, 0, 0, time.UTC).AddDate(0, 0, -14*i).Format(time.RFC3339)
		list := make([]map[string]any, 0, assets)
		for n, name := range names(v) {
			digest := sha256.Sum256([]byte("v" + v + "/" + name))
			list = append(list, map[string]any{
				"url": repo + "/releases/assets/" + strconv.Itoa(id+n), "id": id + n,
				"node_id": "RA_kwDOExample" + hex.EncodeToString(digest[:6]), "name": name, "label": "",
				"uploader": bot, "content_type": "application/octet-stream", "state": "uploaded",
				"size": 1000 + int(digest[0])*70000, "digest": "sha256:" + hex.EncodeToString(digest[:]),
				"download_count": int(digest[1]) * 13, "created_at": stamp, "updated_at": stamp,
				"browser_download_url": "https://github.com/MoonWX/isshoni/releases/download/v" + v + "/" + name,
			})
		}
		body := notes()
		if i == 3 {
			body += "\r\n" + SecurityMarker + "\r\n"
		}
		feed = append(feed, map[string]any{
			"url": repo + "/releases/" + strconv.Itoa(id), "assets_url": repo + "/releases/" + strconv.Itoa(id) + "/assets",
			"upload_url": "https://uploads.github.com/repos/MoonWX/isshoni/releases/" + strconv.Itoa(id) + "/assets{?name,label}",
			"html_url":   "https://github.com/MoonWX/isshoni/releases/tag/v" + v,
			"id":         id, "author": bot, "node_id": "RE_kwDOExample" + strconv.Itoa(id),
			"tag_name": "v" + v, "target_commitish": "main", "name": "v" + v,
			"draft": false, "immutable": true, "prerelease": false,
			"created_at": stamp, "updated_at": stamp, "published_at": stamp, "assets": list,
			"tarball_url": repo + "/tarball/v" + v, "zipball_url": repo + "/zipball/v" + v, "body": body,
		})
	}
	data, err := json.MarshalIndent(feed, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return data
}

// newTestRelease builds a ReleaseCheck that runs as `running`, switched on, over transport.
func newTestRelease(t *testing.T, running string, transport http.RoundTripper, meta MetaStore) (*ReleaseCheck, *fakePolicy, *syncBuffer) {
	t.Helper()
	policy := new(fakePolicy)
	policy.releaseCheck.Store(true)
	log, logs := testLogger()
	r, err := NewReleaseCheck(ReleaseCheckOptions{
		Policy: policy, Version: running, Meta: meta, HTTP: &http.Client{Transport: transport}, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	return r, policy, logs
}

// TestParseReleaseFeed: the parser keeps the published releases with a SemVer tag, in SemVer order, and finds
// the security marker (04 §11.5, §17).
func TestParseReleaseFeed(t *testing.T) {
	got, err := parseReleaseFeed(bytes.NewReader(readFeed(t)))
	if err != nil {
		t.Fatal(err)
	}
	page := func(tag string) string { return "https://github.com/MoonWX/isshoni/releases/tag/" + tag }
	want := []knownRelease{
		// v0.12.0 is a draft and "nightly" is no version: both are gone.
		{Version: "0.11.0-rc.1", URL: page("v0.11.0-rc.1"), Prerelease: true},
		{Version: "0.10.0", URL: page("v0.10.0")}, // after 0.9.1 in the feed, and before it as text: SemVer order
		{Version: "0.9.1", URL: page("v0.9.1"), Security: true},
		{Version: "0.9.0", URL: page("v0.9.0")},
		{Version: "0.3.1", URL: page("v0.3.1"), Security: true},
		{Version: "0.3.0", URL: page("v0.3.0")},
		{Version: "0.2.0-beta.1", URL: page("v0.2.0-beta.1"), Prerelease: true}, // not flagged on GitHub; SemVer says so
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("parseReleaseFeed:\n got %+v\nwant %+v", got, want)
	}
}

func TestParseReleaseFeedEdges(t *testing.T) {
	one := func(fields string) string {
		return `[{"tag_name":"v1.2.3","html_url":"https://example.org/r",` + fields + `}]`
	}
	for _, c := range []struct {
		name string
		feed string
		want []knownRelease
	}{
		{"no releases yet", `[]`, []knownRelease{}},
		{"white space around the list", " \r\n[ ]\n\t", []knownRelease{}},
		{"null", `null`, []knownRelease{}},
		{"null elements", `[null, {}]`, []knownRelease{}},
		{"a tag without v", `[{"tag_name":"0.4.0"}]`, []knownRelease{{Version: "0.4.0"}}},
		{"shorthand versions are padded", `[{"tag_name":"v2"},{"tag_name":"v1.5"}]`, []knownRelease{{Version: "2.0.0"}, {Version: "1.5.0"}}},
		{"build metadata is dropped", `[{"tag_name":"v1.2.3+build.5"}]`, []knownRelease{{Version: "1.2.3"}}},
		{"not SemVer", `[{"tag_name":"v1.2.3.4"},{"tag_name":"latest"},{"tag_name":""},{"tag_name":"v01.2.3"},{"tag_name":"V1.2.3"}]`, []knownRelease{}},
		{"an endless tag", `[{"tag_name":"v1.0.0-` + strings.Repeat("a", 100) + `"}]`, []knownRelease{}},
		{"a draft", one(`"draft":true`), []knownRelease{}},
		{"the prerelease flag", one(`"prerelease":true`), []knownRelease{{Version: "1.2.3", URL: "https://example.org/r", Prerelease: true}}},
		{"the marker anywhere in the notes", one(`"body":"Notes\r\n<!-- isshoni:security -->\r\nMore"`),
			[]knownRelease{{Version: "1.2.3", URL: "https://example.org/r", Security: true}}},
		{"a marker that is not ours", one(`"body":"<!-- isshoni:security-ish --> <!--isshoni:security-->"`),
			[]knownRelease{{Version: "1.2.3", URL: "https://example.org/r"}}},
		{"the same version twice: one entry, a marker on either counts",
			`[{"tag_name":"v1.0.0","html_url":"https://example.org/a"},{"tag_name":"1.0.0","html_url":"https://example.org/b","body":"<!-- isshoni:security -->"}]`,
			[]knownRelease{{Version: "1.0.0", URL: "https://example.org/a", Security: true}}},
		// The dashboard makes a link of the URL: only https survives.
		{"a javascript: URL", `[{"tag_name":"v1.0.0","html_url":"javascript:alert(1)"}]`, []knownRelease{{Version: "1.0.0"}}},
		{"an http URL", `[{"tag_name":"v1.0.0","html_url":"http://example.org/r"}]`, []knownRelease{{Version: "1.0.0"}}},
		{"a URL with credentials", `[{"tag_name":"v1.0.0","html_url":"https://user:pw@example.org/r"}]`, []knownRelease{{Version: "1.0.0"}}},
		{"an endless URL", `[{"tag_name":"v1.0.0","html_url":"https://example.org/` + strings.Repeat("a", 600) + `"}]`, []knownRelease{{Version: "1.0.0"}}},
		{"numeric order, not text order", `[{"tag_name":"v0.9.0"},{"tag_name":"v0.10.0"},{"tag_name":"v0.10.0-rc.2"},{"tag_name":"v0.10.0-rc.10"}]`,
			[]knownRelease{{Version: "0.10.0"}, {Version: "0.10.0-rc.10", Prerelease: true}, {Version: "0.10.0-rc.2", Prerelease: true}, {Version: "0.9.0"}}},
	} {
		got, err := parseReleaseFeed(strings.NewReader(c.feed))
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s:\n got %+v\nwant %+v", c.name, got, c.want)
		}
	}

	// What is not a list of releases is an error: GitHub's error document, HTML from a captive portal, a cut-off
	// body, a list with something after it.
	for _, bad := range []string{
		`{"message":"API rate limit exceeded","documentation_url":"https://docs.github.com/rest"}`,
		`<html><body>Sign in to the hotel Wi-Fi</body></html>`,
		`[{"tag_name":"v1.0.0"`,
		`[{"tag_name":"v1.0.0"}`,
		`[{"tag_name":"v1.0.0"},`,
		`[{"tag_name":"v1.0.0"},]`,
		`[{"tag_name":"v1.0.0"} {"tag_name":"v2.0.0"}]`,
		`[{"tag_name":"v1.0.0"}}`,
		`[{"tag_name":17}]`,
		`["v1.0.0"]`,
		`[`,
		`"v1.0.0"`,
		`17`,
		`true`,
		`[{"tag_name":"v1.0.0"}] [{"tag_name":"v2.0.0"}]`,
		`[{"tag_name":"v1.0.0"}]]`,
		`[{"tag_name":"v1.0.0"}] trailing`,
		`null null`,
		`nul`,
		``,
		`   `,
	} {
		if got, err := parseReleaseFeed(strings.NewReader(bad)); err == nil {
			t.Errorf("parseReleaseFeed(%q) = %+v, want an error", bad, got)
		}
	}

	// A feed with more releases than a page keeps the newest ones.
	var many []string
	for i := range 50 {
		many = append(many, fmt.Sprintf(`{"tag_name":"v1.%d.0"}`, i))
	}
	got, err := parseReleaseFeed(strings.NewReader("[" + strings.Join(many, ",") + "]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != releasePerPage || got[0].Version != "1.49.0" || got[len(got)-1].Version != "1.30.0" {
		t.Errorf("50 releases: kept %d, from %s to %s; want the newest %d, 1.49.0 to 1.30.0",
			len(got), got[0].Version, got[len(got)-1].Version, releasePerPage)
	}

	// The list is cut down while it is read. That changes nothing: the newest version keeps the link of its first
	// entry and takes the marker of an entry several pages later, and versions come back in order from any order.
	many = []string{`{"tag_name":"v1.199.0","html_url":"https://example.org/first"}`}
	for i := range 199 {
		n := i * 7 % 199 // every number below 199 once, in no order
		many = append(many, fmt.Sprintf(`{"tag_name":"v1.%d.0","html_url":"https://example.org/%d"}`, n, n))
	}
	many = append(many, `{"tag_name":"1.199.0","html_url":"https://example.org/last","body":"<!-- isshoni:security -->"}`)
	got, err = parseReleaseFeed(strings.NewReader("[" + strings.Join(many, ",") + "]"))
	if err != nil {
		t.Fatal(err)
	}
	want := []knownRelease{{Version: "1.199.0", URL: "https://example.org/first", Security: true}}
	for n := 198; n > 198-(releasePerPage-1); n-- {
		want = append(want, knownRelease{Version: fmt.Sprintf("1.%d.0", n), URL: fmt.Sprintf("https://example.org/%d", n)})
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("201 releases:\n got %+v\nwant %+v", got, want)
	}
}

// TestViewReleases: what the fixture's releases mean to each running version (04 §11.5): prereleases count only
// for a prerelease, and the security flag is about the releases newer than the running one.
func TestViewReleases(t *testing.T) {
	releases, err := parseReleaseFeed(bytes.NewReader(readFeed(t)))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		running  string
		latest   string
		newer    bool
		security bool
	}{
		{"0.3.0", "0.10.0", true, true},    // 0.3.1 and 0.9.1 are security releases
		{"0.3.1", "0.10.0", true, true},    // 0.9.1 still is
		{"0.9.0", "0.10.0", true, true},    // the marker is on 0.9.1, not on the latest: it counts
		{"0.9.1", "0.10.0", true, false},   // every marked release is this one or older
		{"0.10.0", "0.10.0", false, false}, // up to date; the release candidate of 0.11.0 is not for a stable server
		{"0.10.1", "0.10.0", false, false}, // ahead of the feed
		{"1.0.0", "0.10.0", false, false},
		{"0.11.0-rc.0", "0.11.0-rc.1", true, false}, // a prerelease is told about prereleases
		{"0.11.0-rc.1", "0.11.0-rc.1", false, false},
		{"0.9.1-rc.1", "0.11.0-rc.1", true, true},             // 0.9.1 itself is newer than its release candidate
		{"0.0.0-dev+1a2b3c4d5e6f", "0.11.0-rc.1", true, true}, // a dev build
		{"0.0.0-dev+1a2b3c4d5e6f-dirty", "0.11.0-rc.1", true, true},
		{"devel", "0.10.0", false, false}, // not SemVer: nothing can be called newer
		{"", "0.10.0", false, false},
	} {
		view := viewReleases(c.running, releases)
		if !view.found || view.latest.Version != c.latest || view.newer != c.newer || view.security != c.security {
			t.Errorf("running %q: latest %q newer %v security %v, want %q %v %v",
				c.running, view.latest.Version, view.newer, view.security, c.latest, c.newer, c.security)
		}
	}

	// Only prereleases in the feed: a stable server has no latest release to show.
	pre := []knownRelease{{Version: "0.1.0-rc.2", Prerelease: true}, {Version: "0.1.0-rc.1", Prerelease: true}}
	if view := viewReleases("0.0.9", pre); view.found || view.newer {
		t.Errorf("a stable version against prereleases only: %+v, want nothing", view)
	}
	if view := viewReleases("0.1.0-rc.1", pre); !view.found || view.latest.Version != "0.1.0-rc.2" || !view.newer {
		t.Errorf("a release candidate against a newer one: %+v", view)
	}
	if view := viewReleases("0.3.0", nil); view.found || view.newer || view.security {
		t.Errorf("no releases: %+v", view)
	}
}

// TestReleaseCheckRequest: what goes out is one GET of the feed with the headers of 04 §11.5 and nothing else,
// through real sockets on 127.0.0.1; the second check is conditional and a 304 keeps the result.
func TestReleaseCheckRequest(t *testing.T) {
	const etag = `W/"3f2a9c1e"`
	type seen struct {
		method, uri, host string
		header            http.Header
		body              int
	}
	var mu sync.Mutex
	var requests []seen
	feed := readFeed(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		requests = append(requests, seen{r.Method, r.RequestURI, r.Host, r.Header.Clone(), len(body)})
		mu.Unlock()
		w.Header().Set("ETag", etag)
		if r.Header.Get("If-None-Match") == etag {
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		_, _ = w.Write(feed)
	}))
	defer srv.Close()

	policy := new(fakePolicy)
	policy.releaseCheck.Store(true)
	clock := time.Date(2027, 5, 1, 9, 12, 0, 0, time.UTC)
	// HTTP is nil: this is the client the server uses.
	log, _ := testLogger()
	r, err := NewReleaseCheck(ReleaseCheckOptions{
		Policy: policy, URL: srv.URL + "/repos/MoonWX/isshoni/releases", Version: "0.9.0",
		Now: func() time.Time { return clock }, Logger: log,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := r.Update(); got != nil {
		t.Errorf("Update before the first check = %+v, want nil", got)
	}

	r.tick(t.Context())
	want := &api.UpdateInfo{
		Latest: "0.10.0", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.10.0", Security: true, CheckedAt: clock,
	}
	if got := r.Update(); !reflect.DeepEqual(got, want) {
		t.Errorf("Update after the first check = %+v, want %+v", got, want)
	}

	clock = clock.Add(24 * time.Hour)
	r.tick(t.Context())
	want.CheckedAt = clock // a 304 is a check too
	if got := r.Update(); !reflect.DeepEqual(got, want) {
		t.Errorf("Update after a 304 = %+v, want %+v", got, want)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("%d requests for two checks, want 2", len(requests))
	}
	wantHeader := http.Header{
		"Accept":               {"application/vnd.github+json"},
		"X-Github-Api-Version": {"2022-11-28"},
		"User-Agent":           {"isshoni/0.9.0"},
		// Any HTTP client's: gzip, which is what net/http asks for by itself, and, with one request a day, a
		// connection that is closed.
		"Accept-Encoding": {"gzip"},
		"Connection":      {"close"},
	}
	for i, req := range requests {
		if req.method != http.MethodGet || req.uri != "/repos/MoonWX/isshoni/releases?per_page=20" || req.body != 0 {
			t.Errorf("request %d = %s %s with a body of %d bytes, want GET /repos/MoonWX/isshoni/releases?per_page=20 without one",
				i+1, req.method, req.uri, req.body)
		}
		if req.host != strings.TrimPrefix(srv.URL, "http://") {
			t.Errorf("request %d went to host %q", i+1, req.host)
		}
		if i == 1 {
			wantHeader.Set("If-None-Match", etag)
		}
		if !reflect.DeepEqual(req.header, wantHeader) {
			t.Errorf("request %d headers:\n got %v\nwant %v\nnothing else may be sent (04 §11.5, §16)", i+1, req.header, wantHeader)
		}
	}
}

// A client handed in by a test (or by an embedder) is used for its transport only: its cookies are never sent, and
// the client itself is left as it was.
func TestReleaseCheckSendsNoCookies(t *testing.T) {
	transport := &feedTransport{}
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	feedURL, _ := url.Parse(DefaultReleasesURL)
	jar.SetCookies(feedURL, []*http.Cookie{{
		Name: "user_session", Value: "secret", Secure: true, HttpOnly: true, SameSite: http.SameSiteLaxMode,
	}})
	theirs := &http.Client{Transport: transport, Jar: jar}

	policy := new(fakePolicy)
	policy.releaseCheck.Store(true)
	log, _ := testLogger()
	r, err := NewReleaseCheck(ReleaseCheckOptions{Policy: policy, Version: "0.9.0", HTTP: theirs, Logger: log})
	if err != nil {
		t.Fatal(err)
	}
	r.tick(t.Context())
	if transport.count() != 1 {
		t.Fatalf("%d requests, want 1", transport.count())
	}
	req := transport.last()
	if got := req.URL.String(); got != "https://api.github.com/repos/MoonWX/isshoni/releases?per_page=20" {
		t.Errorf("the default feed URL is %s", got)
	}
	want := http.Header{
		"Accept":               {"application/vnd.github+json"},
		"X-Github-Api-Version": {"2022-11-28"},
		"User-Agent":           {"isshoni/0.9.0"},
		// What net/http sends by itself. The check sets it, so the limit can count the body as it is on the wire.
		"Accept-Encoding": {"gzip"},
	}
	if !reflect.DeepEqual(req.Header, want) {
		t.Errorf("headers = %v, want %v", req.Header, want)
	}
	if req.Body != nil && req.Body != http.NoBody {
		t.Error("the request has a body")
	}
	if theirs.Jar != jar || theirs.CheckRedirect != nil {
		t.Error("NewReleaseCheck changed the client it was given")
	}
}

// A redirect is followed to the same host only (a renamed repository), with the same headers and no Referer.
func TestReleaseCheckRedirects(t *testing.T) {
	type hit struct {
		path   string
		header http.Header
	}
	var mu sync.Mutex
	var elsewhere []hit
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		elsewhere = append(elsewhere, hit{r.URL.Path, r.Header.Clone()})
		mu.Unlock()
		_, _ = w.Write([]byte(`[{"tag_name":"v9.9.9"}]`))
	}))
	defer other.Close()

	var home []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		home = append(home, hit{r.URL.Path, r.Header.Clone()})
		mu.Unlock()
		switch r.URL.Path {
		case "/repos/old/isshoni/releases": // renamed: GitHub answers 301 to the repository's id
			http.Redirect(w, r, "/repositories/42/releases?per_page=20", http.StatusMovedPermanently)
		case "/repositories/42/releases":
			_, _ = w.Write([]byte(`[{"tag_name":"v0.10.0"}]`))
		case "/repos/away/isshoni/releases":
			http.Redirect(w, r, other.URL+"/collect?from=isshoni", http.StatusFound)
		case "/repos/loop/isshoni/releases":
			http.Redirect(w, r, "/repos/loop/isshoni/releases?per_page=20", http.StatusFound)
		}
	}))
	defer srv.Close()

	check := func(path string) (*ReleaseCheck, *syncBuffer) {
		t.Helper()
		policy := new(fakePolicy)
		policy.releaseCheck.Store(true)
		log, logs := testLogger()
		r, err := NewReleaseCheck(ReleaseCheckOptions{Policy: policy, URL: srv.URL + path, Version: "0.9.0", Logger: log})
		if err != nil {
			t.Fatal(err)
		}
		r.tick(t.Context())
		return r, logs
	}

	r, _ := check("/repos/old/isshoni/releases")
	if got := r.Update(); got == nil || got.Latest != "0.10.0" {
		t.Errorf("after a redirect on the same host: Update = %+v, want 0.10.0", got)
	}
	mu.Lock()
	if len(home) != 2 || home[1].path != "/repositories/42/releases" {
		t.Fatalf("requests on the feed's host: %+v", home)
	}
	followed := home[1].header
	mu.Unlock()
	if followed.Get("Referer") != "" {
		t.Errorf("the followed request carries Referer %q", followed.Get("Referer"))
	}
	if followed.Get("User-Agent") != "isshoni/0.9.0" || followed.Get("Accept") != "application/vnd.github+json" ||
		followed.Get("X-Github-Api-Version") != "2022-11-28" || len(followed) != 5 {
		t.Errorf("the followed request's headers = %v, want the first request's", followed)
	}

	r, logs := check("/repos/away/isshoni/releases")
	if got := r.Update(); got != nil {
		t.Errorf("after a redirect to another host: Update = %+v, want nil", got)
	}
	mu.Lock()
	if len(elsewhere) != 0 {
		t.Errorf("the check followed a redirect to another host: %+v", elsewhere)
	}
	mu.Unlock()
	if !strings.Contains(logs.String(), "which is not followed") {
		t.Errorf("no debug line about the refused redirect:\n%s", logs.String())
	}

	before := func() int { mu.Lock(); defer mu.Unlock(); return len(home) }()
	r, _ = check("/repos/loop/isshoni/releases")
	if got := r.Update(); got != nil {
		t.Errorf("after a redirect loop: Update = %+v, want nil", got)
	}
	if n := func() int { mu.Lock(); defer mu.Unlock(); return len(home) }() - before; n != releaseMaxRedirects+1 {
		t.Errorf("%d requests in a redirect loop, want %d", n, releaseMaxRedirects+1)
	}
}

// TestReleaseCheckSchedule: the first check comes 5 to 60 minutes after the start, the next ones every 24 h ± 1 h
// (04 §11.5), and Run stops with its context.
func TestReleaseCheckSchedule(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &feedTransport{answer: answerWith(`"v1"`, string(readFeed(t)))}
		r, _, _ := newTestRelease(t, "0.9.0", transport, nil)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		start := time.Now()
		go func() { done <- r.Run(ctx) }()

		time.Sleep(releaseFirstMin - time.Second)
		synctest.Wait()
		if n := transport.count(); n != 0 {
			t.Fatalf("%d requests in the first 5 minutes", n)
		}
		if got := r.Update(); got != nil {
			t.Errorf("Update before the first check = %+v", got)
		}
		time.Sleep(releaseFirstMax - releaseFirstMin + time.Second)
		synctest.Wait()
		if n := transport.count(); n != 1 {
			t.Fatalf("%d requests in the first hour, want 1", n)
		}
		transport.mu.Lock()
		firstAt := transport.times[0]
		transport.mu.Unlock()
		if got := r.Update(); got == nil || got.Latest != "0.10.0" || !got.Security || !got.CheckedAt.Equal(firstAt.Truncate(time.Millisecond)) {
			t.Errorf("Update after the first check = %+v, want 0.10.0 with a security release, checked at %v", got, firstAt)
		}

		// Two weeks: one check a day, each 23 to 25 hours after the one before.
		time.Sleep(14 * 24 * time.Hour)
		synctest.Wait()
		transport.mu.Lock()
		times := slices.Clone(transport.times)
		transport.mu.Unlock()
		if len(times) < 14 || len(times) > 16 {
			t.Errorf("%d checks in two weeks, want one a day", len(times))
		}
		if first := times[0].Sub(start); first < releaseFirstMin || first > releaseFirstMax {
			t.Errorf("the first check came %v after the start", first)
		}
		for i := 1; i < len(times); i++ {
			if gap := times[i].Sub(times[i-1]); gap < releaseEvery-releaseJitter || gap > releaseEvery+releaseJitter {
				t.Errorf("check %d came %v after the one before, want 24 h ± 1 h", i+1, gap)
			}
		}
		// From the second check on the request is conditional, and the unchanged list keeps the result.
		if got := transport.last().Header.Get("If-None-Match"); got != `"v1"` {
			t.Errorf("If-None-Match = %q, want the ETag of the last answer", got)
		}

		cancel()
		if err := <-done; err != nil {
			t.Errorf("Run returned %v, want nil", err)
		}
		if err := r.Run(t.Context()); err == nil {
			t.Error("a second Run succeeded; Run can be called once")
		}
	})
}

// The jitter is real: over many starts the first delay covers its range, and so does the daily one.
func TestReleaseDelays(t *testing.T) {
	var firstMin, firstMax, nextMin, nextMax time.Duration = time.Hour * 100, 0, time.Hour * 100, 0
	for range 2000 {
		first, next := firstReleaseDelay(), nextReleaseDelay()
		firstMin, firstMax = min(firstMin, first), max(firstMax, first)
		nextMin, nextMax = min(nextMin, next), max(nextMax, next)
	}
	if firstMin < 5*time.Minute || firstMax > 60*time.Minute || firstMax-firstMin < 40*time.Minute {
		t.Errorf("first delays between %v and %v, want them spread over 5 to 60 minutes", firstMin, firstMax)
	}
	if nextMin < 23*time.Hour || nextMax > 25*time.Hour || nextMax-nextMin < 90*time.Minute {
		t.Errorf("daily delays between %v and %v, want them spread over 23 to 25 hours", nextMin, nextMax)
	}
}

// TestReleaseCheckOff: with the setting updateCheck off nothing is sent, ever, and nothing is shown; the switch is
// read at every tick, so it works without a restart (04 §11.5).
func TestReleaseCheckOff(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &feedTransport{answer: answerWith("", string(readFeed(t)))}
		meta := newFakeTransferStore()
		// A result from the time the check was on is in the database.
		meta.meta[MetaReleaseCheck] = `{"v":1,"checkedAt":"1999-12-25T00:00:00.000Z","releases":[{"version":"0.10.0","security":true}]}`
		r, policy, _ := newTestRelease(t, "0.9.0", transport, meta)
		policy.releaseCheck.Store(false)
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- r.Run(ctx) }()

		time.Sleep(30 * 24 * time.Hour)
		synctest.Wait()
		if n := transport.count(); n != 0 {
			t.Fatalf("%d requests in 30 days with the check switched off", n)
		}
		if got := r.Update(); got != nil {
			t.Errorf("Update with the check off = %+v, want nil", got)
		}
		if got := r.Alerts(); got != nil {
			t.Errorf("Alerts with the check off = %+v, want none", got)
		}

		// An admin switches it on: the next tick checks.
		policy.releaseCheck.Store(true)
		if got := r.Update(); got == nil || got.Latest != "0.10.0" {
			t.Errorf("Update right after switching on = %+v, want the stored result", got)
		}
		time.Sleep(releaseEvery + releaseJitter) // the next tick is at most 25 hours away
		synctest.Wait()
		sent := transport.count()
		if sent < 1 || sent > 2 {
			t.Fatalf("%d requests in the 25 hours after switching on, want the next tick's (and at most one more)", sent)
		}

		// And off again: no more requests, and the alert is gone at once.
		if got := r.Alerts(); len(got) != 1 {
			t.Errorf("Alerts while on = %+v, want one", got)
		}
		policy.releaseCheck.Store(false)
		if got := r.Alerts(); got != nil {
			t.Errorf("Alerts right after switching off = %+v", got)
		}
		time.Sleep(30 * 24 * time.Hour)
		synctest.Wait()
		if n := transport.count(); n != sent {
			t.Errorf("%d requests after switching off again", n-sent)
		}
		cancel()
		<-done
	})
}

// TestReleaseAlerts: the dashboard alert of each state (04 §11.4).
func TestReleaseAlerts(t *testing.T) {
	feed := string(readFeed(t))
	for _, c := range []struct {
		running string
		want    []api.Alert
	}{
		{"0.3.0", []api.Alert{{Code: api.AlertCodeReleaseSecurityUpdate, Severity: api.AlertSeverityWarn, Params: map[string]any{"version": "0.10.0"}}}},
		{"0.9.1", []api.Alert{{Code: api.AlertCodeReleaseUpdate, Severity: api.AlertSeverityInfo, Params: map[string]any{"version": "0.10.0"}}}},
		{"0.10.0", nil},
		{"0.11.0", nil},
	} {
		transport := &feedTransport{answer: answerWith("", feed)}
		r, _, logs := newTestRelease(t, c.running, transport, nil)
		if got := r.Alerts(); got != nil {
			t.Errorf("running %s: Alerts before a check = %+v", c.running, got)
		}
		r.tick(t.Context())
		r.tick(t.Context())
		if got := r.Alerts(); !reflect.DeepEqual(got, c.want) {
			t.Errorf("running %s: Alerts = %+v, want %+v", c.running, got, c.want)
		}
		// The journal names a newer version once, not at every check.
		wantLines := 0
		if c.want != nil {
			wantLines = 1
		}
		if n := strings.Count(logs.String(), "a newer isshoni release"); n != wantLines {
			t.Errorf("running %s: %d log lines about a newer release after two checks, want %d:\n%s", c.running, n, wantLines, logs.String())
		}
		if c.running == "0.3.0" && !strings.Contains(logs.String(), `"level":"WARN","msg":"a newer isshoni release with a security fix is available"`) {
			t.Errorf("a security release is not a warning in the log:\n%s", logs.String())
		}
	}
}

// TestReleaseCheckFailures: a check that fails changes nothing, says so at debug level only, and the next tick
// tries again (04 §11.5).
func TestReleaseCheckFailures(t *testing.T) {
	good := answerWith("", `[{"tag_name":"v0.10.0","html_url":"https://example.org/v0.10.0"}]`)
	status := answerStatus
	// A release of 9.9.9 that must never be shown: in a body over the 1 MiB that may be on the wire, and in one
	// that is small there and over the 16 MiB that it may expand to.
	overWire := []byte(`[{"tag_name":"v9.9.9","body":"` + strings.Repeat("a", releaseMaxBytes) + `"}]`)
	overDecoded := []byte(`[{"tag_name":"v9.9.9","body":"` + strings.Repeat("a", releaseMaxDecodedBytes) + `"}]`)
	bomb := gzipped(t, overDecoded, gzip.BestSpeed)
	if len(bomb) > releaseMaxBytes/10 {
		t.Fatalf("the oversized feed is %d bytes compressed: too large for a test of the other limit", len(bomb))
	}
	small := gzipped(t, []byte(`[{"tag_name":"v9.9.9","html_url":"https://example.org/v9.9.9"}]`), gzip.DefaultCompression)
	damaged := bytes.Clone(small)
	damaged[len(damaged)-8] ^= 0xff // the first byte of the checksum
	for _, c := range []struct {
		name    string
		answer  func(*http.Request) feedAnswer
		wantLog string
	}{
		{"rate limited", status(http.StatusForbidden, `{"message":"API rate limit exceeded"}`), "the feed answered 403"},
		{"not found", status(http.StatusNotFound, `{"message":"Not Found"}`), "the feed answered 404"},
		{"server error", status(http.StatusBadGateway, "<html>bad gateway</html>"), "the feed answered 502"},
		{"a redirect without a target", status(http.StatusMovedPermanently, ""), "301"},
		{"a 304 nobody asked for", status(http.StatusNotModified, ""), "304 Not Modified to a request without If-None-Match"},
		{"not JSON", status(http.StatusOK, "<html>Sign in to the Wi-Fi</html>"), "not a JSON list of releases"},
		{"an error document with 200", status(http.StatusOK, `{"message":"hello"}`), "not a JSON list of releases"},
		{"larger than 1 MiB", status(http.StatusOK, string(overWire)), `larger than 1048576 bytes"`},
		{"larger than 1 MiB compressed", answerEncoded("gzip", gzipped(t, overWire, gzip.NoCompression)), `larger than 1048576 bytes"`},
		{"larger than 16 MiB once decompressed", answerEncoded("gzip", bomb), "larger than 16777216 bytes decompressed"},
		{"an encoding nobody asked for", answerEncoded("br", small), "which was not asked for"},
		{"two encodings", answerEncoded("gzip, gzip", gzipped(t, small, gzip.DefaultCompression)), "which was not asked for"},
		{"gzip in the header only", answerEncoded("gzip", []byte(`[{"tag_name":"v9.9.9"}]`)), "not the gzip its header says"},
		{"a compressed body that is cut off", answerEncoded("gzip", small[:len(small)-12]), "reading the feed"},
		{"a compressed body with a wrong checksum", answerEncoded("gzip", damaged), "reading the feed"},
		{"compressed, and not JSON", answerEncoded("gzip", gzipped(t, []byte("<html>"), gzip.DefaultCompression)), "not a JSON list of releases"},
		{"no network", func(*http.Request) feedAnswer { return feedAnswer{err: errors.New("dial tcp: no route to host")} }, "no route to host"},
	} {
		transport := &feedTransport{answer: c.answer}
		meta := newFakeTransferStore()
		r, _, logs := newTestRelease(t, "0.9.0", transport, meta)
		r.tick(t.Context())
		if transport.count() != 1 {
			t.Errorf("%s: %d requests for one tick", c.name, transport.count())
		}
		if got := r.Update(); got != nil {
			t.Errorf("%s: Update = %+v after a failed check, want nil", c.name, got)
		}
		if got := meta.metaValue(MetaReleaseCheck); got != "" {
			t.Errorf("%s: a failed check was stored: %s", c.name, got)
		}
		out := logs.String()
		if strings.Count(out, "\n") != 1 || !strings.Contains(out, `"level":"DEBUG"`) ||
			!strings.Contains(out, "release check failed") || !strings.Contains(out, c.wantLog) {
			t.Errorf("%s: want one debug line with %q, got:\n%s", c.name, c.wantLog, out)
		}

		// The next tick works, and one after that fails again: the good result stays.
		transport.set(good)
		r.tick(t.Context())
		if got := r.Update(); got == nil || got.Latest != "0.10.0" {
			t.Errorf("%s: Update after the retry = %+v, want 0.10.0", c.name, got)
		}
		stored := meta.metaValue(MetaReleaseCheck)
		transport.set(c.answer)
		r.tick(t.Context())
		if got := r.Update(); got == nil || got.Latest != "0.10.0" {
			t.Errorf("%s: a failed check lost the last result: Update = %+v", c.name, got)
		}
		if got := meta.metaValue(MetaReleaseCheck); got != stored {
			t.Errorf("%s: a failed check changed the stored result", c.name)
		}
	}
}

// TestReleaseCheckFullSizeFeed: the 1 MiB of 04 §11.5 is the answer on the wire, where GitHub compresses it. As
// JSON a page of 20 releases is larger than that once a release has a few dozen assets, and such a page is read
// like any other, through real sockets on 127.0.0.1 and the client the server uses.
func TestReleaseCheckFullSizeFeed(t *testing.T) {
	// 40 assets: the 22 of 06 §3 and the desktop builds that later milestones add to the same release.
	feed := fullShapeFeed(t, releasePerPage, 40, 4000)
	wire := gzipped(t, feed, gzip.DefaultCompression)
	t.Logf("a page of %d releases with 40 assets each: %d bytes of JSON, %d bytes compressed", releasePerPage, len(feed), len(wire))
	if len(feed) <= releaseMaxBytes {
		t.Fatalf("the feed is %d bytes of JSON: this test needs one over the %d of the limit", len(feed), releaseMaxBytes)
	}
	if len(wire) > releaseMaxBytes/4 {
		t.Errorf("the feed is %d bytes compressed: a real page must stay well below the limit of %d", len(wire), releaseMaxBytes)
	}

	var compress atomic.Bool
	compress.Store(true)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if !compress.Load() || r.Header.Get("Accept-Encoding") != "gzip" {
			_, _ = w.Write(feed)
			return
		}
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(wire)
	}))
	defer srv.Close()

	check := func() (*ReleaseCheck, *syncBuffer) {
		t.Helper()
		policy := new(fakePolicy)
		policy.releaseCheck.Store(true)
		log, logs := testLogger()
		r, err := NewReleaseCheck(ReleaseCheckOptions{Policy: policy, URL: srv.URL, Version: "0.16.0", Logger: log})
		if err != nil {
			t.Fatal(err)
		}
		r.tick(t.Context())
		return r, logs
	}

	r, logs := check()
	got := r.Update()
	if got == nil {
		t.Fatalf("Update after a full-size feed = nil; the check said:\n%s", logs.String())
	}
	// 0.17.0 is the security release, and the running version is the one before it.
	if got.Latest != "0.20.0" || got.URL != "https://github.com/MoonWX/isshoni/releases/tag/v0.20.0" || !got.Security {
		t.Errorf("Update = %+v, want 0.20.0 with the security flag", got)
	}
	if alerts := r.Alerts(); len(alerts) != 1 || alerts[0].Code != api.AlertCodeReleaseSecurityUpdate {
		t.Errorf("Alerts = %+v, want release.security_update", alerts)
	}

	// The same page from a server or through a proxy that does not compress is over the limit on the wire.
	compress.Store(false)
	r, logs = check()
	if got := r.Update(); got != nil {
		t.Errorf("Update after %d bytes on the wire = %+v, want nil", len(feed), got)
	}
	if !strings.Contains(logs.String(), "larger than 1048576 bytes") {
		t.Errorf("no debug line about the size:\n%s", logs.String())
	}
}

// TestReleaseFeedLimits: the two limits to the byte. On the wire a body may be 1 MiB, whatever it expands to;
// decompressed it may be 16 MiB.
func TestReleaseFeedLimits(t *testing.T) {
	want := []knownRelease{{Version: "9.9.9"}}
	// A compressed body may be several gzip members in a row. With enough empty ones it is over the limit on the
	// wire and expands to a hundred bytes.
	empty := gzipped(t, nil, gzip.BestSpeed)
	members := append(gzipped(t, paddedFeed(100), gzip.BestSpeed), bytes.Repeat(empty, releaseMaxBytes/len(empty)+1)...)
	fourTimes := gzipped(t, paddedFeed(4*releaseMaxBytes), gzip.BestSpeed)
	for _, c := range []struct {
		name     string
		encoding string
		body     []byte
		wantErr  string // "" means the feed is read
	}{
		{"1 MiB as it is", "", paddedFeed(releaseMaxBytes), ""},
		{"1 MiB and a byte", "", paddedFeed(releaseMaxBytes + 1), "larger than 1048576 bytes"},
		{"1 MiB, said to be as it is", "Identity", paddedFeed(releaseMaxBytes), ""},
		{"more than 1 MiB that expands to less", "gzip", members, "larger than 1048576 bytes"},
		{"more than 1 MiB, not compressed inside", "gzip", gzipped(t, paddedFeed(releaseMaxBytes), gzip.NoCompression), "larger than 1048576 bytes"},
		{"more than 1 MiB of JSON in less on the wire", "gzip", fourTimes, ""},
		{"the same under the old name of gzip", "X-GZIP", fourTimes, ""},
		{"16 MiB decompressed", "gzip", gzipped(t, paddedFeed(releaseMaxDecodedBytes), gzip.BestSpeed), ""},
		{"16 MiB and a byte decompressed", "gzip", gzipped(t, paddedFeed(releaseMaxDecodedBytes+1), gzip.BestSpeed), "larger than 16777216 bytes decompressed"},
		{"two members", "gzip", append(gzipped(t, []byte(`[{"tag_name":`), gzip.BestSpeed), gzipped(t, []byte(`"v9.9.9"}]`), gzip.BestSpeed)...), ""},
		{"something after the compressed body", "gzip", append(gzipped(t, paddedFeed(100), gzip.BestSpeed), "more"...), "reading the feed"},
		{"an empty compressed body", "gzip", gzipped(t, nil, gzip.BestSpeed), "not a JSON list of releases"},
		{"no body", "gzip", nil, "not the gzip its header says"},
	} {
		got, err := readFeedBody(c.encoding, c.body)
		switch {
		case c.wantErr == "" && (err != nil || !reflect.DeepEqual(got, want)):
			t.Errorf("%s (%d bytes on the wire): %+v, %v; want it read", c.name, len(c.body), got, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr) || got != nil):
			t.Errorf("%s (%d bytes on the wire): %+v, %v; want an error with %q", c.name, len(c.body), got, err, c.wantErr)
		}
	}

	// An encoding from the network goes into the log, so only the start of it does.
	_, err := readFeedBody(strings.Repeat("z", 5000), []byte("[]"))
	if err == nil || len(err.Error()) > 200 || !strings.Contains(err.Error(), "which was not asked for") {
		t.Errorf("an endless Content-Encoding: %v", err)
	}
}

// A feed that does not answer is given up after 10 s (04 §11.5).
func TestReleaseCheckTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		transport := &feedTransport{answer: func(*http.Request) feedAnswer { return feedAnswer{silent: true} }}
		r, _, logs := newTestRelease(t, "0.9.0", transport, nil)
		start := time.Now()
		r.tick(t.Context())
		if took := time.Since(start); took != releaseTimeout {
			t.Errorf("the check gave up after %v, want %v", took, releaseTimeout)
		}
		if got := r.Update(); got != nil {
			t.Errorf("Update after a timeout = %+v", got)
		}
		if !strings.Contains(logs.String(), "release check failed") {
			t.Errorf("no debug line for the timeout:\n%s", logs.String())
		}

		// A body that stops halfway is cut off by the same 10 s.
		transport.set(func(*http.Request) feedAnswer { return feedAnswer{status: http.StatusOK, stalled: true} })
		start = time.Now()
		r.tick(t.Context())
		if took := time.Since(start); took != releaseTimeout {
			t.Errorf("a stalled body was given up after %v, want %v", took, releaseTimeout)
		}
	})
}

// stalledBody is a response body that delivers nothing until the request is cancelled. net/http's transport ends
// a body like this when the request's context does; a RoundTripper of a test has to do it itself.
type stalledBody struct{ ctx context.Context }

func (b stalledBody) Read([]byte) (int, error) {
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}
func (stalledBody) Close() error { return nil }

// TestReleaseCheckPersistence: the result is in the meta table (04 §11.5), so a restarted server shows it before
// its first check, sends the ETag it has, and reads the stored releases with the version it now runs.
func TestReleaseCheckPersistence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		feed := `[
			{"tag_name":"v0.10.0","html_url":"https://github.com/MoonWX/isshoni/releases/tag/v0.10.0","body":"Notes"},
			{"tag_name":"v0.9.1","html_url":"https://github.com/MoonWX/isshoni/releases/tag/v0.9.1","body":"<!-- isshoni:security -->"},
			{"tag_name":"v0.11.0-rc.1","html_url":"https://github.com/MoonWX/isshoni/releases/tag/v0.11.0-rc.1","prerelease":true}
		]`
		transport := &feedTransport{answer: answerWith(`W/"abc"`, feed)}
		meta := newFakeTransferStore()

		r, _, _ := newTestRelease(t, "0.9.0", transport, meta)
		r.tick(t.Context())
		// The stored form is a contract with every later version of the server: this is it, to the byte.
		const stored = `{"v":1,"checkedAt":"2000-01-01T00:00:00.000Z","etag":"W/\"abc\"","releases":[` +
			`{"version":"0.11.0-rc.1","url":"https://github.com/MoonWX/isshoni/releases/tag/v0.11.0-rc.1","prerelease":true},` +
			`{"version":"0.10.0","url":"https://github.com/MoonWX/isshoni/releases/tag/v0.10.0"},` +
			`{"version":"0.9.1","url":"https://github.com/MoonWX/isshoni/releases/tag/v0.9.1","security":true}]}`
		if got := meta.metaValue(MetaReleaseCheck); got != stored {
			t.Errorf("meta %s:\n got %s\nwant %s", MetaReleaseCheck, got, stored)
		}
		if !json.Valid([]byte(stored)) {
			t.Fatal("the expected value is not JSON")
		}
		checkedAt := time.Now().UTC().Truncate(time.Millisecond)

		// The server restarts a day later, on the same version.
		time.Sleep(24 * time.Hour)
		run := func(running string) (*ReleaseCheck, context.CancelFunc) {
			r, _, _ := newTestRelease(t, running, transport, meta)
			ctx, cancel := context.WithCancel(t.Context())
			go func() { _ = r.Run(ctx) }()
			synctest.Wait()
			return r, cancel
		}
		before := transport.count()
		r, cancel := run("0.9.0")
		want := &api.UpdateInfo{Latest: "0.10.0", URL: "https://github.com/MoonWX/isshoni/releases/tag/v0.10.0", Security: true, CheckedAt: checkedAt}
		if got := r.Update(); !reflect.DeepEqual(got, want) {
			t.Errorf("Update right after a restart = %+v, want the stored result %+v", got, want)
		}
		if got := r.Alerts(); len(got) != 1 || got[0].Code != api.AlertCodeReleaseSecurityUpdate {
			t.Errorf("Alerts right after a restart = %+v", got)
		}
		if n := transport.count() - before; n != 0 {
			t.Errorf("%d requests at the start: the stored result must do until the first check", n)
		}
		time.Sleep(releaseFirstMax)
		synctest.Wait()
		if n := transport.count() - before; n != 1 {
			t.Fatalf("%d requests in the first hour after the restart, want 1", n)
		}
		if got := transport.last().Header.Get("If-None-Match"); got != `W/"abc"` {
			t.Errorf("the first check after a restart sent If-None-Match %q, want the stored ETag", got)
		}
		if got := r.Update(); got == nil || !got.CheckedAt.After(checkedAt) || got.Latest != "0.10.0" {
			t.Errorf("Update after the 304 = %+v, want the same result with a new time", got)
		}
		cancel()
		synctest.Wait()

		// The admin installs 0.9.1: the stored list means something else now, before any request.
		r, cancel = run("0.9.1")
		if got := r.Update(); got == nil || got.Latest != "0.10.0" || got.Security {
			t.Errorf("Update after upgrading to 0.9.1 = %+v, want 0.10.0 without the security flag", got)
		}
		if got := r.Alerts(); len(got) != 1 || got[0].Code != api.AlertCodeReleaseUpdate {
			t.Errorf("Alerts after upgrading to 0.9.1 = %+v, want release.update", got)
		}
		cancel()
		synctest.Wait()

		// On 0.10.0 nothing is newer; on a release candidate of 0.11.0 the stored prerelease counts.
		r, cancel = run("0.10.0")
		if got := r.Alerts(); got != nil {
			t.Errorf("Alerts on the latest release = %+v", got)
		}
		cancel()
		synctest.Wait()
		r, cancel = run("0.11.0-rc.0")
		if got := r.Update(); got == nil || got.Latest != "0.11.0-rc.1" {
			t.Errorf("Update on a release candidate = %+v, want 0.11.0-rc.1", got)
		}
		cancel()
		synctest.Wait()

		// A store that fails costs only the memory: the check still works.
		meta.fail(func(s *fakeTransferStore) {
			s.failGM, s.failSet = errors.New("database is locked"), errors.New("database is locked")
		})
		r, _, logs := newTestRelease(t, "0.9.0", transport, meta)
		r.load(t.Context())
		if got := r.Update(); got != nil {
			t.Errorf("Update when the stored result can't be read = %+v, want nil", got)
		}
		r.tick(t.Context())
		if got := r.Update(); got == nil || got.Latest != "0.10.0" {
			t.Errorf("Update after a check whose result could not be stored = %+v", got)
		}
		if out := logs.String(); !strings.Contains(out, "could not read the last result") || !strings.Contains(out, "could not store the result") {
			t.Errorf("no debug lines about the failing store:\n%s", out)
		}
	})
}

// What the meta table holds is checked like the feed: a restored backup or another build may have written it.
func TestDecodeReleaseState(t *testing.T) {
	for name, value := range map[string]string{
		"missing":           ``,
		"not JSON":          `checked`,
		"another version":   `{"v":2,"checkedAt":"2026-09-29T09:12:00.000Z","releases":[{"version":"0.3.1"}]}`,
		"no version":        `{"checkedAt":"2026-09-29T09:12:00.000Z","releases":[{"version":"0.3.1"}]}`,
		"never checked":     `{"v":1,"releases":[{"version":"0.3.1"}]}`,
		"the old verdict":   `{"latest":"0.3.1","url":"https://example.org","security":true,"checkedAt":"2026-09-29T09:12:00.000Z"}`,
		"a bad time":        `{"v":1,"checkedAt":"yesterday","releases":[]}`,
		"releases a string": `{"v":1,"checkedAt":"2026-09-29T09:12:00.000Z","releases":"0.3.1"}`,
	} {
		if state, ok := decodeReleaseState(value); ok {
			t.Errorf("%s: decodeReleaseState(%s) = %+v, want it refused", name, value, state)
		}
	}

	state, ok := decodeReleaseState(`{"v":1,"checkedAt":"2026-09-29T09:12:00Z","etag":"W/\"x\"\r\nX-Injected: 1","releases":[
		{"version":"0.3.0","url":"javascript:alert(1)"},
		{"version":"not-a-version","url":"https://example.org/x","security":true},
		{"version":"0.10.0","url":"https://example.org/v0.10.0","security":true},
		{"version":"0.4.0-rc.1"},
		{"version":"0.10.0"}
	]}`)
	if !ok {
		t.Fatal("a readable state was refused")
	}
	want := releaseState{
		V:         1,
		CheckedAt: api.WireTime(time.Date(2026, 9, 29, 9, 12, 0, 0, time.UTC)),
		ETag:      "", // it would not survive as a header
		Releases: []knownRelease{
			{Version: "0.10.0", URL: "https://example.org/v0.10.0", Security: true},
			{Version: "0.4.0-rc.1", Prerelease: true},
			{Version: "0.3.0"},
		},
	}
	if !time.Time(state.CheckedAt).Equal(time.Time(want.CheckedAt)) {
		t.Errorf("CheckedAt = %v, want %v", state.CheckedAt, want.CheckedAt)
	}
	state.CheckedAt = want.CheckedAt
	if !reflect.DeepEqual(state, want) {
		t.Errorf("decodeReleaseState:\n got %+v\nwant %+v", state, want)
	}
}

func TestCleanETag(t *testing.T) {
	for in, want := range map[string]string{
		`W/"3f2a9c1e"`:                       `W/"3f2a9c1e"`,
		`"abc def"`:                          `"abc def"`,
		``:                                   ``,
		"\"a\r\nb\"":                         ``,
		"\"a\x00b\"":                         ``,
		"\"caf\u00e9\"":                      ``,
		`"` + strings.Repeat("a", 300) + `"`: ``,
	} {
		if got := cleanETag(in); got != want {
			t.Errorf("cleanETag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestNewReleaseCheck(t *testing.T) {
	policy := new(fakePolicy)

	// The default is the config key's default: one URL in two packages, kept the same by this test.
	key, ok := config.Lookup("updates.release_url")
	if !ok || key.Default != DefaultReleasesURL {
		t.Errorf("config's updates.release_url default is %v, DefaultReleasesURL is %q", key.Default, DefaultReleasesURL)
	}

	for raw, want := range map[string]string{
		"":                                 "https://api.github.com/repos/MoonWX/isshoni/releases?per_page=20",
		DefaultReleasesURL:                 "https://api.github.com/repos/MoonWX/isshoni/releases?per_page=20",
		"http://127.0.0.1:8099/releases":   "http://127.0.0.1:8099/releases?per_page=20",
		"https://example.org/r?per_page=5": "https://example.org/r?per_page=20",
		"https://example.org/r?x=1#top":    "https://example.org/r?per_page=20&x=1",
	} {
		r, err := NewReleaseCheck(ReleaseCheckOptions{Policy: policy, URL: raw})
		if err != nil {
			t.Errorf("NewReleaseCheck(URL %q): %v", raw, err)
			continue
		}
		if r.url != want {
			t.Errorf("URL %q is requested as %s, want %s", raw, r.url, want)
		}
	}
	for _, bad := range []string{
		"api.github.com/repos/MoonWX/isshoni/releases", "ftp://example.org/releases", "file:///etc/passwd",
		"https://", "/releases", "https://token@example.org/releases", "https://user:pw@example.org/releases", "ht!tp://x",
	} {
		if r, err := NewReleaseCheck(ReleaseCheckOptions{Policy: policy, URL: bad}); err == nil {
			t.Errorf("NewReleaseCheck(URL %q) succeeded with %s", bad, r.url)
		}
	}

	func() {
		defer func() {
			if recover() == nil {
				t.Error("NewReleaseCheck without a Policy did not panic: nothing could switch the check off")
			}
		}()
		_, _ = NewReleaseCheck(ReleaseCheckOptions{})
	}()
}

// FuzzParseReleaseFeed: whatever the network delivers as the feed, the parser does not panic, and what it returns
// is safe to store and to show: a bounded list of distinct SemVer versions, newest first, with https links or none.
func FuzzParseReleaseFeed(f *testing.F) {
	f.Add(readFeed(f))
	for _, seed := range []string{
		`[]`, `null`, `{}`, `[null]`, `[{"tag_name":"v1.0.0"}]`,
		`[{"tag_name":"v1.0.0","draft":true},{"tag_name":"v1.0.0-rc.1","prerelease":true}]`,
		`[{"tag_name":"0.4.0","html_url":"javascript:alert(1)","body":"<!-- isshoni:security -->"}]`,
		`[{"tag_name":"v1.2.3+build","html_url":"https://example.org/\u0000"}]`,
		`[{"tag_name":"v1"},{"tag_name":"v1.0"},{"tag_name":"v1.0.0"}]`,
		`{"message":"API rate limit exceeded"}`,
		`[{"tag_name":17,"body":[]}]`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		releases, err := parseReleaseFeed(bytes.NewReader(data))
		// As the body of an answer, as it is and compressed, the feed reads the same; and bytes that are only said
		// to be compressed are refused or read, without a panic.
		if len(data) <= releaseMaxBytes {
			for encoding, wire := range map[string][]byte{"identity": data, "gzip": gzipped(t, data, gzip.BestSpeed)} {
				got, gotErr := readFeedBody(encoding, wire)
				if (gotErr == nil) != (err == nil) || !reflect.DeepEqual(got, releases) {
					t.Fatalf("as a body with the encoding %s: %+v, %v; parsed directly: %+v, %v", encoding, got, gotErr, releases, err)
				}
			}
			_, _ = readFeedBody("gzip", data)
		}
		if err != nil {
			return
		}
		if len(releases) > releasePerPage {
			t.Fatalf("%d releases, more than a page", len(releases))
		}
		for i, rel := range releases {
			v := "v" + rel.Version
			if !semver.IsValid(v) || semver.Canonical(v) != v || len(rel.Version) > releaseMaxTagLen {
				t.Fatalf("release %d has version %q", i, rel.Version)
			}
			if i > 0 && semver.Compare("v"+releases[i-1].Version, v) <= 0 {
				t.Fatalf("release %d (%s) is not older than the one before (%s)", i, rel.Version, releases[i-1].Version)
			}
			if rel.URL != "" {
				u, err := url.Parse(rel.URL)
				if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
					t.Fatalf("release %d has URL %q", i, rel.URL)
				}
			}
			if !rel.Prerelease && semver.Prerelease(v) != "" {
				t.Fatalf("release %d (%s) is a SemVer prerelease and not marked", i, rel.Version)
			}
		}
		// The list survives its own storage format unchanged.
		value, err := json.Marshal(releaseState{V: releaseStateVersion, CheckedAt: api.WireTime(time.Unix(1790000000, 0)), Releases: releases})
		if err != nil {
			t.Fatal(err)
		}
		state, ok := decodeReleaseState(string(value))
		if !ok {
			t.Fatalf("the stored form is refused: %s", value)
		}
		if len(state.Releases) != len(releases) || (len(releases) > 0 && !reflect.DeepEqual(state.Releases, releases)) {
			t.Fatalf("stored and read back:\n got %+v\nwant %+v", state.Releases, releases)
		}
		// And every version can look at it.
		for _, running := range []string{"0.0.0-dev", "0.9.0", "1.0.0-rc.1", "devel"} {
			view := viewReleases(running, releases)
			if view.security && !view.newer {
				t.Fatalf("running %s: a security release without a newer release", running)
			}
			if view.newer && !view.found {
				t.Fatalf("running %s: newer without a latest release", running)
			}
		}
	})
}
