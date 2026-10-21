package main

import (
	"archive/zip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

const (
	// defaultReleaseBase is where the deps releases are: <base>/<tag>/<asset> (06 §4.5).
	defaultReleaseBase = "https://github.com/MoonWX/isshoni/releases/download"
	// maxAssetBytes bounds a download and maxUnpackedBytes what a zip may unpack to.
	maxAssetBytes    = 512 << 20
	maxUnpackedBytes = 1 << 30
	// downloadAttempts and retryDelay: a download is tried three times, waiting 2 s and then 4 s.
	downloadAttempts = 3
	retryDelay       = 2 * time.Second
)

// newHTTPClient returns the client of ffmpeg-fetch: a 10-minute limit per request and redirects over HTTPS only (a
// release asset redirects to GitHub's storage host).
func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 {
				return errors.New("more than 10 redirects")
			}
			if req.URL.Scheme != "https" {
				return fmt.Errorf("refusing a redirect to %s", req.URL.Redacted())
			}
			return nil
		},
	}
}

// runFFmpegFetch is `desktoppkg ffmpeg-fetch` (06 §4.5): the consumers' way to the locked FFmpeg build.
//
// It fails as stale when lock.json does not describe the current native/ffmpeg inputs, downloads
// <base>/<tag>/ffmpeg-<ver>-r<rev>-win-x64.zip into -out (default .deps/ffmpeg/; a copy already there with the
// locked SHA-256 is kept) or takes -zip, checks the zip's SHA-256 against lock.json, unpacks it, and checks the
// unpacked build: the two DLLs with lock.json's names and SHA-256s, ffmpeg-config.json passing 06 §4.4's LGPL
// checks with lock.json's majors, and the include/, lib/ and licenses/ files 04 relies on. It prints the unpacked
// folder, the IM_FFMPEG_ROOT of 04's CMake. -source also fetches and checks the source archive. Everything it
// writes stays inside -out (an os.Root).
func runFFmpegFetch(ctx context.Context, e *env, args []string) error {
	fs := flag.NewFlagSet("ffmpeg-fetch", flag.ContinueOnError)
	fs.SetOutput(e.stderr)
	out := fs.String("out", "", "download and unpack into this folder (default: <root>/.deps/ffmpeg)")
	zipFile := fs.String("zip", "", "verify and unpack this local copy of the zip instead of downloading it")
	source := fs.Bool("source", false, "also download and verify the source archive")
	base := fs.String("base", defaultReleaseBase, "release download base URL: <base>/<tag>/<asset>")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() != 0 {
		return fmt.Errorf("%w: ffmpeg-fetch takes no arguments", errUsage)
	}
	if u, err := url.Parse(*base); err != nil || u.Scheme != "https" || u.Host == "" {
		return fmt.Errorf("%w: -base must be an https URL", errUsage)
	}

	st, err := readFFmpegState(e.root)
	if err != nil {
		return err
	}
	lock, err := st.currentLock()
	if err != nil {
		return err
	}
	dir := *out
	if dir == "" {
		dir = filepath.Join(e.root, ".deps", "ffmpeg")
	}
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // G703: the folder the caller asked for
		return err
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		return err
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return err
	}
	defer root.Close()

	zipName := st.pins.ZipName()
	var zipData *os.File
	if *zipFile != "" {
		zipData, err = os.Open(*zipFile) //nolint:gosec // G703: the zip the caller named, checked against lock.json
	} else {
		if err := fetchAsset(ctx, e, root, *base, lock.Tag, zipName, lock.Assets[zipName]); err != nil {
			return err
		}
		zipData, err = root.Open(zipName)
	}
	if err != nil {
		return err
	}
	defer zipData.Close()
	if err := checkSHA256(zipData, zipName, lock.Assets[zipName]); err != nil {
		return err
	}
	if *source {
		name := st.pins.SourceName()
		if err := fetchAsset(ctx, e, root, *base, lock.Tag, name, lock.Assets[name]); err != nil {
			return err
		}
	}

	top := strings.TrimSuffix(zipName, ".zip")
	if err := unpackBuild(root, zipData, top, lock); err != nil {
		return err
	}
	_, err = fmt.Fprintln(e.stdout, filepath.Join(dir, top))
	return err
}

// fetchAsset makes the file name in root the release asset <base>/<tag>/<name> with SHA-256 want: a file already
// there with that hash stays, anything else is downloaded again (three attempts).
func fetchAsset(ctx context.Context, e *env, root *os.Root, base, tag, name, want string) error {
	if f, err := root.Open(name); err == nil {
		err = checkSHA256(f, name, want)
		_ = f.Close()
		if err == nil {
			fmt.Fprintf(e.stderr, "desktoppkg ffmpeg-fetch: %s is already there\n", name)
			return nil
		}
	}
	assetURL := strings.TrimSuffix(base, "/") + "/" + url.PathEscape(tag) + "/" + url.PathEscape(name)
	var err error
	for attempt := 1; attempt <= downloadAttempts; attempt++ {
		var retry bool
		retry, err = download(ctx, e.client, assetURL, root, name, want)
		if err == nil {
			fmt.Fprintf(e.stderr, "desktoppkg ffmpeg-fetch: downloaded %s\n", assetURL)
			return nil
		}
		if !retry || attempt == downloadAttempts {
			break
		}
		fmt.Fprintf(e.stderr, "desktoppkg ffmpeg-fetch: %v; trying again\n", err)
		if serr := e.sleep(ctx, retryDelay*time.Duration(attempt)); serr != nil {
			return serr
		}
	}
	return err
}

// download writes assetURL to name in root through name.part and checks its SHA-256. retry says whether another
// attempt can help (a network error, a 5xx or a 429).
func download(ctx context.Context, client *http.Client, assetURL string, root *os.Root, name, want string) (retry bool, err error) {
	part := name + ".part"
	defer func() {
		if err != nil {
			_ = root.Remove(part)
		}
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return false, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("GET %s: %w", assetURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		retry := resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		return retry, fmt.Errorf("GET %s: %s", assetURL, resp.Status)
	}
	f, err := root.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return false, err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxAssetBytes+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return ctx.Err() == nil, fmt.Errorf("GET %s: %w", assetURL, err)
	}
	if n > maxAssetBytes {
		return false, fmt.Errorf("GET %s: larger than %d bytes", assetURL, maxAssetBytes)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return false, fmt.Errorf("%s: SHA-256 %s, but lock.json has %s", name, got, want)
	}
	return false, root.Rename(part, name)
}

// checkSHA256 fails unless the rest of r, the file called name, has the SHA-256 want. A seekable r is rewound
// before and after.
func checkSHA256(r io.Reader, name, want string) error {
	if s, ok := r.(io.Seeker); ok {
		if _, err := s.Seek(0, io.SeekStart); err != nil {
			return err
		}
		defer func() { _, _ = s.Seek(0, io.SeekStart) }()
	}
	h := sha256.New()
	if _, err := io.Copy(h, r); err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: SHA-256 %s, but lock.json has %s", name, got, want)
	}
	return nil
}

// unpackBuild unpacks the zip into top/ in root (replacing an older copy) and checks it against the lock; nothing is
// left behind when a check fails. Every entry must be a regular file under top/.
func unpackBuild(root *os.Root, zipFile *os.File, top string, lock *Lock) error {
	fi, err := zipFile.Stat()
	if err != nil {
		return err
	}
	zr, err := zip.NewReader(zipFile, fi.Size())
	if err != nil {
		return fmt.Errorf("%s: %w", top+".zip", err)
	}

	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return err
	}
	tmp := "." + top + ".tmp-" + hex.EncodeToString(suffix[:])
	if err := root.Mkdir(tmp, 0o750); err != nil {
		return err
	}
	defer func() { _ = root.RemoveAll(tmp) }()
	if err := extractAndCheck(zr, root, tmp, top, lock); err != nil {
		return err
	}
	if err := root.RemoveAll(top); err != nil {
		return err
	}
	return root.Rename(tmp+"/"+top, top)
}

// extractAndCheck extracts the zip into tmp/ in root and checks tmp/top; it closes every handle it opens, so Windows
// lets unpackBuild move the folder afterwards.
func extractAndCheck(zr *zip.Reader, root *os.Root, tmp, top string, lock *Lock) error {
	tmpRoot, err := root.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer tmpRoot.Close()
	if err := extract(zr, tmpRoot, top); err != nil {
		return fmt.Errorf("%s: %w", top+".zip", err)
	}
	unpacked, err := tmpRoot.OpenRoot(top)
	if err != nil {
		return fmt.Errorf("%s: %w", top+".zip", err)
	}
	defer unpacked.Close()
	return checkUnpacked(unpacked, lock)
}

// extract writes the zip's files into root, which no entry can leave.
func extract(zr *zip.Reader, root *os.Root, top string) error {
	var total uint64
	for _, f := range zr.File {
		name := f.Name
		if strings.HasSuffix(name, "/") && f.FileInfo().IsDir() {
			continue
		}
		if strings.Contains(name, `\`) || path.IsAbs(name) || path.Clean(name) != name ||
			!strings.HasPrefix(name, top+"/") || !fs.ValidPath(name) {
			return fmt.Errorf("entry %q is not a file under %s/", name, top)
		}
		if !f.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", name)
		}
		total += f.UncompressedSize64
		if f.UncompressedSize64 > maxUnpackedBytes || total > maxUnpackedBytes {
			return fmt.Errorf("unpacks to more than %d bytes", maxUnpackedBytes)
		}
		if err := root.MkdirAll(path.Dir(name), 0o750); err != nil {
			return err
		}
		if err := extractFile(root, f); err != nil {
			return fmt.Errorf("entry %q: %w", name, err)
		}
	}
	return nil
}

func extractFile(root *os.Root, f *zip.File) error {
	rc, err := f.Open()
	if err != nil {
		return err
	}
	defer rc.Close()
	w, err := root.OpenFile(f.Name, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	// extract checked the declared size; the zip reader checks the size and CRC-32 when it reaches the end.
	_, err = io.Copy(w, io.LimitReader(rc, maxUnpackedBytes+1))
	if cerr := w.Close(); err == nil {
		err = cerr
	}
	return err
}

// checkUnpacked checks the unpacked build in root against the lock and 06 §4.4, §17: bin/ holds exactly the two
// locked DLLs with their SHA-256s, ffmpeg-config.json passes the LGPL checks with the locked majors, and the files 04
// compiles against and the license files are there.
func checkUnpacked(root *os.Root, lock *Lock) error {
	bin, err := root.Open("bin")
	if err != nil {
		return fmt.Errorf("the build has no bin/: %w", err)
	}
	entries, err := bin.ReadDir(-1)
	_ = bin.Close()
	if err != nil {
		return err
	}
	var dlls []string
	for _, ent := range entries {
		if strings.EqualFold(path.Ext(ent.Name()), ".dll") {
			dlls = append(dlls, ent.Name())
		}
	}
	slices.Sort(dlls)
	if len(dlls) != 2 || !slices.Contains(dlls, lock.AvcodecDLL()) || !slices.Contains(dlls, lock.AvutilDLL()) {
		return fmt.Errorf("bin/ holds %v; lock.json names %s and %s", dlls, lock.AvcodecDLL(), lock.AvutilDLL())
	}
	for _, name := range dlls {
		f, err := root.Open("bin/" + name)
		if err != nil {
			return err
		}
		err = checkSHA256(f, name, lock.DLLs[name])
		_ = f.Close()
		if err != nil {
			return err
		}
	}
	data, err := root.ReadFile("ffmpeg-config.json")
	if err != nil {
		return err
	}
	cfg, err := checkFFmpegConfig(data)
	if err != nil {
		return err
	}
	if cfg.AvcodecMajor != lock.AvcodecMajor || cfg.AvutilMajor != lock.AvutilMajor {
		return fmt.Errorf("ffmpeg-config.json has majors %d and %d; lock.json has %d and %d",
			cfg.AvcodecMajor, cfg.AvutilMajor, lock.AvcodecMajor, lock.AvutilMajor)
	}
	for _, rel := range []string{
		"include/libavcodec/avcodec.h", "include/libavutil/avutil.h", "include/libavutil/hwcontext_d3d11va.h",
		"lib/avcodec.lib", "lib/avutil.lib",
		"licenses/ffmpeg/COPYING.LGPLv2.1", "licenses/ffmpeg/LICENSE.md", "licenses/ffmpeg/README-LGPL.txt",
		"licenses/libvpl/LICENSE", "licenses/nv-codec-headers/LICENSE", "licenses/amf/LICENSE.txt",
		"build-manifest.json",
	} {
		if fi, err := root.Stat(rel); err != nil || !fi.Mode().IsRegular() {
			return fmt.Errorf("the build has no %s", rel)
		}
	}
	return nil
}
