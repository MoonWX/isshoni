package ops

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/mod/semver"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/protocol/api"
	"github.com/MoonWX/isshoni/internal/version"
)

// The release check (04 §11.5): once a day the server asks GitHub for the project's list of releases, and the
// dashboard then says whether a newer one exists and whether it fixes a security problem.
//
// It is the only thing the server tells GitHub, and it is on the privacy page (04 §16), so what goes out is fixed:
// one GET of the releases feed with these headers and nothing else of ours, no cookie, no token, no identifier, no
// request body:
//
//	GET https://api.github.com/repos/MoonWX/isshoni/releases?per_page=20
//	Accept: application/vnd.github+json
//	X-GitHub-Api-Version: 2022-11-28
//	User-Agent: isshoni/<version>
//	If-None-Match: <the ETag of the last answer>      (when there is one)
//
// Next to them go the headers of any HTTP client, which name nothing of ours: Host, Accept-Encoding: gzip and,
// from the check's own client, Connection: close.
//
// The setting updateCheck (policy key updates.release_check) switches it off; it is read through Policy before
// every request, so an admin's switch needs no restart, and with it off nothing is sent at all. A redirect is
// followed only to the same host over the same scheme, which is how GitHub answers for a repository that was
// renamed; one that points anywhere else ends the check.

// DefaultReleasesURL is the releases feed of the project on the GitHub API: the default of the hidden config key
// updates.release_url, which tests point at a server of their own.
const DefaultReleasesURL = "https://api.github.com/repos/MoonWX/isshoni/releases"

// SecurityMarker is the HTML comment that marks a release as a security release: 06's release template puts it in
// the release notes, and the check looks for it in the body of every release newer than the running one.
const SecurityMarker = "<!-- isshoni:security -->"

const (
	// The first check runs 5 to 60 minutes after the start, at random, so that a fleet restarted together (an
	// unattended upgrade, a provider's maintenance) does not ask at the same moment; then every 24 h ± 1 h.
	releaseFirstMin = 5 * time.Minute
	releaseFirstMax = 60 * time.Minute
	releaseEvery    = 24 * time.Hour
	releaseJitter   = time.Hour
	// releaseTimeout covers one check from the connection to the last byte.
	releaseTimeout = 10 * time.Second
	// releaseMaxBytes is the most one answer may be on the wire (04 §11.5), compressed as GitHub sends it. The
	// limit can't count the JSON itself: GitHub describes every asset of every release in about 2 kB, so a page of
	// 20 releases with 22 assets each (06 §3) is about 1 MB of JSON. Compressed it is less than a tenth of that.
	releaseMaxBytes = 1 << 20
	// releaseMaxDecodedBytes is the most an answer may be once it is decompressed: the bound on an answer that is
	// made to expand without end, and room for some 350 assets per release. It is read as a stream, one release
	// at a time, never as a whole.
	releaseMaxDecodedBytes = 16 << 20
	// releasePerPage is how many releases one check looks at, newest first.
	releasePerPage      = 20
	releaseMaxRedirects = 3
	// Bounds on what is kept from a feed: it comes from the network, and it goes into the database and the dashboard.
	releaseMaxTagLen  = 64
	releaseMaxURLLen  = 512
	releaseMaxETagLen = 200
	// releaseMaxEncodingLen is how much of a Content-Encoding the check does not know goes into the log.
	releaseMaxEncodingLen = 32
	// releaseStateVersion is the version of the JSON in MetaReleaseCheck.
	releaseStateVersion = 1
	// releaseStoreTimeout bounds one read or write of the meta key.
	releaseStoreTimeout = 5 * time.Second
)

// ReleaseCheckOptions configures NewReleaseCheck. Only Policy is required.
type ReleaseCheckOptions struct {
	// Policy is the switch: ReleaseCheck() is asked before every request and by Update and Alerts. Required.
	Policy Policy
	// URL is updates.release_url. "" means DefaultReleasesURL.
	URL string
	// Version is the running version without a leading v. "" means version.Version().
	Version string
	// Meta keeps the last result across restarts (MetaReleaseCheck). nil keeps it in memory only.
	Meta MetaStore
	// HTTP is the client the request goes through (server.Deps.ReleaseHTTP: tests). Its transport, proxy and TLS
	// settings are used; its cookie jar and redirect policy are not. nil means a client of the check's own, which
	// keeps no connection open between checks.
	HTTP *http.Client
	// Now is the clock for the time of a check. nil means time.Now.
	Now func() time.Time
	// Logger is the server's logger; the check adds component=ops. nil means slog.Default().
	Logger *slog.Logger
}

// knownRelease is one published release of the feed, as far as the check needs it. It is also what
// MetaReleaseCheck stores, so the JSON names are fixed.
type knownRelease struct {
	Version    string `json:"version"`              // canonical SemVer without the leading v, "0.3.1"
	URL        string `json:"url,omitempty"`        // the release page, an https URL; "" when the feed had none
	Prerelease bool   `json:"prerelease,omitempty"` // flagged as one on GitHub, or a SemVer prerelease
	Security   bool   `json:"security,omitempty"`   // its notes carry SecurityMarker
}

// releaseState is the result of the last check and the value of MetaReleaseCheck. It holds the releases, not a
// verdict: what they mean depends on the running version, and that changes with an upgrade while this state
// stays in the database.
type releaseState struct {
	V         int            `json:"v"`
	CheckedAt api.WireTime   `json:"checkedAt"`
	ETag      string         `json:"etag,omitempty"`
	Releases  []knownRelease `json:"releases"` // newest version first
}

func (s releaseState) checked() bool { return !s.CheckedAt.IsZero() }

// ReleaseCheck is the daily release check. NewReleaseCheck builds it, Run drives it until the server stops; Update
// and Alerts may be called at any time and from any goroutine.
type ReleaseCheck struct {
	policy  Policy
	url     string // the feed with per_page set
	version string
	meta    MetaStore
	client  *http.Client
	now     func() time.Time
	log     *slog.Logger

	running atomic.Bool

	mu        sync.Mutex
	state     releaseState
	announced string // the newer version that the log has named already
}

// NewReleaseCheck builds the check. It starts nothing and sends nothing. A nil Policy is a wiring bug and panics:
// without it nothing could switch the check off. The error is for a URL that is not an absolute http or https URL.
func NewReleaseCheck(o ReleaseCheckOptions) (*ReleaseCheck, error) {
	if o.Policy == nil {
		panic("ops: NewReleaseCheck: nil Policy")
	}
	raw := o.URL
	if raw == "" {
		raw = DefaultReleasesURL
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("ops: release check: updates.release_url: %w", err)
	}
	if (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil {
		return nil, fmt.Errorf("ops: release check: updates.release_url must be an http(s) URL without credentials, got %q", raw)
	}
	q := u.Query()
	q.Set("per_page", strconv.Itoa(releasePerPage))
	u.RawQuery = q.Encode()
	u.Fragment = ""

	running := o.Version
	if running == "" {
		running = version.Version()
	}
	now := o.Now
	if now == nil {
		now = time.Now
	}
	log := o.Logger
	if log == nil {
		log = slog.Default()
	}

	var client http.Client
	if o.HTTP != nil {
		client = *o.HTTP
	} else {
		// One request a day: a connection kept alive in between would only be a socket that GitHub closes.
		client.Transport = &http.Transport{Proxy: http.ProxyFromEnvironment, DisableKeepAlives: true}
	}
	client.Jar = nil // no cookie is ever sent or kept
	client.CheckRedirect = sameHostRedirect

	return &ReleaseCheck{
		policy:  o.Policy,
		url:     u.String(),
		version: running,
		meta:    o.Meta,
		client:  &client,
		now:     now,
		log:     log.With(slog.String("component", "ops")),
	}, nil
}

// sameHostRedirect is the client's redirect policy: follow a redirect only to the host and scheme of the first
// request, a few times at most, and without the Referer that net/http would add.
func sameHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) > releaseMaxRedirects {
		return errors.New("ops: release check: too many redirects")
	}
	if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
		return fmt.Errorf("ops: release check: the feed redirects to %s://%s, which is not followed", req.URL.Scheme, req.URL.Host)
	}
	req.Header.Del("Referer")
	return nil
}

// Run loads the last result from the meta table, so the dashboard has it right after a restart, then checks 5 to
// 60 minutes after the start and every 24 h ± 1 h from then on, until ctx ends; it then returns nil. A tick with
// the check switched off sends nothing. A check that fails is logged at debug level, and the next tick tries again.
// Run can be called once.
func (r *ReleaseCheck) Run(ctx context.Context) error {
	if !r.running.CompareAndSwap(false, true) {
		return errors.New("ops: ReleaseCheck.Run called more than once")
	}
	r.load(ctx)

	timer := time.NewTimer(firstReleaseDelay())
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timer.C:
			r.tick(ctx)
			timer.Reset(nextReleaseDelay())
		}
	}
}

// firstReleaseDelay is the time from the start to the first check: 5 to 60 minutes.
func firstReleaseDelay() time.Duration {
	return releaseFirstMin + rand.N(releaseFirstMax-releaseFirstMin+1) //nolint:gosec // G404: jitter, not a secret
}

// nextReleaseDelay is the time from one check to the next: 23 to 25 hours.
func nextReleaseDelay() time.Duration {
	return releaseEvery - releaseJitter + rand.N(2*releaseJitter+1) //nolint:gosec // G404: jitter, not a secret
}

// tick is one round of Run: ask the policy, then check. It is the only caller of check, so nothing reaches the
// network past the switch.
func (r *ReleaseCheck) tick(ctx context.Context) {
	if !r.policy.ReleaseCheck() {
		return
	}
	if err := r.check(ctx); err != nil {
		if ctx.Err() == nil {
			r.log.LogAttrs(ctx, slog.LevelDebug, "release check failed; trying again at the next one", logx.Err(err))
		}
		return
	}
	r.announce(ctx)
}

// check does one request and takes its answer: 200 with the list of releases, or 304 when the list is the one of
// the last check.
func (r *ReleaseCheck) check(ctx context.Context) error {
	now := r.now()
	r.mu.Lock()
	prev := r.state
	r.mu.Unlock()

	reqCtx, cancel := context.WithTimeout(ctx, releaseTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, r.url, nil)
	if err != nil {
		return fmt.Errorf("ops: release check: %w", err)
	}
	// Everything that is sent (04 §11.5). net/http adds Host, which names nothing of ours.
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "isshoni/"+r.version)
	// This is the Accept-Encoding net/http sends by itself, so the request is the same. Set here, it makes the
	// transport hand over the body as it came instead of decompressing it out of sight, and readReleaseFeed can
	// count the bytes on the wire.
	req.Header.Set("Accept-Encoding", "gzip")
	conditional := prev.checked() && prev.ETag != ""
	if conditional {
		req.Header.Set("If-None-Match", prev.ETag)
	}
	res, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("ops: release check: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	next := prev
	switch res.StatusCode {
	case http.StatusNotModified:
		if !conditional {
			return errors.New("ops: release check: 304 Not Modified to a request without If-None-Match")
		}
	case http.StatusOK:
		releases, err := readReleaseFeed(res)
		if err != nil {
			return err
		}
		next = releaseState{V: releaseStateVersion, ETag: cleanETag(res.Header.Get("ETag")), Releases: releases}
	default:
		return fmt.Errorf("ops: release check: the feed answered %s", res.Status)
	}
	// In the form the meta table gives back, so the result reads the same before and after a restart.
	next.CheckedAt = api.WireTime(now.UTC().Truncate(time.Millisecond))

	r.mu.Lock()
	r.state = next
	r.mu.Unlock()
	r.save(ctx, next)
	return nil
}

// readReleaseFeed reads and parses the body of a 200 answer within the two size limits: releaseMaxBytes for the
// body as it is on the wire, which is 04 §11.5's response limit, and releaseMaxDecodedBytes for what a compressed
// body expands to. The body is read to its end, so a compressed one is checked against its checksum, and a body
// over a limit is refused however much of it parsed.
func readReleaseFeed(res *http.Response) ([]knownRelease, error) {
	wire := &io.LimitedReader{R: res.Body, N: releaseMaxBytes + 1}
	var plain io.Reader = wire
	switch encoding := strings.ToLower(strings.TrimSpace(strings.Join(res.Header.Values("Content-Encoding"), ","))); encoding {
	case "", "identity":
		// As it is: a server or a proxy that does not compress.
	case "gzip", "x-gzip":
		unzip, err := gzip.NewReader(wire)
		if err != nil {
			return nil, fmt.Errorf("ops: release check: the feed is not the gzip its header says: %w", err)
		}
		defer func() { _ = unzip.Close() }()
		plain = unzip
	default:
		// Only gzip was asked for. What this is can't be told, so it is not parsed.
		if len(encoding) > releaseMaxEncodingLen {
			encoding = encoding[:releaseMaxEncodingLen]
		}
		return nil, fmt.Errorf("ops: release check: the feed has the content encoding %q, which was not asked for", encoding)
	}
	source := &failingReader{r: plain}
	decoded := &io.LimitedReader{R: source, N: releaseMaxDecodedBytes + 1}

	releases, err := parseReleaseFeed(decoded)
	// The parser's error says what it found where the body stopped. Why the body stopped there is the cause:
	switch {
	case wire.N <= 0:
		// The limit cut it off, or the list was complete in a body that is too large all the same.
		return nil, fmt.Errorf("ops: release check: the feed is larger than %d bytes", releaseMaxBytes)
	case decoded.N <= 0:
		return nil, fmt.Errorf("ops: release check: the feed is larger than %d bytes decompressed", releaseMaxDecodedBytes)
	case source.err != nil:
		// The connection, the 10 s, or a compressed body that is cut off or damaged.
		return nil, fmt.Errorf("ops: release check: reading the feed: %w", source.err)
	}
	return releases, err
}

// failingReader passes reads through and keeps the first error that is not the end of the stream, so that a body
// which could not be read is not reported as one that could not be parsed.
type failingReader struct {
	r   io.Reader
	err error
}

func (f *failingReader) Read(p []byte) (int, error) {
	n, err := f.r.Read(p)
	if err != nil && !errors.Is(err, io.EOF) && f.err == nil {
		f.err = err
	}
	return n, err
}

// feedRelease is one element of GitHub's "list releases" answer; the other fields are not read.
type feedRelease struct {
	TagName    string `json:"tag_name"`
	HTMLURL    string `json:"html_url"`
	Draft      bool   `json:"draft"`
	Prerelease bool   `json:"prerelease"`
	Body       string `json:"body"`
}

// parseReleaseFeed reads the feed's JSON, a list of releases, to its end and returns the published ones with a
// SemVer tag, newest version first and each version once. Drafts are left out (04 §11.5), and so is a release
// whose tag is not a version ("nightly"). At most releasePerPage come back.
//
// The list is decoded one release at a time, and what is kept of it is cut down as it grows, so the memory in use
// is that of the largest release (most of which is the description of its assets, which is skipped), not that of
// the feed.
func parseReleaseFeed(feed io.Reader) ([]knownRelease, error) {
	notAList := func(err error) error {
		if errors.Is(err, io.EOF) {
			err = io.ErrUnexpectedEOF // the feed ended where more of it had to follow
		}
		return fmt.Errorf("ops: release check: the feed is not a JSON list of releases: %w", err)
	}
	// Not what was found instead: that is as long as the feed makes it.
	errNoList := errors.New("ops: release check: the feed is not a JSON list of releases")
	dec := json.NewDecoder(feed)
	releases := make([]knownRelease, 0, 2*releasePerPage)

	start, err := dec.Token()
	if err != nil {
		return nil, notAList(err)
	}
	switch start {
	case nil:
		// null: no list is an empty list.
	case json.Delim('['):
		for dec.More() {
			var f feedRelease
			if err := dec.Decode(&f); err != nil {
				return nil, notAList(err)
			}
			if f.Draft {
				continue
			}
			v, ok := tagVersion(f.TagName)
			if !ok {
				continue
			}
			releases = append(releases, knownRelease{
				Version:    v,
				URL:        cleanReleaseURL(f.HTMLURL),
				Prerelease: f.Prerelease || semver.Prerelease("v"+v) != "",
				Security:   strings.Contains(f.Body, SecurityMarker),
			})
			if len(releases) >= 2*releasePerPage {
				// More than a page: keep the newest page of what came so far. A version that is cut here has a
				// page of newer ones before it, so it would not be in the result anyway.
				releases = orderReleases(releases)
			}
		}
		if _, err := dec.Token(); err != nil { // the closing bracket
			return nil, notAList(err)
		}
	default:
		return nil, errNoList // GitHub's error document, a number, a string
	}
	// Nothing may follow the list. Reading on to the end is also what makes the caller's size limits exact.
	switch _, err := dec.Token(); {
	case err == nil:
		return nil, errNoList
	case !errors.Is(err, io.EOF):
		return nil, notAList(err)
	}
	return orderReleases(releases), nil
}

// tagVersion turns a release tag ("v0.3.1"; "0.3.1" is accepted too) into the canonical version without the v. A
// tag that is not SemVer, or longer than a version ever is, is not one.
func tagVersion(tag string) (string, bool) {
	if len(tag) > releaseMaxTagLen {
		return "", false
	}
	v := tag
	if !strings.HasPrefix(v, "v") {
		v = "v" + v
	}
	if !semver.IsValid(v) {
		return "", false
	}
	return strings.TrimPrefix(semver.Canonical(v), "v"), true
}

// cleanReleaseURL keeps a release page's URL only when it is an https URL of a sane length: the dashboard makes a
// link of it, and the feed is input from the network.
func cleanReleaseURL(raw string) string {
	if raw == "" || len(raw) > releaseMaxURLLen {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil {
		return ""
	}
	return u.String()
}

// cleanETag keeps an ETag that can go back in If-None-Match: visible ASCII of a sane length. Without one the next
// check is an unconditional GET.
func cleanETag(etag string) string {
	if len(etag) > releaseMaxETagLen {
		return ""
	}
	for i := range len(etag) {
		if c := etag[i]; c < 0x20 || c > 0x7e {
			return ""
		}
	}
	return etag
}

// orderReleases sorts by version, newest first, merges entries of the same version (a security marker on either
// counts) and cuts the list to releasePerPage.
func orderReleases(releases []knownRelease) []knownRelease {
	slices.SortStableFunc(releases, func(a, b knownRelease) int {
		return semver.Compare("v"+b.Version, "v"+a.Version)
	})
	out := releases[:0]
	for _, rel := range releases {
		if n := len(out); n > 0 && out[n-1].Version == rel.Version {
			out[n-1].Security = out[n-1].Security || rel.Security
			continue
		}
		out = append(out, rel)
	}
	if len(out) > releasePerPage {
		out = out[:releasePerPage]
	}
	return out
}

// releaseView is what a list of releases means to one running version.
type releaseView struct {
	latest   knownRelease // the newest release that counts for this version
	found    bool         // there is one
	newer    bool         // and it is newer than the running version
	security bool         // a release newer than the running version is a security release
}

// viewReleases compares the running version with a list from parseReleaseFeed (04 §11.5). Prereleases count only
// when the running version is one itself (a release candidate, a dev build): somebody on a stable release is not
// told about a release candidate. A running version that is not SemVer can't be compared, so nothing is newer.
func viewReleases(running string, releases []knownRelease) releaseView {
	run := "v" + running
	comparable := semver.IsValid(run)
	withPrereleases := comparable && semver.Prerelease(run) != ""

	var view releaseView
	for _, rel := range releases { // newest first
		if rel.Prerelease && !withPrereleases {
			continue
		}
		if !view.found {
			view.latest, view.found = rel, true
		}
		if !comparable || semver.Compare("v"+rel.Version, run) <= 0 {
			break // this one and all after it are not newer
		}
		view.newer = true
		view.security = view.security || rel.Security
	}
	return view
}

// view returns what the last check means to the running version; ok is false while there is nothing to show: the
// check is switched off, there was no check yet, or the feed had no release that counts.
func (r *ReleaseCheck) view() (view releaseView, checkedAt time.Time, ok bool) {
	if !r.policy.ReleaseCheck() {
		return releaseView{}, time.Time{}, false
	}
	r.mu.Lock()
	state := r.state
	r.mu.Unlock()
	if !state.checked() {
		return releaseView{}, time.Time{}, false
	}
	view = viewReleases(r.version, state.Releases)
	return view, time.Time(state.CheckedAt), view.found
}

// Update returns the result of the last check for the dashboard, `isshoni admin status` and doctor
// (api.UpdateInfo): the latest release, which may be the running one or older, and whether a newer release is a
// security release. It is nil before the first check, when the feed listed no release, and while the check is
// switched off.
func (r *ReleaseCheck) Update() *api.UpdateInfo {
	view, checkedAt, ok := r.view()
	if !ok {
		return nil
	}
	return &api.UpdateInfo{
		Latest:    view.latest.Version,
		URL:       view.latest.URL,
		Security:  view.security,
		CheckedAt: checkedAt,
	}
}

// Alerts returns the dashboard alert of the last check (04 §11.4): release.security_update when a release newer
// than the running version is a security release, release.update when one is merely newer, none otherwise.
// Params carries the latest version.
func (r *ReleaseCheck) Alerts() []api.Alert {
	view, _, ok := r.view()
	if !ok || !view.newer {
		return nil
	}
	alert := api.Alert{
		Code:     api.AlertCodeReleaseUpdate,
		Severity: api.AlertSeverityInfo,
		Params:   map[string]any{"version": view.latest.Version},
	}
	if view.security {
		alert.Code, alert.Severity = api.AlertCodeReleaseSecurityUpdate, api.AlertSeverityWarn
	}
	return []api.Alert{alert}
}

// announce writes one log line per newer version, so that an operator who reads the journal and never opens the
// dashboard learns of it too.
func (r *ReleaseCheck) announce(ctx context.Context) {
	view, _, ok := r.view()
	if !ok || !view.newer {
		return
	}
	r.mu.Lock()
	seen := r.announced == view.latest.Version
	r.announced = view.latest.Version
	r.mu.Unlock()
	if seen {
		return
	}
	level, msg := slog.LevelInfo, "a newer isshoni release is available"
	if view.security {
		level, msg = slog.LevelWarn, "a newer isshoni release with a security fix is available"
	}
	r.log.LogAttrs(ctx, level, msg, slog.String("latest", view.latest.Version),
		slog.String("running", r.version), slog.String("url", view.latest.URL))
}

// load reads the last result from the meta table. A value that is missing, unreadable or from another version of
// this code is no result: the first check fetches the list again.
func (r *ReleaseCheck) load(ctx context.Context) {
	if r.meta == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, releaseStoreTimeout)
	defer cancel()
	value, err := r.meta.Meta(ctx, MetaReleaseCheck)
	if err != nil {
		r.log.LogAttrs(ctx, slog.LevelDebug, "release check: could not read the last result", logx.Err(err))
		return
	}
	state, ok := decodeReleaseState(value)
	if !ok {
		return
	}
	r.mu.Lock()
	if !r.state.checked() { // a check that already ran wins
		r.state = state
	}
	r.mu.Unlock()
}

// save writes the result to the meta table. A failure costs only the result after the next restart.
func (r *ReleaseCheck) save(ctx context.Context, state releaseState) {
	if r.meta == nil {
		return
	}
	value, err := json.Marshal(state)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, releaseStoreTimeout)
		defer cancel()
		err = r.meta.SetMeta(ctx, MetaReleaseCheck, string(value))
	}
	if err != nil {
		r.log.LogAttrs(ctx, slog.LevelDebug, "release check: could not store the result", logx.Err(err))
	}
}

// decodeReleaseState reads MetaReleaseCheck. The value comes from the database, where a restored backup or an
// older build may have put it, so every part is checked again as if it came from the feed.
func decodeReleaseState(value string) (releaseState, bool) {
	var state releaseState
	if value == "" || json.Unmarshal([]byte(value), &state) != nil {
		return releaseState{}, false
	}
	if state.V != releaseStateVersion || !state.checked() {
		return releaseState{}, false
	}
	releases := make([]knownRelease, 0, min(len(state.Releases), releasePerPage))
	for _, rel := range state.Releases {
		v, ok := tagVersion(rel.Version)
		if !ok {
			continue
		}
		releases = append(releases, knownRelease{
			Version:    v,
			URL:        cleanReleaseURL(rel.URL),
			Prerelease: rel.Prerelease || semver.Prerelease("v"+v) != "",
			Security:   rel.Security,
		})
	}
	state.Releases = orderReleases(releases)
	state.ETag = cleanETag(state.ETag)
	return state, true
}
