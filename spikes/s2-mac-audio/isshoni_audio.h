/*
 * isshoni media engine C ABI: DRAFT v0, audio subset (spike S2, macOS).
 *
 * Same ABI as spike S1 (Windows, spikes/s1-win-audio/native/isshoni_audio.h);
 * only the platform notes differ. Captures "all system audio except excluded
 * apps" as 48 kHz stereo float, delivered in 10 ms chunks. In M2/M3 this is
 * promoted into native/include/isshoni_media.h (which adds video and Opus).
 *
 * Threading: all functions may be called from any thread, concurrently
 * (im_audio_stop during a blocked im_audio_read is safe); im_audio_read has a
 * single consumer. The engine never calls back into the host; the host pulls
 * audio with im_audio_read(). Strings are UTF-8. JSON outputs are
 * NUL-terminated; functions returning JSON return its length, or
 * IM_ERR_BUFFER_TOO_SMALL if cap is too small.
 */
#ifndef ISSHONI_AUDIO_H
#define ISSHONI_AUDIO_H

#include <stdint.h>

#define IM_API

#ifdef __cplusplus
extern "C" {
#endif

#define IM_ABI_VERSION 0x000001

enum im_status {
  IM_OK = 0,
  IM_ERR_INVALID_ARG = -1,
  IM_ERR_NOT_STARTED = -2,
  IM_ERR_ALREADY_STARTED = -3,
  IM_ERR_UNSUPPORTED_OS = -4,
  IM_ERR_PERMISSION_DENIED = -5,
  IM_ERR_AUDIO_DEVICE_CHANGED = -6,
  IM_ERR_TIMEOUT = -7,
  IM_ERR_BUFFER_TOO_SMALL = -8,
  IM_ERR_INTERNAL = -99
};

enum im_audio_mode {
  /* All system audio except excluded apps (default). Windows: include-set
   * mixer. macOS: one global Core Audio process tap with an exclude list. */
  IM_AUDIO_MODE_INCLUDE_SET = 0,
  IM_AUDIO_MODE_EXCLUDE_ONE = 1, /* Windows only (comparison); macOS returns IM_ERR_INVALID_ARG */
  IM_AUDIO_MODE_ENDPOINT = 2     /* everything, no exclusion (baseline/fallback) */
};

enum im_rule_kind {
  /* Windows: executable name ("discord.exe"), matched on the process or an
   *   ancestor up to a launcher.
   * macOS: bundle ID ("com.hnc.Discord"), matched at a dot boundary against
   *   the process's bundle ID, every .app bundle enclosing its executable, and
   *   its responsible app (so Electron/Chromium helpers and XPC services match). */
  IM_RULE_APP = 0,
  /* Process id (decimal string); matches it, every descendant, and (macOS)
   * every process it is responsible for. */
  IM_RULE_INSTANCE = 1
};

#define IM_FLAG_EXCLUDE_MIC_USERS 0x1u /* also exclude apps that have an open microphone */
/* Spike-only switches (macOS), for A/B measurements: */
#define IM_FLAG_NO_BUNDLE_IDS 0x100u   /* macOS 26+: don't also exclude by CATapDescription.bundleIDs */
#define IM_FLAG_AGG_WITH_OUTPUT 0x200u /* aggregate device also contains the output device (clock) */
#define IM_FLAG_NO_CROSSFADE 0x400u    /* change exclusions of audible apps in place (hard cut) */

#define IM_AUDIO_SAMPLE_RATE 48000
#define IM_AUDIO_CHANNELS 2
#define IM_AUDIO_CHUNK_FRAMES 480 /* 10 ms */

IM_API int32_t im_version(void);

/* {"os":"27.0","process_tap":true,"bundle_ids":true,"device":"…","device_rate":48000,"error":""} */
IM_API int32_t im_audio_probe(char *json, int32_t cap);

/* Rules apply to the running engine within ~0.25 s. */
IM_API int32_t im_audio_clear_rules(void);
IM_API int32_t im_audio_add_rule(int32_t kind, const char *value);

/* One-shot snapshot of audio processes and how the current rules classify them. */
IM_API int32_t im_audio_list_apps(uint32_t flags, char *json, int32_t cap);

IM_API int32_t im_audio_start(int32_t mode, uint32_t flags);

/* Blocks up to timeout_ms for the next 10 ms chunk. Writes IM_AUDIO_CHUNK_FRAMES
 * interleaved stereo frames (max_frames must be >= that) and the chunk's
 * capture time in host-clock nanoseconds (mach_absolute_time scaled). Returns
 * frames written or an im_status. */
IM_API int32_t im_audio_read(float *out, int32_t max_frames, int64_t *capture_ns, int32_t timeout_ms);

/* "What friends hear": audio apps with their classification, events, warnings. */
IM_API int32_t im_audio_status(char *json, int32_t cap);

IM_API void im_audio_stop(void);

/* Last error message for the calling thread (UTF-8). */
IM_API int32_t im_last_error(char *buf, int32_t cap);

/* Host clock in the same nanoseconds as capture_ns. */
IM_API int64_t im_now_ns(void);

#ifdef __cplusplus
}
#endif

#endif /* ISSHONI_AUDIO_H */
