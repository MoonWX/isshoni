package main

import "context"

// runDLLCheck is `desktoppkg dll-check` (06 §5.1, §16): around `dumpbin` from the MSVC environment, the exports of
// isshoni-media.dll equal the IM_API functions of native/include/isshoni_media.h; every import is listed in the
// matching section (static: or delay:) of deploy/windows/dll-imports.txt, and an avcodec-*, avutil-* or C runtime
// import always fails; VERSIONINFO carries the release's version fields; the FFmpeg DLLs import neither mfplat.dll,
// mf.dll, mfreadwrite.dll nor evr.dll, are named after lock.json's majors and have its SHA-256s. Slice WA11 fills
// this file.
func runDLLCheck(_ context.Context, _ *env, _ []string) error {
	return notImplemented("WA11")
}
