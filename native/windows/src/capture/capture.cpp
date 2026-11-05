// The capture area's entry points (README WA50: the picker and its tokens; WA60: im_list_sources, the listed monitors
// and the Windows 10 monitor mapping).
#include "capture/capture.h"

#include <memory>
#include <utility>
#include <vector>

#include "capture/outputs.h"
#include "capture/outputs_win.h"
#include "capture/picked_item.h"
#include "capture/picker.h"
#include "capture/window_match.h"
#include "capture/winrt_support.h"
#include "im/abi/status.h"
#include "session/sources.h"

namespace im::capture {

im_status ListSources(uint32_t flags, std::string &json) {
  if (flags != 0) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_list_sources: flags 0x%x are reserved (0)",
                      static_cast<unsigned>(flags));
  }
  Win32Outputs outputs;
  json = SourcesJson(outputs.Outputs());
  return IM_OK;
}

im_status ResolveMonitor(std::string_view id, session::SourceSpec *out) {
  Win32Outputs outputs;
  const std::vector<DisplayOutput> list = outputs.Outputs();
  const std::optional<std::size_t> i = FindMonitor(list, id);
  if (!i) {
    return abi::Fail(IM_ERR_SOURCE_GONE, {}, "im_start: no attached monitor has this id (im_list_sources lists them)");
  }
  const DisplayOutput &o = list[*i];
  session::SourceSpec spec;
  spec.kind = IM_SOURCE_MONITOR;
  spec.capture_path = IM_CAPTURE_DD;
  spec.width = static_cast<uint32_t>(o.size().width);
  spec.height = static_cast<uint32_t>(o.size().height);
  spec.flags = 0;
  spec.test = false;
  auto item = std::make_shared<PickedItem>();
  item->output = o;
  spec.item = std::move(item);
  *out = std::move(spec);
  return IM_OK;
}

im_status PickSource(const im_pick_params &params, im_source_info &out) {
  PickResult r;
  if (const im_status st = ThePicker().Pick(static_cast<HWND>(params.parent_window), params.timeout_ms, &r);
      st != IM_OK) {
    return st;
  }
  const WindowIdentity id = r.picked ? r.picked->identity : WindowIdentity{};

  // §7.1, §7.5 (outputs.h's PlanPick): WGC for windows and Windows 11 monitors, Desktop Duplication for a Windows 10
  // monitor that mapped to an output, WGC with the border for one that didn't.
  session::SourceSpec spec;
  spec.kind = r.kind;
  spec.capture_path = r.plan.capture_path;
  spec.width = r.width;
  spec.height = r.height;
  spec.flags = r.plan.flags;
  spec.test = false;
  spec.item = r.picked;
  const std::string token = session::Sources().AddPick(std::move(spec));

  out.kind = r.kind;
  CopyUtf8(token, out.id, sizeof out.id);
  CopyUtf8(r.name, out.name, sizeof out.name);
  CopyUtf8(r.plan.monitor_id, out.monitor_id, sizeof out.monitor_id);
  out.width = r.width;
  out.height = r.height;
  out.capture_path = r.plan.capture_path;
  out.flags = r.plan.flags;
  if (r.kind == IM_SOURCE_WINDOW) {
    out.pid = id.pid;
    out.window = id.window;
  }
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

uint32_t OsFlags(uint32_t borderless_wait_ms) noexcept {
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
  if (BorderlessAllowed(borderless_wait_ms)) {
    f |= IM_OS_WGC_BORDERLESS;
  }
  return f;
}

} // namespace im::capture
