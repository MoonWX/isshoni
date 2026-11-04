#include "encode/ffmpeg_api.h"

#include "encode/encode.h"

#include <windows.h>

#include <atomic>
#include <cctype>
#include <cstdarg>
#include <cstdio>
#include <mutex>
#include <string>
#include <vector>

#include "engine/engine.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::encode::ffmpeg {
namespace {

thread_local LogCapture *t_capture = nullptr;

#if IM_ENCODE_FFMPEG

// Not defined in every SDK's winerror.h.
constexpr DWORD kErrorSystemIntegrityPolicyViolation = 4551; // ERROR_SYSTEM_INTEGRITY_POLICY_VIOLATION
constexpr DWORD kStatusInvalidImageHash = 577;               // ERROR_INVALID_IMAGE_HASH (STATUS_INVALID_IMAGE_HASH)

bool BlockedByPolicy(DWORD err) noexcept {
  return err == ERROR_ACCESS_DISABLED_BY_POLICY || err == kStatusInvalidImageHash ||
         err == kErrorSystemIntegrityPolicyViolation;
}

std::wstring Widen(const std::string &s) {
  if (s.empty()) {
    return {};
  }
  const int n = MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s.data(), static_cast<int>(s.size()), nullptr, 0);
  if (n <= 0) {
    return {};
  }
  std::wstring w(static_cast<std::size_t>(n), L'\0');
  MultiByteToWideChar(CP_UTF8, MB_ERR_INVALID_CHARS, s.data(), static_cast<int>(s.size()), w.data(), n);
  return w;
}

static_assert(LIBAVCODEC_VERSION_MAJOR == IM_AVCODEC_MAJOR,
              "IM_FFMPEG_ROOT's headers are not the build native/ffmpeg/lock.json pins (avcodecMajor)");
static_assert(LIBAVUTIL_VERSION_MAJOR == IM_AVUTIL_MAJOR,
              "IM_FFMPEG_ROOT's headers are not the build native/ffmpeg/lock.json pins (avutilMajor)");

struct Expected {
  uint32_t avutil_major = LIBAVUTIL_VERSION_MAJOR;
  uint32_t avutil_minor = LIBAVUTIL_VERSION_MINOR;
  uint32_t avcodec_major = LIBAVCODEC_VERSION_MAJOR;
  uint32_t avcodec_minor = LIBAVCODEC_VERSION_MINOR;
};

struct State {
  std::mutex mu;
  Expected expected;
  HMODULE avutil = nullptr;
  HMODULE avcodec = nullptr;
  Api api;
  std::atomic<const Api *> loaded{nullptr};
  std::string loaded_dir;
  std::string other_dir_logged;
};

State &TheState() noexcept {
  static State *const s = new State(); // never destroyed: FFmpeg stays loaded (and may log) until the process ends
  return *s;
}

std::string VersionText(unsigned v) {
  char buf[32];
  std::snprintf(buf, sizeof buf, "%u.%u.%u", v >> 16, (v >> 8) & 0xFF, v & 0xFF);
  return buf;
}

std::string DllName(const char *lib, uint32_t major) {
  char buf[32];
  std::snprintf(buf, sizeof buf, "%s-%u.dll", lib, static_cast<unsigned>(major));
  return buf;
}

// FFmpeg's log callback: warning and above (the level set at load) into the engine's ring and the thread's capture.
// FFmpeg calls it from encoder threads too; it formats into a thread-local buffer and never blocks.
void LogCallback(void *avcl, int level, const char *fmt, va_list vl) {
  if (level > AV_LOG_WARNING) {
    return;
  }
  const Api *api = TheState().loaded.load(std::memory_order_acquire);
  thread_local char line[1024];
  thread_local int print_prefix = 1;
  int n = -1;
  if (api != nullptr && api->av_log_format_line2 != nullptr) {
    va_list copy;
    va_copy(copy, vl);
    n = api->av_log_format_line2(avcl, level, fmt, copy, line, static_cast<int>(sizeof line), &print_prefix);
    va_end(copy);
  } else {
    va_list copy;
    va_copy(copy, vl);
    n = std::vsnprintf(line, sizeof line, fmt, copy);
    va_end(copy);
  }
  if (n < 0) {
    return;
  }
  std::string_view text(line);
  while (!text.empty() && (text.back() == '\n' || text.back() == '\r' || text.back() == ' ')) {
    text.remove_suffix(1);
  }
  if (text.empty()) {
    return;
  }
  if (t_capture != nullptr) {
    t_capture->Append(text);
  }
  if (level <= AV_LOG_ERROR) {
    log::Error("ffmpeg", text);
  } else {
    log::Warn("ffmpeg", text);
  }
}

LoadResult Failure(im_status status, std::string_view token, std::string detail) {
  LoadResult r;
  r.status = status;
  r.token = token;
  r.detail = std::move(detail);
  return r;
}

// Loads one DLL of the pair by full path; on failure the result says why.
HMODULE LoadOne(const std::string &dir, const std::string &name, LoadResult *why) {
  std::string path = dir;
  if (!path.empty() && path.back() != '\\' && path.back() != '/') {
    path += '\\';
  }
  path += name;
  const std::wstring wide = Widen(path);
  if (wide.empty()) {
    *why = Failure(IM_ERR_DEPENDENCY, abi::token::kMissing, name + ": the FFmpeg folder is not valid UTF-8");
    return nullptr;
  }
  if (GetFileAttributesW(wide.c_str()) == INVALID_FILE_ATTRIBUTES) {
    *why = Failure(IM_ERR_DEPENDENCY, abi::token::kMissing, name + " is not in the FFmpeg folder");
    return nullptr;
  }
  const HMODULE m =
      LoadLibraryExW(wide.c_str(), nullptr, LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR | LOAD_LIBRARY_SEARCH_SYSTEM32);
  if (m == nullptr) {
    const DWORD err = GetLastError();
    char code[64];
    std::snprintf(code, sizeof code, " failed to load (error %lu)", static_cast<unsigned long>(err));
    *why = Failure(IM_ERR_DEPENDENCY, BlockedByPolicy(err) ? abi::token::kBlockedByPolicy : abi::token::kMissing,
                   name + code);
  }
  return m;
}

template <class Fn> bool Resolve(HMODULE m, const char *name, Fn &out) {
  out = reinterpret_cast<Fn>(reinterpret_cast<void *>(GetProcAddress(m, name)));
  return out != nullptr;
}

// Resolves every function; the first missing one is named in the detail.
bool ResolveAll(HMODULE avutil, HMODULE avcodec, Api &a, std::string *missing) {
#define IM_RESOLVE(mod, fn)                                                                                            \
  if (!Resolve(mod, #fn, a.fn)) {                                                                                      \
    *missing = #fn;                                                                                                    \
    return false;                                                                                                      \
  }
  IM_RESOLVE(avutil, avutil_version)
  IM_RESOLVE(avutil, avutil_license)
  IM_RESOLVE(avutil, av_log_set_callback)
  IM_RESOLVE(avutil, av_log_set_level)
  IM_RESOLVE(avutil, av_log_format_line2)
  IM_RESOLVE(avutil, av_strerror)
  IM_RESOLVE(avutil, av_frame_alloc)
  IM_RESOLVE(avutil, av_frame_free)
  IM_RESOLVE(avutil, av_frame_unref)
  IM_RESOLVE(avutil, av_frame_get_buffer)
  IM_RESOLVE(avutil, av_frame_make_writable)
  IM_RESOLVE(avutil, av_buffer_ref)
  IM_RESOLVE(avutil, av_buffer_unref)
  IM_RESOLVE(avutil, av_hwdevice_ctx_alloc)
  IM_RESOLVE(avutil, av_hwdevice_ctx_init)
  IM_RESOLVE(avutil, av_hwdevice_ctx_create_derived)
  IM_RESOLVE(avutil, av_hwframe_ctx_alloc)
  IM_RESOLVE(avutil, av_hwframe_ctx_init)
  IM_RESOLVE(avutil, av_hwframe_ctx_create_derived)
  IM_RESOLVE(avutil, av_hwframe_get_buffer)
  IM_RESOLVE(avutil, av_hwframe_map)
  IM_RESOLVE(avutil, av_opt_set)
  IM_RESOLVE(avutil, av_opt_set_int)
  IM_RESOLVE(avcodec, avcodec_version)
  IM_RESOLVE(avcodec, avcodec_license)
  IM_RESOLVE(avcodec, avcodec_find_encoder_by_name)
  IM_RESOLVE(avcodec, avcodec_get_hw_config)
  IM_RESOLVE(avcodec, avcodec_alloc_context3)
  IM_RESOLVE(avcodec, avcodec_free_context)
  IM_RESOLVE(avcodec, avcodec_open2)
  IM_RESOLVE(avcodec, avcodec_send_frame)
  IM_RESOLVE(avcodec, avcodec_receive_packet)
  IM_RESOLVE(avcodec, av_packet_alloc)
  IM_RESOLVE(avcodec, av_packet_free)
  IM_RESOLVE(avcodec, av_packet_unref)
#undef IM_RESOLVE
  return true;
}

// Checks one library's version against the expected major and minimum minor.
bool VersionOk(unsigned v, uint32_t major, uint32_t minor) noexcept {
  return (v >> 16) == major && ((v >> 8) & 0xFF) >= minor;
}

LoadResult LoadLocked(State &s, const std::string &dir) {
  const std::string avutil_name = DllName("avutil", s.expected.avutil_major);
  const std::string avcodec_name = DllName("avcodec", s.expected.avcodec_major);
  LoadResult why;
  // avutil first: avcodec imports it by name, and LOAD_LIBRARY_SEARCH_DLL_LOAD_DIR finds it next to avcodec anyway.
  HMODULE avutil = LoadOne(dir, avutil_name, &why);
  if (avutil == nullptr) {
    return why;
  }
  Api a;
  std::string missing;
  if (!Resolve(avutil, "avutil_version", a.avutil_version)) {
    FreeLibrary(avutil);
    return Failure(IM_ERR_DEPENDENCY, abi::token::kVersion, avutil_name + " exports no avutil_version");
  }
  if (const unsigned v = a.avutil_version(); !VersionOk(v, s.expected.avutil_major, s.expected.avutil_minor)) {
    FreeLibrary(avutil);
    char want[64];
    std::snprintf(want, sizeof want, "%u.%u or a later minor", static_cast<unsigned>(s.expected.avutil_major),
                  static_cast<unsigned>(s.expected.avutil_minor));
    return Failure(IM_ERR_DEPENDENCY, abi::token::kVersion,
                   avutil_name + " is libavutil " + VersionText(v) + ", the engine needs " + want);
  }
  HMODULE avcodec = LoadOne(dir, avcodec_name, &why);
  if (avcodec == nullptr) {
    FreeLibrary(avutil);
    return why;
  }
  if (!ResolveAll(avutil, avcodec, a, &missing)) {
    FreeLibrary(avcodec);
    FreeLibrary(avutil);
    return Failure(IM_ERR_DEPENDENCY, abi::token::kVersion, "the FFmpeg DLLs export no " + missing);
  }
  if (const unsigned v = a.avcodec_version(); !VersionOk(v, s.expected.avcodec_major, s.expected.avcodec_minor)) {
    FreeLibrary(avcodec);
    FreeLibrary(avutil);
    char want[64];
    std::snprintf(want, sizeof want, "%u.%u or a later minor", static_cast<unsigned>(s.expected.avcodec_major),
                  static_cast<unsigned>(s.expected.avcodec_minor));
    return Failure(IM_ERR_DEPENDENCY, abi::token::kVersion,
                   avcodec_name + " is libavcodec " + VersionText(v) + ", the engine needs " + want);
  }
  s.avutil = avutil;
  s.avcodec = avcodec;
  s.api = a;
  s.loaded_dir = dir;
  s.loaded.store(&s.api, std::memory_order_release);
  // Warnings and errors only: FFmpeg's info level prints per-frame statistics for some encoders.
  s.api.av_log_set_level(AV_LOG_WARNING);
  s.api.av_log_set_callback(&LogCallback);
  log::Info("ffmpeg", "loaded",
            {{"avutil", VersionText(s.api.avutil_version())},
             {"avcodec", VersionText(s.api.avcodec_version())},
             {"license", s.api.avcodec_license()}});
  return LoadResult{};
}

#endif // IM_ENCODE_FFMPEG

} // namespace

#if IM_ENCODE_FFMPEG

std::string Api::ErrorText(int err) const {
  char buf[128] = {};
  if (av_strerror == nullptr || av_strerror(err, buf, sizeof buf) < 0) {
    std::snprintf(buf, sizeof buf, "error %d", err);
  }
  return buf;
}

const Api *Load(const std::string &dir_in, LoadResult *why) noexcept {
  try {
    State &s = TheState();
    std::lock_guard<std::mutex> lock(s.mu);
    const std::string dir = dir_in.empty() ? engine::os::ModuleDirectory() : dir_in;
    if (const Api *a = s.loaded.load(std::memory_order_acquire)) {
      if (dir != s.loaded_dir && dir != s.other_dir_logged) {
        s.other_dir_logged = dir;
        log::Info("ffmpeg", "already loaded from another folder; keeping it");
      }
      return a;
    }
    if (dir.empty()) {
      if (why != nullptr) {
        *why = Failure(IM_ERR_DEPENDENCY, abi::token::kMissing, "the engine's folder is unknown");
      }
      return nullptr;
    }
    LoadResult r = LoadLocked(s, dir);
    if (r.status != IM_OK) {
      log::Info("ffmpeg", "not loaded", {{"token", r.token}, {"detail", r.detail}});
      if (why != nullptr) {
        *why = std::move(r);
      }
      return nullptr;
    }
    return s.loaded.load(std::memory_order_acquire);
  } catch (...) {
    if (why != nullptr) {
      why->status = IM_ERR_INTERNAL;
      why->token = {};
      why->detail = "out of memory loading FFmpeg";
    }
    return nullptr;
  }
}

const Api *Loaded() noexcept { return TheState().loaded.load(std::memory_order_acquire); }

std::string AvutilDllName() { return DllName("avutil", IM_AVUTIL_MAJOR); }
std::string AvcodecDllName() { return DllName("avcodec", IM_AVCODEC_MAJOR); }

Versions LoadedVersions() {
  Versions v;
  if (const Api *a = Loaded()) {
    v.loaded = true;
    v.avutil = VersionText(a->avutil_version());
    v.avcodec = VersionText(a->avcodec_version());
    if (const char *lic = a->avcodec_license()) {
      v.license = lic;
    }
  }
  return v;
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void SetExpectedVersionsForTest(uint32_t avutil_major, uint32_t avutil_minor, uint32_t avcodec_major,
                                uint32_t avcodec_minor) noexcept {
  State &s = TheState();
  std::lock_guard<std::mutex> lock(s.mu);
  if (avutil_major == 0) {
    s.expected = Expected{};
    return;
  }
  s.expected.avutil_major = avutil_major;
  s.expected.avutil_minor = avutil_minor;
  s.expected.avcodec_major = avcodec_major;
  s.expected.avcodec_minor = avcodec_minor;
}
#endif

#else // !IM_ENCODE_FFMPEG

const Api *Load(const std::string &, LoadResult *why) noexcept {
  if (why != nullptr) {
    why->status = IM_ERR_DEPENDENCY;
    why->token = abi::token::kMissing;
    why->detail = "this engine was built without FFmpeg's headers (IM_FFMPEG_ROOT)";
  }
  return nullptr;
}

const Api *Loaded() noexcept { return nullptr; }

std::string AvutilDllName() { return {}; }
std::string AvcodecDllName() { return {}; }

Versions LoadedVersions() { return {}; }

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void SetExpectedVersionsForTest(uint32_t, uint32_t, uint32_t, uint32_t) noexcept {}
#endif

#endif // IM_ENCODE_FFMPEG

std::string DefaultDir() {
  const engine::Options o = engine::CurrentOptions();
  if (o.ffmpeg_dir) {
    return *o.ffmpeg_dir;
  }
  return engine::os::ModuleDirectory();
}

LogCapture::LogCapture() noexcept : previous_(t_capture) { t_capture = this; }

LogCapture::~LogCapture() { t_capture = previous_; }

void LogCapture::Append(std::string_view line) noexcept {
  try {
    if (text_.size() < 16384) {
      text_.append(line);
      text_.push_back('\n');
    }
  } catch (...) {
    // A capture that runs out of memory just stops collecting.
  }
  if (previous_ != nullptr) {
    previous_->Append(line); // the enclosing capture collects it too
  }
}

bool LogCapture::Contains(std::string_view needle) const noexcept {
  if (needle.empty() || needle.size() > text_.size()) {
    return needle.empty();
  }
  const auto lower = [](char c) { return static_cast<char>(std::tolower(static_cast<unsigned char>(c))); };
  for (std::size_t i = 0; i + needle.size() <= text_.size(); ++i) {
    std::size_t j = 0;
    while (j < needle.size() && lower(text_[i + j]) == lower(needle[j])) {
      ++j;
    }
    if (j == needle.size()) {
      return true;
    }
  }
  return false;
}

} // namespace im::encode::ffmpeg

namespace im::encode {

FfmpegInfo GetFfmpegInfo() {
  const ffmpeg::Versions v = ffmpeg::LoadedVersions();
  FfmpegInfo info;
  info.loaded = v.loaded;
  info.avcodec = v.avcodec;
  info.avutil = v.avutil;
  info.license = v.license;
  return info;
}

} // namespace im::encode
