// im_protect_window's area (docs/m2/04-windows-engine.md §11; README WA70): WDA_EXCLUDEFROMCAPTURE on a window,
// used in M2 by isshoni-enginetest's protect-check only (the app's own windows are 03's winos). The ABI keeps the call
// for M3, where the macOS engine adds the window to the SCContentFilter's exclusions.
//
// On Windows, Protect(window, 0) calls SetWindowDisplayAffinity(window, WDA_EXCLUDEFROMCAPTURE) and reads the
// affinity back with GetWindowDisplayAffinity; IM_PROTECT_OFF sets WDA_NONE the same way. Whatever set it, a window
// with that affinity is left out of WGC monitor captures and of Desktop Duplication alike (DWM composes the content
// behind it into the captured image), which isshoni-enginetest protect-check proves on the owner's PC.
//
// The rules (04 §11):
// - window must be a top-level window (GetAncestor(GA_ROOT) is itself) of this process: Windows refuses the affinity
//   for another process's window, so the engine says so first, as IM_ERR_INVALID_ARG; a handle that is no window and
//   a child window are IM_ERR_INVALID_ARG too. Any thread of the process may call it.
// - flags is 0 or IM_PROTECT_OFF; other bits are IM_ERR_INVALID_ARG.
// - Below build 19041 (no WDA_EXCLUDEFROMCAPTURE) it is IM_ERR_UNSUPPORTED_OS with the token os_too_old.
// - Windows refusing the affinity, or reading back another one, is IM_ERR_INTERNAL with the Win32 error.
// - The log records only the outcome, the flags and the build: never the window's title or class (README §4).
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::window {

// The first Windows build with WDA_EXCLUDEFROMCAPTURE (Windows 10 2004).
inline constexpr uint32_t kExcludeFromCaptureBuild = 19041;

// window is a non-NULL HWND; flags IM_PROTECT_*. IM_ERR_UNSUPPORTED_OS (os_too_old) below build 19041.
im_status Protect(void *window, uint32_t flags);

// Protect on a given Windows build (RtlGetVersion's; 0 = unknown, not gated): the tests' seam for the build gate.
im_status ProtectOnBuild(void *window, uint32_t flags, uint32_t build);

} // namespace im::window
