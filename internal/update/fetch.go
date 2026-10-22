package update

import (
	"compress/gzip"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/logx"
	"github.com/MoonWX/isshoni/internal/version"
)

// The HTTP side of the updater (06 §8.1, §8.4, §8.6): where it looks, the client and its redirect rule, the size
// and time limits, the beta listing and the installer download.

const (
	// apiReleasesURL is the GitHub API's release listing of the repository (06 §8.4, channel beta).
	apiReleasesURL = "https://api.github.com/repos/MoonWX/isshoni/releases"

	fetchTimeout    = 20 * time.Second // the manifest and the signature, each
	maxListingSize  = 1 << 20          // the beta listing
	listingPageSize = 10
	maxRedirects    = 5
	stallTimeout    = 60 * time.Second // the installer: no byte for 60 s
	downloadTimeout = 60 * time.Minute // the installer: in all
	diskReserve     = 64 << 20         // free space beyond the installer's size
	progressStep    = 0.01             // publish the download's progress in steps of 1 %
)

var (
	// errTurnedOff ends a check whose Enabled() turned false before a request (06 §8.8).
	errTurnedOff = errors.New("update: turned off")

	errStalled      = errors.New("no byte for 60 s")
	errTotalTimeout = errors.New("not finished within 60 min")
)

// endpoints are the URLs the updater asks, and the rule its redirects must follow.
type endpoints struct {
	releases string   // ReleasesURL: <releases>/latest/download/<asset> and <releases>/download/v<v>/<asset>
	api      string   // apiReleasesURL; the listing is <api>?per_page=10
	testBase *url.URL // ISSHONI_UPDATE_TEST_BASE in updatetest builds: redirects stay under it; nil otherwise
}

func defaultEndpoints() endpoints { return endpoints{releases: ReleasesURL, api: apiReleasesURL} }

// testBasePathRE is the path of a test base: "" or /-separated segments of unreserved characters, none starting with
// a dot, and no trailing slash (tools/desktoppkg manifest -releases checks the same).
var testBasePathRE = regexp.MustCompile(`^(/[A-Za-z0-9_~-][A-Za-z0-9._~-]*)*$`)

// testEndpoints returns the endpoints under a test base, the value of ISSHONI_UPDATE_TEST_BASE (06 §8.11): it
// replaces the releases URL, so the stable channel asks <base>/latest/download/<asset> and every manifest URL must be
// under <base>, and the beta API, which becomes <base>/api/releases. base is an https URL or an
// http://127.0.0.1:<port> origin, with an optional path and no trailing slash, query, fragment or user. Redirects
// are then followed only under base. Only the updatetest hooks call it.
func testEndpoints(base string) (endpoints, error) {
	u, err := url.Parse(base)
	if err != nil {
		return endpoints{}, err
	}
	switch {
	case u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" ||
		strings.ContainsAny(base, "?#%") || !testBasePathRE.MatchString(u.Path) || u.String() != base:
		return endpoints{}, fmt.Errorf("%q: want a scheme, a host and an optional path, without a trailing slash, "+
			"query, fragment or user", base)
	case u.Scheme == "https" && u.Hostname() != "" && (u.Port() == "" || validPort(u.Port())):
	case u.Scheme == "http" && u.Hostname() == "127.0.0.1" && validPort(u.Port()):
	default:
		return endpoints{}, fmt.Errorf("%q: want https://…, or http://127.0.0.1:<port>", base)
	}
	return endpoints{releases: base, api: base + "/api/releases", testBase: u}, nil
}

// validPort reports whether p is a TCP port number, 1 to 65535, without leading zeros.
func validPort(p string) bool {
	n, err := strconv.ParseUint(p, 10, 16)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == p
}

// readSignersFile reads an allowed-signers file for ISSHONI_UPDATE_TEST_SIGNERS (06 §8.11). Only the updatetest
// hooks call it.
func readSignersFile(path string) ([]TrustedKey, error) {
	f, err := os.Open(path) //nolint:gosec // G304: the test build's signers file, named by its environment
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, MaxManifestSize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxManifestSize {
		return nil, fmt.Errorf("%s: above %d bytes", filepath.Base(path), MaxManifestSize)
	}
	return ParseSigners(data)
}

// allowRedirect is the redirect rule of 06 §8.1: https only, to github.com or a host under githubusercontent.com,
// on the default port, without a user; with a test base, only URLs under it.
func (ep endpoints) allowRedirect(u *url.URL) error {
	if u.User != nil {
		return fmt.Errorf("redirect to a URL with a user refused")
	}
	if b := ep.testBase; b != nil {
		p := strings.TrimSuffix(b.Path, "/")
		if u.Scheme == b.Scheme && strings.EqualFold(u.Host, b.Host) &&
			(u.Path == p || strings.HasPrefix(u.Path, p+"/")) {
			return nil
		}
		return fmt.Errorf("redirect to %s refused: not under the test base", origin(u))
	}
	host := strings.ToLower(u.Hostname())
	switch {
	case u.Scheme != "https":
		return fmt.Errorf("redirect to %s refused: not https", origin(u))
	case u.Port() != "" && u.Port() != "443":
		return fmt.Errorf("redirect to %s refused: another port", origin(u))
	case host == "github.com":
	case strings.HasSuffix(host, ".githubusercontent.com") && len(host) > len(".githubusercontent.com"):
	default:
		return fmt.Errorf("redirect to %s refused: neither github.com nor *.githubusercontent.com", origin(u))
	}
	return nil
}

// origin returns u's scheme and host, for messages.
func origin(u *url.URL) string { return u.Scheme + "://" + u.Host }

// redact returns u without its query, fragment and user: GitHub's asset URLs carry signed query strings, which are
// never logged (06 §8.9).
func redact(u *url.URL) string {
	if u == nil {
		return ""
	}
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

// newClient returns the updater's HTTP client: base's transport (or a clone of http.DefaultTransport with TLS 1.2
// or later and the system roots, 06 §8.6), no cookie jar, and the redirect rule of 06 §8.1, which also asks enabled
// before each redirect and drops the Referer that net/http would add.
func newClient(base *http.Client, ep endpoints, enabled func() bool) *http.Client {
	c := &http.Client{}
	if base != nil {
		c.Transport = base.Transport
	}
	if c.Transport == nil {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
		c.Transport = tr
	}
	c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) > maxRedirects {
			return fmt.Errorf("more than %d redirects", maxRedirects)
		}
		if !enabled() {
			return errTurnedOff
		}
		req.Header.Del("Referer")
		return ep.allowRedirect(req.URL)
	}
	return c
}

// do sends a GET with the updater's headers (06 §8.4: User-Agent isshoni-desktop/<version>, no cookies, no
// Referer) after asking Enabled. A transport failure is CodeNetwork, with the URL redacted.
func (u *Updater) do(ctx context.Context, rawURL string, hdr http.Header) (*http.Response, error) {
	if !u.enabled() {
		return nil, errTurnedOff
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, &Error{Code: CodeNetwork, Err: err}
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", "isshoni-desktop/"+u.o.CurrentVersion)
	resp, err := u.client.Do(req)
	if err != nil {
		if ue, ok := errors.AsType[*url.Error](err); ok {
			err = ue.Err // its URL may be a signed redirect target
		}
		if errors.Is(err, errTurnedOff) {
			return nil, errTurnedOff
		}
		return nil, &Error{Code: CodeNetwork, Err: fmt.Errorf("GET %s: %w", redact(req.URL), err)}
	}
	return resp, nil
}

// statusError is the error of a response other than 200: 429 is CodeRateLimited, and so is 403 from the API
// (06 §8.4); the rest is CodeNetwork.
func statusError(resp *http.Response, api bool) error {
	code := CodeNetwork
	if resp.StatusCode == http.StatusTooManyRequests || api && resp.StatusCode == http.StatusForbidden {
		code = CodeRateLimited
	}
	return &Error{Code: code, Err: fmt.Errorf("GET %s: HTTP %d", redact(resp.Request.URL), resp.StatusCode)}
}

// decodedBody returns resp's body decoded: identity or gzip; any other Content-Encoding is refused (06 §8.6). The
// Transport decodes a gzip body it asked for itself and drops the header.
func decodedBody(resp *http.Response) (io.Reader, error) {
	switch ce := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); ce {
	case "", "identity":
		return resp.Body, nil
	case "gzip":
		zr, err := gzip.NewReader(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("gzip: %w", err)
		}
		return zr, nil
	default:
		return nil, fmt.Errorf("Content-Encoding %q refused", truncate(ce))
	}
}

// get fetches a small document: within 20 s, at most limit bytes after decoding.
func (u *Updater) get(ctx context.Context, rawURL string, limit int64, hdr http.Header, api bool) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	resp, err := u.do(ctx, rawURL, hdr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, statusError(resp, api)
	}
	r, err := decodedBody(resp)
	if err != nil {
		return nil, &Error{Code: CodeNetwork, Err: fmt.Errorf("GET %s: %w", redact(resp.Request.URL), err)}
	}
	data, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, &Error{Code: CodeNetwork, Err: fmt.Errorf("GET %s: %w", redact(resp.Request.URL), err)}
	}
	if int64(len(data)) > limit {
		return nil, &Error{Code: CodeNetwork,
			Err: fmt.Errorf("GET %s: more than %d bytes", redact(resp.Request.URL), limit)}
	}
	return data, nil
}

// fetchManifest fetches the manifest and its signature for the channel (06 §8.4).
func (u *Updater) fetchManifest(ctx context.Context, ch Channel) (data, sig []byte, err error) {
	mURL := u.env.ep.releases + "/latest/download/" + ManifestName
	sURL := u.env.ep.releases + "/latest/download/" + SignatureName
	if ch == ChannelBeta {
		if mURL, sURL, err = u.betaRelease(ctx); err != nil {
			return nil, nil, err
		}
	}
	if data, err = u.get(ctx, mURL, MaxManifestSize, nil, false); err != nil {
		return nil, nil, err
	}
	if sig, err = u.get(ctx, sURL, MaxSignatureSize, nil, false); err != nil {
		return nil, nil, err
	}
	return data, sig, nil
}

// ghRelease is the part of a GitHub API release that the beta channel reads.
type ghRelease struct {
	TagName string `json:"tag_name"`
	Draft   bool   `json:"draft"`
	Assets  []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// betaRelease asks the API's release listing (06 §8.4) and returns the manifest and signature URLs of its newest
// release.
func (u *Updater) betaRelease(ctx context.Context) (manifestURL, sigURL string, err error) {
	hdr := http.Header{
		"Accept":               {"application/vnd.github+json"},
		"X-Github-Api-Version": {"2022-11-28"},
	}
	data, err := u.get(ctx, u.env.ep.api+"?per_page="+strconv.Itoa(listingPageSize), maxListingSize, hdr, true)
	if err != nil {
		return "", "", err
	}
	return pickRelease(data, u.env.ep.releases, u.env.testBuild)
}

// pickRelease picks from a release listing the newest release a beta app may take: not a draft, a tag v<SemVer>
// whose version is Eligible (stable or rc.N; ci.N too in an updatetest build), and both the manifest and its
// signature as assets whose browser_download_url is exactly <releases>/download/v<version>/<asset>. Other tags
// (deps-ffmpeg-…) are skipped. "Newest" is the highest version (internal/version).
func pickRelease(data []byte, releases string, testBuild bool) (manifestURL, sigURL string, err error) {
	var list []ghRelease
	if err := json.Unmarshal(data, &list); err != nil {
		return "", "", &Error{Code: CodeNetwork, Err: fmt.Errorf("the release listing: %w", err)}
	}
	var best string
	for _, r := range list {
		v, ok := strings.CutPrefix(r.TagName, "v")
		if r.Draft || !ok {
			continue
		}
		if _, err := canonical(v); err != nil || !eligible(v, testBuild) {
			continue
		}
		dl := releases + "/download/v" + v + "/"
		var hasManifest, hasSig bool
		for _, a := range r.Assets {
			switch {
			case a.Name == ManifestName && a.URL == dl+ManifestName:
				hasManifest = true
			case a.Name == SignatureName && a.URL == dl+SignatureName:
				hasSig = true
			}
		}
		if !hasManifest || !hasSig {
			continue
		}
		if best == "" {
			best = v
		} else if c, err := version.Compare(v, best); err == nil && c > 0 {
			best = v
		}
	}
	if best == "" {
		return "", "", &Error{Code: CodeNetwork,
			Err: fmt.Errorf("the release listing has no release with %s and its signature", ManifestName)}
	}
	dl := releases + "/download/v" + best + "/"
	return dl + ManifestName, dl + SignatureName, nil
}

// download fetches and verifies the installer a of the verified manifest v (06 §8.6, §8.7), then makes it the
// pending update: StateDownloading with progress, then StateReady, or StateError with download_failed,
// size_mismatch, hash_mismatch or disk. found holds the fields of the newer version for the status.
func (u *Updater) download(ctx context.Context, v *Verified, a Artifact, found Status) (Status, error) {
	found.State = StateDownloading
	u.set(found, nil)
	if err := os.MkdirAll(u.dir, 0o700); err != nil {
		return u.fail(found, &Error{Code: CodeDisk, Err: err})
	}
	need := uint64(a.Size) + diskReserve //nolint:gosec // a.Size is in 1…256 MiB (Artifact.check)
	switch free, err := u.env.sys.freeSpace(u.dir); {
	case errors.Is(err, errors.ErrUnsupported):
	case err != nil:
		return u.fail(found, &Error{Code: CodeDisk, Err: fmt.Errorf("free space: %w", err)})
	case free < need:
		return u.fail(found, &Error{Code: CodeDisk,
			Err: fmt.Errorf("%d bytes free in the updates folder, %d needed", free, need)})
	}
	last := 0.0
	path, err := u.fetchInstaller(ctx, a, func(n int64) {
		if p := float64(n) / float64(a.Size); p-last >= progressStep || n == a.Size {
			last = p
			st := found
			st.Progress = min(p, 1)
			u.set(st, nil)
		}
	})
	if errors.Is(err, errTurnedOff) {
		return u.turnOff(), nil
	}
	if err != nil {
		if ctx.Err() != nil {
			u.set(Status{State: StateIdle}, nil)
			return u.Status(), ctx.Err()
		}
		return u.fail(found, err)
	}
	rec := &pendingRec{Version: v.Version, File: filepath.Base(path), Size: a.Size, SHA256: a.SHA256,
		ExpiresAt: wireTime{v.ExpiresAt}, Required: found.Required, NotesURL: v.NotesURL}
	if err := u.setPending(rec); err != nil {
		u.removeFile(rec.File)
		return u.fail(found, &Error{Code: CodeDisk, Err: err})
	}
	u.log.Info("update downloaded", "version", v.Version, "required", found.Required)
	return u.becomeReady(found, rec)
}

// fetchInstaller downloads a into <name>.<random>.partial in the updates folder, checks its size and SHA-256 and
// renames it to <name>. A mismatch or a failure deletes the partial file. progress gets the byte count so far.
func (u *Updater) fetchInstaller(ctx context.Context, a Artifact, progress func(n int64)) (path string, err error) {
	ctx, cancel := context.WithTimeoutCause(ctx, downloadTimeout, errTotalTimeout)
	defer cancel()
	ctx, stall := context.WithCancelCause(ctx)
	defer stall(nil)
	stallTimer := time.AfterFunc(stallTimeout, func() { stall(errStalled) })
	defer stallTimer.Stop()

	failed := func(code string, err error) (string, error) {
		if cause := context.Cause(ctx); errors.Is(cause, errStalled) || errors.Is(cause, errTotalTimeout) {
			err = cause // rather than "context canceled"
		}
		return "", &Error{Code: code, Err: fmt.Errorf("the installer %s: %w", a.Name, err)}
	}
	resp, err := u.do(ctx, a.URL, nil)
	if errors.Is(err, errTurnedOff) {
		return "", errTurnedOff
	}
	if err != nil {
		return failed(CodeDownloadFailed, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return failed(CodeDownloadFailed, statusError(resp, false))
	}
	if resp.ContentLength >= 0 && resp.ContentLength != a.Size {
		return failed(CodeSizeMismatch, fmt.Errorf("Content-Length %d, want %d", resp.ContentLength, a.Size))
	}
	r, err := decodedBody(resp)
	if err != nil {
		return failed(CodeDownloadFailed, err)
	}
	f, err := os.CreateTemp(u.dir, a.Name+".*.partial")
	if err != nil {
		return failed(CodeDisk, err)
	}
	partial := f.Name()
	defer func() {
		if err != nil {
			_ = f.Close()
			if rerr := os.Remove(partial); rerr != nil && !errors.Is(rerr, fs.ErrNotExist) {
				u.log.Warn("can't delete a partial download", logx.Err(rerr))
			}
		}
	}()
	h := sha256.New()
	buf := make([]byte, 64<<10)
	var n int64
	for {
		m, rerr := r.Read(buf)
		if m > 0 {
			stallTimer.Reset(stallTimeout)
			if n+int64(m) > a.Size {
				return failed(CodeSizeMismatch, fmt.Errorf("more than %d bytes", a.Size))
			}
			if _, werr := f.Write(buf[:m]); werr != nil {
				return failed(CodeDisk, werr)
			}
			h.Write(buf[:m])
			n += int64(m)
			progress(n)
		}
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			return failed(CodeDownloadFailed, rerr)
		}
	}
	if n != a.Size {
		return failed(CodeSizeMismatch, fmt.Errorf("%d bytes, want %d", n, a.Size))
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != a.SHA256 {
		return failed(CodeHashMismatch, fmt.Errorf("SHA-256 %s, want %s", got, a.SHA256))
	}
	if err = f.Sync(); err != nil {
		return failed(CodeDisk, err)
	}
	if err = f.Close(); err != nil {
		return failed(CodeDisk, err)
	}
	path = filepath.Join(u.dir, a.Name)
	if err = u.env.sys.rename(partial, path); err != nil {
		return failed(CodeDisk, err)
	}
	return path, nil
}
