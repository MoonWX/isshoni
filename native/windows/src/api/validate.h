// The argument checks of the exports (docs/m2/04-windows-engine.md §5.1, §5.2; the audio rules 05 §4.3). They check
// what the header itself documents (pointers, struct_size, the defined constants, the ranges written next to a field)
// and leave the rest to the area behind the export. A failure returns its status with the thread's detail set: "<fn>:
// <what is wrong>", with the 04 §5.7 token where one applies (abi). Nothing is applied after a failed check.
#pragma once

#include <cstdint>
#include <optional>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::api {

// A JSON output's buffer (04 §5.1): cap >= 0, and a buffer when cap > 0.
im_status CheckJsonOut(const char *fn, const char *buf, int32_t cap) noexcept;

// Copies doc to buf with the snprintf rule (04 §5.1) after CheckJsonOut passed; returns doc's full length.
int32_t JsonOut(const char *fn, std::string_view doc, char *buf, int32_t cap) noexcept;

// An optional absolute path argument (im_init_params.ffmpeg_dir, .openh264_path, im_set_openh264_path): NULL or ""
// gives nothing; otherwise valid UTF-8, shorter than 32 767 bytes and absolute (a drive letter or a UNC path).
im_status ReadAbsolutePath(const char *fn, const char *what, const char *path, std::optional<std::string> &out);

// Whether a Windows path is absolute: "C:\..." or "C:/...", or "\\server\share..." (also "\\?\...").
bool IsAbsoluteWindowsPath(std::string_view path) noexcept;

// An im_exclusions argument (05 §4.3): non-NULL, struct_size at least the v1.0 size (else IM_ERR_ABI), rule_count <=
// IM_MAX_RULES with a rules array, and every rule valid: a known kind and source, a non-empty UTF-8 value shorter than
// IM_MAX_RULE_VALUE, a decimal pid for IM_RULE_INSTANCE, an app_id that is NULL, "" or [a-z0-9:._-]{1,63}, and a
// UTF-8 name. The detail names the rule ("rules[3].app_id"). Unknown flag bits are ignored.
im_status CheckExclusions(const char *fn, const char *what, const im_exclusions *ex) noexcept;

// im_audio_config (04 §5.2, 05 §4.2): a mode of the header (IM_AUDIO_MODE_EVERYTHING is reserved and refused in M2,
// README §3 item 15), bitrate_bps 0 or 32 000..510 000, frame_ms 0 or 20.
im_status CheckAudioConfig(const char *fn, const im_audio_config &audio) noexcept;

// im_layer_config: a NUL-terminated, non-empty rid within its 4 bytes, fps 1..240, gop_ms 0 or 1000..10000.
im_status CheckLayerConfig(const char *fn, const char *what, const im_layer_config &layer) noexcept;

// im_start_config after its struct_size check: layer_count <= IM_MAX_LAYERS; a source exactly when there are layers
// (no source and no layers is an audio-only session, 04 §10.3); a source id shorter than 64 bytes; a known profile
// (with layers), content hint and encoder (IM_ENC_FAKE only in IM_TEST_HOOKS builds); valid, distinct layers; the
// audio config; the exclusions when given.
im_status CheckStartConfig(const char *fn, const im_start_config &cfg) noexcept;

// A video layer index (< IM_MAX_LAYERS), or also IM_ALL_LAYERS when all_ok.
im_status CheckLayerIndex(const char *fn, uint32_t layer, bool all_ok) noexcept;

// A uint32_t boolean argument: 0 or 1 (04 §5.1).
im_status CheckBool(const char *fn, const char *what, uint32_t value) noexcept;

} // namespace im::api
