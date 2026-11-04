package native

import (
	"compress/bzip2"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/MoonWX/isshoni/internal/media"
)

// This file is README WA51's (04 E8, §9.9, §12.5): Cisco's OpenH264, downloaded at run time when the user turns it on,
// never shipped. The code is pure Go: the DLL exists only on Windows, but the helpers and their tests run everywhere.
//
// The pin here and the engine's (native/windows/src/encode/openh264_pin.h, whose IM_OPENH264_VERSION the headers in
// native/windows/third_party/openh264/ match) are one release: TestOpenH264PinMatchesEngine reads both files and
// fails when they disagree.

// OpenH264Release pins one Cisco OpenH264 release (04 §9.9).
type OpenH264Release struct {
	Version, URL string
	SHA256       string // hex SHA-256 of the decompressed DLL
	Size         int64  // byte size of the decompressed DLL: the decompression limit (04 §9.9)
	FileName     string
}

// OpenH264 is the pinned release: the newest Cisco release when README WA51 was built. Cisco's binary host serves it
// over HTTPS too, so the URL is HTTPS (04 §9.9 asks for it when the host offers it); the size and hash are pinned
// either way, because they, not the transport, are what make the download trustworthy.
var OpenH264 = OpenH264Release{
	Version:  "2.7.0",
	URL:      "https://ciscobinary.openh264.org/openh264-2.7.0-win64.dll.bz2",
	SHA256:   "0ef5e7f3b38753b15c13afa0418d4d7805fa228e6adfd3e425bbf758cff1c288",
	Size:     975352,
	FileName: "openh264-2.7.0-win64.dll",
}

const (
	// maxOpenH264Compressed is the compressed body limit (04 §9.9 step 1): Cisco's file is about 450 KB.
	maxOpenH264Compressed = 10 << 20
	// openH264Timeout bounds the whole download (04 §9.9 step 1).
	openH264Timeout = 60 * time.Second
)

// The Reasons of the OpenH264 helpers' errors (Go only, like self_window; the engine's own failures keep 04 §5.7's
// tokens: missing, version, blocked_by_policy).
const (
	// reasonDownload: the download failed (network, an HTTP status other than 200, a body cut off before its
	// Content-Length); trying again may help.
	reasonDownload = "download"
	// reasonCorrupt: the bytes are not the pinned release: the compressed body is over its limit, the bzip2 stream is
	// damaged, the file is shorter or longer than the pinned size, or its SHA-256 differs.
	reasonCorrupt = "corrupt"
)

// The temporary files InstallOpenH264 writes in dir before the rename (RemoveOpenH264 removes leftovers too).
const openH264TempPattern = ".openh264-*.tmp"

var errCompressedTooLarge = errors.New("the compressed body is over 10 MiB")

// InstallOpenH264 downloads, bounds, hashes while streaming and renames into dir (04 §9.9 steps 1–4); on any
// failure nothing is left in dir. dir is %LOCALAPPDATA%\isshoni\codecs (03 passes it); it is created when missing.
// c nil means a client of its own. The whole download has 60 s. The result is the installed file's absolute path,
// which SetOpenH264Path takes. Errors: *media.Error with Op "install_openh264": dependency/download (network or HTTP
// status, a body that broke off), dependency/corrupt (limits, bzip2, size or hash), cancelled or timeout (ctx),
// invalid (dir), internal (the file system).
func InstallOpenH264(ctx context.Context, c *http.Client, dir string) (path string, err error) {
	return installOpenH264(ctx, c, dir, OpenH264, nil)
}

// installOpenH264 is InstallOpenH264 with the pin and, for the tests, a wrapper of the temp file's writer (the
// counting writer that proves at most Size + 1 bytes are written).
func installOpenH264(ctx context.Context, c *http.Client, dir string, pin OpenH264Release,
	wrap func(io.Writer) io.Writer) (path string, err error) {
	const op = "install_openh264"
	if dir == "" {
		return "", codeError(media.CodeInvalid, op, "", "no folder")
	}
	if dir, err = filepath.Abs(dir); err != nil {
		return "", codeError(media.CodeInvalid, op, "", "the folder: %v", err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", codeError(media.CodeInternal, op, "", "creating the folder: %v", err)
	}
	if c == nil {
		c = &http.Client{}
	}
	ctx, cancel := context.WithTimeout(ctx, openH264Timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pin.URL, nil)
	if err != nil {
		return "", codeError(media.CodeInternal, op, "", "the request: %v", err)
	}
	resp, err := c.Do(req)
	if err != nil {
		return "", downloadError(ctx, op, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", codeError(media.CodeDependency, op, reasonDownload, "HTTP status %d", resp.StatusCode)
	}

	tmp, err := os.CreateTemp(dir, openH264TempPattern)
	if err != nil {
		return "", codeError(media.CodeInternal, op, "", "creating the temporary file: %v", err)
	}
	installed := false
	defer func() {
		if !installed {
			_ = tmp.Close()
			_ = os.Remove(tmp.Name()) // any failure leaves nothing behind (04 §9.9 step 4)
		}
	}()

	// Steps 1–3: the compressed body through its limit, bzip2 as a stream (one block of at most 900 KB in memory),
	// the decompressed bytes through Size + 1, hashed while they are written.
	body := &cappedReader{r: resp.Body, max: maxOpenH264Compressed}
	h := sha256.New()
	var w io.Writer = tmp
	if wrap != nil {
		w = wrap(tmp)
	}
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(bzip2.NewReader(body), pin.Size+1))
	if err != nil {
		var se bzip2.StructuralError
		switch {
		case ctx.Err() != nil:
			return "", downloadError(ctx, op, err)
		case errors.Is(err, errCompressedTooLarge):
			return "", codeError(media.CodeDependency, op, reasonCorrupt, "%v", err)
		case body.err != nil, resp.ContentLength > 0 && body.n < resp.ContentLength:
			// The transfer broke off (net/http says io.ErrUnexpectedEOF for a body shorter than its
			// Content-Length, which bzip2 passes on): the network's fault, so trying again may help.
			return "", downloadError(ctx, op, fmt.Errorf("the body broke off after %d bytes: %w", body.n, err))
		case errors.As(err, &se), errors.Is(err, io.ErrUnexpectedEOF):
			// A damaged stream, or one that really ends early (the body arrived whole).
			return "", codeError(media.CodeDependency, op, reasonCorrupt, "the bzip2 stream: %v", err)
		default:
			var pe *fs.PathError
			if errors.As(err, &pe) {
				return "", codeError(media.CodeInternal, op, "", "writing the temporary file: %v", err)
			}
			return "", downloadError(ctx, op, err)
		}
	}
	// Step 4: exactly Size bytes with the pinned hash.
	switch {
	case n > pin.Size:
		return "", codeError(media.CodeDependency, op, reasonCorrupt, "more than the pinned %d bytes", pin.Size)
	case n < pin.Size:
		return "", codeError(media.CodeDependency, op, reasonCorrupt, "%d bytes, want %d", n, pin.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, pin.SHA256) {
		return "", codeError(media.CodeDependency, op, reasonCorrupt, "SHA-256 %s, want %s", got, pin.SHA256)
	}
	if err := tmp.Sync(); err != nil {
		return "", codeError(media.CodeInternal, op, "", "writing the temporary file: %v", err)
	}
	if err := tmp.Close(); err != nil {
		return "", codeError(media.CodeInternal, op, "", "writing the temporary file: %v", err)
	}
	path = filepath.Join(dir, pin.FileName)
	if err := os.Rename(tmp.Name(), path); err != nil {
		// On Windows a file the engine has loaded can't be replaced; the temp file goes in the deferred cleanup.
		return "", codeError(media.CodeInternal, op, "", "renaming into place: %v", err)
	}
	installed = true
	return path, nil
}

// downloadError is the error of a failed download: cancelled or timeout when ctx ended (the 60 s included), else
// dependency/download.
func downloadError(ctx context.Context, op string, err error) error {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return codeError(media.CodeTimeout, op, "", "the download took over %s: %v", openH264Timeout, err)
	case ctx.Err() != nil:
		return codeError(media.CodeCancelled, op, "", "the download was cancelled: %v", err)
	}
	return codeError(media.CodeDependency, op, reasonDownload, "%v", err)
}

// cappedReader reads r and fails with errCompressedTooLarge once more than max bytes came through (it reads at most
// max + 1). n counts the bytes that came through; err is r's own first error other than io.EOF (a broken transfer).
type cappedReader struct {
	r   io.Reader
	max int64
	n   int64
	err error
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n > c.max {
		return 0, errCompressedTooLarge
	}
	if room := c.max + 1 - c.n; int64(len(p)) > room {
		p = p[:room]
	}
	n, err := c.r.Read(p)
	c.n += int64(n)
	if err != nil && !errors.Is(err, io.EOF) && c.err == nil {
		c.err = err
	}
	if c.n > c.max {
		return 0, errCompressedTooLarge
	}
	return n, err
}

// VerifyOpenH264 checks an installed file against the pin: its size (os.Stat, without reading it), then its SHA-256,
// streamed. Errors: *media.Error with Op "verify_openh264": dependency/missing (no such file, or not a regular file),
// dependency/corrupt (size or hash), internal (a read error).
func VerifyOpenH264(path string) error {
	return verifyOpenH264(path, OpenH264, func(p string) (io.ReadCloser, error) { return os.Open(p) })
}

// verifyOpenH264 is VerifyOpenH264 with the pin and the file opener (tests count whether the file was read).
func verifyOpenH264(path string, pin OpenH264Release, open func(string) (io.ReadCloser, error)) error {
	const op = "verify_openh264"
	fi, err := os.Stat(path)
	switch {
	case err != nil:
		return codeError(media.CodeDependency, op, tokenMissing, "the OpenH264 file: %v", err)
	case !fi.Mode().IsRegular():
		return codeError(media.CodeDependency, op, tokenMissing, "the OpenH264 path is not a regular file")
	case fi.Size() != pin.Size:
		return codeError(media.CodeDependency, op, reasonCorrupt, "%d bytes, want %d", fi.Size(), pin.Size)
	}
	f, err := open(path)
	if err != nil {
		return codeError(media.CodeDependency, op, tokenMissing, "the OpenH264 file: %v", err)
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	// The size again while hashing: the file may have changed since the Stat.
	n, err := io.Copy(h, io.LimitReader(f, pin.Size+1))
	if err != nil {
		return codeError(media.CodeInternal, op, "", "reading the OpenH264 file: %v", err)
	}
	if n != pin.Size {
		return codeError(media.CodeDependency, op, reasonCorrupt, "%d bytes read, want %d", n, pin.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); !strings.EqualFold(got, pin.SHA256) {
		return codeError(media.CodeDependency, op, reasonCorrupt, "SHA-256 %s, want %s", got, pin.SHA256)
	}
	return nil
}

// RemoveOpenH264 removes OpenH264 from dir: the pinned file, any other release's openh264-*-win64.dll and leftover
// temporary files of an interrupted install. A missing dir or file is not an error. On Windows a DLL the engine
// still has loaded can't be deleted: turn OpenH264 off first (SetOpenH264Path("")); a share that still encodes with
// it holds it until the share ends. Errors: *media.Error with Op "remove_openh264", internal.
func RemoveOpenH264(dir string) error {
	const op = "remove_openh264"
	if dir == "" {
		return codeError(media.CodeInvalid, op, "", "no folder")
	}
	var names []string
	for _, pattern := range []string{OpenH264.FileName, "openh264-*-win64.dll", openH264TempPattern} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return codeError(media.CodeInternal, op, "", "listing the folder: %v", err)
		}
		names = append(names, m...)
	}
	var errs []error
	for _, name := range names {
		if err := os.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		return codeError(media.CodeInternal, op, "", "removing: %v", errors.Join(errs...))
	}
	return nil
}

// SetOpenH264Path verifies the file (VerifyOpenH264) and hands it to the engine (im_set_openh264_path), which checks
// the version and uses it from the next probe and im_start; "" disables OpenH264 (the engine unloads the DLL once no
// encoder uses it, so RemoveOpenH264 can follow). Errors: VerifyOpenH264's (Op "set_openh264_path"), and the
// engine's: dependency with missing, version or blocked_by_policy (04 §5.3).
func (e *Engine) SetOpenH264Path(path string) error {
	const op = "set_openh264_path"
	var arg *byte
	if path != "" {
		abs, err := filepath.Abs(path)
		if err != nil {
			return codeError(media.CodeInvalid, op, "", "the path: %v", err)
		}
		if err := VerifyOpenH264(abs); err != nil {
			var me *media.Error
			if errors.As(err, &me) {
				c := *me
				c.Op = op
				return &c
			}
			return err
		}
		if arg, err = cString(op, abs); err != nil {
			return err
		}
	}
	if err := e.begin(op); err != nil {
		return err
	}
	defer e.end()
	var pn runtime.Pinner
	defer pn.Unpin()
	r, lastErr := e.c.call(procSetOpenH264Path, addrOf(&pn, arg))
	if st := int32(r); st != imOK {
		return e.failed(statusError(op, procSetOpenH264Path.String(), st, lastErr))
	}
	return nil
}

// String describes the pin for logs and tool output.
func (r OpenH264Release) String() string {
	return fmt.Sprintf("OpenH264 %s (%s, %d bytes, sha256 %s)", r.Version, r.FileName, r.Size, r.SHA256)
}
