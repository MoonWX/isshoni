// im_protect_window on Windows (docs/m2/04-windows-engine.md §11; README WA70): see protect.h.
#include "window/protect.h"

#include <windows.h>

#include "capture/outputs_win.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

// WDA_EXCLUDEFROMCAPTURE is in winuser.h from the Windows 10 2004 SDK on; the engine builds with a newer one, but the
// value is part of the OS contract either way.
#ifndef WDA_EXCLUDEFROMCAPTURE
#define WDA_EXCLUDEFROMCAPTURE 0x00000011
#endif

namespace im::window {
namespace {

constexpr uint32_t kKnownFlags = IM_PROTECT_OFF;

} // namespace

im_status ProtectOnBuild(void *window, uint32_t flags, uint32_t build) {
  if (window == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_protect_window: native_window is NULL");
  }
  if ((flags & ~kKnownFlags) != 0) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_protect_window: unknown flags 0x%x",
                      static_cast<unsigned>(flags & ~kKnownFlags));
  }
  if (build != 0 && build < kExcludeFromCaptureBuild) {
    return abi::Failf(IM_ERR_UNSUPPORTED_OS, abi::token::kOsTooOld,
                      "im_protect_window: WDA_EXCLUDEFROMCAPTURE needs build %u, this is build %u",
                      static_cast<unsigned>(kExcludeFromCaptureBuild), static_cast<unsigned>(build));
  }
  const HWND hwnd = static_cast<HWND>(window);
  if (::IsWindow(hwnd) == FALSE) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_protect_window: not a window");
  }
  DWORD pid = 0;
  if (::GetWindowThreadProcessId(hwnd, &pid) == 0 || pid != ::GetCurrentProcessId()) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_protect_window: the window belongs to another process");
  }
  if (::GetAncestor(hwnd, GA_ROOT) != hwnd) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_protect_window: not a top-level window");
  }

  const bool off = (flags & IM_PROTECT_OFF) != 0;
  const DWORD want = off ? static_cast<DWORD>(WDA_NONE) : static_cast<DWORD>(WDA_EXCLUDEFROMCAPTURE);
  if (::SetWindowDisplayAffinity(hwnd, want) == FALSE) {
    const DWORD err = ::GetLastError();
    log::Warn("window", "display affinity refused", {{"off", off}, {"win32", static_cast<uint32_t>(err)}});
    return abi::Failf(IM_ERR_INTERNAL, {}, "im_protect_window: SetWindowDisplayAffinity failed (Win32 error %lu)",
                      static_cast<unsigned long>(err));
  }
  DWORD got = 0;
  if (::GetWindowDisplayAffinity(hwnd, &got) == FALSE) {
    const DWORD err = ::GetLastError();
    return abi::Failf(IM_ERR_INTERNAL, {}, "im_protect_window: GetWindowDisplayAffinity failed (Win32 error %lu)",
                      static_cast<unsigned long>(err));
  }
  if (got != want) {
    log::Warn("window", "display affinity not kept",
              {{"off", off}, {"want", static_cast<uint32_t>(want)}, {"got", static_cast<uint32_t>(got)}});
    return abi::Failf(IM_ERR_INTERNAL, {}, "im_protect_window: the affinity reads 0x%lx, want 0x%lx",
                      static_cast<unsigned long>(got), static_cast<unsigned long>(want));
  }
  // Never the window's title, class or process (README §4 "Logging").
  log::Info("window", off ? "unprotected" : "protected", {{"build", build}});
  return IM_OK;
}

im_status Protect(void *window, uint32_t flags) { return ProtectOnBuild(window, flags, capture::WindowsBuild()); }

} // namespace im::window
