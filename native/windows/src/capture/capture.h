// The capture area's entry points behind the exports (docs/m2/04-windows-engine.md §7; README WA50 the picker and
// WGC, WA60 Desktop Duplication and im_list_sources). Arguments arrive checked (src/api/validate.h); failures set the
// thread's detail (im::abi::Fail).
//
// The area's parts:
//   picker.h             the picker's STA thread (§7.2 steps 1–3)
//   window_match.h       what a pick is: window identity, kind, self (§7.2 step 4, §7.6); plain C++, unit-tested
//   window_system_win.h  the Win32 queries behind it
//   picked_item.h        what a pick token or a listed monitor holds (session::SourceSpec::item)
//   capture_source.h     what a session sees of its capture, and which capture runs a source (README WF12)
//   wgc_capture.h        WGC for one session into its frame slots (§7.3, §7.7, §7.8)
//   outputs.h            the monitors as DXGI outputs: im_list_sources, "mon:" ids, §7.5's mapping, how a pick is
//                        captured; plain C++, unit-tested (README WA60)
//   outputs_win.h        the DXGI / display-config queries behind it, the Windows build, DPI awareness
//   dd_capture.h         Desktop Duplication for one session into its frame slots (§7.4, §7.7-§7.9)
//   dd_policy.h          DD's interruption, retry and pointer-frame timing; plain C++, unit-tested
//   source_watch.h       resize debounce and minimize edges for the session's supervisor (§7.6, §7.7)
//   winrt_support.h      C++/WinRT, ApiInformation checks, borderless access, HRESULT mapping
#pragma once

#include <cstdint>
#include <string>
#include <string_view>

#include "isshoni_media.h"
#include "session/sources.h"

namespace im::capture {

// im_list_sources, on the engine worker (bound::kListSources): the 04 §5.6 sources document, monitors only (windows
// come from the OS picker). flags is reserved: anything but 0 is IM_ERR_INVALID_ARG.
im_status ListSources(uint32_t flags, std::string &json);

// A listed monitor id ("mon:" + DeviceName, from im_list_sources) as a source for im_start (with
// IM_START_ALLOW_LISTED_SOURCE): kind IM_SOURCE_MONITOR, capture path IM_CAPTURE_DD, the output's upright size, test
// false, and its output in a PickedItem without a WGC item. IM_ERR_SOURCE_GONE when no attached output has that id.
// session::SourceRegistry::Resolve calls it for "mon:" ids.
im_status ResolveMonitor(std::string_view id, session::SourceSpec *out);

// im_pick_source, on the caller's thread: shows the OS picker on the picker's STA thread and blocks until the user
// picks, cancels or params.timeout_ms passes (0 = 120 000, at most 600 000). out is zeroed with struct_size =
// sizeof(im_source_info). A pick is stored under a new token (session::Sources().AddPick, §7.2's token rules) and
// described in out: kind, id, name, size, capture path, IM_SRCF_OS_BORDER / IM_SRCF_SELF / IM_SRCF_MAPPED,
// monitor_id (a monitor that mapped to an output, §7.5), pid and window.
im_status PickSource(const im_pick_params &params, im_source_info &out);

// im_cancel_pick: a blocked PickSource returns IM_ERR_CANCELLED; IM_OK (nothing to do) when none is open.
im_status CancelPick();

// im_release_source: forgets an unused pick token at once (session::Sources().Release); unknown ids are ignored.
void ReleaseSource(std::string_view id) noexcept;

// im_shutdown: cancels an open pick, ends the picker thread and forgets every token.
void Shutdown() noexcept;

// The capture part of im_caps.os_flags, asked from Windows itself (for the probe, on the engine worker, COM MTA):
// IM_OS_WGC and IM_OS_WGC_PICKER (GraphicsCaptureSession::IsSupported()), IM_OS_WGC_CURSOR_TOGGLE,
// IM_OS_WGC_MIN_INTERVAL and IM_OS_WGC_SECONDARY_WINDOWS (the properties are present), IM_OS_WGC_BORDERLESS
// (IsBorderRequired present and GraphicsCaptureAccess allows borderless, §7.1). borderless_wait_ms bounds the access
// query (BorderlessAllowed); 0 asks nothing and uses the answer kept from an earlier query (a pick or a full
// probe), as a quick probe must (< 100 ms).
uint32_t OsFlags(uint32_t borderless_wait_ms) noexcept;

} // namespace im::capture
