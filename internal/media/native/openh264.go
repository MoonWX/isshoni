package native

import (
	"context"
	"net/http"
)

// This file is README WA51's (04 E8, §9.9, §12.5): Cisco's OpenH264, downloaded at run time when the user turns it on,
// never shipped. README WA10 declares the surface (docs/m2/README.md §4, "Interfaces first"); until WA51 the helpers
// return a not-implemented *media.Error, and the pin's SHA256 and Size are unset. The code is pure Go: the DLL exists
// only on Windows, but the helpers and their tests run everywhere.

// OpenH264Release pins one Cisco OpenH264 release (04 §9.9).
type OpenH264Release struct {
	Version, URL string
	SHA256       string // hex SHA-256 of the decompressed DLL
	Size         int64  // byte size of the decompressed DLL: the decompression limit (04 §9.9)
	FileName     string
}

// OpenH264 is the pinned release. It and the engine's compiled IM_OPENH264_VERSION must agree (a unit test reads
// both, README WA51). SHA256 and Size are set by README WA51.
var OpenH264 = OpenH264Release{
	Version:  "2.6.0",
	URL:      "http://ciscobinary.openh264.org/openh264-2.6.0-win64.dll.bz2",
	FileName: "openh264-2.6.0-win64.dll",
}

// InstallOpenH264 downloads, bounds, hashes while streaming and renames into dir (04 §9.9 steps 1–4); on any
// failure nothing is left in dir. dir is %LOCALAPPDATA%\isshoni\codecs (03 passes it). Not implemented yet
// (README WA51).
func InstallOpenH264(ctx context.Context, c *http.Client, dir string) (path string, err error) {
	return "", notImplemented("install_openh264")
}

// VerifyOpenH264 checks an installed file against the pin: its size, then its SHA-256. Not implemented yet
// (README WA51).
func VerifyOpenH264(path string) error {
	return notImplemented("verify_openh264")
}

// RemoveOpenH264 removes the installed DLL from dir. Not implemented yet (README WA51).
func RemoveOpenH264(dir string) error {
	return notImplemented("remove_openh264")
}

// SetOpenH264Path verifies the file (VerifyOpenH264) and hands it to the engine (im_set_openh264_path), which uses
// it from the next probe and im_start; "" disables OpenH264. Not implemented yet (README WA51).
func (e *Engine) SetOpenH264Path(path string) error {
	return notImplemented("set_openh264_path")
}
