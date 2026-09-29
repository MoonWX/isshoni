/*
 * isshoni media engine C ABI: DRAFT v0, audio subset (spike S1, Windows).
 *
 * Captures "all system audio except excluded apps" as 48 kHz stereo float,
 * delivered in 10 ms chunks. In M2 this is promoted into
 * native/include/isshoni_media.h (which adds video and Opus output).
 *
 * Threading: all functions may be called from any thread, concurrently
 * (im_audio_stop during a blocked im_audio_read is safe). The engine never
 * calls back into the host; the host pulls audio with im_audio_read().
 * Strings are UTF-8. JSON outputs are NUL-terminated; functions returning JSON
 * return its length, or IM_ERR_BUFFER_TOO_SMALL if cap is too small.
 */
#ifndef ISSHONI_AUDIO_H
#define ISSHONI_AUDIO_H

#include <stdint.h>

#if defined(_WIN32)
#  if defined(IM_BUILDING)
#    define IM_API __declspec(dllexport)
#  else
#    define IM_API __declspec(dllimport)
#  endif
#else
#  define IM_API
#endif

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
  IM_AUDIO_MODE_INCLUDE_SET = 0, /* one INCLUDE stream per allowed app, mixed (default) */
  IM_AUDIO_MODE_EXCLUDE_ONE = 1, /* one EXCLUDE stream on a single excluded app tree (comparison only) */
  IM_AUDIO_MODE_ENDPOINT = 2     /* classic loopback of the default output; no exclusion (fallback/baseline) */
};

enum im_rule_kind {
  IM_RULE_APP = 0,     /* executable name, e.g. "discord.exe"; matches the audio process or an
                          ancestor, stopping at launchers/shells (explorer, steam, cmd, …) */
  IM_RULE_INSTANCE = 1 /* process id (decimal string); matches it and every descendant */
};

#define IM_FLAG_EXCLUDE_MIC_USERS 0x1u /* also exclude apps that have an open microphone session */

#define IM_AUDIO_SAMPLE_RATE 48000
#define IM_AUDIO_CHANNELS 2
#define IM_AUDIO_CHUNK_FRAMES 480 /* 10 ms */

IM_API int32_t im_version(void);

/* {"os_build":22631,"process_loopback":true,"error":""} */
IM_API int32_t im_audio_probe(char *json, int32_t cap);

/* Rules apply to the running engine at its next poll (~0.5 s). */
IM_API int32_t im_audio_clear_rules(void);
IM_API int32_t im_audio_add_rule(int32_t kind, const char *value);

/* One-shot snapshot of audio sessions and how the current rules classify them. */
IM_API int32_t im_audio_list_apps(uint32_t flags, char *json, int32_t cap);

IM_API int32_t im_audio_start(int32_t mode, uint32_t flags);

/* Blocks up to timeout_ms for the next 10 ms chunk. Writes IM_AUDIO_CHUNK_FRAMES
 * interleaved stereo frames (max_frames must be >= that) and the chunk's
 * capture time in QPC nanoseconds. Returns frames written or an im_status. */
IM_API int32_t im_audio_read(float *out, int32_t max_frames, int64_t *capture_ns, int32_t timeout_ms);

/* "What friends hear": live streams, excluded sessions with reasons, warnings. */
IM_API int32_t im_audio_status(char *json, int32_t cap);

IM_API void im_audio_stop(void);

/* Last error message for the calling thread (UTF-8). */
IM_API int32_t im_last_error(char *buf, int32_t cap);

#ifdef __cplusplus
}
#endif

#endif /* ISSHONI_AUDIO_H */
