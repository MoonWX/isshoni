#include "im/abi/layout.h"

#include <cstddef>
#include <type_traits>

#include "im/json/writer.h"

namespace im::abi {
namespace {

#define IM_FIELD(S, f)                                                                                                 \
  FieldLayout { #f, offsetof(S, f) }

// The v1.0 structs must stay standard-layout C structs: that is what makes offsetof and the Go mirror valid.
template <class... T> constexpr bool kAllStandardLayout = (std::is_standard_layout_v<T> && ...);
static_assert(
    kAllStandardLayout<im_init_params, im_clock_sample, im_adapter_info, im_encoder_info, im_caps, im_pick_params,
                       im_source_info, im_rule, im_exclusions, im_audio_config, im_audio_level, im_audio_levels,
                       im_layer_config, im_start_config, im_layer_info, im_session_info, im_packet>);

const FieldLayout kInitParams[] = {
    IM_FIELD(im_init_params, struct_size), IM_FIELD(im_init_params, abi_version),
    IM_FIELD(im_init_params, flags),       IM_FIELD(im_init_params, log_level),
    IM_FIELD(im_init_params, ffmpeg_dir),  IM_FIELD(im_init_params, openh264_path),
};

const FieldLayout kClockSample[] = {
    IM_FIELD(im_clock_sample, struct_size),    IM_FIELD(im_clock_sample, reserved),
    IM_FIELD(im_clock_sample, mono_ns),        IM_FIELD(im_clock_sample, unix_ns),
    IM_FIELD(im_clock_sample, uncertainty_ns),
};

const FieldLayout kAdapterInfo[] = {
    IM_FIELD(im_adapter_info, luid),      IM_FIELD(im_adapter_info, vendor_id),
    IM_FIELD(im_adapter_info, device_id), IM_FIELD(im_adapter_info, driver_version),
    IM_FIELD(im_adapter_info, name),      IM_FIELD(im_adapter_info, flags),
    IM_FIELD(im_adapter_info, reserved),
};

const FieldLayout kEncoderInfo[] = {
    IM_FIELD(im_encoder_info, kind),           IM_FIELD(im_encoder_info, adapter_index),
    IM_FIELD(im_encoder_info, flags),          IM_FIELD(im_encoder_info, status),
    IM_FIELD(im_encoder_info, max_width),      IM_FIELD(im_encoder_info, max_height),
    IM_FIELD(im_encoder_info, test_encode_us), IM_FIELD(im_encoder_info, reserved),
    IM_FIELD(im_encoder_info, codec),          IM_FIELD(im_encoder_info, reason),
};

const FieldLayout kCaps[] = {
    IM_FIELD(im_caps, struct_size), IM_FIELD(im_caps, os_build),      IM_FIELD(im_caps, os_ubr),
    IM_FIELD(im_caps, os_flags),    IM_FIELD(im_caps, audio_flags),   IM_FIELD(im_caps, adapter_count),
    IM_FIELD(im_caps, adapters),    IM_FIELD(im_caps, encoder_count), IM_FIELD(im_caps, reserved0),
    IM_FIELD(im_caps, encoders),    IM_FIELD(im_caps, probe_ms),      IM_FIELD(im_caps, reserved),
};

const FieldLayout kPickParams[] = {
    IM_FIELD(im_pick_params, struct_size), IM_FIELD(im_pick_params, flags),    IM_FIELD(im_pick_params, parent_window),
    IM_FIELD(im_pick_params, timeout_ms),  IM_FIELD(im_pick_params, reserved),
};

const FieldLayout kSourceInfo[] = {
    IM_FIELD(im_source_info, struct_size), IM_FIELD(im_source_info, kind),         IM_FIELD(im_source_info, id),
    IM_FIELD(im_source_info, name),        IM_FIELD(im_source_info, monitor_id),   IM_FIELD(im_source_info, width),
    IM_FIELD(im_source_info, height),      IM_FIELD(im_source_info, capture_path), IM_FIELD(im_source_info, flags),
    IM_FIELD(im_source_info, pid),         IM_FIELD(im_source_info, reserved0),    IM_FIELD(im_source_info, window),
    IM_FIELD(im_source_info, reserved),
};

const FieldLayout kRule[] = {
    IM_FIELD(im_rule, kind),   IM_FIELD(im_rule, source), IM_FIELD(im_rule, value),
    IM_FIELD(im_rule, app_id), IM_FIELD(im_rule, name),
};

const FieldLayout kExclusions[] = {
    IM_FIELD(im_exclusions, struct_size), IM_FIELD(im_exclusions, flags),      IM_FIELD(im_exclusions, rules),
    IM_FIELD(im_exclusions, rule_count),  IM_FIELD(im_exclusions, shared_pid),
};

const FieldLayout kAudioConfig[] = {
    IM_FIELD(im_audio_config, mode),  IM_FIELD(im_audio_config, bitrate_bps), IM_FIELD(im_audio_config, frame_ms),
    IM_FIELD(im_audio_config, flags), IM_FIELD(im_audio_config, reserved),
};

const FieldLayout kAudioLevel[] = {
    IM_FIELD(im_audio_level, app_id), IM_FIELD(im_audio_level, peak_db),  IM_FIELD(im_audio_level, rms_db),
    IM_FIELD(im_audio_level, source), IM_FIELD(im_audio_level, reserved),
};

const FieldLayout kAudioLevels[] = {
    IM_FIELD(im_audio_levels, struct_size), IM_FIELD(im_audio_levels, report_seq), IM_FIELD(im_audio_levels, qpc_ns),
    IM_FIELD(im_audio_levels, mix_peak_db), IM_FIELD(im_audio_levels, mix_rms_db), IM_FIELD(im_audio_levels, count),
    IM_FIELD(im_audio_levels, reserved),    IM_FIELD(im_audio_levels, apps),
};

const FieldLayout kLayerConfig[] = {
    IM_FIELD(im_layer_config, rid),        IM_FIELD(im_layer_config, width), IM_FIELD(im_layer_config, height),
    IM_FIELD(im_layer_config, max_pixels), IM_FIELD(im_layer_config, fps),   IM_FIELD(im_layer_config, bitrate_bps),
    IM_FIELD(im_layer_config, gop_ms),     IM_FIELD(im_layer_config, flags), IM_FIELD(im_layer_config, reserved),
};

const FieldLayout kStartConfig[] = {
    IM_FIELD(im_start_config, struct_size),  IM_FIELD(im_start_config, flags),
    IM_FIELD(im_start_config, source_id),    IM_FIELD(im_start_config, profile),
    IM_FIELD(im_start_config, content_hint), IM_FIELD(im_start_config, encoder),
    IM_FIELD(im_start_config, layer_count),  IM_FIELD(im_start_config, layers),
    IM_FIELD(im_start_config, audio),        IM_FIELD(im_start_config, exclusions),
    IM_FIELD(im_start_config, reserved),
};

const FieldLayout kLayerInfo[] = {
    IM_FIELD(im_layer_info, rid),     IM_FIELD(im_layer_info, encoder), IM_FIELD(im_layer_info, width),
    IM_FIELD(im_layer_info, height),  IM_FIELD(im_layer_info, fps),     IM_FIELD(im_layer_info, bitrate_bps),
    IM_FIELD(im_layer_info, profile), IM_FIELD(im_layer_info, flags),   IM_FIELD(im_layer_info, reserved),
};

const FieldLayout kSessionInfo[] = {
    IM_FIELD(im_session_info, struct_size),  IM_FIELD(im_session_info, flags),
    IM_FIELD(im_session_info, capture_path), IM_FIELD(im_session_info, layer_count),
    IM_FIELD(im_session_info, epoch_ns),     IM_FIELD(im_session_info, audio_state),
    IM_FIELD(im_session_info, reserved0),    IM_FIELD(im_session_info, layers),
    IM_FIELD(im_session_info, reserved),
};

const FieldLayout kPacket[] = {
    IM_FIELD(im_packet, struct_size), IM_FIELD(im_packet, kind),          IM_FIELD(im_packet, layer),
    IM_FIELD(im_packet, flags),       IM_FIELD(im_packet, capture_ts_ns), IM_FIELD(im_packet, data),
    IM_FIELD(im_packet, size),        IM_FIELD(im_packet, duration_us),   IM_FIELD(im_packet, width),
    IM_FIELD(im_packet, height),      IM_FIELD(im_packet, profile),       IM_FIELD(im_packet, event),
    IM_FIELD(im_packet, status),      IM_FIELD(im_packet, reserved),
};

#undef IM_FIELD

#define IM_STRUCT(S, fields)                                                                                           \
  StructLayout { #S, sizeof(S), fields }

const StructLayout kStructs[] = {
    IM_STRUCT(im_init_params, kInitParams),
    IM_STRUCT(im_clock_sample, kClockSample),
    IM_STRUCT(im_adapter_info, kAdapterInfo),
    IM_STRUCT(im_encoder_info, kEncoderInfo),
    IM_STRUCT(im_caps, kCaps),
    IM_STRUCT(im_pick_params, kPickParams),
    IM_STRUCT(im_source_info, kSourceInfo),
    IM_STRUCT(im_rule, kRule),
    IM_STRUCT(im_exclusions, kExclusions),
    IM_STRUCT(im_audio_config, kAudioConfig),
    IM_STRUCT(im_audio_level, kAudioLevel),
    IM_STRUCT(im_audio_levels, kAudioLevels),
    IM_STRUCT(im_layer_config, kLayerConfig),
    IM_STRUCT(im_start_config, kStartConfig),
    IM_STRUCT(im_layer_info, kLayerInfo),
    IM_STRUCT(im_session_info, kSessionInfo),
    IM_STRUCT(im_packet, kPacket),
};

#undef IM_STRUCT

} // namespace

std::span<const StructLayout> Layouts() noexcept { return kStructs; }

std::string LayoutJson() {
  json::Writer w;
  w.BeginObject();
  w.Key("abi").Uint(IM_ABI_VERSION);
  w.Key("structs").BeginObject();
  for (const StructLayout &s : kStructs) {
    w.Key(s.name).BeginObject();
    w.Key("size").Uint(s.size);
    w.Key("fields").BeginObject();
    for (const FieldLayout &f : s.fields) {
      w.Key(f.name).Uint(f.offset);
    }
    w.EndObject();
    w.EndObject();
  }
  w.EndObject();
  w.EndObject();
  return w.Take();
}

} // namespace im::abi
