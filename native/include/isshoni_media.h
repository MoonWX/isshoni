/*
 * isshoni_media.h: the native media engine C ABI, v1.
 * Spec: docs/m2/04-windows-engine.md (audio block: docs/m2/05-windows-audio.md §4). Implemented by the Windows DLL
 * isshoni-media.dll (M2), the macOS engine (M3, static) and the Linux shim (M4, static).
 * SPDX-License-Identifier: Apache-2.0
 *
 * Conventions (04 §5.1): plain C, fixed-width integers, booleans are uint32_t 0/1, enumerations are #defined
 * constants on uint32_t fields. Every top-level struct starts with struct_size (the caller sets it to the sizeof it
 * was built with; below the v1.0 size is IM_ERR_ABI); structs embedded by value or used as array elements have no
 * struct_size and are frozen in v1. Strings are UTF-8 and NUL-terminated. JSON outputs follow snprintf: they return
 * the full length n and write min(n, cap - 1) bytes and a NUL when cap > 0. The engine copies all input before a call
 * returns. After a negative status, im_last_error on the same OS thread returns "<token>: detail" for logs.
 * A minor ABI bump only appends fields, constants or functions (04 §5.5).
 */
#ifndef ISSHONI_MEDIA_H
#define ISSHONI_MEDIA_H

#include <stdint.h>

#if defined(_WIN32)
#if defined(IM_BUILDING_DLL)
#define IM_API __declspec(dllexport)
#elif defined(IM_STATIC)
#define IM_API
#else
#define IM_API __declspec(dllimport)
#endif
#define IM_CALL __cdecl
#else
#define IM_API __attribute__((visibility("default")))
#define IM_CALL
#endif

#ifdef __cplusplus
extern "C" {
#endif

/* ---------------------------------------------------------------- version */
#define IM_ABI_MAJOR   1u
#define IM_ABI_MINOR   0u
#define IM_ABI_VERSION ((IM_ABI_MAJOR << 16) | IM_ABI_MINOR)

/* ---------------------------------------------------------------- status */
typedef int32_t im_status;
#define IM_OK                       0
#define IM_TIMEOUT                  1 /* im_next_packet: nothing within timeout_ms */
#define IM_ENDED                    2 /* im_next_packet: the session ended (its last event said why) */
#define IM_ERR_INVALID_ARG          -1
#define IM_ERR_BAD_STATE            -2 /* not initialized, stopped session, call in the wrong state */
#define IM_ERR_ABI                  -3 /* struct_size too small, ABI major mismatch */
#define IM_ERR_UNSUPPORTED_OS       -4
#define IM_ERR_PERMISSION_DENIED    -5
#define IM_ERR_AUDIO_DEVICE_CHANGED -6 /* never returned by the Windows engine in M2 (S1 value kept) */
#define IM_ERR_SOURCE_GONE          -7 /* also a consumed, expired or evicted pick token (token pick_expired) */
#define IM_ERR_ENCODER_LOST         -8
#define IM_ERR_NO_ENCODER           -9
#define IM_ERR_CANCELLED            -10 /* the user closed the OS picker, or the pick timed out */
#define IM_ERR_LIMIT                -11 /* too many sessions or picks */
#define IM_ERR_DEPENDENCY           -12 /* FFmpeg or OpenH264 missing, wrong version or blocked */
#define IM_ERR_AUDIO_UNAVAILABLE    -13 /* never returned in M2 on Windows (audio never fails a call, 05) */
#define IM_ERR_NOT_IMPLEMENTED      -14
#define IM_ERR_TIMEOUT              -15 /* an engine thread didn't answer in time (04 §6.1) */
#define IM_ERR_INTERNAL             -99

/* ---------------------------------------------------------------- init, build info, clock */
#define IM_LOG_ERROR 0u
#define IM_LOG_WARN  1u
#define IM_LOG_INFO  2u
#define IM_LOG_DEBUG 3u

#define IM_INIT_NO_AUDIO 0x1u /* never touch audio APIs (video-only tools) */

typedef struct im_init_params {
  uint32_t struct_size;
  uint32_t abi_version;      /* IM_ABI_VERSION the caller was built with */
  uint32_t flags;            /* IM_INIT_* */
  uint32_t log_level;        /* IM_LOG_* */
  const char *ffmpeg_dir;    /* absolute dir of the FFmpeg DLLs; NULL = the engine DLL's own directory */
  const char *openh264_path; /* absolute path of a verified OpenH264 library; NULL = none */
} im_init_params;

IM_API uint32_t IM_CALL im_version(void); /* the DLL's IM_ABI_VERSION; callable before im_init */
IM_API im_status IM_CALL im_init(const im_init_params *params);
IM_API void IM_CALL im_shutdown(void); /* stops every session; im_init may follow */
IM_API int32_t IM_CALL im_build_info(char *json, int32_t cap);
IM_API im_status IM_CALL im_set_openh264_path(const char *path_or_null);

typedef struct im_clock_sample {
  uint32_t struct_size;
  uint32_t reserved;
  int64_t mono_ns;        /* engine clock: QPC ns (04 §12.3), mach_absolute_time (macOS), CLOCK_MONOTONIC (Linux) */
  int64_t unix_ns;        /* wall clock at the same instant */
  int64_t uncertainty_ns; /* half the bracket around the wall-clock read */
} im_clock_sample;

IM_API im_status IM_CALL im_clock(im_clock_sample *out);

/* ---------------------------------------------------------------- probe */
#define IM_MAX_ADAPTERS 4u
#define IM_MAX_ENCODERS 8u

#define IM_ENC_AUTO         0u
#define IM_ENC_NVENC        1u   /* token "nvenc" (04 §5.6) */
#define IM_ENC_QSV          2u   /* "qsv" */
#define IM_ENC_AMF          3u   /* "amf" */
#define IM_ENC_MF_HW        4u   /* "mf-hw" */
#define IM_ENC_MF_SW        5u   /* "mf-sw" */
#define IM_ENC_OPENH264     6u   /* "openh264" */
#define IM_ENC_VIDEOTOOLBOX 7u   /* "videotoolbox", later (M3) */
#define IM_ENC_GSTREAMER    8u   /* "gstreamer", later (M4) */
#define IM_ENC_FAKE         100u /* "fake", IM_TEST_HOOKS builds only (04 §18.2) */

#define IM_ENCF_AVAILABLE    0x01u /* opened and passed the one-frame test encode */
#define IM_ENCF_HARDWARE     0x02u
#define IM_ENCF_LIVE_BITRATE 0x04u /* bitrate changes apply without a re-init */
#define IM_ENCF_ZERO_COPY    0x08u /* takes D3D11 textures (no CPU readback) */
#define IM_ENCF_HIGH         0x10u /* can encode High (6400) */
#define IM_ENCF_CB           0x20u /* can encode Constrained Baseline (42e0) */
#define IM_ENCF_FORCED_IDR   0x40u /* honours a per-frame IDR request */

#define IM_ADAPTER_SOFTWARE    0x1u /* WARP / Microsoft Basic Render Driver */
#define IM_ADAPTER_HAS_OUTPUTS 0x2u

#define IM_OS_SUPPORTED             0x00000001u /* meets isshoni's minimum (Win10 22H2 = build 19045) */
#define IM_OS_WGC                   0x00000002u /* GraphicsCaptureSession::IsSupported() */
#define IM_OS_WGC_PICKER            0x00000004u
#define IM_OS_WGC_BORDERLESS        0x00000008u /* IsBorderRequired present and access Allowed */
#define IM_OS_WGC_MIN_INTERVAL      0x00000010u /* MinUpdateInterval present (24H2+): > 60 fps possible */
#define IM_OS_WGC_SECONDARY_WINDOWS 0x00000020u /* IncludeSecondaryWindows present (24H2+) */
#define IM_OS_WGC_CURSOR_TOGGLE     0x00000040u /* IsCursorCaptureEnabled present (19041+) */
#define IM_OS_DESKTOP_DUPLICATION   0x00000080u /* DuplicateOutput1 works on at least one output */
#define IM_OS_EXCLUDE_FROM_CAPTURE  0x00000100u /* WDA_EXCLUDEFROMCAPTURE (19041+) */
#define IM_OS_MEDIA_FOUNDATION      0x00000200u /* MFStartup works (not on N editions without the Media Feature Pack) */
#define IM_OS_HDR_DISPLAY           0x00000400u /* at least one output has advanced color on */

/* audio_flags (probe flags; their names stay IM_AUDIO_*): semantics in docs/m2/05-windows-audio.md §11.1 */
#define IM_AUDIO_PROCESS_LOOPBACK  0x1u
#define IM_AUDIO_ENDPOINT_LOOPBACK 0x2u
#define IM_AUDIO_SESSION_EVENTS    0x4u
#define IM_AUDIO_MIC_DETECTION     0x8u

typedef struct im_adapter_info {
  uint64_t luid;      /* (HighPart << 32) | LowPart */
  uint32_t vendor_id; /* PCI vendor: 0x10DE NVIDIA, 0x8086 Intel, 0x1002 AMD, 0x1414 Microsoft */
  uint32_t device_id;
  uint64_t driver_version; /* UMD version, four 16-bit parts */
  char name[128];
  uint32_t flags; /* IM_ADAPTER_* */
  uint32_t reserved;
} im_adapter_info;

typedef struct im_encoder_info {
  uint32_t kind;                  /* IM_ENC_*; its token names the encoder everywhere (04 §5.6) */
  uint32_t adapter_index;         /* into im_caps.adapters; 0xFFFFFFFF for software encoders */
  uint32_t flags;                 /* IM_ENCF_* */
  im_status status;               /* IM_OK, or why it is unavailable */
  uint32_t max_width, max_height; /* 0 = unknown */
  uint32_t test_encode_us;        /* open + one 1080p IDR + close, wall time */
  uint32_t reserved[3];
  char codec[32];  /* diagnostic: the FFmpeg codec name ("h264_nvenc") or "openh264" */
  char reason[32]; /* the 04 §5.7 token when status != IM_OK ("driver_too_old"); "" when IM_OK */
} im_encoder_info;

typedef struct im_caps {
  uint32_t struct_size;
  uint32_t os_build;    /* e.g. 19045, 22631, 26100, 26200 */
  uint32_t os_ubr;      /* update build revision */
  uint32_t os_flags;    /* IM_OS_* */
  uint32_t audio_flags; /* IM_AUDIO_* probe flags */
  uint32_t adapter_count;
  im_adapter_info adapters[IM_MAX_ADAPTERS];
  uint32_t encoder_count;
  uint32_t reserved0;
  im_encoder_info encoders[IM_MAX_ENCODERS]; /* in the engine's fallback order */
  uint32_t probe_ms;
  uint32_t reserved[15];
} im_caps;

#define IM_PROBE_QUICK 0x0u /* OS flags and which encoders load; no test encodes (< 100 ms) */
#define IM_PROBE_FULL  0x1u /* plus a one-frame test encode per encoder (< 2 s) */

IM_API im_status IM_CALL im_probe(uint32_t flags, im_caps *caps);

/* ---------------------------------------------------------------- sources */
#define IM_SOURCE_MONITOR      1u
#define IM_SOURCE_WINDOW       2u
#define IM_SOURCE_TEST_PATTERN 3u

#define IM_CAPTURE_WGC    1u
#define IM_CAPTURE_DD     2u
#define IM_CAPTURE_TEST   3u
#define IM_CAPTURE_SCK    4u /* later (M3) */
#define IM_CAPTURE_PORTAL 5u /* later (M4) */
#define IM_CAPTURE_X11    6u /* later (M4) */

#define IM_SRCF_OS_BORDER 0x1u /* Windows will draw its yellow capture border */
#define IM_SRCF_SELF      0x2u /* the user picked an isshoni window */
#define IM_SRCF_MAPPED    0x4u /* Win10: picked monitor mapped to a DD output */

/* JSON: {"monitors":[...],"windows":[]} (04 §5.6). Windows lists monitors only: windows come from the OS picker. */
IM_API int32_t IM_CALL im_list_sources(uint32_t flags, char *json, int32_t cap);

typedef struct im_pick_params {
  uint32_t struct_size;
  uint32_t flags;      /* reserved (macOS M3: picker modes) */
  void *parent_window; /* HWND (Windows), NSWindow* (macOS); NULL = an engine-owned owner window */
  uint32_t timeout_ms; /* 0 = 120000; at most 600000 */
  uint32_t reserved;
} im_pick_params;

typedef struct im_source_info {
  uint32_t struct_size;
  uint32_t kind;          /* IM_SOURCE_* */
  char id[64];            /* opaque: "pick:7", "mon:\\.\DISPLAY1", "test:pattern" */
  char name[256];         /* window title or monitor name: shown locally, never sent anywhere */
  char monitor_id[64];    /* monitor sources: the im_list_sources id; else "" */
  uint32_t width, height; /* physical pixels at pick time */
  uint32_t capture_path;  /* IM_CAPTURE_*: what im_start will use */
  uint32_t flags;         /* IM_SRCF_* */
  uint32_t pid;           /* window sources: owning process if identified (04 §7.6), else 0 */
  uint32_t reserved0;
  uint64_t window; /* window sources: HWND if identified, else 0 */
  uint32_t reserved[4];
} im_source_info;

/* A pick token is single use: a successful im_start consumes it, a failed one leaves it valid. It expires 5 min after
   the pick; at most 4 unused ones are kept (the oldest is evicted). im_start with a consumed, expired or evicted token
   is IM_ERR_SOURCE_GONE with token pick_expired. */
IM_API im_status IM_CALL im_pick_source(const im_pick_params *params, im_source_info *out);
IM_API im_status IM_CALL im_cancel_pick(void);         /* makes a blocked im_pick_source return IM_ERR_CANCELLED */
IM_API void IM_CALL im_release_source(const char *id); /* forget an unused pick */

/* ---- audio (docs/m2/05-windows-audio.md §4; semantics, report JSON and rule matching are 05's) ---- */
#define IM_AUDIO_SAMPLE_RATE 48000u
#define IM_AUDIO_CHANNELS    2u
#define IM_MAX_RULES         512u
#define IM_MAX_RULE_VALUE    520u /* bytes including the NUL (MAX_PATH-sized paths, UTF-8) */
#define IM_APP_ID_LEN        64u  /* bytes including the NUL */
#define IM_MAX_LEVEL_APPS    64u

/* im_audio_config.mode and im_audio_set_mode: modes 0..99 are 05's; 100 is 04's test tone (04 §10.3: 440 Hz, a 1 kHz
   beep in the first 100 ms of each second, no capture). EVERYTHING is reserved for later ("All sound": everything
   except isshoni); in M2 im_start and im_audio_set_mode answer it with IM_ERR_INVALID_ARG (docs/m2/README.md §3 item
   15). */
#define IM_AUDIO_MODE_OFF          0u   /* im_start: no audio packets; mid-share: silence, no capture */
#define IM_AUDIO_MODE_EXCLUDE_APPS 1u   /* default: everything except kept-out apps and isshoni */
#define IM_AUDIO_MODE_EVERYTHING   2u   /* reserved (later); IM_ERR_INVALID_ARG in M2 */
#define IM_AUDIO_MODE_TEST_TONE    100u /* 04's test tone; no capture */

#define IM_RULE_APP_EXCLUDE      0u /* value: exe file name; matches the process or an app ancestor */
#define IM_RULE_APP_ALLOW        1u /* value: exe name; the "Include" answer */
#define IM_RULE_INSTANCE         2u /* value: decimal pid; matches it and every OS descendant */
#define IM_RULE_STOP             3u /* value: launcher/shell exe name; ends app walks */
#define IM_RULE_SHARED_RUNTIME   4u /* value: exe name that belongs to its parent's app whatever its folder */
#define IM_RULE_BROWSER          5u /* value: exe name of a browser */
#define IM_RULE_WEBVIEW_DATA_DIR 6u /* value: full path of isshoni's WebView2 user data folder */

#define IM_RULE_SRC_BUILTIN 0u /* the built-in voice-app list */
#define IM_RULE_SRC_USER    1u /* a remembered user decision */
#define IM_RULE_SRC_SELF    2u /* isshoni itself */
#define IM_RULE_SRC_VIEWER  3u /* a browser that plays the user's isshoni viewer */
#define IM_RULE_SRC_DATA    4u /* launchers, shared runtimes, browsers */

typedef struct im_rule { /* an array element: frozen in v1; new behavior means new kind values */
  uint32_t kind;         /* IM_RULE_* */
  uint32_t source;       /* IM_RULE_SRC_* */
  const char *value;     /* UTF-8, < IM_MAX_RULE_VALUE; compared case-insensitively */
  const char *app_id;    /* report id, < IM_APP_ID_LEN, [a-z0-9:._-]; NULL or "" = "exe:<value>" */
  const char *name;      /* display name for reports; NULL = the exe's FileDescription */
} im_rule;

#define IM_EXCLF_KEEP_OUT_MIC_USERS 0x1u /* "Keep out apps that use my microphone" (default on) */
#define IM_EXCLF_FAST_PATH          0x2u /* allow the single-EXCLUDE fast path (default on) */

typedef struct im_exclusions {
  uint32_t struct_size; /* sizeof(im_exclusions) */
  uint32_t flags;       /* IM_EXCLF_* */
  const im_rule *rules;
  uint32_t rule_count; /* <= IM_MAX_RULES */
  uint32_t shared_pid; /* the shared window's process (from the pick, via Go); 0 = monitor or unknown */
} im_exclusions;

#define IM_AUDIOF_TAP_PCM         0x1u        /* also emit IM_PACKET_PCM chunks (audiotest; shipped) */
#define IM_AUDIOF_TEST_FAIL_PROBE 0x80000000u /* audiotest only: behave as if the startup probe failed (shipped) */

typedef struct im_audio_config { /* embedded by value in im_start_config: fixed size, no struct_size */
  uint32_t mode;                 /* IM_AUDIO_MODE_* */
  uint32_t bitrate_bps;          /* 32000..510000, 0 = 128000 */
  uint32_t frame_ms;             /* 0 = 20; M2 only 20 */
  uint32_t flags;                /* IM_AUDIOF_* */
  uint32_t reserved[4];
} im_audio_config;

typedef struct im_audio_level {
  char app_id[IM_APP_ID_LEN]; /* NUL-terminated; matches apps[].id of the report */
  float peak_db;              /* max over the last 100 ms, dBFS; -120 = silence */
  float rms_db;               /* captured: RMS over 100 ms; activity: = peak_db */
  uint32_t source;            /* 0 = captured (what friends hear), 1 = activity (session peak meter) */
  uint32_t reserved;
} im_audio_level;

typedef struct im_audio_levels {
  uint32_t struct_size; /* sizeof(im_audio_levels), set by the caller */
  uint32_t report_seq;  /* equals the report's "seq"; a change means: fetch the report */
  int64_t qpc_ns;       /* engine clock (QPC ns, im_clock().mono_ns) of this sample */
  float mix_peak_db;    /* the mix fed to Opus, last 100 ms; -120 when not live or silenced */
  float mix_rms_db;
  uint32_t count; /* entries written to apps[] */
  uint32_t reserved;
  im_audio_level apps[IM_MAX_LEVEL_APPS];
} im_audio_levels;

/* "What friends hear": 05's report JSON (05 §12.1). flags: 0 (reserved). Live or watching: the current report;
   otherwise a one-shot snapshot (<= 500 ms). */
IM_API int32_t IM_CALL im_list_audio_apps(uint32_t flags, char *json, int32_t cap);
/* Watcher without capture (the share sheet before going live). Idempotent; the rules replace the previous ones. */
IM_API im_status IM_CALL im_audio_watch(const im_exclusions *ex);
IM_API im_status IM_CALL im_audio_unwatch(void); /* no-op while a share with audio runs */
/* Replaces the rules (and flags, shared_pid) of the watcher and of a running share (copied). Applied within 50 ms;
   without a watcher or share they are stored for the next im_audio_watch or im_start. */
IM_API im_status IM_CALL im_set_exclusions(const im_exclusions *ex);
/* Mid-share: set_mode (OFF, EXCLUDE_APPS; EVERYTHING is IM_ERR_INVALID_ARG in M2) crossfades in 20 ms; set_muted is
   the pause mute 01's core sets: it fades in 10 ms and silent Opus frames keep flowing. */
IM_API im_status IM_CALL im_audio_set_mode(uint32_t mode);
IM_API im_status IM_CALL im_audio_set_muted(uint32_t muted);
/* Level meters (05 §12.2). Lock-free, < 50 us. IM_ERR_BAD_STATE (token not_watching) unless watching or live. */
IM_API im_status IM_CALL im_audio_levels_get(im_audio_levels *out);
/* First-run sound check (05 §11.3): captures the chime of helper_pid's process tree, outside the policy; 0 = the DLL
   plays the chime itself. timeout_ms: 1000..10000. Result JSON as 05 §11.3, snprintf rule. */
IM_API int32_t IM_CALL im_audio_sound_check(uint32_t helper_pid, int32_t timeout_ms, char *json, int32_t cap);

/* ---------------------------------------------------------------- sessions */
#define IM_MAX_LAYERS 3u /* M2 uses 2: "f" and "q"; "h" is M5 */
#define IM_ALL_LAYERS 0xFFFFFFFEu
#define IM_NO_LAYER   0xFFFFFFFFu /* audio and PCM packets; events that name no layer */

#define IM_PROFILE_HIGH 0x6400u /* profile_idc << 8 | constraint byte, as protocol CodecKey "h264/6400" */
#define IM_PROFILE_CB   0x42E0u /* "h264/42e0" */

#define IM_CONTENT_AUTO   0u /* presets Auto */
#define IM_CONTENT_MOTION 1u /* Game, Movie */
#define IM_CONTENT_DETAIL 2u /* Text */

#define IM_LAYER_INACTIVE 0x1u /* start with this layer paused (im_set_layer_active) */

typedef struct im_layer_config {
  char rid[4];            /* "f", "q"; NUL-terminated */
  uint32_t width, height; /* bounding box in pixels; 0 = none */
  uint32_t max_pixels;    /* pixel budget (ShareParams maxPixels); 0 = none */
  uint32_t fps;           /* 1..240 */
  uint32_t bitrate_bps;   /* initial target (01: ratectl's initial value, <= maxBitrate) */
  uint32_t gop_ms;        /* 0 = default (layer 0: 4000, others: 2000); 1000..10000 */
  uint32_t flags;         /* IM_LAYER_* */
  uint32_t reserved[2];
} im_layer_config;

#define IM_START_HIDE_CURSOR         0x1u /* don't draw the pointer (0 = drawn: WGC cursor capture on, DD blends it) */
#define IM_START_PAUSED              0x2u
#define IM_START_ALLOW_LISTED_SOURCE 0x4u /* accept "mon:" and "test:" ids (tools, tests); the app never sets it */
#define IM_START_KEEP_BORDER         0x8u /* don't ask for borderless capture */

typedef struct im_start_config {
  uint32_t struct_size;
  uint32_t flags;        /* IM_START_* */
  const char *source_id; /* from im_pick_source (or im_list_sources with ALLOW_LISTED); NULL = audio only */
  uint32_t profile;      /* IM_PROFILE_* */
  uint32_t content_hint; /* IM_CONTENT_* */
  uint32_t encoder;      /* IM_ENC_AUTO, or one kind to force */
  uint32_t layer_count;  /* 0 (audio only) .. IM_MAX_LAYERS */
  im_layer_config layers[IM_MAX_LAYERS]; /* wire order: "f" first */
  im_audio_config audio;                 /* mode IM_AUDIO_MODE_OFF = no audio packets at all */
  const im_exclusions *exclusions;       /* NULL = the set from the last im_set_exclusions/im_audio_watch */
  uint32_t reserved[8];
} im_start_config;

#define IM_SESS_NO_OS_INDICATOR 0x1u /* no OS border (WGC borderless or DD): the app must show its own indicator */
#define IM_SESS_AUDIO           0x2u

#define IM_AUDIO_STATE_OFF       0u /* mirrors 05's report "state" (05 §12.1); stats JSON uses the strings */
#define IM_AUDIO_STATE_STARTING  1u /* "starting": the startup probe runs */
#define IM_AUDIO_STATE_OK        2u /* "ok" */
#define IM_AUDIO_STATE_DEGRADED  3u /* "degraded": probe failed or unsupported, endpoint loopback (05) */
#define IM_AUDIO_STATE_NO_DEVICE 4u /* "no-device": silent frames until a render endpoint appears */

typedef struct im_layer_info {
  char rid[4];
  uint32_t encoder; /* IM_ENC_* in use */
  uint32_t width, height, fps, bitrate_bps;
  uint32_t profile; /* the SPS's key, e.g. 0x6400 */
  uint32_t flags;   /* IM_ENCF_* of that encoder */
  uint32_t reserved[2];
} im_layer_info;

typedef struct im_session_info {
  uint32_t struct_size;
  uint32_t flags;        /* IM_SESS_* */
  uint32_t capture_path; /* IM_CAPTURE_* */
  uint32_t layer_count;
  int64_t epoch_ns;     /* engine clock at im_start; every capture_ts_ns, of every kind, >= epoch_ns */
  uint32_t audio_state; /* IM_AUDIO_STATE_* */
  uint32_t reserved0;
  im_layer_info layers[IM_MAX_LAYERS];
  uint32_t reserved[8];
} im_session_info;

typedef uint64_t im_session; /* 0 is never a valid session */

IM_API im_status IM_CALL im_start(const im_start_config *cfg, im_session *out, im_session_info *info);
IM_API im_status IM_CALL im_get_session_info(im_session s, im_session_info *info);

/* ---------------------------------------------------------------- packets and events */
#define IM_PACKET_VIDEO 1u
#define IM_PACKET_AUDIO 2u /* one 20 ms Opus packet */
#define IM_PACKET_EVENT 3u
#define IM_PACKET_PCM   4u   /* 480 float32 stereo frames (3840 bytes), only with IM_AUDIOF_TAP_PCM */
#define IM_PACKET_DEBUG 100u /* IM_TEST_HOOKS builds only */

/* im_packet.flags. REINIT on video: the first AU after an encoder (re)open (always with KEYFRAME): new SPS/PPS, maybe
   a new size or profile. REINIT on audio: the first packet of a session and the first after a new timestamp grid
   starts (PcmSink::OnGap); the time step may be any value. DISCONTINUITY: packets of this stream were dropped before
   this one (ring full, stale); the audio grid is unchanged. */
#define IM_PKT_KEYFRAME      0x1u /* video: IDR access unit with SPS and PPS in front */
#define IM_PKT_REINIT        0x2u /* video: first AU after an encoder (re)open; audio: a new timestamp grid */
#define IM_PKT_REFRESH       0x4u /* video: a re-encode of an unchanged frame */
#define IM_PKT_DISCONTINUITY 0x8u /* packets of this stream were dropped before this one */

/* im_packet.event of IM_PACKET_EVENT packets; the detail JSON is 04 §5.6's. ENCODER_CHANGED (status
   IM_ERR_ENCODER_LOST) is queued before the new encoder's first REINIT|KEYFRAME packet. DEVICE_RESET is reserved for
   M5's device-loss recovery (the GPU was reset or removed and the pipeline was rebuilt); the M2 Windows engine never
   sends it. */
#define IM_EVENT_ENDED               1 /* final; status = the cause (IM_OK after im_stop) */
#define IM_EVENT_SOURCE_RESIZED      2
#define IM_EVENT_SOURCE_MINIMIZED    3
#define IM_EVENT_SOURCE_RESTORED     4
#define IM_EVENT_CAPTURE_INTERRUPTED 5 /* secure desktop, mode change: the last frame repeats */
#define IM_EVENT_CAPTURE_RESUMED     6
#define IM_EVENT_ENCODER_CHANGED     7  /* status IM_ERR_ENCODER_LOST: fell back to the next encoder */
#define IM_EVENT_DEVICE_RESET        8  /* reserved (M5); never sent in M2 */
#define IM_EVENT_LIMIT_CHANGED       9  /* the encoder can't keep up ("cpu", "gpu") or can again ("none") */
#define IM_EVENT_RING_OVERFLOW       10 /* the consumer fell behind; packets were dropped */
/* 32..63 reserved (audio reports go through im_list_audio_apps and im_audio_levels_get, 05) */

typedef struct im_packet {
  uint32_t struct_size;
  uint32_t kind;         /* IM_PACKET_* */
  uint32_t layer;        /* video: layer index; audio, PCM: IM_NO_LAYER; events may name one */
  uint32_t flags;        /* IM_PKT_* */
  int64_t capture_ts_ns; /* engine clock (im_clock), >= epoch_ns; events: when it happened */
  const uint8_t *data;   /* Annex B AU, one Opus packet, PCM f32 interleaved, or event detail JSON */
  uint32_t size;
  uint32_t duration_us;   /* audio: 20000 (Opus); PCM: 10000; else 0 */
  uint16_t width, height; /* video */
  uint32_t profile;       /* video: the SPS's key */
  int32_t event;          /* IM_EVENT_*; 0 for media */
  im_status status;       /* events: the code this event carries */
  uint32_t reserved[4];
} im_packet;

/* Blocks up to timeout_ms (at most 1000). IM_OK: *out is filled. IM_TIMEOUT: nothing yet. IM_ENDED: done. */
IM_API im_status IM_CALL im_next_packet(im_session s, uint32_t timeout_ms, im_packet *out);

/* ---------------------------------------------------------------- control (non-blocking, any thread) */
IM_API im_status IM_CALL im_request_keyframe(im_session s, uint32_t layer);          /* or IM_ALL_LAYERS */
IM_API im_status IM_CALL im_set_bitrate(im_session s, uint32_t layer, uint32_t bps); /* video layers only */
IM_API im_status IM_CALL im_set_layer_active(im_session s, uint32_t layer, uint32_t active);
IM_API im_status IM_CALL im_update_layer(im_session s, uint32_t layer, const im_layer_config *cfg);
IM_API im_status IM_CALL im_set_profile(im_session s, uint32_t profile);
IM_API im_status IM_CALL im_set_content_hint(im_session s, uint32_t hint); /* IM_CONTENT_*: re-inits every layer */
IM_API im_status IM_CALL im_set_paused(im_session s, uint32_t paused);
IM_API im_status IM_CALL im_set_audio_bitrate(im_session s, uint32_t bps);
IM_API int32_t IM_CALL im_get_stats(im_session s, char *json, int32_t cap);
IM_API im_status IM_CALL im_stop(im_session s); /* blocks <= 2 s */

/* ---------------------------------------------------------------- own windows */
#define IM_PROTECT_OFF 0x1u /* undo (tests) */
IM_API im_status IM_CALL im_protect_window(void *native_window, uint32_t flags);

/* ---------------------------------------------------------------- diagnostics */
IM_API int32_t IM_CALL im_last_error(char *buf, int32_t cap);  /* this thread's last failure ("<token>: ...") */
IM_API int32_t IM_CALL im_read_logs(char *buf, int32_t cap);   /* drains whole NDJSON lines; 0 = empty */
IM_API int32_t IM_CALL im_abi_layout(char *json, int32_t cap); /* sizeof and offsetof of every struct */

/* -------------------------------------------- test exports: shipped, capture nothing (docs/m2/05 §4.1) */
/* For cmd/isshoni-audiotest only; stable within v1, not for apps. They render or decode test signals.
   endpoint_id: an IMMDevice id (UTF-8); NULL or "" = the default render endpoint (eConsole). */
IM_API im_status IM_CALL im_test_render_tone(const char *endpoint_id, float hz, float amp, int32_t ms);
/* Returns the number of onsets written to onset_ns, or a negative status. onset_ns: each burst's predicted
   device-position time (engine clock), from the render client's IAudioClock, never from the mixer (05 §13.2). */
IM_API int32_t IM_CALL im_test_render_bursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms,
                                             int32_t period_ms, int32_t count, int64_t *onset_ns, int32_t cap);
/* Returns the frames per channel decoded into pcm (interleaved stereo float), or a negative status. */
IM_API int32_t IM_CALL im_test_opus_decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames);

#ifdef __cplusplus
}
#endif
#endif /* ISSHONI_MEDIA_H */
