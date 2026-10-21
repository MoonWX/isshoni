// The capture area's entry points behind the exports (docs/m2/04-windows-engine.md §7; README WA50 the picker and
// WGC, WA60 Desktop Duplication and im_list_sources). capture_stub.cpp is the skeleton's version, replaced by those
// slices. Arguments arrive checked (src/api/validate.h); failures set the thread's detail (im::abi::Fail).
#pragma once

#include <cstdint>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::capture {

// im_list_sources, on the engine worker (bound::kListSources): the 04 §5.6 sources document.
im_status ListSources(uint32_t flags, std::string &json);

// im_pick_source, on the caller's thread: shows the OS picker on the picker's STA thread and blocks until the user
// picks, cancels or params.timeout_ms passes (0 = 120 000, at most 600 000). out is zeroed with struct_size =
// sizeof(im_source_info).
im_status PickSource(const im_pick_params &params, im_source_info &out);

// im_cancel_pick: a blocked PickSource returns IM_ERR_CANCELLED.
im_status CancelPick();

// im_release_source: forgets an unused pick token; unknown ids are ignored.
void ReleaseSource(std::string_view id) noexcept;

// im_shutdown: cancels an open pick and forgets every token.
void Shutdown() noexcept;

} // namespace im::capture
