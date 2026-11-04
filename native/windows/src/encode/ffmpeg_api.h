// libavutil and libavcodec, loaded at run time (docs/m2/04-windows-engine.md §9.2, §13.3; "04" below).
//
// - The engine never import-links FFmpeg: on first use it loads <dir>\avutil-<U>.dll, then <dir>\avcodec-<C>.dll,
//   each by full path with LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32 (04 §13.3 layer 1), and
//   resolves the functions below into one table. <dir> is im_init_params.ffmpeg_dir, or the engine DLL's own folder
//   (the installed layout). <U> and <C> are the majors of native/ffmpeg/lock.json, which native/CMakeLists.txt passes
//   in as IM_AVUTIL_MAJOR and IM_AVCODEC_MAJOR.
// - Version check: avutil_version() >> 16 and avcodec_version() >> 16 must equal the majors built against, and the
//   minors must be at least the headers'. No hash check: the LGPL lets users replace the library, and a same-major
//   build of their own must keep working.
// - Failures are IM_ERR_DEPENDENCY with 04 §5.7's token: missing (a DLL absent or unusable), version (another major,
//   an older minor or a missing function) or blocked_by_policy (ERROR_ACCESS_DISABLED_BY_POLICY,
//   STATUS_INVALID_IMAGE_HASH, ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION). A failed load leaves nothing loaded, so a
//   later probe tries again; a successful one stays for the process's lifetime (FFmpeg is never unloaded, like the
//   engine itself).
// - FFmpeg's log: messages at warning and above go into the engine's log ring (component "ffmpeg"), from whatever
//   thread FFmpeg calls the callback on, without blocking. A LogCapture on a thread also collects that thread's
//   messages, so an encoder open can tell NVENC's "driver too old" and session-limit failures apart (04 §9.10).
//
// Built without FFmpeg's headers (no IM_FFMPEG_ROOT, or a tree without lock.json's majors), Load always answers
// IM_ERR_DEPENDENCY missing and the engine runs without its libavcodec encoders.
#pragma once

#include <cstdint>
#include <string>
#include <string_view>

#include "isshoni_media.h"

#if defined(IM_HAVE_FFMPEG_HEADERS) && IM_HAVE_FFMPEG_HEADERS && defined(IM_AVCODEC_MAJOR) && defined(IM_AVUTIL_MAJOR)
#define IM_ENCODE_FFMPEG 1
#else
#define IM_ENCODE_FFMPEG 0
#endif

#if IM_ENCODE_FFMPEG
extern "C" {
#include <libavcodec/avcodec.h>
#include <libavutil/avutil.h>
#include <libavutil/frame.h>
#include <libavutil/hwcontext.h>
#include <libavutil/log.h>
#include <libavutil/opt.h>
}
#endif

namespace im::encode::ffmpeg {

#if IM_ENCODE_FFMPEG
// The functions the engine uses, with the headers' own signatures.
struct Api {
  // libavutil
  decltype(&::avutil_version) avutil_version = nullptr;
  decltype(&::avutil_license) avutil_license = nullptr;
  decltype(&::av_log_set_callback) av_log_set_callback = nullptr;
  decltype(&::av_log_set_level) av_log_set_level = nullptr;
  decltype(&::av_log_format_line2) av_log_format_line2 = nullptr;
  decltype(&::av_strerror) av_strerror = nullptr;
  decltype(&::av_frame_alloc) av_frame_alloc = nullptr;
  decltype(&::av_frame_free) av_frame_free = nullptr;
  decltype(&::av_frame_unref) av_frame_unref = nullptr;
  decltype(&::av_frame_get_buffer) av_frame_get_buffer = nullptr;
  decltype(&::av_frame_make_writable) av_frame_make_writable = nullptr;
  decltype(&::av_buffer_ref) av_buffer_ref = nullptr;
  decltype(&::av_buffer_unref) av_buffer_unref = nullptr;
  decltype(&::av_hwdevice_ctx_alloc) av_hwdevice_ctx_alloc = nullptr;
  decltype(&::av_hwdevice_ctx_init) av_hwdevice_ctx_init = nullptr;
  decltype(&::av_hwdevice_ctx_create_derived) av_hwdevice_ctx_create_derived = nullptr;
  decltype(&::av_hwframe_ctx_alloc) av_hwframe_ctx_alloc = nullptr;
  decltype(&::av_hwframe_ctx_init) av_hwframe_ctx_init = nullptr;
  decltype(&::av_hwframe_ctx_create_derived) av_hwframe_ctx_create_derived = nullptr;
  decltype(&::av_hwframe_get_buffer) av_hwframe_get_buffer = nullptr;
  decltype(&::av_hwframe_map) av_hwframe_map = nullptr;
  decltype(&::av_opt_set) av_opt_set = nullptr;
  decltype(&::av_opt_set_int) av_opt_set_int = nullptr;
  // libavcodec
  decltype(&::avcodec_version) avcodec_version = nullptr;
  decltype(&::avcodec_license) avcodec_license = nullptr;
  decltype(&::avcodec_find_encoder_by_name) avcodec_find_encoder_by_name = nullptr;
  decltype(&::avcodec_get_hw_config) avcodec_get_hw_config = nullptr;
  decltype(&::avcodec_alloc_context3) avcodec_alloc_context3 = nullptr;
  decltype(&::avcodec_free_context) avcodec_free_context = nullptr;
  decltype(&::avcodec_open2) avcodec_open2 = nullptr;
  decltype(&::avcodec_send_frame) avcodec_send_frame = nullptr;
  decltype(&::avcodec_receive_packet) avcodec_receive_packet = nullptr;
  decltype(&::av_packet_alloc) av_packet_alloc = nullptr;
  decltype(&::av_packet_free) av_packet_free = nullptr;
  decltype(&::av_packet_unref) av_packet_unref = nullptr;

  // FFmpeg's text for an AVERROR code ("Resource temporarily unavailable"), for logs.
  std::string ErrorText(int err) const;
};
#else
struct Api {};
#endif

// Why a load failed (or IM_OK): the status, the 04 §5.7 token and a detail for im_last_error and the log. The detail
// names a DLL by its file name only (never a path: logs and reports stay free of local paths).
struct LoadResult {
  im_status status = IM_OK;
  std::string_view token;
  std::string detail;
};

// Loads FFmpeg from dir (UTF-8, absolute; "" = the engine DLL's own folder) unless it is loaded already, and returns
// the table, or nullptr with *why set (when why is non-null). Thread-safe; the first success wins for the process: a
// later call with another folder gets the loaded table (logged at info once per folder change).
const Api *Load(const std::string &dir, LoadResult *why) noexcept;

// The loaded table, or nullptr when no Load succeeded yet.
const Api *Loaded() noexcept;

// The folder Load uses for the current im_init: im_init_params.ffmpeg_dir, or the engine DLL's own folder.
std::string DefaultDir();

// The DLL file names this build loads ("avutil-61.dll", "avcodec-63.dll"); "" when built without FFmpeg.
std::string AvutilDllName();
std::string AvcodecDllName();

// What im_build_info says about the loaded libraries: "61.1.102"-style versions and avcodec_license(), "" each when
// not loaded.
struct Versions {
  bool loaded = false;
  std::string avutil;
  std::string avcodec;
  std::string license;
};
Versions LoadedVersions();

// Collects the FFmpeg log messages of the calling thread (any level FFmpeg reports at warning or above) while it
// lives. One per thread at a time; nesting is not supported (the inner one wins).
class LogCapture {
public:
  LogCapture() noexcept;
  ~LogCapture();
  LogCapture(const LogCapture &) = delete;
  LogCapture &operator=(const LogCapture &) = delete;

  // Whether any captured message contains needle (ASCII, compared without case).
  bool Contains(std::string_view needle) const noexcept;
  const std::string &text() const noexcept { return text_; }

  // Called by the log callback.
  void Append(std::string_view line) noexcept;

private:
  LogCapture *previous_;
  std::string text_;
};

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// ffmpeg_load_test: the versions Load expects instead of the build's (majors name the DLL files, so a test can offer
// a renamed real DLL as "another major"); a major of 0 restores the build's. Only before the first successful Load.
void SetExpectedVersionsForTest(uint32_t avutil_major, uint32_t avutil_minor, uint32_t avcodec_major,
                                uint32_t avcodec_minor) noexcept;
#endif

} // namespace im::encode::ffmpeg
