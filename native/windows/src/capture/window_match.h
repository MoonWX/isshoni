// What a pick is (docs/m2/04-windows-engine.md §7.2 step 4, §7.6): a GraphicsCaptureItem from the picker carries
// neither an HWND nor whether it is a window or a monitor, so the engine identifies it once, at pick time:
//
// - Window identity (§7.6): among the visible top-level windows (EnumWindows), skipping cloaked ones (DWMWA_CLOAKED),
//   those whose title equals item.DisplayName() and whose DWMWA_EXTENDED_FRAME_BOUNDS size equals item.Size() within
//   kSizeTolerance pixels. Exactly one match gives `window` and `pid` (GetWindowThreadProcessId); none or several leave
//   both 0. A match whose process image is applicationframehost.exe (base name, compared without case: UWP and other
//   frame-hosted apps) takes `pid` from its Windows.UI.Core.CoreWindow child, and 0 without one (`window` stays the
//   frame's).
// - Kind (§7.2 step 4): an identified window is a window; otherwise a pick whose size equals a monitor's is that
//   monitor; otherwise a window of unknown identity.
// - Self (§7.2, §11): IM_SRCF_SELF when the identified window's process is this process or one of its msedgewebview2
//   children (WebView2's processes descend from the host that created the environment).
//
// This file is plain C++ over an abstract window system, so window_match_test runs the rules over fake windows; the
// Win32 implementation is window_system_win.cpp. Nothing here logs a title or an image name (README §4 "Logging").
#pragma once

#include <cstddef>
#include <cstdint>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

namespace im::capture {

using WindowHandle = uint64_t; // an HWND as an integer (im_source_info.window); 0 = none

// The frame bounds' tolerance against item.Size() (§7.6).
inline constexpr int32_t kSizeTolerance = 2;

inline constexpr std::wstring_view kFrameHostImage = L"applicationframehost.exe";
inline constexpr std::wstring_view kWebView2Image = L"msedgewebview2.exe";
// How many parent links IsSelfProcess follows from a WebView2 process up to the host (browser process → host; a
// renderer → browser process → host; one more for utility processes started by a helper).
inline constexpr int kMaxSelfDepth = 4;

struct PixelSize {
  int32_t width = 0;
  int32_t height = 0;
  friend constexpr bool operator==(const PixelSize &, const PixelSize &) = default;
};

// The OS queries the matcher needs. Every call may fail (a window that closed meanwhile): the defaults say so.
class WindowSystem {
public:
  virtual ~WindowSystem() = default;
  // The top-level windows (EnumWindows), in z-order.
  virtual std::vector<WindowHandle> TopLevelWindows() = 0;
  // IsWindowVisible.
  virtual bool Visible(WindowHandle w) = 0;
  // DWMWA_CLOAKED is non-zero (another virtual desktop, a suspended UWP app, a hidden shell window).
  virtual bool Cloaked(WindowHandle w) = 0;
  // GetWindowTextW ("" when it has none or the call failed).
  virtual std::wstring Title(WindowHandle w) = 0;
  // The size of DWMWA_EXTENDED_FRAME_BOUNDS in physical pixels (the caller's thread is per-monitor DPI aware), or
  // nullopt.
  virtual std::optional<PixelSize> FrameSize(WindowHandle w) = 0;
  // GetWindowThreadProcessId (0 when the window is gone).
  virtual uint32_t ProcessId(WindowHandle w) = 0;
  // The base name of the process's image (QueryFullProcessImageNameW), "" when unknown.
  virtual std::wstring ImageBaseName(uint32_t pid) = 0;
  // FindWindowExW(w, nullptr, L"Windows.UI.Core.CoreWindow", nullptr), 0 when there is none.
  virtual WindowHandle CoreWindowChild(WindowHandle w) = 0;
};

struct WindowIdentity {
  WindowHandle window = 0; // 0: not identified
  uint32_t pid = 0;        // 0: unknown (also for an identified frame host without a CoreWindow child)
  bool identified() const noexcept { return window != 0; }
};

// §7.6's window match for a picked item titled `title` of `size`.
WindowIdentity MatchWindow(WindowSystem &ws, std::wstring_view title, PixelSize size);

// Whether two sizes are equal within kSizeTolerance on each side.
bool SizeMatches(PixelSize a, PixelSize b) noexcept;

// The base name of a Windows path (after the last '\' or '/').
std::wstring_view BaseName(std::wstring_view path) noexcept;

// Equality of two image names, ASCII letters compared without case (the names compared here are ASCII; any other
// character must be equal).
bool SameImageName(std::wstring_view a, std::wstring_view b) noexcept;

// What a pick is (§7.2 step 4): IM_SOURCE_WINDOW for an identified window; IM_SOURCE_MONITOR when no window was
// identified and item equals one of the monitors' sizes exactly (physical pixels); IM_SOURCE_WINDOW otherwise (a
// window of unknown identity).
uint32_t ClassifyPick(const WindowIdentity &id, std::span<const PixelSize> monitors, PixelSize item) noexcept;

// One row of the process table (a Toolhelp snapshot).
struct ProcessEntry {
  uint32_t pid = 0;
  uint32_t parent = 0;
  std::wstring image; // base name
};

// IM_SRCF_SELF (§7.2): pid is self, or a msedgewebview2.exe process whose parent chain (at most kMaxSelfDepth links,
// through any images) reaches self. pid 0 is never self.
bool IsSelfProcess(std::span<const ProcessEntry> table, uint32_t pid, uint32_t self) noexcept;

// UTF-16 (wchar_t units; on a platform with 32-bit wchar_t, code points) to UTF-8. Unpaired surrogates become U+FFFD.
std::string Utf8FromWide(std::wstring_view s);

// Copies s into dst[cap] NUL-terminated, cut on a UTF-8 character boundary when it doesn't fit (cap ≥ 1).
void CopyUtf8(std::string_view s, char *dst, std::size_t cap) noexcept;

} // namespace im::capture
