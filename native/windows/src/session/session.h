// The session area (docs/m2/04-windows-engine.md §6, §12.3; README WA23 and later): what the session exports forward
// to after src/api/exports.cpp checked their arguments and that the engine is initialized. session_stub.cpp is the
// skeleton's version: im_start is not implemented yet, so no handle is ever valid. The slice that builds sessions
// replaces the stub with the real area, keeping these declarations.
//
// Threads (04 §5.3): Start runs on the engine worker (posted with bound::kStart; a late success is stopped again by
// the export's late handler); NextPacket on the single consumer's thread; the rest on any caller's thread. Every
// function returns an im_status and sets the thread's detail on failure (im::abi::Fail); a stale, foreign or 0 handle
// is IM_ERR_INVALID_ARG.
#pragma once

#include <cstdint>
#include <string>

#include "api/owned.h"
#include "isshoni_media.h"

namespace im::session {

// im_start's output, owned by the posted job and copied to the caller after a success.
struct StartResult {
  im_session handle = 0;
  im_session_info info{}; // filled up to sizeof(im_session_info); the export writes it up to the caller's struct_size
};

// im_start: starts a session for req (copied, checked) and fills out.
im_status Start(const api::StartRequest &req, StartResult &out);

// im_get_session_info: info is zeroed with struct_size = sizeof(im_session_info).
im_status GetInfo(im_session s, im_session_info &info);

// im_next_packet: out is zeroed with struct_size = sizeof(im_packet); timeout_ms <= 1000.
im_status NextPacket(im_session s, uint32_t timeout_ms, im_packet &out);

// The control calls (04 §6.1: validate, queue, return). layer < IM_MAX_LAYERS (RequestKeyframe: or IM_ALL_LAYERS).
im_status RequestKeyframe(im_session s, uint32_t layer);
im_status SetBitrate(im_session s, uint32_t layer, uint32_t bps);
im_status SetLayerActive(im_session s, uint32_t layer, bool active);
im_status UpdateLayer(im_session s, uint32_t layer, const im_layer_config &cfg); // cfg copied and checked
im_status SetProfile(im_session s, uint32_t profile);                            // IM_PROFILE_HIGH or IM_PROFILE_CB
im_status SetContentHint(im_session s, uint32_t hint);                           // IM_CONTENT_*
im_status SetPaused(im_session s, bool paused);
im_status SetAudioBitrate(im_session s, uint32_t bps); // 32 000..510 000

// im_get_stats: the 04 §5.6 stats document of the session.
im_status GetStats(im_session s, std::string &json);

// im_stop (≤ 2 s); afterwards the handle is invalid.
im_status Stop(im_session s);

// im_shutdown: stops every session (≤ 2 s).
void StopAll() noexcept;

} // namespace im::session
