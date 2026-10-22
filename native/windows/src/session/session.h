// The session area (docs/m2/04-windows-engine.md §6, §8.4–§8.5, §9.4–§9.8, §10.3; README WA23): what the session
// exports forward to after src/api/exports.cpp checked their arguments and that the engine is initialized.
//
// A session (session_object.h) is a running capture and encode behind a handle (index << 32 | generation into a
// 16-slot table, at most 4 live sessions and 1 with audio, §6.4): one SPSC ring per stream (each video layer, Opus,
// the PCM tap) and an event queue, drained by im_next_packet; a video thread (pacer, encoders, normalizer); a
// supervisor (real-time flags into events, the test pattern's timer, ending the session); and, in
// IM_AUDIO_MODE_TEST_TONE, a test-tone thread feeding the Opus stage (05's mixer does that for real audio, wired here
// through audio::AudioEngine, §10.2).
//
// Threads (04 §5.3): Start runs on the engine worker (posted with bound::kStart; a late success is stopped again by
// the export's late handler); NextPacket on the single consumer's thread; the rest on any caller's thread. Every
// function returns an im_status and sets the thread's detail on failure (im::abi::Fail); a stale, foreign or 0 handle
// is IM_ERR_INVALID_ARG. A control call on a session that has ended (ENDED queued) succeeds and does nothing.
#pragma once

#include <cstdint>
#include <string>

#include "api/owned.h"
#include "isshoni_media.h"

namespace im::session {

inline constexpr uint32_t kMaxSessions = 4;      // 04 §6.4
inline constexpr uint32_t kMaxAudioSessions = 1; // 05's mixer is per process

// im_start's output, owned by the posted job and copied to the caller after a success.
struct StartResult {
  im_session handle = 0;
  im_session_info info{}; // filled up to sizeof(im_session_info); the export writes it up to the caller's struct_size
};

// im_start: starts a session for req (copied, checked) and fills out. IM_ERR_LIMIT (session_limit) beyond 4 sessions
// or a second one with audio; IM_ERR_SOURCE_GONE (pick_expired) for a used, expired or evicted pick token (which a
// failed start never consumes); IM_ERR_NO_ENCODER (layer_failed, forced_failed) when a layer gets no encoder.
im_status Start(const api::StartRequest &req, StartResult &out);

// im_get_session_info: info is zeroed with struct_size = sizeof(im_session_info).
im_status GetInfo(im_session s, im_session_info &info);

// im_next_packet: out is zeroed with struct_size = sizeof(im_packet); timeout_ms <= 1000.
im_status NextPacket(im_session s, uint32_t timeout_ms, im_packet &out);

// The control calls (04 §6.1: validate, queue, return). layer < IM_MAX_LAYERS (RequestKeyframe: or IM_ALL_LAYERS);
// a layer the session doesn't have is IM_ERR_INVALID_ARG.
im_status RequestKeyframe(im_session s, uint32_t layer);
im_status SetBitrate(im_session s, uint32_t layer, uint32_t bps);
im_status SetLayerActive(im_session s, uint32_t layer, bool active);
im_status UpdateLayer(im_session s, uint32_t layer, const im_layer_config &cfg); // cfg copied and checked
im_status SetProfile(im_session s, uint32_t profile);                            // IM_PROFILE_HIGH or IM_PROFILE_CB
im_status SetContentHint(im_session s, uint32_t hint);                           // IM_CONTENT_*
im_status SetPaused(im_session s, bool paused);
im_status SetAudioBitrate(im_session s, uint32_t bps); // 32 000..510 000; IM_ERR_INVALID_ARG without audio

// im_get_stats: the 04 §5.6 stats document of the session.
im_status GetStats(im_session s, std::string &json);

// im_stop (≤ 2 s); afterwards the handle is invalid.
im_status Stop(im_session s);

// im_shutdown: stops every session (≤ 2 s each) and forgets every pick token.
void StopAll() noexcept;

// Set when a session ended on a removed device (04 §9.8): the probe's cached result is stale, and README WA44's probe
// runs again (lazily) before the next start relies on it, then clears the mark.
bool ProbeCacheStale() noexcept;
void MarkProbeCacheStale() noexcept;
void ResetProbeCacheStale() noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// For src/testhooks/ (testhooks.h documents them).
enum class InjectedFault : uint8_t { kEncodeError, kDeviceRemoved };
im_status InjectFault(im_session s, uint32_t layer, InjectedFault fault);
uint64_t DeviceOf(im_session s);
#endif

} // namespace im::session
