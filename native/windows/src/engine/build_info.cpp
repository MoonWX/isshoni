#include "engine/build_info.h"

#include <filesystem>
#include <optional>
#include <string>

#include "audio_bridge/audio_bridge.h"
#include "encode/encode.h"
#include "engine/engine.h"
#include "engine/os.h"
#include "im/json/writer.h"
#include "isshoni_media.h"

// Set by native/windows/CMakeLists.txt from IM_VERSION, IM_COMMIT and native/ffmpeg/lock.json.
#ifndef IM_BUILD_VERSION
#define IM_BUILD_VERSION "0.0.0-dev"
#endif
#ifndef IM_BUILD_COMMIT
#define IM_BUILD_COMMIT ""
#endif
#ifndef IM_BUILD_FFMPEG_TAG
#define IM_BUILD_FFMPEG_TAG ""
#endif

namespace im::engine {
namespace {

std::string Compiler() {
#if defined(_MSC_VER) && !defined(__clang__)
  return "MSVC " + std::to_string(_MSC_VER / 100) + "." + std::to_string(_MSC_VER % 100);
#elif defined(__clang__)
  return "clang " + std::to_string(__clang_major__) + "." + std::to_string(__clang_minor__);
#elif defined(__GNUC__)
  return "gcc " + std::to_string(__GNUC__) + "." + std::to_string(__GNUC_MINOR__);
#else
  return "unknown";
#endif
}

constexpr std::string_view Arch() noexcept {
#if defined(_M_X64) || defined(__x86_64__)
  return "amd64";
#elif defined(_M_ARM64) || defined(__aarch64__)
  return "arm64";
#else
  return "unknown";
#endif
}

std::filesystem::path PathFromUtf8(const std::string &s) {
  return std::filesystem::path(std::u8string(s.begin(), s.end()));
}

// ffmpeg.dir: the FFmpeg folder relative to the engine DLL's, or nothing when it isn't below that folder (another
// drive, or a path that climbs out with "..", which would put folder names such as the user's into logs and
// diagnostics) or the DLL's folder is unknown.
std::optional<std::string> FfmpegDirRelative() {
  const Options o = CurrentOptions();
  if (!o.ffmpeg_dir) {
    return std::string(".");
  }
  const std::string module_dir = os::ModuleDirectory();
  if (module_dir.empty()) {
    return std::nullopt;
  }
  const auto rel =
      PathFromUtf8(*o.ffmpeg_dir).lexically_normal().lexically_relative(PathFromUtf8(module_dir).lexically_normal());
  if (rel.empty() || rel.is_absolute() || rel.has_root_name() || *rel.begin() == "..") {
    return std::nullopt;
  }
  const std::u8string u8 = rel.generic_u8string();
  std::string out(u8.begin(), u8.end());
  while (out.size() > 1 && out.back() == '/') {
    out.pop_back();
  }
  return out;
}

void StringOrNull(json::Writer &w, std::string_view key, std::string_view value) {
  w.Key(key);
  if (value.empty()) {
    w.Null();
  } else {
    w.String(value);
  }
}

} // namespace

std::string_view EngineVersion() noexcept { return IM_BUILD_VERSION; }
std::string_view EngineCommit() noexcept { return IM_BUILD_COMMIT; }
std::string_view FfmpegTag() noexcept { return IM_BUILD_FFMPEG_TAG; }

bool TestHooks() noexcept {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  return true;
#else
  return false;
#endif
}

std::string BuildInfoJson() {
  const encode::FfmpegInfo ff = encode::GetFfmpegInfo();
  const encode::OpenH264Info oh = encode::GetOpenH264Info();
  json::Writer w;
  w.BeginObject();
  w.Key("engine").String(EngineVersion());
  w.Key("abi").Uint(IM_ABI_VERSION);
  StringOrNull(w, "commit", EngineCommit());
  w.Key("testHooks").Bool(TestHooks());
  w.Key("os").String(os::OsName());
  w.Key("arch").String(Arch());
  w.Key("compiler").String(Compiler());

  w.Key("ffmpeg").BeginObject();
  w.Key("loaded").Bool(ff.loaded);
  w.Key("dir");
  if (const auto dir = FfmpegDirRelative()) {
    w.String(*dir);
  } else {
    w.Null();
  }
  StringOrNull(w, "tag", FfmpegTag());
  StringOrNull(w, "avcodec", ff.avcodec);
  StringOrNull(w, "avutil", ff.avutil);
  StringOrNull(w, "license", ff.license);
  w.EndObject();

  StringOrNull(w, "opus", audio_bridge::OpusVersion());

  w.Key("openh264").BeginObject();
  w.Key("loaded").Bool(oh.loaded);
  StringOrNull(w, "version", oh.version);
  w.EndObject();

  w.EndObject();
  return w.Take();
}

} // namespace im::engine
