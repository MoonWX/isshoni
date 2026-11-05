package native

import (
	"testing"

	"github.com/MoonWX/isshoni/internal/media"
)

// TestProtectWindow (README WA70, 04 §11): ProtectWindow is im_protect_window(hwnd, 0); a zero handle is invalid
// without a DLL call; the engine's failures map by 04 §5.7 with Op "protect_window"; after Close nothing reaches the
// DLL.
func TestProtectWindow(t *testing.T) {
	f := newFakeCaller(t)
	const own, foreign, old = uintptr(0x1234), uintptr(0x10010), uintptr(0x2468)
	f.set(procProtectWindow, func(args []uintptr) (uintptr, string) {
		switch args[0] {
		case own:
			if args[1] != 0 {
				return fakeResult(imErrInvalidArg), "im_protect_window: unknown flags"
			}
			return fakeResult(imOK), ""
		case foreign:
			return fakeResult(imErrInvalidArg), "im_protect_window: the window belongs to another process"
		case old:
			return fakeResult(imErrUnsupportedOS), "os_too_old: im_protect_window needs build 19041"
		}
		return fakeResult(imErrInternal), "im_protect_window: unexpected window"
	})
	e := openFake(t, f, Options{})

	if err := e.ProtectWindow(own); err != nil {
		t.Fatal(err)
	}
	calls := f.callsOf(procProtectWindow)
	if len(calls) != 1 || len(calls[0]) != 2 || calls[0][0] != own || calls[0][1] != 0 {
		t.Fatalf("calls %v, want one im_protect_window(hwnd, 0)", calls)
	}

	wantError(t, e.ProtectWindow(0), media.CodeInvalid, "", "protect_window")
	if n := f.count(procProtectWindow); n != 1 {
		t.Errorf("a zero handle reached the DLL (%d calls)", n)
	}
	detail := wantError(t, e.ProtectWindow(foreign), media.CodeInvalid, "", "protect_window")
	if detail != "im_protect_window: im_protect_window: the window belongs to another process" {
		t.Errorf("detail %q", detail)
	}
	wantError(t, e.ProtectWindow(old), media.CodeUnsupportedOS, tokenOSTooOld, "protect_window")

	if err := e.Close(); err != nil {
		t.Fatal(err)
	}
	calls0 := f.count(procProtectWindow)
	wantError(t, e.ProtectWindow(own), media.CodeInternal, tokenNotInitialized, "protect_window")
	if f.count(procProtectWindow) != calls0 {
		t.Error("a call after Close reached the DLL")
	}
}

// TestCapturePath (README WA70): a session's running capture path is im_session_info.capture_path at start, which may
// differ from the source's planned one; any other media.Session has none.
func TestCapturePath(t *testing.T) {
	for _, tc := range []struct {
		path uint32
		want string
	}{{imCaptureWGC, "wgc"}, {imCaptureDD, "dd"}, {imCaptureTest, "test"}, {99, ""}} {
		sf := newSessionFake(t)
		sf.info.capturePath = tc.path
		_, s := startFake(t, sf, baseConfig())
		if got := CapturePath(s); got != tc.want {
			t.Errorf("capture_path %d: CapturePath = %q, want %q", tc.path, got, tc.want)
		}
	}
	if got := CapturePath(nil); got != "" {
		t.Errorf("CapturePath(nil) = %q", got)
	}
}
