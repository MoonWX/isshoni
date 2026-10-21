//go:build windows

package native

import (
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"golang.org/x/sys/windows"

	"github.com/MoonWX/isshoni/internal/media"
)

// The tests against a real isshoni-media.dll (04 §18.2, §18.6): ISSHONI_TEST_ENGINE_DLL names the DLL or its folder
// (set by CI's windows-native job and by enginetest only); without it they skip. They need no GPU and no audio device:
// the engine is opened with IM_INIT_NO_AUDIO.

// engineDLLDir is the folder of the DLL under test.
func engineDLLDir(t *testing.T) string {
	t.Helper()
	p := os.Getenv("ISSHONI_TEST_ENGINE_DLL")
	if p == "" {
		t.Skip("ISSHONI_TEST_ENGINE_DLL is not set: no engine DLL to test against")
	}
	if strings.EqualFold(filepath.Ext(p), ".dll") {
		if !strings.EqualFold(filepath.Base(p), dllName) {
			t.Fatalf("ISSHONI_TEST_ENGINE_DLL names %s; Open loads %s", filepath.Base(p), dllName)
		}
		p = filepath.Dir(p)
	}
	dir, err := filepath.Abs(p)
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// testLog writes the binding's and the engine's logs into the test's output.
type testLog struct{ t *testing.T }

func (w testLog) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimRight(string(p), "\n"))
	return len(p), nil
}

func openDLL(t *testing.T) *Engine {
	t.Helper()
	dir := engineDLLDir(t)
	e, err := Open(Options{Dir: dir, NoAudio: true, LogLevel: slog.LevelDebug,
		Logger: slog.New(slog.NewTextHandler(testLog{t}, &slog.HandlerOptions{Level: slog.LevelDebug}))})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = e.Close() })
	return e
}

// TestDLLOpen: Open loads the DLL by full path from Dir, the ABI check and the clock cross-check pass, and the build
// info decodes.
func TestDLLOpen(t *testing.T) {
	e := openDLL(t)
	dir := engineDLLDir(t)
	if dllSearch != 0x100|0x800 { // LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32 (libloaderapi.h)
		t.Errorf("LoadLibraryEx flags %#x, want DLL_LOAD_DIR | SYSTEM32", dllSearch)
	}
	if got := loadedModulePath(t, dllName); !strings.EqualFold(got, filepath.Join(dir, dllName)) {
		t.Errorf("%s was loaded from %s, want the one in Dir", dllName, filepath.Base(filepath.Dir(got)))
	}
	info := e.Info()
	if info.Name != "windows" || info.ABI != media.ABIVersion || info.Build == "" {
		t.Errorf("Info() = %+v", info)
	}
	b := e.BuildInfo()
	if b.ABI != imABIVersion || b.OS != "windows" || b.Arch != runtime.GOARCH || b.Engine != info.Build ||
		b.Compiler == "" || filepath.IsAbs(b.FFmpeg.Dir) {
		t.Errorf("BuildInfo() = %+v", b)
	}
	if r, _ := e.c.call(procVersion); uint32(r) != imABIVersion {
		t.Errorf("im_version = %#x", uint32(r))
	}
}

// loadedModulePath is the full path of a module already loaded in this process.
func loadedModulePath(t *testing.T, name string) string {
	t.Helper()
	n, err := windows.UTF16PtrFromString(name)
	if err != nil {
		t.Fatal(err)
	}
	var h windows.Handle
	if err := windows.GetModuleHandleEx(windows.GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT, n, &h); err != nil {
		t.Fatalf("GetModuleHandleEx(%s): %v", name, err)
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	l, err := windows.GetModuleFileName(h, &buf[0], uint32(len(buf)))
	if err != nil {
		t.Fatalf("GetModuleFileName: %v", err)
	}
	return windows.UTF16ToString(buf[:l])
}

// TestDLLLayout: the DLL's im_abi_layout equals the Go mirrors and the golden file (04 §5.4).
func TestDLLLayout(t *testing.T) {
	e := openDLL(t)
	doc, err := callJSON(e.c, "abi_layout", procABILayout, jsonMaxCap)
	if err != nil {
		t.Fatal(err)
	}
	var l abiLayout
	if err := json.Unmarshal(doc, &l); err != nil {
		t.Fatalf("im_abi_layout: %v", err)
	}
	compareLayouts(t, "im_abi_layout", l)
}

// TestDLLMissingExport: an export the DLL lacks fails Open's resolution with dependency/abi naming it (04 §12.4).
func TestDLLMissingExport(t *testing.T) {
	h, err := loadDLL(filepath.Join(engineDLLDir(t), dllName))
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(name string) (uintptr, bool) {
		if name == "im_audio_sound_check" {
			name += "_not_exported"
		}
		a, err := windows.GetProcAddress(h, name)
		return a, err == nil && a != 0
	}
	_, err = resolveProcs(lookup)
	detail := wantError(t, err, media.CodeDependency, tokenABI, "open")
	if !strings.Contains(detail, "im_audio_sound_check") {
		t.Errorf("detail %q does not name the export", detail)
	}
	// And the real table resolves completely.
	if _, err := resolveProcs(func(name string) (uintptr, bool) {
		a, err := windows.GetProcAddress(h, name)
		return a, err == nil && a != 0
	}); err != nil {
		t.Errorf("resolveProcs: %v", err)
	}
}

// TestDLLMissing: a folder without the DLL is dependency/missing, naming the file but not the folder.
func TestDLLMissing(t *testing.T) {
	engineDLLDir(t)
	_, err := Open(Options{Dir: t.TempDir(), NoAudio: true})
	detail := wantError(t, err, media.CodeDependency, tokenMissing, "open")
	if !strings.Contains(detail, dllName) || strings.Contains(detail, string(filepath.Separator)) {
		t.Errorf("detail %q", detail)
	}
}

// TestDLLClock: im_clock agrees with Go's QPC (04 §12.3).
func TestDLLClock(t *testing.T) {
	e := openDLL(t)
	before := qpcNS()
	s, err := e.Clock()
	after := qpcNS()
	if err != nil {
		t.Fatal(err)
	}
	if s.MonoNS < before-clockSlack || s.MonoNS > after+clockSlack || s.UnixNS <= 0 || s.UncertaintyNS < 0 {
		t.Errorf("Clock() = %+v, Go's QPC [%d, %d]", s, before, after)
	}
	if b, a := qpcNS(), qpcNS(); a < b {
		t.Errorf("qpcNS went back: %d then %d", b, a)
	}
}

// TestDLLLastError: a failed call's im_last_error is read on the same locked thread and starts the error's
// detail; a NULL output pointer is IM_ERR_INVALID_ARG.
func TestDLLLastError(t *testing.T) {
	e := openDLL(t)
	r, lastErr := e.c.call(procClock, 0)
	if int32(r) != imErrInvalidArg || lastErr == "" {
		t.Fatalf("im_clock(NULL) = %s, %q", statusName(int32(r)), lastErr)
	}
	err := statusError("clock", procClock.String(), int32(r), lastErr)
	if !errors.Is(err, media.ErrInvalid) || !strings.HasPrefix(err.Detail, "im_clock: ") {
		t.Errorf("error %#v", err)
	}
	// A successful call reports no detail.
	if r, lastErr := e.c.call(procVersion); uint32(r) != imABIVersion || lastErr != "" {
		t.Errorf("im_version = %#x, %q", uint32(r), lastErr)
	}
}
