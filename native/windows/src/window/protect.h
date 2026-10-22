// im_protect_window's area (docs/m2/04-windows-engine.md §11; README WA70): WDA_EXCLUDEFROMCAPTURE on a window,
// used in M2 by isshoni-enginetest's protect-check only (the app's own windows are 03's winos). protect_stub.cpp is the
// skeleton's version.
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::window {

// window is a non-NULL HWND; flags IM_PROTECT_*. IM_ERR_UNSUPPORTED_OS (os_too_old) below build 19041.
im_status Protect(void *window, uint32_t flags);

} // namespace im::window
