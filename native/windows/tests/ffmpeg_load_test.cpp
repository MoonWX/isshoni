// ffmpeg_load_test (docs/m2/04-windows-engine.md §9.2, §9.10, §13.3, §18.2; README WA44): the engine with the pinned
// FFmpeg build (native/ffmpeg/lock.json), in a win-x64-ci test binary (IM_TEST_HOOKS).
//
// The FFmpeg folder is ISSHONI_TEST_FFMPEG_DIR (the windows-native job points it at the locked zip's bin\); without
// it the cases that need FFmpeg are skipped, except in CI (CI=true), where a missing folder fails them.
//
// It asserts:
//   - an empty folder as im_init_params.ffmpeg_dir: im_probe still answers IM_OK, every libavcodec encoder's entry is
//     IM_ERR_DEPENDENCY with token "missing", im_build_info says FFmpeg isn't loaded, and an audio-only test-tone
//     session still starts and sends Opus packets (04 §9.2: the engine loads, probes and does audio without FFmpeg);
//   - another major (the real avutil DLL offered under the name of another major, with the expected versions moved
//     by the test hook) and a minor below the build's give IM_ERR_DEPENDENCY "version", and nothing stays loaded;
//   - the pinned build loads: im_build_info's versions have the lock's majors and the LGPL license; a full probe lists
//     every encoder with a status, in 04 §9.1's order, none of them available on a GPU-less runner except MF software
//     (when Media Foundation works there: mf_sw_test encodes with it), and finishes within its 2 s;
//   - encode::Probe fills im_caps.audio_flags from audio_bridge::Engine().ProbeFlags() (04 §9.10 step 4; group 4's
//     hand-over of audio_bridge_test's ProbeFlags case, kept in this file with the probe it checks);
//   - dll_search_test's vendor-runtime and libvpl part (04 §18.2): in a child process whose current directory and one
//     PATH entry hold marker DLLs named nvEncodeAPI64.dll, nvcuda.dll, amfrt64.dll, libmfx64-gen.dll and
//     libmfxhw64.dll, with ONEVPL_SEARCH_PATH and ONEVPL_PRIORITY_PATH pointed at the marker folder and no process
//     default set, im_init, a full im_probe and an open of each vendor encoder (h264_nvenc, h264_qsv, h264_amf on a
//     default device: the probe plans none of them on a GPU-less runner, so the child forces the loads; each open must
//     show in FFmpeg's log that it reached its runtime or libvpl's dispatcher) load none of them: no marker file
//     appears and EnumProcessModules lists none from the marker folders. On a device without video support (the
//     runner's Basic Render Driver: no ID3D11VideoDevice) the engine's open stops at av_hwdevice_ctx_init(D3D11VA)
//     before any runtime, so the child then opens each encoder without a device (system-memory NV12), which reaches
//     the same loaders inside avcodec_open2, and says so in its output. There nvEncodeAPI64.dll is never asked for
//     (FFmpeg loads nvcuda.dll first and stops when it is absent): the output says so, and a PC with an NVIDIA
//     driver checks it. A failure of the libvpl names is 04 §17's libvpl risk, not a flake.
//
// The cases run in this order in one process: FFmpeg, once loaded, stays loaded (04 §9.2), so the cases that need it
// not loaded come first and skip themselves if it already is (a shuffled run).
#include <gtest/gtest.h>

#include <windows.h>

#include <psapi.h>

#include <algorithm>
#include <atomic>
#include <charconv>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <cwchar>
#include <filesystem>
#include <fstream>
#include <memory>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "encode/ffmpeg_api.h"
#include "encode/selector.h"
#include "gpu/device.h"
#include "im/encpolicy/encoders.h"
#include "isshoni_media.h"

namespace fs = std::filesystem;

namespace {

// DllMajor reads N from "<prefix>-N.dll" (avutil-61.dll); false when the name has another shape. std::from_chars,
// not sscanf, which MSVC's /WX build refuses (C4996).
bool DllMajor(std::string_view name, std::string_view prefix, unsigned *major) {
  if (name.size() <= prefix.size() + 5 || name.substr(0, prefix.size()) != prefix || name[prefix.size()] != '-' ||
      name.substr(name.size() - 4) != ".dll") {
    return false;
  }
  const char *first = name.data() + prefix.size() + 1;
  const char *last = name.data() + name.size() - 4;
  const auto [end, ec] = std::from_chars(first, last, *major);
  return ec == std::errc() && end == last;
}

using Clock = std::chrono::steady_clock;

constexpr wchar_t kFfmpegDirVar[] = L"ISSHONI_TEST_FFMPEG_DIR";
constexpr wchar_t kChildVar[] = L"IM_FFMPEG_LOAD_CHILD";
constexpr wchar_t kResultVar[] = L"IM_FFMPEG_LOAD_RESULT";
constexpr wchar_t kPathMarkerDirVar[] = L"IM_FFMPEG_LOAD_PATH_MARKERS"; // the PATH entry's marker folder, for the child

// The vendor runtimes FFmpeg's encoders load (nvenc, nvcuda, AMF) and libvpl's runtimes (04 §13.3 layer 4).
constexpr const wchar_t *kVendorDlls[] = {L"nvEncodeAPI64.dll", L"nvcuda.dll", L"amfrt64.dll", L"libmfx64-gen.dll",
                                          L"libmfxhw64.dll"};

std::optional<std::wstring> GetEnv(const wchar_t *name) {
  const DWORD n = GetEnvironmentVariableW(name, nullptr, 0);
  if (n == 0) {
    return std::nullopt;
  }
  std::wstring v(n, L'\0');
  const DWORD m = GetEnvironmentVariableW(name, v.data(), n);
  v.resize(m);
  return v;
}

class ScopedEnv {
public:
  ScopedEnv(const wchar_t *name, const std::wstring &value) : name_(name), old_(GetEnv(name)) {
    SetEnvironmentVariableW(name, value.c_str());
  }
  ~ScopedEnv() { SetEnvironmentVariableW(name_, old_ ? old_->c_str() : nullptr); }
  ScopedEnv(const ScopedEnv &) = delete;
  ScopedEnv &operator=(const ScopedEnv &) = delete;

private:
  const wchar_t *name_;
  std::optional<std::wstring> old_;
};

std::string Utf8(const fs::path &p) {
  const std::u8string u8 = p.u8string();
  return std::string(u8.begin(), u8.end());
}

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

std::string BuildInfo() {
  std::string doc(8192, '\0');
  const int32_t n = im_build_info(doc.data(), static_cast<int32_t>(doc.size()));
  doc.resize(n > 0 ? std::min<std::size_t>(static_cast<std::size_t>(n), doc.size() - 1) : 0);
  return doc;
}

// The FFmpeg folder of the pinned build, or nullopt (the test then skips, or fails in CI).
std::optional<fs::path> FfmpegDir() {
  const std::optional<std::wstring> dir = GetEnv(kFfmpegDirVar);
  if (!dir || dir->empty()) {
    return std::nullopt;
  }
  return fs::path(*dir);
}

bool InCi() { return GetEnv(L"CI").value_or(L"") == L"true"; }

#define REQUIRE_FFMPEG_DIR(var)                                                                                        \
  const std::optional<fs::path> var = FfmpegDir();                                                                     \
  if (!var) {                                                                                                          \
    if (InCi()) {                                                                                                      \
      FAIL() << "ISSHONI_TEST_FFMPEG_DIR is not set: the windows-native job must point it at the locked FFmpeg bin";   \
    }                                                                                                                  \
    GTEST_SKIP() << "ISSHONI_TEST_FFMPEG_DIR is not set (the pinned FFmpeg build's bin folder)";                       \
  }

bool IsFfmpegKind(uint32_t kind) {
  return kind == IM_ENC_NVENC || kind == IM_ENC_QSV || kind == IM_ENC_AMF || kind == IM_ENC_MF_HW ||
         kind == IM_ENC_MF_SW;
}

im_caps Probe(uint32_t flags) {
  im_caps caps{};
  caps.struct_size = sizeof caps;
  const im_status st = im_probe(flags, &caps);
  EXPECT_EQ(st, IM_OK) << LastError();
  return caps;
}

class TempDir {
public:
  TempDir() {
    static int counter = 0;
    path_ = fs::temp_directory_path() /
            (L"im-ffmpeg-load-" + std::to_wstring(GetCurrentProcessId()) + L"-" + std::to_wstring(++counter));
    fs::remove_all(path_);
    fs::create_directories(path_);
  }
  ~TempDir() {
    std::error_code ec;
    fs::remove_all(path_, ec);
  }
  TempDir(const TempDir &) = delete;
  TempDir &operator=(const TempDir &) = delete;
  const fs::path &path() const { return path_; }

private:
  fs::path path_;
};

// The engine for one case: im_init with an FFmpeg folder, im_shutdown at the end.
class Engine {
public:
  explicit Engine(const std::optional<fs::path> &ffmpeg_dir, uint32_t flags = IM_INIT_NO_AUDIO) {
    if (ffmpeg_dir) {
      dir_ = Utf8(*ffmpeg_dir);
    }
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = flags;
    p.log_level = IM_LOG_DEBUG;
    p.ffmpeg_dir = ffmpeg_dir ? dir_.c_str() : nullptr;
    status_ = im_init(&p);
  }
  ~Engine() { im_shutdown(); }
  Engine(const Engine &) = delete;
  Engine &operator=(const Engine &) = delete;
  im_status status() const { return status_; }

private:
  std::string dir_;
  im_status status_ = IM_ERR_INTERNAL;
};

// ------------------------------------------------------------------------------------------- without FFmpeg

TEST(FfmpegLoad, EmptyFolderIsMissingAndAudioStillWorks) {
  if (im::encode::ffmpeg::Loaded() != nullptr) {
    GTEST_SKIP() << "FFmpeg is already loaded in this process (a shuffled run): this case needs it absent";
  }
  TempDir empty;
  Engine engine(empty.path());
  ASSERT_EQ(engine.status(), IM_OK) << LastError();

  const im_caps caps = Probe(IM_PROBE_FULL);
  int ffmpeg_kinds = 0;
  for (uint32_t i = 0; i < caps.encoder_count; ++i) {
    const im_encoder_info &e = caps.encoders[i];
    if (!IsFfmpegKind(e.kind)) {
      continue;
    }
    ++ffmpeg_kinds;
    EXPECT_EQ(e.status, IM_ERR_DEPENDENCY) << e.codec;
    EXPECT_STREQ(e.reason, "missing") << e.codec;
    EXPECT_EQ(e.flags & IM_ENCF_AVAILABLE, 0u) << e.codec;
  }
  EXPECT_GE(ffmpeg_kinds, 1) << "MF software is always listed";
  EXPECT_EQ(im::encode::ffmpeg::Loaded(), nullptr);
  EXPECT_NE(BuildInfo().find("\"ffmpeg\":{\"loaded\":false"), std::string::npos) << BuildInfo();

  // An audio-only test-tone session works without FFmpeg (04 §9.2).
  im_start_config cfg{};
  cfg.struct_size = sizeof cfg;
  cfg.profile = IM_PROFILE_HIGH;
  cfg.source_id = nullptr;
  cfg.layer_count = 0;
  cfg.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  im_session s = 0;
  ASSERT_EQ(im_start(&cfg, &s, nullptr), IM_OK) << LastError();
  int audio = 0;
  const auto deadline = Clock::now() + std::chrono::seconds(3);
  while (audio < 10 && Clock::now() < deadline) {
    im_packet p{};
    p.struct_size = sizeof p;
    const im_status st = im_next_packet(s, 100, &p);
    if (st == IM_ENDED) {
      break;
    }
    if (st == IM_OK && p.kind == IM_PACKET_AUDIO) {
      ++audio;
    }
  }
  EXPECT_GE(audio, 10) << "the test tone's Opus packets (50 per second)";
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST(FfmpegLoad, MissingFolderLoadsNothing) {
  if (im::encode::ffmpeg::Loaded() != nullptr) {
    GTEST_SKIP() << "FFmpeg is already loaded in this process";
  }
  TempDir empty;
  im::encode::ffmpeg::LoadResult why;
  EXPECT_EQ(im::encode::ffmpeg::Load(Utf8(empty.path() / L"absent"), &why), nullptr);
  EXPECT_EQ(why.status, IM_ERR_DEPENDENCY);
  EXPECT_EQ(why.token, "missing");
  EXPECT_EQ(why.detail.find(Utf8(empty.path())), std::string::npos) << "a detail names no path: " << why.detail;
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
TEST(FfmpegLoad, AnotherMajorOrAnOlderMinorIsVersion) {
  REQUIRE_FFMPEG_DIR(dir);
  if (im::encode::ffmpeg::Loaded() != nullptr) {
    GTEST_SKIP() << "FFmpeg is already loaded in this process";
  }
  const std::string avutil = im::encode::ffmpeg::AvutilDllName();
  const std::string avcodec = im::encode::ffmpeg::AvcodecDllName();
  ASSERT_FALSE(avutil.empty()) << "built without FFmpeg's headers (IM_FFMPEG_ROOT)";
  unsigned avutil_major = 0;
  unsigned avcodec_major = 0;
  ASSERT_TRUE(DllMajor(avutil, "avutil", &avutil_major)) << avutil;
  ASSERT_TRUE(DllMajor(avcodec, "avcodec", &avcodec_major)) << avcodec;
  struct Reset {
    ~Reset() { im::encode::ffmpeg::SetExpectedVersionsForTest(0, 0, 0, 0); }
  } reset;

  // Another major: the real avutil offered as avutil-<major - 1>.dll, which the engine then expects; the library
  // says it is <major>, so the load stops at the version check.
  TempDir other;
  const uint32_t wrong = avutil_major - 1;
  fs::copy_file(*dir / avutil, other.path() / ("avutil-" + std::to_string(wrong) + ".dll"));
  fs::copy_file(*dir / avcodec, other.path() / avcodec);
  im::encode::ffmpeg::SetExpectedVersionsForTest(wrong, 0, avcodec_major, 0);
  im::encode::ffmpeg::LoadResult why;
  EXPECT_EQ(im::encode::ffmpeg::Load(Utf8(other.path()), &why), nullptr);
  EXPECT_EQ(why.status, IM_ERR_DEPENDENCY) << why.detail;
  EXPECT_EQ(why.token, "version") << why.detail;
  {
    Engine engine(other.path());
    ASSERT_EQ(engine.status(), IM_OK) << LastError();
    const im_caps caps = Probe(IM_PROBE_FULL);
    for (uint32_t i = 0; i < caps.encoder_count; ++i) {
      if (IsFfmpegKind(caps.encoders[i].kind)) {
        EXPECT_EQ(caps.encoders[i].status, IM_ERR_DEPENDENCY) << caps.encoders[i].codec;
        EXPECT_STREQ(caps.encoders[i].reason, "version") << caps.encoders[i].codec;
      }
    }
  }

  // A minor below the build's, for each library.
  im::encode::ffmpeg::SetExpectedVersionsForTest(avutil_major, 255, avcodec_major, 0);
  why = {};
  EXPECT_EQ(im::encode::ffmpeg::Load(Utf8(*dir), &why), nullptr);
  EXPECT_EQ(why.token, "version") << why.detail;
  im::encode::ffmpeg::SetExpectedVersionsForTest(avutil_major, 0, avcodec_major, 255);
  why = {};
  EXPECT_EQ(im::encode::ffmpeg::Load(Utf8(*dir), &why), nullptr);
  EXPECT_EQ(why.token, "version") << why.detail;

  EXPECT_EQ(im::encode::ffmpeg::Loaded(), nullptr);
  EXPECT_EQ(GetModuleHandleW(std::wstring(avcodec.begin(), avcodec.end()).c_str()), nullptr)
      << "a refused avcodec stays unloaded";
}
#endif

// ------------------------------------------------------------------------------------------- with FFmpeg

// 05's AudioEngine as the probe sees it: ProbeFlags answers a known value; nothing else is used here.
class ProbeFlagsEngine final : public im::audio::AudioEngine {
public:
  static constexpr uint32_t kFlags = IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_SESSION_EVENTS | IM_AUDIO_MIC_DETECTION;
  im_status Init(uint32_t) override { return IM_OK; }
  void Shutdown() override {}
  uint32_t ProbeFlags() override {
    ++calls;
    return kFlags;
  }
  int32_t ListAppsJson(uint32_t, char *, int32_t) override { return IM_ERR_NOT_IMPLEMENTED; }
  im_status SetRules(const im_exclusions *) override { return IM_OK; }
  im_status Watch(const im_exclusions *) override { return IM_OK; }
  im_status Unwatch() override { return IM_OK; }
  im_status SetMode(uint32_t) override { return IM_OK; }
  im_status SetMuted(bool) override { return IM_OK; }
  im_status LevelsGet(im_audio_levels *) override { return IM_ERR_BAD_STATE; }
  int32_t SoundCheck(uint32_t, int32_t, char *, int32_t) override { return IM_ERR_NOT_IMPLEMENTED; }
  std::unique_ptr<im::audio::Mixer> NewMixer() override { return nullptr; }
  std::atomic<int> calls{0};
};

TEST(FfmpegLoad, PinnedBuildLoadsAndProbes) {
  REQUIRE_FFMPEG_DIR(dir);
  static ProbeFlagsEngine audio; // static: the engine may outlive the case (04 §6.1's abandoned threads)
  im::audio_bridge::SetEngineForTest(&audio);
  struct Unset {
    ~Unset() { im::audio_bridge::SetEngineForTest(nullptr); }
  } unset;
  Engine engine(dir, 0); // with audio: Init and ProbeFlags reach the fake
  ASSERT_EQ(engine.status(), IM_OK) << LastError();

  const auto t0 = Clock::now();
  const im_caps caps = Probe(IM_PROBE_FULL);
  const auto ms = std::chrono::duration_cast<std::chrono::milliseconds>(Clock::now() - t0).count();
  EXPECT_LT(ms, 2000) << "04 §9.10: the full probe stays under 2 s";
  EXPECT_EQ(caps.struct_size, sizeof caps);

  // 04 §9.10 step 4 (group 4's hand-over): the audio flags are 05's, through audio_bridge::Engine().
  EXPECT_EQ(caps.audio_flags, ProbeFlagsEngine::kFlags);
  EXPECT_GE(audio.calls.load(), 1);

  ASSERT_NE(im::encode::ffmpeg::Loaded(), nullptr);
  const std::string info = BuildInfo();
  EXPECT_NE(info.find("\"ffmpeg\":{\"loaded\":true"), std::string::npos) << info;
  const im::encode::ffmpeg::Versions v = im::encode::ffmpeg::LoadedVersions();
  unsigned avutil_major = 0;
  unsigned avcodec_major = 0;
  ASSERT_TRUE(DllMajor(im::encode::ffmpeg::AvutilDllName(), "avutil", &avutil_major));
  ASSERT_TRUE(DllMajor(im::encode::ffmpeg::AvcodecDllName(), "avcodec", &avcodec_major));
  EXPECT_EQ(v.avutil.rfind(std::to_string(avutil_major) + ".", 0), 0u) << v.avutil;
  EXPECT_EQ(v.avcodec.rfind(std::to_string(avcodec_major) + ".", 0), 0u) << v.avcodec;
  EXPECT_EQ(v.license.rfind("LGPL", 0), 0u) << "an LGPL build, never GPL: " << v.license;

  ASSERT_GT(caps.os_build, 0u);
  EXPECT_NE(caps.os_flags & IM_OS_SUPPORTED, 0u) << "windows-2025 is newer than Windows 10 22H2";
  ASSERT_GE(caps.encoder_count, 2u);
  ASSERT_LE(caps.encoder_count, IM_MAX_ENCODERS);
  bool seen_mf_sw = false;
  bool seen_openh264 = false;
  for (uint32_t i = 0; i < caps.encoder_count; ++i) {
    const im_encoder_info &e = caps.encoders[i];
    const std::string name = std::string(im::encpolicy::EncoderToken(e.kind));
    EXPECT_FALSE(name.empty()) << "kind " << e.kind;
    EXPECT_EQ((e.flags & IM_ENCF_AVAILABLE) != 0, e.status == IM_OK) << name << ": AVAILABLE iff IM_OK";
    EXPECT_EQ(e.status == IM_OK, e.reason[0] == '\0') << name << ": a token iff unavailable";
    if (e.status != IM_OK) {
      EXPECT_FALSE(std::string(e.reason).empty()) << name;
    }
    const bool hardware = (e.flags & IM_ENCF_HARDWARE) != 0;
    if (hardware) {
      ASSERT_LT(e.adapter_index, caps.adapter_count) << name;
      EXPECT_EQ(caps.adapters[e.adapter_index].flags & IM_ADAPTER_SOFTWARE, 0u)
          << name << ": hardware encoders are planned on hardware adapters only";
    } else {
      EXPECT_EQ(e.adapter_index, 0xFFFFFFFFu) << name;
    }
    // The order (04 §9.1): hardware encoders first, then MF software, then OpenH264.
    if (e.kind == IM_ENC_MF_SW) {
      seen_mf_sw = true;
    } else if (e.kind == IM_ENC_OPENH264) {
      seen_openh264 = true;
      EXPECT_TRUE(seen_mf_sw) << "OpenH264 comes after MF software";
      EXPECT_EQ(e.status, IM_ERR_DEPENDENCY);
      EXPECT_STREQ(e.reason, "disabled") << "OpenH264 is off until README WA51";
    } else if (hardware) {
      EXPECT_FALSE(seen_mf_sw) << name << " after MF software";
    }
    if (e.kind == IM_ENC_MF_SW || e.kind == IM_ENC_MF_HW) {
      if ((caps.os_flags & IM_OS_MEDIA_FOUNDATION) == 0) {
        EXPECT_EQ(e.status, IM_ERR_DEPENDENCY) << name;
        EXPECT_STREQ(e.reason, "no_media_foundation") << name;
      }
    }
    if (e.kind == IM_ENC_MF_SW && e.status == IM_OK) {
      EXPECT_EQ(e.flags & IM_ENCF_HARDWARE, 0u);
      EXPECT_EQ(e.flags & IM_ENCF_ZERO_COPY, 0u) << "MF software reads back (04 §9.1)";
      EXPECT_EQ(e.flags & IM_ENCF_LIVE_BITRATE, 0u) << "MF re-inits for a bitrate change (04 §9.5)";
      EXPECT_NE(e.flags & IM_ENCF_HIGH, 0u);
      EXPECT_NE(e.flags & IM_ENCF_CB, 0u);
      EXPECT_GT(e.test_encode_us, 0u);
    }
    std::printf("encoder %u: %-8s adapter %d status %d reason '%s' flags 0x%02x test %u us\n", i, name.c_str(),
                e.adapter_index == 0xFFFFFFFFu ? -1 : static_cast<int>(e.adapter_index), e.status, e.reason, e.flags,
                e.test_encode_us);
  }
  EXPECT_TRUE(seen_mf_sw);
  EXPECT_TRUE(seen_openh264);
  for (uint32_t i = 0; i < caps.adapter_count; ++i) {
    std::printf("adapter %u: vendor 0x%04x device 0x%04x flags 0x%x\n", i, caps.adapters[i].vendor_id,
                caps.adapters[i].device_id, caps.adapters[i].flags);
  }
  std::printf("os build %u.%u flags 0x%x, probe %u ms\n", caps.os_build, caps.os_ubr, caps.os_flags, caps.probe_ms);

  // A quick probe after the full one (04 §5.3: "cached until the next full probe; a quick probe re-reads OS flags
  // only", < 100 ms): the full probe's adapters, encoders and audio flags, the OS read again.
  const im_caps quick = Probe(IM_PROBE_QUICK);
  EXPECT_LT(quick.probe_ms, 100u) << "04 §5.3: a quick probe takes under 100 ms";
  EXPECT_EQ(quick.os_build, caps.os_build);
  EXPECT_EQ(quick.os_flags, caps.os_flags);
  EXPECT_EQ(quick.audio_flags, caps.audio_flags);
  ASSERT_EQ(quick.adapter_count, caps.adapter_count);
  for (uint32_t i = 0; i < quick.adapter_count; ++i) {
    EXPECT_EQ(quick.adapters[i].luid, caps.adapters[i].luid) << i;
  }
  ASSERT_EQ(quick.encoder_count, caps.encoder_count);
  for (uint32_t i = 0; i < quick.encoder_count; ++i) {
    EXPECT_EQ(quick.encoders[i].kind, caps.encoders[i].kind) << i;
    EXPECT_EQ(quick.encoders[i].status, caps.encoders[i].status) << i;
    EXPECT_EQ(quick.encoders[i].flags, caps.encoders[i].flags) << i;
    EXPECT_STREQ(quick.encoders[i].reason, caps.encoders[i].reason) << i;
    EXPECT_EQ(quick.encoders[i].test_encode_us, caps.encoders[i].test_encode_us) << i;
  }

  // With nothing stored, a quick probe only says which encoders the build has: no test encodes, nothing tested.
  im::encode::ForgetProbe();
  const im_caps listed = Probe(IM_PROBE_QUICK);
  EXPECT_EQ(listed.encoder_count, caps.encoder_count);
  for (uint32_t i = 0; i < listed.encoder_count; ++i) {
    EXPECT_EQ(listed.encoders[i].kind, caps.encoders[i].kind) << i;
    if (listed.encoders[i].kind != IM_ENC_FAKE) {
      EXPECT_EQ(listed.encoders[i].flags & IM_ENCF_AVAILABLE, 0u) << i;
      EXPECT_EQ(listed.encoders[i].test_encode_us, 0u) << i;
    }
  }
  EXPECT_EQ(listed.audio_flags, 0u) << "no stored probe: a quick probe asks 05 nothing";
}

// ------------------------------------------------------------- dll_search_test's vendor-runtime and libvpl part

void WriteResult(const std::string &result) {
  const std::optional<std::wstring> file = GetEnv(kResultVar);
  ASSERT_TRUE(file.has_value());
  std::ofstream out(fs::path(*file), std::ios::binary);
  out << result;
}

// The child: im_init and a full probe with the planted markers around, then which of them this process loaded.
TEST(FfmpegLoadChild, Run) {
  const std::optional<std::wstring> mode = GetEnv(kChildVar);
  if (!mode) {
    GTEST_SKIP() << "runs only as ffmpeg_load_test's child process";
  }
  REQUIRE_FFMPEG_DIR(dir);
  {
    Engine engine(dir);
    ASSERT_EQ(engine.status(), IM_OK) << LastError();
    (void)Probe(IM_PROBE_FULL);
    ASSERT_NE(im::encode::ffmpeg::Loaded(), nullptr) << "the probe must have loaded FFmpeg for this check to count";

    // On a GPU-less runner the probe plans no vendor encoder (no hardware adapter), so nothing above tried the vendor
    // runtimes or libvpl's dispatcher. Open each vendor encoder here on a default device (the Basic Render Driver or
    // WARP): NVENC loads nvcuda.dll and nvEncodeAPI64.dll, AMF amfrt64.dll (through FFmpeg's win32_dlopen), and QSV's
    // session runs MFXLoad and MFXEnumImplementations, which read ONEVPL_* and search for libmfx64-gen.dll /
    // libmfxhw64.dll. They are expected to fail; each must have got as far as that load, which FFmpeg's log of the open
    // shows (the engine's open has its own LogCapture inside this one; nested captures each collect).
    std::unique_ptr<im::gpu::Device> device;
    ASSERT_EQ(im::gpu::Device::Create(im::gpu::DeviceRequest{}, &device), IM_OK) << LastError();
    struct VendorOpen {
      uint32_t kind;
      const char *codec;
      std::vector<std::string_view> needles; // one must be in FFmpeg's log of the open (compared without case)
    };
    const VendorOpen opens[] = {
        {IM_ENC_NVENC, "h264_nvenc", {"nvcuda", "nvEncodeAPI"}},
        // libvpl's failure is "Error creating a MFX session: <sts>" (error), logged only after MFXEnumImplementations
        // has run the DLL search. Not plain "mfx": qsv.c's "Error creating a MFX loader" / "... configuration" lines
        // come before any search. Its "Use Intel(R) oneVPL" line is verbose, below the level the engine sets.
        {IM_ENC_QSV, "h264_qsv", {"Error creating a MFX session"}},
        {IM_ENC_AMF, "h264_amf", {"amfrt64"}},
    };
    for (const VendorOpen &v : opens) {
      const im::session::OpenEncoderFn open = im::encode::OpenFunctionFor(v.kind);
      ASSERT_NE(open, nullptr) << v.codec;
      im::session::EncoderParams p;
      p.kind = v.kind;
      p.width = 1920;
      p.height = 1080;
      p.fps = 60;
      p.bitrate_bps = 8'000'000;
      p.device = device.get();
      std::unique_ptr<im::session::VideoEncoder> enc;
      im::encode::ffmpeg::LogCapture capture;
      const im_status st = open(p, &enc);
      const std::string detail = LastError();
      enc.reset();
      std::printf("vendor open %s: status %d, detail '%s'\n", v.codec, st, detail.c_str());
      ASSERT_EQ(detail.find("the FFmpeg build has no"), std::string::npos)
          << "the pinned FFmpeg build must have " << v.codec << " for this check to count: " << detail;
      bool attempted = st == IM_OK; // an open that worked loaded its runtime from where it may
      for (const std::string_view needle : v.needles) {
        attempted = attempted || capture.Contains(needle);
      }
      std::string how = "the engine's open";
      std::string ffmpeg_log = capture.text();
      if (!attempted && device->video_device() == nullptr &&
          detail.find("av_hwdevice_ctx_init(D3D11VA)") != std::string::npos) {
        // This device has no ID3D11VideoDevice (Device::Create retried without VIDEO_SUPPORT), so FFmpeg's
        // d3d11va_device_init fails (AVERROR_UNKNOWN, unlogged) before avcodec_open2, where the runtimes load. They
        // load there through the same loaders with or without a device: open the encoder in system memory instead.
        im::encode::ffmpeg::LogCapture direct;
        std::string err;
        const int r = im::encode::ffmpeg::OpenEncoderWithoutDeviceForTest(v.kind, &err);
        std::printf("vendor open %s: this device has no video support, so opened without a device: %d '%s'\n", v.codec,
                    r, err.c_str());
        how = "the open without a device (this device has no video support)";
        attempted = r == 0;
        for (const std::string_view needle : v.needles) {
          attempted = attempted || direct.Contains(needle);
        }
        ffmpeg_log = direct.text();
      }
      std::printf("vendor open %s: FFmpeg's log of %s:\n%s", v.codec, how.c_str(), ffmpeg_log.c_str());
      EXPECT_TRUE(attempted) << v.codec << " did not get as far as loading its runtime, so this check proves nothing; "
                             << "status " << st << ", detail '" << detail << "', FFmpeg's log of " << how << ":\n"
                             << ffmpeg_log;
      if (v.kind == IM_ENC_NVENC && attempted && ffmpeg_log.find("nvcuda.dll") != std::string::npos) {
        std::printf("vendor open h264_nvenc: nvEncodeAPI64.dll not asked for here (nvcuda.dll is absent and FFmpeg "
                    "loads it first); a PC with an NVIDIA driver checks that name\n");
      }
    }
  }
  // Only copies from the marker folders count (their .loaded files say so too): on a PC with the real runtimes,
  // System32's nvcuda.dll or amfrt64.dll is where they belong, and is listed but not counted.
  const std::optional<std::wstring> path_marker_dir = GetEnv(kPathMarkerDirVar);
  ASSERT_TRUE(path_marker_dir.has_value()) << "the parent sets " << Utf8(kPathMarkerDirVar);
  const std::vector<fs::path> marker_dirs = {fs::current_path(), fs::path(*path_marker_dir)};
  const auto in_marker_dir = [&](const fs::path &module) {
    for (const fs::path &d : marker_dirs) {
      std::error_code ec;
      if (fs::equivalent(module.parent_path(), d, ec)) {
        return true;
      }
    }
    return false;
  };
  std::string loaded;
  std::vector<HMODULE> modules(1024);
  DWORD needed = 0;
  if (EnumProcessModules(GetCurrentProcess(), modules.data(), static_cast<DWORD>(modules.size() * sizeof(HMODULE)),
                         &needed)) {
    modules.resize(std::min<std::size_t>(modules.size(), needed / sizeof(HMODULE)));
    for (const HMODULE m : modules) {
      wchar_t path[MAX_PATH] = {};
      if (GetModuleFileNameW(m, path, MAX_PATH) == 0) {
        continue;
      }
      const std::wstring base = fs::path(path).filename().wstring();
      for (const wchar_t *name : kVendorDlls) {
        if (_wcsicmp(base.c_str(), name) != 0) {
          continue;
        }
        if (in_marker_dir(fs::path(path))) {
          loaded += Utf8(path) + ";";
        } else {
          std::printf("%s is loaded from outside the marker folders (a real runtime): not counted\n",
                      Utf8(base).c_str());
        }
      }
    }
  } else {
    ADD_FAILURE() << "EnumProcessModules failed (" << GetLastError() << ")";
  }
  WriteResult(loaded.empty() ? "none" : "loaded " + loaded);
}

TEST(FfmpegLoad, VendorRuntimesAndLibvplNeverLoadFromCwdOrPath) {
  REQUIRE_FFMPEG_DIR(dir);
  const fs::path marker = fs::path(IM_TEST_DLL_DIR) / L"im-test-missing.dll";
  ASSERT_TRUE(fs::exists(marker)) << "dll_search_test's marker DLL is built with the tests";
  TempDir root;
  const fs::path cwd = root.path() / L"cwd";
  const fs::path path_dir = root.path() / L"path";
  fs::create_directories(cwd);
  fs::create_directories(path_dir);
  for (const wchar_t *name : kVendorDlls) {
    fs::copy_file(marker, cwd / name);
    fs::copy_file(marker, path_dir / name);
  }
  const fs::path result_file = root.path() / L"result.txt";
  DWORD exit_code = 0xFFFFFFFF;
  {
    const std::wstring old_path = GetEnv(L"PATH").value_or(L"");
    ScopedEnv child(kChildVar, L"vendor");
    ScopedEnv result(kResultVar, result_file.wstring());
    ScopedEnv markers(kPathMarkerDirVar, path_dir.wstring());
    ScopedEnv path(L"PATH", path_dir.wstring() + L";" + old_path);
    // libvpl's dispatcher reads these; im_init clears them before FFmpeg loads (04 §13.3 layer 4).
    ScopedEnv search(L"ONEVPL_SEARCH_PATH", path_dir.wstring());
    ScopedEnv priority(L"ONEVPL_PRIORITY_PATH", cwd.wstring());

    wchar_t exe[MAX_PATH] = {};
    ASSERT_GT(GetModuleFileNameW(nullptr, exe, MAX_PATH), 0u);
    std::wstring cmd = L"\"" + std::wstring(exe) + L"\" --gtest_filter=FfmpegLoadChild.Run";
    std::vector<wchar_t> cmdline(cmd.begin(), cmd.end());
    cmdline.push_back(L'\0');
    STARTUPINFOW si{};
    si.cb = sizeof si;
    si.dwFlags = STARTF_USESTDHANDLES;
    si.hStdInput = GetStdHandle(STD_INPUT_HANDLE);
    si.hStdOutput = GetStdHandle(STD_OUTPUT_HANDLE);
    si.hStdError = GetStdHandle(STD_ERROR_HANDLE);
    PROCESS_INFORMATION pi{};
    ASSERT_TRUE(CreateProcessW(exe, cmdline.data(), nullptr, nullptr, TRUE, 0, nullptr, cwd.c_str(), &si, &pi))
        << "CreateProcessW failed (" << GetLastError() << ")";
    if (WaitForSingleObject(pi.hProcess, 120000) != WAIT_OBJECT_0) {
      TerminateProcess(pi.hProcess, 99);
      WaitForSingleObject(pi.hProcess, 5000);
      ADD_FAILURE() << "the child did not finish within 120 s";
    }
    GetExitCodeProcess(pi.hProcess, &exit_code);
    CloseHandle(pi.hThread);
    CloseHandle(pi.hProcess);
  }
  std::string result;
  {
    std::ifstream in(result_file, std::ios::binary);
    std::getline(in, result);
  }
  EXPECT_EQ(exit_code, 0u) << result;
  EXPECT_EQ(result, "none") << "a vendor runtime or libvpl runtime loaded from a planted copy";
  for (const wchar_t *name : kVendorDlls) {
    const std::wstring flag = std::wstring(name) + L".loaded";
    // libvpl's dispatcher (not FFmpeg's loaders) searches for the libmfx names itself (04 §13.3 layer 4).
    const bool libvpl = std::wstring_view(name).starts_with(L"libmfx");
    const char *whose = libvpl ? " (libvpl's dispatcher: 04 §17's libvpl risk)" : " (FFmpeg's loader)";
    EXPECT_FALSE(fs::exists(cwd / flag)) << Utf8(name) << " loaded from the current directory" << whose;
    EXPECT_FALSE(fs::exists(path_dir / flag)) << Utf8(name) << " loaded from the PATH entry" << whose;
  }
}

} // namespace
