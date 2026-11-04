// The capture area's entry points (README WA50): the picker and its tokens. im_list_sources stays README WA60's.
#include "capture/capture.h"

#include <utility>

#include "capture/picker.h"
#include "capture/window_match.h"
#include "capture/winrt_support.h"
#include "im/abi/status.h"
#include "session/sources.h"

namespace im::capture {

im_status ListSources(uint32_t, std::string &) {
  return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_list_sources: not implemented yet (README WA60)");
}

im_status PickSource(const im_pick_params &params, im_source_info &out) {
  PickResult r;
  if (const im_status st = ThePicker().Pick(static_cast<HWND>(params.parent_window), params.timeout_ms, &r);
      st != IM_OK) {
    return st;
  }
  const WindowIdentity id = r.picked ? r.picked->identity : WindowIdentity{};

  session::SourceSpec spec;
  spec.kind = r.kind;
  // §7.1: WGC for windows and, on Windows 11, monitors; README WA60 maps a Windows 10 monitor to Desktop Duplication
  // (§7.5) and keeps WGC with the border when the mapping fails.
  spec.capture_path = IM_CAPTURE_WGC;
  spec.width = r.width;
  spec.height = r.height;
  spec.flags = (r.borderless ? 0u : IM_SRCF_OS_BORDER) | (r.self ? IM_SRCF_SELF : 0u);
  spec.test = false;
  spec.item = r.picked;
  const std::string token = session::Sources().AddPick(std::move(spec));

  out.kind = r.kind;
  CopyUtf8(token, out.id, sizeof out.id);
  CopyUtf8(r.name, out.name, sizeof out.name);
  out.width = r.width;
  out.height = r.height;
  out.capture_path = IM_CAPTURE_WGC;
  out.flags = (r.borderless ? 0u : IM_SRCF_OS_BORDER) | (r.self ? IM_SRCF_SELF : 0u);
  if (r.kind == IM_SOURCE_WINDOW) {
    out.pid = id.pid;
    out.window = id.window;
  }
  // monitor_id stays "" until README WA60's im_list_sources ids exist.
  return IM_OK;
}

im_status CancelPick() {
  ThePicker().Cancel();
  return IM_OK;
}

void ReleaseSource(std::string_view id) noexcept { session::Sources().Release(id); }

void Shutdown() noexcept {
  ThePicker().Shutdown();
  session::Sources().Clear();
}

uint32_t OsFlags() noexcept {
  if (!WgcSupported()) {
    return 0;
  }
  uint32_t f = IM_OS_WGC | IM_OS_WGC_PICKER;
  if (SessionHasProperty(L"IsCursorCaptureEnabled")) {
    f |= IM_OS_WGC_CURSOR_TOGGLE;
  }
  if (SessionHasProperty(L"MinUpdateInterval")) {
    f |= IM_OS_WGC_MIN_INTERVAL;
  }
  if (SessionHasProperty(L"IncludeSecondaryWindows")) {
    f |= IM_OS_WGC_SECONDARY_WINDOWS;
  }
  if (BorderlessAllowed()) {
    f |= IM_OS_WGC_BORDERLESS;
  }
  return f;
}

} // namespace im::capture
