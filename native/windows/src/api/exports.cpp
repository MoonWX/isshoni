// The C ABI of isshoni-media.dll (docs/m2/04-windows-engine.md §5, "04" below): every IM_API function of
// native/include/isshoni_media.h, exported through isshoni-media.def under the same names.
//
// Each export, inside a noexcept Guard (api/guard.h):
//   1. clears the thread's detail (04 §5.1), except im_last_error, which reads it;
//   2. checks its arguments (api/validate.h): pointers, struct_size (below the v1.0 size: IM_ERR_ABI, token abi),
//      the header's constants and documented ranges;
//   3. checks that im_init ran (IM_ERR_BAD_STATE, not_initialized), except the calls that work before it: im_version,
//      im_build_info, im_clock, im_last_error, im_read_logs, im_abi_layout and im_test_opus_decode;
//   4. forwards to its area: the session, capture, encode, window and audio functions declared in their folders'
//      headers. Calls that 04 §5.3 runs on the engine worker (im_probe, im_list_sources, im_set_openh264_path,
//      im_start) post a job with their bound (engine/engine.h); the job gets copies of the input and its own output,
//      which the export copies back to the caller after a success (engine/worker.h).
// Output structs are written up to min(struct_size, sizeof) with struct_size set to the bytes written; JSON outputs
// follow snprintf (04 §5.1).
#include <cstddef>
#include <cstring>
#include <memory>
#include <optional>
#include <string.h>
#include <string>
#include <utility>

#include "api/guard.h"
#include "api/owned.h"
#include "api/validate.h"
#include "audio/mixer.h"
#include "audio/test_exports.h"
#include "audio_bridge/audio_bridge.h"
#include "capture/capture.h"
#include "encode/encode.h"
#include "engine/build_info.h"
#include "engine/clock.h"
#include "engine/engine.h"
#include "im/abi/layout.h"
#include "im/abi/status.h"
#include "im/abi/struct_size.h"
#include "im/log/ring.h"
#include "isshoni_media.h"
#include "session/session.h"
#include "window/protect.h"

namespace {

using im::abi::ClearLastError;
using im::abi::Failf;

// Returns from the export when a check failed.
#define IM_CHECK(expr)                                                                                                 \
  do {                                                                                                                 \
    if (const im_status im_check_st_ = (expr); im_check_st_ != IM_OK) {                                                \
      return im_check_st_;                                                                                             \
    }                                                                                                                  \
  } while (0)

// The struct layouts of 04 §5.4, pinned in the DLL's own build too (the release preset builds no tests): a size or an
// offset that changes without a minor ABI bump fails here. The numbers are v1.0's; a minor bump that appends fields to
// a top-level struct updates its size here and in native/common/tests/abi_layout_test.cpp with the bump.
static_assert(IM_ABI_VERSION == 0x00010000u, "the layouts below are v1.0's: update them with the ABI bump");
#define IM_SIZE(T, n)  static_assert(sizeof(T) == (n), #T " has the size of 04 §5.4")
#define IM_AT(T, f, n) static_assert(offsetof(T, f) == (n), #T "." #f " has the offset of 04 §5.4")
IM_SIZE(im_init_params, 32);
IM_AT(im_init_params, ffmpeg_dir, 16);
IM_AT(im_init_params, openh264_path, 24);
IM_SIZE(im_clock_sample, 32);
IM_SIZE(im_adapter_info, 160);
IM_AT(im_adapter_info, name, 24);
IM_SIZE(im_encoder_info, 104);
IM_AT(im_encoder_info, codec, 40);
IM_AT(im_encoder_info, reason, 72);
IM_SIZE(im_caps, 1568);
IM_AT(im_caps, adapters, 24);
IM_AT(im_caps, encoder_count, 664);
IM_AT(im_caps, encoders, 672);
IM_AT(im_caps, probe_ms, 1504);
IM_SIZE(im_pick_params, 24);
IM_AT(im_pick_params, parent_window, 8);
IM_SIZE(im_source_info, 440);
IM_AT(im_source_info, id, 8);
IM_AT(im_source_info, name, 72);
IM_AT(im_source_info, monitor_id, 328);
IM_AT(im_source_info, width, 392);
IM_AT(im_source_info, pid, 408);
IM_AT(im_source_info, window, 416);
IM_SIZE(im_rule, 32);
IM_AT(im_rule, value, 8);
IM_AT(im_rule, app_id, 16);
IM_AT(im_rule, name, 24);
IM_SIZE(im_exclusions, 24);
IM_AT(im_exclusions, rules, 8);
IM_AT(im_exclusions, rule_count, 16);
IM_AT(im_exclusions, shared_pid, 20);
IM_SIZE(im_audio_config, 32);
IM_AT(im_audio_config, mode, 0);
IM_AT(im_audio_config, bitrate_bps, 4);
IM_AT(im_audio_config, frame_ms, 8);
IM_AT(im_audio_config, flags, 12);
IM_SIZE(im_audio_level, 80);
IM_AT(im_audio_level, peak_db, 64);
IM_AT(im_audio_level, rms_db, 68);
IM_AT(im_audio_level, source, 72);
IM_SIZE(im_audio_levels, 5152);
IM_AT(im_audio_levels, qpc_ns, 8);
IM_AT(im_audio_levels, mix_peak_db, 16);
IM_AT(im_audio_levels, count, 24);
IM_AT(im_audio_levels, apps, 32);
IM_SIZE(im_layer_config, 40);
IM_SIZE(im_start_config, 224);
IM_AT(im_start_config, source_id, 8);
IM_AT(im_start_config, layers, 32);
IM_AT(im_start_config, audio, 152);
IM_AT(im_start_config, exclusions, 184);
IM_AT(im_start_config, reserved, 192);
IM_SIZE(im_layer_info, 40);
IM_SIZE(im_session_info, 184);
IM_AT(im_session_info, epoch_ns, 16);
IM_AT(im_session_info, layers, 32);
IM_SIZE(im_packet, 72);
IM_AT(im_packet, capture_ts_ns, 16);
IM_AT(im_packet, data, 24);
IM_AT(im_packet, size, 32);
IM_AT(im_packet, width, 40);
IM_AT(im_packet, profile, 44);
IM_AT(im_packet, event, 48);
IM_AT(im_packet, status, 52);
#undef IM_SIZE
#undef IM_AT

// Every top-level struct is exactly its v1.0 size (the struct_size floor of ReadIn and CheckOut), struct_size first.
template <class T> constexpr bool kV1Exact = sizeof(T) == im::abi::V1Size<T>::value && offsetof(T, struct_size) == 0;
static_assert(kV1Exact<im_init_params> && kV1Exact<im_clock_sample> && kV1Exact<im_caps> && kV1Exact<im_pick_params> &&
              kV1Exact<im_source_info> && kV1Exact<im_exclusions> && kV1Exact<im_audio_levels> &&
              kV1Exact<im_start_config> && kV1Exact<im_session_info> && kV1Exact<im_packet>);

template <class T> T Zeroed() noexcept {
  T v{};
  v.struct_size = sizeof(T);
  return v;
}

} // namespace

extern "C" {

// ---------------------------------------------------------------------------------------------- init, build info

IM_API uint32_t IM_CALL im_version(void) {
  ClearLastError();
  return IM_ABI_VERSION;
}

IM_API im_status IM_CALL im_init(const im_init_params *params) {
  return im::api::Guard("im_init", [&]() -> im_status {
    ClearLastError();
    im_init_params p;
    IM_CHECK(im::abi::ReadIn(params, p, "im_init: params"));
    if ((p.abi_version >> 16) != IM_ABI_MAJOR) {
      return Failf(IM_ERR_ABI, im::abi::token::kAbi, "im_init: the caller's ABI %u.%u has another major than %u.%u",
                   static_cast<unsigned>(p.abi_version >> 16), static_cast<unsigned>(p.abi_version & 0xFFFFu),
                   IM_ABI_MAJOR, IM_ABI_MINOR);
    }
    if (p.log_level > IM_LOG_DEBUG) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_init: params->log_level %u is not an IM_LOG_* level",
                   static_cast<unsigned>(p.log_level));
    }
    im::engine::Options o;
    o.init_flags = p.flags;
    o.log_level = p.log_level;
    o.caller_abi = p.abi_version;
    IM_CHECK(im::api::ReadAbsolutePath("im_init", "params->ffmpeg_dir", p.ffmpeg_dir, o.ffmpeg_dir));
    IM_CHECK(im::api::ReadAbsolutePath("im_init", "params->openh264_path", p.openh264_path, o.openh264_path));
    return im::engine::Init(std::move(o));
  });
}

IM_API void IM_CALL im_shutdown(void) {
  im::api::Guard("im_shutdown", [] {
    ClearLastError();
    im::engine::Shutdown();
  });
}

IM_API int32_t IM_CALL im_build_info(char *json, int32_t cap) {
  return im::api::Guard("im_build_info", [&]() -> int32_t {
    ClearLastError();
    IM_CHECK(im::api::CheckJsonOut("im_build_info", json, cap));
    return im::api::JsonOut("im_build_info", im::engine::BuildInfoJson(), json, cap);
  });
}

IM_API im_status IM_CALL im_set_openh264_path(const char *path_or_null) {
  return im::api::Guard("im_set_openh264_path", [&]() -> im_status {
    ClearLastError();
    std::optional<std::string> path;
    IM_CHECK(im::api::ReadAbsolutePath("im_set_openh264_path", "path", path_or_null, path));
    IM_CHECK(im::engine::RequireInit("im_set_openh264_path"));
    auto owned = std::make_shared<const std::optional<std::string>>(std::move(path));
    return im::engine::TheWorker().Post("im_set_openh264_path", im::engine::bound::kSetOpenH264Path,
                                        [owned]() -> int32_t { return im::encode::SetOpenH264Path(*owned); });
  });
}

IM_API im_status IM_CALL im_clock(im_clock_sample *out) {
  return im::api::Guard("im_clock", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::abi::CheckOut(out, "im_clock: out"));
    auto sample = Zeroed<im_clock_sample>();
    im::engine::SampleClock(sample);
    im::abi::WriteOut(out, sample);
    return IM_OK;
  });
}

// ----------------------------------------------------------------------------------------------------------- probe

IM_API im_status IM_CALL im_probe(uint32_t flags, im_caps *caps) {
  return im::api::Guard("im_probe", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::abi::CheckOut(caps, "im_probe: caps"));
    IM_CHECK(im::engine::RequireInit("im_probe"));
    auto result = std::make_shared<im_caps>(Zeroed<im_caps>());
    const auto bound = (flags & IM_PROBE_FULL) != 0 ? im::engine::bound::kProbeFull : im::engine::bound::kProbeQuick;
    const int32_t st = im::engine::TheWorker().Post(
        "im_probe", bound, [result, flags]() -> int32_t { return im::encode::Probe(flags, *result); });
    if (st >= 0) {
      im::abi::WriteOut(caps, *result);
    }
    return st;
  });
}

// --------------------------------------------------------------------------------------------------------- sources

IM_API int32_t IM_CALL im_list_sources(uint32_t flags, char *json, int32_t cap) {
  return im::api::Guard("im_list_sources", [&]() -> int32_t {
    ClearLastError();
    IM_CHECK(im::api::CheckJsonOut("im_list_sources", json, cap));
    IM_CHECK(im::engine::RequireInit("im_list_sources"));
    auto doc = std::make_shared<std::string>();
    const int32_t st =
        im::engine::TheWorker().Post("im_list_sources", im::engine::bound::kListSources,
                                     [doc, flags]() -> int32_t { return im::capture::ListSources(flags, *doc); });
    if (st < 0) {
      return st;
    }
    return im::api::JsonOut("im_list_sources", *doc, json, cap);
  });
}

IM_API im_status IM_CALL im_pick_source(const im_pick_params *params, im_source_info *out) {
  return im::api::Guard("im_pick_source", [&]() -> im_status {
    ClearLastError();
    im_pick_params p;
    IM_CHECK(im::abi::ReadIn(params, p, "im_pick_source: params"));
    IM_CHECK(im::abi::CheckOut(out, "im_pick_source: out"));
    if (p.timeout_ms > 600000) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_pick_source: params->timeout_ms %u is above 600000",
                   static_cast<unsigned>(p.timeout_ms));
    }
    IM_CHECK(im::engine::RequireInit("im_pick_source"));
    auto info = Zeroed<im_source_info>();
    IM_CHECK(im::capture::PickSource(p, info));
    im::abi::WriteOut(out, info);
    return IM_OK;
  });
}

IM_API im_status IM_CALL im_cancel_pick(void) {
  return im::api::Guard("im_cancel_pick", []() -> im_status {
    ClearLastError();
    IM_CHECK(im::engine::RequireInit("im_cancel_pick"));
    return im::capture::CancelPick();
  });
}

IM_API void IM_CALL im_release_source(const char *id) {
  im::api::Guard("im_release_source", [&] {
    ClearLastError();
    if (id == nullptr || !im::engine::Initialized()) {
      return; // nothing to forget
    }
    const std::size_t n = ::strnlen(id, sizeof(im_source_info::id));
    if (n < sizeof(im_source_info::id)) {
      im::capture::ReleaseSource(std::string_view(id, n));
    }
  });
}

// ----------------------------------------------------------------------------------------------------------- audio

IM_API int32_t IM_CALL im_list_audio_apps(uint32_t flags, char *json, int32_t cap) {
  return im::api::Guard("im_list_audio_apps", [&]() -> int32_t {
    ClearLastError();
    IM_CHECK(im::api::CheckJsonOut("im_list_audio_apps", json, cap));
    IM_CHECK(im::engine::RequireInit("im_list_audio_apps"));
    return im::audio_bridge::Engine().ListAppsJson(flags, json, cap);
  });
}

IM_API im_status IM_CALL im_audio_watch(const im_exclusions *ex) {
  return im::api::Guard("im_audio_watch", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckExclusions("im_audio_watch", "ex", ex));
    IM_CHECK(im::engine::RequireInit("im_audio_watch"));
    return im::audio_bridge::Engine().Watch(ex);
  });
}

IM_API im_status IM_CALL im_audio_unwatch(void) {
  return im::api::Guard("im_audio_unwatch", []() -> im_status {
    ClearLastError();
    IM_CHECK(im::engine::RequireInit("im_audio_unwatch"));
    return im::audio_bridge::Engine().Unwatch();
  });
}

IM_API im_status IM_CALL im_set_exclusions(const im_exclusions *ex) {
  return im::api::Guard("im_set_exclusions", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckExclusions("im_set_exclusions", "ex", ex));
    IM_CHECK(im::engine::RequireInit("im_set_exclusions"));
    return im::audio_bridge::Engine().SetRules(ex);
  });
}

IM_API im_status IM_CALL im_audio_set_mode(uint32_t mode) {
  return im::api::Guard("im_audio_set_mode", [&]() -> im_status {
    ClearLastError();
    if (mode == IM_AUDIO_MODE_EVERYTHING) {
      // Reserved for "All sound" (later); refused in M2 by every layer (README §3 item 15).
      return Failf(IM_ERR_INVALID_ARG, {}, "im_audio_set_mode: IM_AUDIO_MODE_EVERYTHING is reserved and refused in M2");
    }
    if (mode != IM_AUDIO_MODE_OFF && mode != IM_AUDIO_MODE_EXCLUDE_APPS) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_audio_set_mode: mode %u is not IM_AUDIO_MODE_OFF or _EXCLUDE_APPS",
                   static_cast<unsigned>(mode));
    }
    IM_CHECK(im::engine::RequireInit("im_audio_set_mode"));
    return im::audio_bridge::Engine().SetMode(mode);
  });
}

IM_API im_status IM_CALL im_audio_set_muted(uint32_t muted) {
  return im::api::Guard("im_audio_set_muted", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckBool("im_audio_set_muted", "muted", muted));
    IM_CHECK(im::engine::RequireInit("im_audio_set_muted"));
    return im::audio_bridge::Engine().SetMuted(muted != 0);
  });
}

IM_API im_status IM_CALL im_audio_levels_get(im_audio_levels *out) {
  return im::api::Guard("im_audio_levels_get", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::abi::CheckOut(out, "im_audio_levels_get: out"));
    IM_CHECK(im::engine::RequireInit("im_audio_levels_get"));
    return im::audio_bridge::Engine().LevelsGet(out);
  });
}

IM_API int32_t IM_CALL im_audio_sound_check(uint32_t helper_pid, int32_t timeout_ms, char *json, int32_t cap) {
  return im::api::Guard("im_audio_sound_check", [&]() -> int32_t {
    ClearLastError();
    if (timeout_ms < 1000 || timeout_ms > 10000) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_audio_sound_check: timeout_ms %d is outside 1000..10000",
                   static_cast<int>(timeout_ms));
    }
    IM_CHECK(im::api::CheckJsonOut("im_audio_sound_check", json, cap));
    IM_CHECK(im::engine::RequireInit("im_audio_sound_check"));
    return im::audio_bridge::Engine().SoundCheck(helper_pid, timeout_ms, json, cap);
  });
}

// -------------------------------------------------------------------------------------------------------- sessions

IM_API im_status IM_CALL im_start(const im_start_config *cfg, im_session *out, im_session_info *info) {
  return im::api::Guard("im_start", [&]() -> im_status {
    ClearLastError();
    if (out == nullptr) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_start: out is NULL");
    }
    *out = 0; // 0 is never a session: what a failed im_start leaves
    im_start_config c;
    IM_CHECK(im::abi::ReadIn(cfg, c, "im_start: cfg"));
    if (info != nullptr) {
      IM_CHECK(im::abi::CheckOut(info, "im_start: info"));
    }
    IM_CHECK(im::api::CheckStartConfig("im_start", c));
    IM_CHECK(im::engine::RequireInit("im_start"));
    auto req = std::make_shared<const im::api::StartRequest>(c);
    auto res = std::make_shared<im::session::StartResult>();
    res->info.struct_size = sizeof(im_session_info);
    const int32_t st = im::engine::TheWorker().Post(
        "im_start", im::engine::bound::kStart, [req, res]() -> int32_t { return im::session::Start(*req, *res); },
        [res](int32_t late) {
          // The caller gave up waiting (04 §6.1): nobody will stop this session, so the worker does.
          if (late == IM_OK && res->handle != 0) {
            im::session::Stop(res->handle);
          }
        });
    if (st != IM_OK) {
      return st;
    }
    *out = res->handle;
    if (info != nullptr) {
      im::abi::WriteOut(info, res->info);
    }
    return IM_OK;
  });
}

IM_API im_status IM_CALL im_get_session_info(im_session s, im_session_info *info) {
  return im::api::Guard("im_get_session_info", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::abi::CheckOut(info, "im_get_session_info: info"));
    IM_CHECK(im::engine::RequireInit("im_get_session_info"));
    auto tmp = Zeroed<im_session_info>();
    IM_CHECK(im::session::GetInfo(s, tmp));
    im::abi::WriteOut(info, tmp);
    return IM_OK;
  });
}

IM_API im_status IM_CALL im_next_packet(im_session s, uint32_t timeout_ms, im_packet *out) {
  return im::api::Guard("im_next_packet", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::abi::CheckOut(out, "im_next_packet: out"));
    if (timeout_ms > 1000) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_next_packet: timeout_ms %u is above 1000",
                   static_cast<unsigned>(timeout_ms));
    }
    IM_CHECK(im::engine::RequireInit("im_next_packet"));
    auto pkt = Zeroed<im_packet>();
    const im_status st = im::session::NextPacket(s, timeout_ms, pkt);
    if (st == IM_OK) {
      im::abi::WriteOut(out, pkt);
    }
    return st;
  });
}

IM_API im_status IM_CALL im_request_keyframe(im_session s, uint32_t layer) {
  return im::api::Guard("im_request_keyframe", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckLayerIndex("im_request_keyframe", layer, true));
    IM_CHECK(im::engine::RequireInit("im_request_keyframe"));
    return im::session::RequestKeyframe(s, layer);
  });
}

IM_API im_status IM_CALL im_set_bitrate(im_session s, uint32_t layer, uint32_t bps) {
  return im::api::Guard("im_set_bitrate", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckLayerIndex("im_set_bitrate", layer, false));
    IM_CHECK(im::api::CheckVideoBitrate("im_set_bitrate", layer, bps));
    IM_CHECK(im::engine::RequireInit("im_set_bitrate"));
    return im::session::SetBitrate(s, layer, bps);
  });
}

IM_API im_status IM_CALL im_set_layer_active(im_session s, uint32_t layer, uint32_t active) {
  return im::api::Guard("im_set_layer_active", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckLayerIndex("im_set_layer_active", layer, false));
    IM_CHECK(im::api::CheckBool("im_set_layer_active", "active", active));
    IM_CHECK(im::engine::RequireInit("im_set_layer_active"));
    return im::session::SetLayerActive(s, layer, active != 0);
  });
}

IM_API im_status IM_CALL im_update_layer(im_session s, uint32_t layer, const im_layer_config *cfg) {
  return im::api::Guard("im_update_layer", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckLayerIndex("im_update_layer", layer, false));
    if (cfg == nullptr) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_update_layer: cfg is NULL");
    }
    const im_layer_config copy = *cfg;
    IM_CHECK(im::api::CheckLayerConfig("im_update_layer", "cfg", copy));
    IM_CHECK(im::engine::RequireInit("im_update_layer"));
    return im::session::UpdateLayer(s, layer, copy);
  });
}

IM_API im_status IM_CALL im_set_profile(im_session s, uint32_t profile) {
  return im::api::Guard("im_set_profile", [&]() -> im_status {
    ClearLastError();
    if (profile != IM_PROFILE_HIGH && profile != IM_PROFILE_CB) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_set_profile: profile 0x%04X is neither IM_PROFILE_HIGH nor _CB",
                   static_cast<unsigned>(profile));
    }
    IM_CHECK(im::engine::RequireInit("im_set_profile"));
    return im::session::SetProfile(s, profile);
  });
}

IM_API im_status IM_CALL im_set_content_hint(im_session s, uint32_t hint) {
  return im::api::Guard("im_set_content_hint", [&]() -> im_status {
    ClearLastError();
    if (hint > IM_CONTENT_DETAIL) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_set_content_hint: unknown hint %u", static_cast<unsigned>(hint));
    }
    IM_CHECK(im::engine::RequireInit("im_set_content_hint"));
    return im::session::SetContentHint(s, hint);
  });
}

IM_API im_status IM_CALL im_set_paused(im_session s, uint32_t paused) {
  return im::api::Guard("im_set_paused", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::api::CheckBool("im_set_paused", "paused", paused));
    IM_CHECK(im::engine::RequireInit("im_set_paused"));
    return im::session::SetPaused(s, paused != 0);
  });
}

IM_API im_status IM_CALL im_set_audio_bitrate(im_session s, uint32_t bps) {
  return im::api::Guard("im_set_audio_bitrate", [&]() -> im_status {
    ClearLastError();
    if (bps < 32000 || bps > 510000) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_set_audio_bitrate: %u is outside 32000..510000",
                   static_cast<unsigned>(bps));
    }
    IM_CHECK(im::engine::RequireInit("im_set_audio_bitrate"));
    return im::session::SetAudioBitrate(s, bps);
  });
}

IM_API int32_t IM_CALL im_get_stats(im_session s, char *json, int32_t cap) {
  return im::api::Guard("im_get_stats", [&]() -> int32_t {
    ClearLastError();
    IM_CHECK(im::api::CheckJsonOut("im_get_stats", json, cap));
    IM_CHECK(im::engine::RequireInit("im_get_stats"));
    std::string doc;
    IM_CHECK(im::session::GetStats(s, doc));
    return im::api::JsonOut("im_get_stats", doc, json, cap);
  });
}

IM_API im_status IM_CALL im_stop(im_session s) {
  return im::api::Guard("im_stop", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::engine::RequireInit("im_stop"));
    return im::session::Stop(s);
  });
}

// ----------------------------------------------------------------------------------------------------- own windows

IM_API im_status IM_CALL im_protect_window(void *native_window, uint32_t flags) {
  return im::api::Guard("im_protect_window", [&]() -> im_status {
    ClearLastError();
    if (native_window == nullptr) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_protect_window: native_window is NULL");
    }
    IM_CHECK(im::engine::RequireInit("im_protect_window"));
    return im::window::Protect(native_window, flags);
  });
}

// ----------------------------------------------------------------------------------------------------- diagnostics

IM_API int32_t IM_CALL im_last_error(char *buf, int32_t cap) {
  // Reads, never clears: the detail belongs to the call before.
  return im::abi::LastError(buf, cap);
}

IM_API int32_t IM_CALL im_read_logs(char *buf, int32_t cap) {
  return im::api::Guard("im_read_logs", [&]() -> int32_t {
    ClearLastError();
    if (buf == nullptr || cap <= im::log::kMaxLineBytes) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_read_logs: needs a buffer and cap above %d (got %d)",
                   static_cast<int>(im::log::kMaxLineBytes), static_cast<int>(cap));
    }
    return im::log::Default().Drain(buf, cap);
  });
}

IM_API int32_t IM_CALL im_abi_layout(char *json, int32_t cap) {
  return im::api::Guard("im_abi_layout", [&]() -> int32_t {
    ClearLastError();
    IM_CHECK(im::api::CheckJsonOut("im_abi_layout", json, cap));
    return im::api::JsonOut("im_abi_layout", im::abi::LayoutJson(), json, cap);
  });
}

// -------------------------------------------------------------------------------------------- test exports (05)

IM_API im_status IM_CALL im_test_render_tone(const char *endpoint_id, float hz, float amp, int32_t ms) {
  return im::api::Guard("im_test_render_tone", [&]() -> im_status {
    ClearLastError();
    IM_CHECK(im::engine::RequireInit("im_test_render_tone"));
    return im::audio::TestRenderTone(endpoint_id, hz, amp, ms);
  });
}

IM_API int32_t IM_CALL im_test_render_bursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms,
                                             int32_t period_ms, int32_t count, int64_t *onset_ns, int32_t cap) {
  return im::api::Guard("im_test_render_bursts", [&]() -> int32_t {
    ClearLastError();
    if (cap < 0 || (cap > 0 && onset_ns == nullptr)) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_test_render_bursts: onset_ns is NULL or cap %d is negative",
                   static_cast<int>(cap));
    }
    IM_CHECK(im::engine::RequireInit("im_test_render_bursts"));
    return im::audio::TestRenderBursts(endpoint_id, hz, amp, burst_ms, period_ms, count, onset_ns, cap);
  });
}

IM_API int32_t IM_CALL im_test_opus_decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames) {
  return im::api::Guard("im_test_opus_decode", [&]() -> int32_t {
    ClearLastError();
    if (pkt == nullptr || len <= 0) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_test_opus_decode: no packet (pkt NULL or len %d)",
                   static_cast<int>(len));
    }
    if (pcm == nullptr || max_frames <= 0) {
      return Failf(IM_ERR_INVALID_ARG, {}, "im_test_opus_decode: no output (pcm NULL or max_frames %d)",
                   static_cast<int>(max_frames));
    }
    return im::audio_bridge::TestOpusDecode(pkt, len, pcm, max_frames);
  });
}

} // extern "C"
