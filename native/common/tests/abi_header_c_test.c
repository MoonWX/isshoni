/* isshoni_media.h compiled as C11 (docs/m2/04-windows-engine.md §21 E1): the header has to work for C callers (the
 * M3 and M4 engines through cgo) with every warning an error. On top of the build's flags it is included with stricter
 * warnings, -Wpadded (MSVC: C4820) among them: no ABI struct may have implicit padding, which is where a C struct and
 * its Go mirror would silently disagree. The sizes and offsets are 04 §5.4's. */
#include <stddef.h>
#include <stdio.h>

#if defined(__clang__) || defined(__GNUC__)
#pragma GCC diagnostic push
#pragma GCC diagnostic error "-Wpadded"
#pragma GCC diagnostic error "-Wconversion"
#pragma GCC diagnostic error "-Wsign-conversion"
#pragma GCC diagnostic error "-Wcast-qual"
#pragma GCC diagnostic error "-Wstrict-prototypes"
#pragma GCC diagnostic error "-Wundef"
#elif defined(_MSC_VER)
#pragma warning(push)
#pragma warning(1 : 4820) /* off by default even at /W4 */
#pragma warning(error : 4820)
#endif

#include "isshoni_media.h"

#if defined(__clang__) || defined(__GNUC__)
#pragma GCC diagnostic pop
#elif defined(_MSC_VER)
#pragma warning(pop)
#endif

#define IM_SIZE(T, n)  _Static_assert(sizeof(T) == (n), #T " has the size of 04 §5.4")
#define IM_AT(T, f, n) _Static_assert(offsetof(T, f) == (n), #T "." #f " has the offset of 04 §5.4")

_Static_assert(sizeof(void *) == 8, "the v1 layouts are for 64-bit targets (amd64, arm64)");
_Static_assert(IM_ABI_VERSION == 0x00010000u, "v1.0");
_Static_assert(IM_ABI_MINOR == 0u, "M2 ships v1.0");

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

/* The declarations link only against an engine; taking the address of one in an unevaluated sizeof checks that the
 * prototypes are complete C prototypes without needing one. */
_Static_assert(sizeof(&im_next_packet) == sizeof(void (*)(void)), "function pointers");

int main(void) {
  /* A C caller fills a start config the way Go does: struct_size first, fixed-width fields only. */
  im_start_config cfg = {0};
  cfg.struct_size = (uint32_t)sizeof cfg;
  cfg.layer_count = 1;
  cfg.layers[0].rid[0] = 'f';
  cfg.layers[0].fps = 60;
  cfg.audio.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  if (cfg.struct_size != 224u || IM_NO_LAYER != 0xFFFFFFFFu || IM_ERR_INTERNAL != -99) {
    fprintf(stderr, "unexpected values\n");
    return 1;
  }
  return 0;
}
