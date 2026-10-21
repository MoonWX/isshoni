// exports_test (docs/m2/04-windows-engine.md §18.2, README WA11): the C ABI's contract at the exports, which holds
// whatever the areas behind them implement. isshoni-media.def lists exactly the header's IM_API functions (both
// parsed); null and invalid arguments return their documented codes, with the 04 §5.7 token where one applies;
// struct_size below the v1.0 size is IM_ERR_ABI and above it is accepted; calls before im_init are not_initialized;
// JSON outputs follow snprintf; im_clock is monotonic and near the system time; im_abi_layout equals the golden file;
// a stalled engine worker makes a posted call return IM_ERR_TIMEOUT (engine_timeout) after its bound + 1 s and leaves
// its output alone; a refused audio argument never reaches the AudioEngine (README §3 item 15); a failed
// AudioEngine::Init doesn't fail im_init, and one that finishes after im_init gave up is shut down by the worker.
//
// The test links the engine's objects (im_engine), so it can reach the worker and the AudioEngine override directly.
#include <gtest/gtest.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <fstream>
#include <functional>
#include <future>
#include <memory>
#include <regex>
#include <set>
#include <sstream>
#include <stdexcept>
#include <string>
#include <thread>
#include <vector>

#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "engine/engine.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"
#include "isshoni_media.h"

namespace {

using std::chrono::duration_cast;
using std::chrono::milliseconds;
using std::chrono::steady_clock;

std::string ReadFile(const std::string &path) {
  std::ifstream in(path, std::ios::binary);
  std::stringstream ss;
  ss << in.rdbuf();
  return ss.str();
}

std::string NativeFile(const char *rel) { return std::string(IM_NATIVE_DIR) + "/" + rel; }

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  if (n < 0) {
    return "<im_last_error failed>";
  }
  return std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

// Checks a call's status and its detail: the 04 §5.7 token in front ("<token>: ") when token is set, and a fragment
// of the text when contains is set.
::testing::AssertionResult Fails(int32_t got, int32_t want, const char *token = nullptr,
                                 const char *contains = nullptr) {
  const std::string detail = LastError();
  if (got != want) {
    return ::testing::AssertionFailure() << "status " << got << ", want " << want << " (detail: " << detail << ")";
  }
  if (token != nullptr && detail.rfind(std::string(token) + ": ", 0) != 0) {
    return ::testing::AssertionFailure() << "detail \"" << detail << "\" doesn't start with \"" << token << ": \"";
  }
  if (contains != nullptr && detail.find(contains) == std::string::npos) {
    return ::testing::AssertionFailure() << "detail \"" << detail << "\" lacks \"" << contains << "\"";
  }
  return ::testing::AssertionSuccess();
}

im_init_params InitParams() {
  im_init_params p{};
  p.struct_size = sizeof p;
  p.abi_version = IM_ABI_VERSION;
  p.flags = IM_INIT_NO_AUDIO; // CI runners have no audio device; nothing here needs one
  p.log_level = IM_LOG_DEBUG;
  return p;
}

// A start config that passes every argument check: a pick token that doesn't exist (so no area ever starts a
// session from it), one layer, no audio.
im_start_config StartConfig() {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.source_id = "pick:999999";
  c.profile = IM_PROFILE_HIGH;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_AUTO;
  c.layer_count = 1;
  std::memcpy(c.layers[0].rid, "f", 2);
  c.layers[0].width = 1920;
  c.layers[0].height = 1080;
  c.layers[0].fps = 60;
  c.layers[0].bitrate_bps = 8000000;
  c.audio.mode = IM_AUDIO_MODE_OFF;
  return c;
}

im_layer_config LayerConfig() {
  im_layer_config l{};
  std::memcpy(l.rid, "q", 2);
  l.fps = 15;
  l.bitrate_bps = 300000;
  return l;
}

im_rule Rule(uint32_t kind, const char *value, const char *app_id = nullptr, const char *name = nullptr) {
  return im_rule{kind, IM_RULE_SRC_BUILTIN, value, app_id, name};
}

im_exclusions Exclusions(const im_rule *rules, uint32_t count) {
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH;
  ex.rules = rules;
  ex.rule_count = count;
  return ex;
}

template <class T> T Sized(uint32_t size) {
  T v{};
  v.struct_size = size;
  return v;
}

// Handles no session can have: 0, a slot index past the table, generation 0.
constexpr im_session kBadHandles[] = {0, 1, 0xFFFFFFFF00000001ull, 0x0000000100000000ull};

// The engine started for one test and shut down after it.
class Engine : public ::testing::Test {
protected:
  void SetUp() override {
    const im_init_params p = InitParams();
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  }
  void TearDown() override { im_shutdown(); }
};

// ---------------------------------------------------------------------------------------------- the export list

std::string StripCComments(const std::string &s) {
  std::string out;
  for (std::size_t i = 0; i < s.size(); ++i) {
    if (s.compare(i, 2, "/*") == 0) {
      const std::size_t end = s.find("*/", i + 2);
      i = end == std::string::npos ? s.size() : end + 1;
      out.push_back(' ');
    } else if (s.compare(i, 2, "//") == 0) {
      const std::size_t end = s.find('\n', i);
      i = end == std::string::npos ? s.size() : end - 1;
    } else {
      out.push_back(s[i]);
    }
  }
  return out;
}

std::vector<std::string> HeaderExports(const std::string &header) {
  const std::string text = StripCComments(header);
  const std::regex decl(R"(IM_API\s+[^;(]*?\b(im_[a-z0-9_]+)\s*\()");
  std::vector<std::string> names;
  for (auto it = std::sregex_iterator(text.begin(), text.end(), decl); it != std::sregex_iterator(); ++it) {
    names.push_back((*it)[1].str());
  }
  return names;
}

struct DefFile {
  std::string library;
  std::vector<std::string> exports;
};

DefFile ParseDef(const std::string &def) {
  DefFile out;
  std::istringstream in(def);
  std::string line;
  bool in_exports = false;
  while (std::getline(in, line)) {
    if (const std::size_t semi = line.find(';'); semi != std::string::npos) {
      line.resize(semi);
    }
    const auto first = line.find_first_not_of(" \t\r");
    if (first == std::string::npos) {
      continue;
    }
    line = line.substr(first, line.find_last_not_of(" \t\r") - first + 1);
    if (line.rfind("LIBRARY", 0) == 0) {
      out.library = line.substr(7);
      out.library.erase(0, out.library.find_first_not_of(" \t\""));
      out.library.erase(out.library.find_last_not_of(" \t\"") + 1);
      in_exports = false;
    } else if (line == "EXPORTS") {
      in_exports = true;
    } else if (in_exports) {
      out.exports.push_back(line);
    } else {
      out.exports.push_back("<unexpected line: " + line + ">");
    }
  }
  return out;
}

TEST(Exports, DefListsExactlyTheHeaderFunctions) {
  const std::string header = ReadFile(NativeFile("include/isshoni_media.h"));
  const std::string def = ReadFile(NativeFile("windows/isshoni-media.def"));
  ASSERT_FALSE(header.empty());
  ASSERT_FALSE(def.empty());

  const std::vector<std::string> want = HeaderExports(header);
  const DefFile got = ParseDef(def);
  EXPECT_EQ(got.library, "isshoni-media");
  ASSERT_EQ(want.size(), 39u) << "the header of v1.0 declares 39 IM_API functions";

  const std::set<std::string> want_set(want.begin(), want.end());
  const std::set<std::string> got_set(got.exports.begin(), got.exports.end());
  EXPECT_EQ(want_set.size(), want.size()) << "the header declares a function twice";
  EXPECT_EQ(got_set.size(), got.exports.size()) << "the .def lists a name twice";
  for (const std::string &name : want_set) {
    EXPECT_TRUE(got_set.count(name) > 0) << name << " is declared IM_API but not exported";
  }
  for (const std::string &name : got_set) {
    EXPECT_TRUE(want_set.count(name) > 0) << name << " is exported but not an IM_API function of the header";
  }
}

TEST(Exports, VersionIsV1) {
  EXPECT_EQ(im_version(), IM_ABI_VERSION);
  EXPECT_EQ(im_version() >> 16, 1u);
}

// ---------------------------------------------------------------------------------------------- im_init

TEST(Exports, InitChecksItsArguments) {
  im_shutdown();
  EXPECT_TRUE(Fails(im_init(nullptr), IM_ERR_INVALID_ARG));

  im_init_params p = InitParams();
  p.struct_size = sizeof p - 1;
  EXPECT_TRUE(Fails(im_init(&p), IM_ERR_ABI, "abi"));

  for (const uint32_t abi : {0u, 0x00020000u, 0x00000001u}) {
    p = InitParams();
    p.abi_version = abi;
    EXPECT_TRUE(Fails(im_init(&p), IM_ERR_ABI, "abi")) << "abi_version 0x" << std::hex << abi;
  }

  for (const char *dir : {"relative\\ffmpeg", "/usr/lib", "C:", "\\single", "\xff\xfe"}) {
    p = InitParams();
    p.ffmpeg_dir = dir;
    EXPECT_TRUE(Fails(im_init(&p), IM_ERR_INVALID_ARG, nullptr, "ffmpeg_dir")) << dir;
  }
  p = InitParams();
  p.openh264_path = "openh264-2.6.0-win64.dll";
  EXPECT_TRUE(Fails(im_init(&p), IM_ERR_INVALID_ARG, nullptr, "openh264_path"));
  for (const uint32_t level : {IM_LOG_DEBUG + 1, 0xFFFFFFFFu}) {
    p = InitParams();
    p.log_level = level;
    EXPECT_TRUE(Fails(im_init(&p), IM_ERR_INVALID_ARG, nullptr, "log_level")) << level;
  }
  EXPECT_FALSE(im::engine::Initialized()) << "a refused im_init left the engine initialized";
}

TEST(Exports, InitAcceptsNewerCallersAndRunsOnce) {
  im_shutdown();
  // A newer caller's bigger struct (and a newer minor) is accepted; empty paths mean none.
  struct {
    im_init_params p;
    uint64_t newer_field;
  } big{};
  big.p = InitParams();
  big.p.struct_size = sizeof big;
  big.p.abi_version = IM_ABI_VERSION + 3;
  big.p.ffmpeg_dir = "";
  big.p.openh264_path = "";
  ASSERT_EQ(im_init(&big.p), IM_OK) << LastError();
  EXPECT_TRUE(im::engine::Initialized());

  const im_init_params p = InitParams();
  EXPECT_TRUE(Fails(im_init(&p), IM_ERR_BAD_STATE, nullptr, "already initialized"));
  im_shutdown();
  EXPECT_FALSE(im::engine::Initialized());
  im_shutdown(); // a second shutdown is a no-op

  im_init_params abs = InitParams();
  abs.ffmpeg_dir = "C:\\Program Files\\isshoni";
  abs.openh264_path = "\\\\server\\share\\openh264.dll";
  EXPECT_EQ(im_init(&abs), IM_OK) << LastError(); // im_init may follow im_shutdown
  im_shutdown();
}

// ---------------------------------------------------------------------------------------------- before im_init

TEST(Exports, CallsBeforeInitAreNotInitialized) {
  im_shutdown();
  ASSERT_FALSE(im::engine::Initialized());

  im_caps caps = Sized<im_caps>(sizeof(im_caps));
  im_pick_params pick = Sized<im_pick_params>(sizeof(im_pick_params));
  im_source_info source = Sized<im_source_info>(sizeof(im_source_info));
  const im_rule rule = Rule(IM_RULE_APP_EXCLUDE, "discord.exe");
  const im_exclusions ex = Exclusions(&rule, 1);
  im_audio_levels levels = Sized<im_audio_levels>(sizeof(im_audio_levels));
  const im_start_config cfg = StartConfig();
  im_session session = 7;
  im_session_info info = Sized<im_session_info>(sizeof(im_session_info));
  im_packet pkt = Sized<im_packet>(sizeof(im_packet));
  const im_layer_config layer = LayerConfig();
  int64_t onsets[2];
  int window = 0;

  const std::vector<std::pair<const char *, std::function<int32_t()>>> calls = {
      {"im_set_openh264_path", [] { return im_set_openh264_path(nullptr); }},
      {"im_probe", [&] { return im_probe(IM_PROBE_QUICK, &caps); }},
      {"im_list_sources", [] { return im_list_sources(0, nullptr, 0); }},
      {"im_pick_source", [&] { return im_pick_source(&pick, &source); }},
      {"im_cancel_pick", [] { return im_cancel_pick(); }},
      {"im_list_audio_apps", [] { return im_list_audio_apps(0, nullptr, 0); }},
      {"im_audio_watch", [&] { return im_audio_watch(&ex); }},
      {"im_audio_unwatch", [] { return im_audio_unwatch(); }},
      {"im_set_exclusions", [&] { return im_set_exclusions(&ex); }},
      {"im_audio_set_mode", [] { return im_audio_set_mode(IM_AUDIO_MODE_OFF); }},
      {"im_audio_set_muted", [] { return im_audio_set_muted(1); }},
      {"im_audio_levels_get", [&] { return im_audio_levels_get(&levels); }},
      {"im_audio_sound_check", [] { return im_audio_sound_check(0, 1000, nullptr, 0); }},
      {"im_start", [&] { return im_start(&cfg, &session, &info); }},
      {"im_get_session_info", [&] { return im_get_session_info(1, &info); }},
      {"im_next_packet", [&] { return im_next_packet(1, 0, &pkt); }},
      {"im_request_keyframe", [] { return im_request_keyframe(1, IM_ALL_LAYERS); }},
      {"im_set_bitrate", [] { return im_set_bitrate(1, 0, 4000000); }},
      {"im_set_layer_active", [] { return im_set_layer_active(1, 0, 1); }},
      {"im_update_layer", [&] { return im_update_layer(1, 1, &layer); }},
      {"im_set_profile", [] { return im_set_profile(1, IM_PROFILE_CB); }},
      {"im_set_content_hint", [] { return im_set_content_hint(1, IM_CONTENT_DETAIL); }},
      {"im_set_paused", [] { return im_set_paused(1, 1); }},
      {"im_set_audio_bitrate", [] { return im_set_audio_bitrate(1, 256000); }},
      {"im_get_stats", [] { return im_get_stats(1, nullptr, 0); }},
      {"im_stop", [] { return im_stop(1); }},
      {"im_protect_window", [&] { return im_protect_window(&window, 0); }},
      {"im_test_render_tone", [] { return im_test_render_tone(nullptr, 440.0f, 0.5f, 100); }},
      {"im_test_render_bursts", [&] { return im_test_render_bursts(nullptr, 2000.0f, 0.5f, 5, 1000, 2, onsets, 2); }},
  };
  for (const auto &[name, call] : calls) {
    EXPECT_TRUE(Fails(call(), IM_ERR_BAD_STATE, "not_initialized", name)) << name;
  }
  EXPECT_EQ(session, 0u) << "a failed im_start leaves *out at 0";
  im_release_source("pick:1"); // nothing to forget, no failure

  // The calls that work before im_init.
  EXPECT_EQ(im_version(), IM_ABI_VERSION);
  EXPECT_GT(im_build_info(nullptr, 0), 0) << LastError();
  im_clock_sample clock = Sized<im_clock_sample>(sizeof(im_clock_sample));
  EXPECT_EQ(im_clock(&clock), IM_OK) << LastError();
  EXPECT_GT(im_abi_layout(nullptr, 0), 0) << LastError();
  std::vector<char> logs(static_cast<std::size_t>(im::log::kMaxLineBytes) + 1);
  EXPECT_GE(im_read_logs(logs.data(), static_cast<int32_t>(logs.size())), 0) << LastError();
  const uint8_t opus[3] = {0xF8, 0xFF, 0xFE};
  float pcm[960 * 2];
  EXPECT_NE(im_test_opus_decode(opus, 3, pcm, 960), IM_ERR_BAD_STATE) << LastError();
}

// ---------------------------------------------------------------------------------------------- struct_size

TEST_F(Engine, StructSizeBelowV1IsAbi) {
  auto clock = Sized<im_clock_sample>(31);
  EXPECT_TRUE(Fails(im_clock(&clock), IM_ERR_ABI, "abi", "im_clock"));

  auto caps = Sized<im_caps>(1567);
  EXPECT_TRUE(Fails(im_probe(IM_PROBE_QUICK, &caps), IM_ERR_ABI, "abi", "im_probe"));

  auto pick = Sized<im_pick_params>(23);
  auto source = Sized<im_source_info>(sizeof(im_source_info));
  EXPECT_TRUE(Fails(im_pick_source(&pick, &source), IM_ERR_ABI, "abi", "params"));
  pick.struct_size = sizeof pick;
  source.struct_size = 439;
  EXPECT_TRUE(Fails(im_pick_source(&pick, &source), IM_ERR_ABI, "abi", "out"));

  const im_rule rule = Rule(IM_RULE_APP_EXCLUDE, "discord.exe");
  im_exclusions ex = Exclusions(&rule, 1);
  ex.struct_size = 23;
  EXPECT_TRUE(Fails(im_audio_watch(&ex), IM_ERR_ABI, "abi", "im_audio_watch"));
  EXPECT_TRUE(Fails(im_set_exclusions(&ex), IM_ERR_ABI, "abi", "im_set_exclusions"));

  auto levels = Sized<im_audio_levels>(5151);
  EXPECT_TRUE(Fails(im_audio_levels_get(&levels), IM_ERR_ABI, "abi", "im_audio_levels_get"));

  im_session s = 5;
  auto info = Sized<im_session_info>(sizeof(im_session_info));
  im_start_config cfg = StartConfig();
  cfg.struct_size = 223;
  EXPECT_TRUE(Fails(im_start(&cfg, &s, &info), IM_ERR_ABI, "abi", "cfg"));
  cfg = StartConfig();
  info.struct_size = 183;
  EXPECT_TRUE(Fails(im_start(&cfg, &s, &info), IM_ERR_ABI, "abi", "info"));
  info.struct_size = sizeof info;
  cfg.exclusions = &ex; // ex.struct_size is 23
  EXPECT_TRUE(Fails(im_start(&cfg, &s, &info), IM_ERR_ABI, "abi", "exclusions"));
  EXPECT_EQ(s, 0u);

  info.struct_size = 183;
  EXPECT_TRUE(Fails(im_get_session_info(1, &info), IM_ERR_ABI, "abi", "im_get_session_info"));
  auto pkt = Sized<im_packet>(71);
  EXPECT_TRUE(Fails(im_next_packet(1, 0, &pkt), IM_ERR_ABI, "abi", "im_next_packet"));
}

TEST_F(Engine, StructSizeAboveV1IsAccepted) {
  // An output struct from a newer caller: written up to the engine's size, struct_size set to the bytes written, the
  // rest untouched.
  struct {
    im_clock_sample s;
    uint8_t newer[16];
  } clock;
  std::memset(&clock, 0xAB, sizeof clock);
  clock.s.struct_size = sizeof clock;
  ASSERT_EQ(im_clock(&clock.s), IM_OK) << LastError();
  EXPECT_EQ(clock.s.struct_size, 32u);
  for (const uint8_t b : clock.newer) {
    EXPECT_EQ(b, 0xAB) << "im_clock wrote past its own struct";
  }

  // Input structs from a newer caller pass the checks (what the area answers then is the area's business).
  struct {
    im_start_config c;
    uint8_t newer[32];
  } big{};
  big.c = StartConfig();
  big.c.struct_size = sizeof big;
  im_session s = 0;
  const int32_t st = im_start(&big.c, &s, nullptr);
  EXPECT_NE(st, IM_ERR_ABI) << LastError();
  EXPECT_NE(st, IM_ERR_INVALID_ARG) << LastError();
  if (st == IM_OK) {
    im_stop(s);
  }

  const im_rule rule = Rule(IM_RULE_APP_EXCLUDE, "discord.exe");
  struct {
    im_exclusions ex;
    uint64_t newer;
  } ex{};
  ex.ex = Exclusions(&rule, 1);
  ex.ex.struct_size = sizeof ex;
  const int32_t set = im_set_exclusions(&ex.ex);
  EXPECT_NE(set, IM_ERR_ABI) << LastError();
  EXPECT_NE(set, IM_ERR_INVALID_ARG) << LastError();
}

// ---------------------------------------------------------------------------------------------- null arguments

TEST_F(Engine, NullArgumentsAreInvalid) {
  char buf[16];
  EXPECT_TRUE(Fails(im_clock(nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_probe(IM_PROBE_QUICK, nullptr), IM_ERR_INVALID_ARG));
  auto pick = Sized<im_pick_params>(sizeof(im_pick_params));
  auto source = Sized<im_source_info>(sizeof(im_source_info));
  EXPECT_TRUE(Fails(im_pick_source(nullptr, &source), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_pick_source(&pick, nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_watch(nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_set_exclusions(nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_levels_get(nullptr), IM_ERR_INVALID_ARG));

  im_start_config cfg = StartConfig();
  im_session s = 3;
  auto info = Sized<im_session_info>(sizeof(im_session_info));
  EXPECT_TRUE(Fails(im_start(nullptr, &s, &info), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_start(&cfg, nullptr, &info), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_get_session_info(1, nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_next_packet(1, 0, nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_update_layer(1, 0, nullptr), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_protect_window(nullptr, 0), IM_ERR_INVALID_ARG));

  // JSON outputs: a NULL buffer with cap > 0, or a negative cap.
  EXPECT_TRUE(Fails(im_build_info(nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_build_info(buf, -1), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_list_sources(0, nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_list_audio_apps(0, nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_sound_check(0, 1000, nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_get_stats(1, nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_abi_layout(nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_abi_layout(buf, -5), IM_ERR_INVALID_ARG));

  std::vector<char> logs(static_cast<std::size_t>(im::log::kMaxLineBytes) + 1);
  EXPECT_TRUE(Fails(im_read_logs(nullptr, static_cast<int32_t>(logs.size())), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_read_logs(logs.data(), im::log::kMaxLineBytes), IM_ERR_INVALID_ARG, nullptr, "8192"));

  float pcm[16];
  const uint8_t opus[3] = {0xF8, 0xFF, 0xFE};
  EXPECT_TRUE(Fails(im_test_opus_decode(nullptr, 3, pcm, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_test_opus_decode(opus, 0, pcm, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_test_opus_decode(opus, 3, nullptr, 8), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_test_opus_decode(opus, 3, pcm, 0), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_test_render_bursts(nullptr, 2000.0f, 0.5f, 5, 1000, 2, nullptr, 2), IM_ERR_INVALID_ARG));

  im_release_source(nullptr); // ignored
}

// ---------------------------------------------------------------------------------------------- invalid arguments

TEST_F(Engine, AudioArgumentsAreChecked) {
  EXPECT_TRUE(Fails(im_audio_set_mode(IM_AUDIO_MODE_EVERYTHING), IM_ERR_INVALID_ARG, nullptr, "EVERYTHING"));
  EXPECT_TRUE(Fails(im_audio_set_mode(IM_AUDIO_MODE_TEST_TONE), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_set_mode(7), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_set_muted(2), IM_ERR_INVALID_ARG, nullptr, "muted"));
  EXPECT_TRUE(Fails(im_audio_sound_check(0, 999, nullptr, 0), IM_ERR_INVALID_ARG, nullptr, "timeout_ms"));
  EXPECT_TRUE(Fails(im_audio_sound_check(0, 10001, nullptr, 0), IM_ERR_INVALID_ARG, nullptr, "timeout_ms"));

  const std::string long_value(IM_MAX_RULE_VALUE, 'a');
  const std::string long_app_id(IM_APP_ID_LEN, 'a');
  struct Case {
    const char *name;
    im_rule rule;
    const char *contains;
  };
  const Case cases[] = {
      {"unknown kind", Rule(IM_RULE_WEBVIEW_DATA_DIR + 1, "x.exe"), "rules[0].kind"},
      {"unknown source", im_rule{IM_RULE_APP_EXCLUDE, IM_RULE_SRC_DATA + 1, "x.exe", nullptr, nullptr},
       "rules[0].source"},
      {"NULL value", Rule(IM_RULE_APP_EXCLUDE, nullptr), "rules[0].value"},
      {"empty value", Rule(IM_RULE_APP_EXCLUDE, ""), "rules[0].value"},
      {"long value", Rule(IM_RULE_APP_EXCLUDE, long_value.c_str()), "rules[0].value"},
      {"value not UTF-8", Rule(IM_RULE_APP_EXCLUDE, "\xc3\x28.exe"), "rules[0].value"},
      {"pid with a letter", Rule(IM_RULE_INSTANCE, "12a"), "pid"},
      {"pid 0", Rule(IM_RULE_INSTANCE, "0"), "pid"},
      {"pid too big", Rule(IM_RULE_INSTANCE, "4294967296"), "pid"},
      {"app_id upper case", Rule(IM_RULE_APP_EXCLUDE, "discord.exe", "Discord"), "rules[0].app_id"},
      {"app_id too long", Rule(IM_RULE_APP_EXCLUDE, "discord.exe", long_app_id.c_str()), "rules[0].app_id"},
      {"name not UTF-8", Rule(IM_RULE_APP_EXCLUDE, "discord.exe", nullptr, "\xff"), "rules[0].name"},
  };
  for (const Case &c : cases) {
    const im_exclusions ex = Exclusions(&c.rule, 1);
    EXPECT_TRUE(Fails(im_set_exclusions(&ex), IM_ERR_INVALID_ARG, nullptr, c.contains)) << c.name;
    EXPECT_TRUE(Fails(im_audio_watch(&ex), IM_ERR_INVALID_ARG, nullptr, c.contains)) << c.name;
  }

  // The checks pass valid rules: an app_id of the allowed characters, a pid, NULL and "" app ids.
  const im_rule good[] = {Rule(IM_RULE_APP_EXCLUDE, "discord.exe", "voice:discord_1.x-y"),
                          Rule(IM_RULE_INSTANCE, "1234", ""), Rule(IM_RULE_BROWSER, "chrome.exe", nullptr, "Chrome")};
  const im_exclusions ok = Exclusions(good, 3);
  const int32_t st = im_set_exclusions(&ok);
  EXPECT_NE(st, IM_ERR_INVALID_ARG) << LastError();
  EXPECT_NE(st, IM_ERR_ABI) << LastError();

  std::vector<im_rule> many(IM_MAX_RULES + 1, Rule(IM_RULE_APP_EXCLUDE, "a.exe"));
  const im_exclusions too_many = Exclusions(many.data(), IM_MAX_RULES + 1);
  EXPECT_TRUE(Fails(im_set_exclusions(&too_many), IM_ERR_INVALID_ARG, nullptr, "rule_count"));
  const im_exclusions no_rules = Exclusions(nullptr, 1);
  EXPECT_TRUE(Fails(im_set_exclusions(&no_rules), IM_ERR_INVALID_ARG, nullptr, "rules is NULL"));
}

TEST_F(Engine, StartConfigIsChecked) {
  im_session s = 9;
  const im_rule bad_rule = Rule(IM_RULE_APP_EXCLUDE, "");
  const im_exclusions bad_ex = Exclusions(&bad_rule, 1);
  const std::string long_id = "pick:" + std::string(59, '7'); // 64 bytes: no room for the NUL in im_source_info.id
  struct Case {
    const char *name;
    std::function<void(im_start_config &)> edit;
    const char *contains;
  };
  const std::vector<Case> cases = {
      {"too many layers", [](im_start_config &c) { c.layer_count = IM_MAX_LAYERS + 1; }, "layer_count"},
      {"a source without layers", [](im_start_config &c) { c.layer_count = 0; }, "layer_count is 0"},
      {"layers without a source", [](im_start_config &c) { c.source_id = nullptr; }, "source_id is NULL"},
      {"empty source id", [](im_start_config &c) { c.source_id = ""; }, "source_id"},
      {"long source id", [&](im_start_config &c) { c.source_id = long_id.c_str(); }, "source_id"},
      {"unknown profile", [](im_start_config &c) { c.profile = 0x4D00; }, "profile"},
      {"unknown content hint", [](im_start_config &c) { c.content_hint = IM_CONTENT_DETAIL + 1; }, "content_hint"},
      {"unknown encoder", [](im_start_config &c) { c.encoder = IM_ENC_GSTREAMER + 1; }, "encoder"},
      {"encoder 101", [](im_start_config &c) { c.encoder = IM_ENC_FAKE + 1; }, "encoder"},
      {"empty rid", [](im_start_config &c) { c.layers[0].rid[0] = '\0'; }, "rid"},
      {"rid not terminated", [](im_start_config &c) { std::memcpy(c.layers[0].rid, "abcd", 4); }, "rid"},
      {"fps 0", [](im_start_config &c) { c.layers[0].fps = 0; }, "fps"},
      {"fps 241", [](im_start_config &c) { c.layers[0].fps = 241; }, "fps"},
      {"gop 999", [](im_start_config &c) { c.layers[0].gop_ms = 999; }, "gop_ms"},
      {"gop 10001", [](im_start_config &c) { c.layers[0].gop_ms = 10001; }, "gop_ms"},
      {"a repeated rid",
       [](im_start_config &c) {
         c.layer_count = 2;
         c.layers[1] = c.layers[0];
       },
       "repeats"},
      {"audio EVERYTHING", [](im_start_config &c) { c.audio.mode = IM_AUDIO_MODE_EVERYTHING; }, "EVERYTHING"},
      {"unknown audio mode", [](im_start_config &c) { c.audio.mode = 3; }, "audio.mode"},
      {"audio bitrate low", [](im_start_config &c) { c.audio.bitrate_bps = 31999; }, "bitrate_bps"},
      {"audio bitrate high", [](im_start_config &c) { c.audio.bitrate_bps = 510001; }, "bitrate_bps"},
      {"10 ms audio frames", [](im_start_config &c) { c.audio.frame_ms = 10; }, "frame_ms"},
      {"an invalid exclusion rule", [&](im_start_config &c) { c.exclusions = &bad_ex; }, "exclusions->rules[0]"},
  };
  for (const Case &c : cases) {
    im_start_config cfg = StartConfig();
    c.edit(cfg);
    s = 9;
    EXPECT_TRUE(Fails(im_start(&cfg, &s, nullptr), IM_ERR_INVALID_ARG, nullptr, c.contains)) << c.name;
    EXPECT_EQ(s, 0u) << c.name;
  }
#if !(defined(IM_TEST_HOOKS) && IM_TEST_HOOKS)
  im_start_config fake = StartConfig();
  fake.encoder = IM_ENC_FAKE;
  EXPECT_TRUE(Fails(im_start(&fake, &s, nullptr), IM_ERR_INVALID_ARG, nullptr, "IM_ENC_FAKE"));
#endif

  // An audio-only session (no source, no layers) passes the checks.
  im_start_config audio_only = StartConfig();
  audio_only.source_id = nullptr;
  audio_only.layer_count = 0;
  audio_only.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  const int32_t st = im_start(&audio_only, &s, nullptr);
  EXPECT_NE(st, IM_ERR_INVALID_ARG) << LastError();
  if (st == IM_OK) {
    im_stop(s);
  }
}

TEST_F(Engine, OtherArgumentsAreChecked) {
  auto pick = Sized<im_pick_params>(sizeof(im_pick_params));
  auto source = Sized<im_source_info>(sizeof(im_source_info));
  pick.timeout_ms = 600001;
  EXPECT_TRUE(Fails(im_pick_source(&pick, &source), IM_ERR_INVALID_ARG, nullptr, "timeout_ms"));

  auto pkt = Sized<im_packet>(sizeof(im_packet));
  EXPECT_TRUE(Fails(im_next_packet(1, 1001, &pkt), IM_ERR_INVALID_ARG, nullptr, "timeout_ms"));

  EXPECT_TRUE(Fails(im_request_keyframe(1, IM_MAX_LAYERS), IM_ERR_INVALID_ARG, nullptr, "layer"));
  EXPECT_TRUE(Fails(im_set_bitrate(1, IM_ALL_LAYERS, 1000000), IM_ERR_INVALID_ARG, nullptr, "layer"));
  EXPECT_TRUE(Fails(im_set_bitrate(1, IM_NO_LAYER, 1000000), IM_ERR_INVALID_ARG, nullptr, "layer"));
  // 04 §6.4's ranges (§9.5): layer 0 100 kbps..50 Mbps, the other layers 50 kbps..2 Mbps; 0 is no bitrate.
  for (const uint32_t bps : {0u, 99999u, 50000001u, 0xFFFFFFFFu}) {
    EXPECT_TRUE(Fails(im_set_bitrate(1, 0, bps), IM_ERR_INVALID_ARG, nullptr, "bps")) << "layer 0, " << bps;
  }
  for (const uint32_t upper_layer : {1u, 2u}) {
    for (const uint32_t bps : {0u, 49999u, 2000001u}) {
      EXPECT_TRUE(Fails(im_set_bitrate(1, upper_layer, bps), IM_ERR_INVALID_ARG, nullptr, "bps"))
          << "layer " << upper_layer << ", " << bps;
    }
  }
  EXPECT_TRUE(Fails(im_set_layer_active(1, 3, 1), IM_ERR_INVALID_ARG, nullptr, "layer"));
  EXPECT_TRUE(Fails(im_set_layer_active(1, 0, 2), IM_ERR_INVALID_ARG, nullptr, "active"));
  im_layer_config layer = LayerConfig();
  EXPECT_TRUE(Fails(im_update_layer(1, IM_ALL_LAYERS, &layer), IM_ERR_INVALID_ARG, nullptr, "layer"));
  layer.fps = 0;
  EXPECT_TRUE(Fails(im_update_layer(1, 1, &layer), IM_ERR_INVALID_ARG, nullptr, "fps"));
  EXPECT_TRUE(Fails(im_set_profile(1, 0), IM_ERR_INVALID_ARG, nullptr, "profile"));
  EXPECT_TRUE(Fails(im_set_profile(1, 0x4200), IM_ERR_INVALID_ARG, nullptr, "profile"));
  EXPECT_TRUE(Fails(im_set_content_hint(1, IM_CONTENT_DETAIL + 1), IM_ERR_INVALID_ARG, nullptr, "hint"));
  EXPECT_TRUE(Fails(im_set_paused(1, 2), IM_ERR_INVALID_ARG, nullptr, "paused"));
  EXPECT_TRUE(Fails(im_set_audio_bitrate(1, 31999), IM_ERR_INVALID_ARG, nullptr, "32000..510000"));
  EXPECT_TRUE(Fails(im_set_audio_bitrate(1, 510001), IM_ERR_INVALID_ARG, nullptr, "32000..510000"));

  for (const char *path : {"openh264.dll", "codecs\\openh264.dll", "/tmp/openh264.dll", "\xe9.dll"}) {
    EXPECT_TRUE(Fails(im_set_openh264_path(path), IM_ERR_INVALID_ARG, nullptr, "path")) << path;
  }
}

TEST_F(Engine, UnknownSessionHandlesAreInvalid) {
  auto info = Sized<im_session_info>(sizeof(im_session_info));
  auto pkt = Sized<im_packet>(sizeof(im_packet));
  const im_layer_config layer = LayerConfig();
  char buf[64];
  for (const im_session s : kBadHandles) {
    EXPECT_TRUE(Fails(im_get_session_info(s, &info), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_next_packet(s, 0, &pkt), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_request_keyframe(s, IM_ALL_LAYERS), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_set_bitrate(s, 0, 4000000), IM_ERR_INVALID_ARG)) << s;
    // The ends of 04 §6.4's ranges pass the check and reach the session area, which refuses the handle.
    const std::pair<uint32_t, uint32_t> range_ends[] = {{0u, 100000u}, {0u, 50000000u}, {1u, 50000u}, {2u, 2000000u}};
    for (const auto &end : range_ends) {
      EXPECT_TRUE(Fails(im_set_bitrate(s, end.first, end.second), IM_ERR_INVALID_ARG)) << s;
      EXPECT_EQ(LastError().find("bps"), std::string::npos) << "layer " << end.first << ", " << end.second;
    }
    EXPECT_TRUE(Fails(im_set_layer_active(s, 1, 0), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_update_layer(s, 1, &layer), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_set_profile(s, IM_PROFILE_CB), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_set_content_hint(s, IM_CONTENT_MOTION), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_set_paused(s, 1), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_set_audio_bitrate(s, 128000), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_get_stats(s, buf, static_cast<int32_t>(sizeof buf)), IM_ERR_INVALID_ARG)) << s;
    EXPECT_TRUE(Fails(im_stop(s), IM_ERR_INVALID_ARG)) << s;
  }
}

// ---------------------------------------------------------------------------------------------- JSON outputs

// The snprintf rule of 04 §5.1 on one JSON export: the full length n always; min(n, cap - 1) bytes and a NUL when
// cap > 0; nothing written past cap.
void ExpectSnprintf(const char *name, const std::function<int32_t(char *, int32_t)> &fn) {
  SCOPED_TRACE(name);
  const int32_t n = fn(nullptr, 0);
  ASSERT_GT(n, 10) << LastError();

  std::vector<char> full(static_cast<std::size_t>(n) + 2, 'x');
  EXPECT_EQ(fn(full.data(), n + 1), n);
  EXPECT_EQ(full[static_cast<std::size_t>(n)], '\0');
  EXPECT_EQ(full[static_cast<std::size_t>(n) + 1], 'x');
  const std::string doc(full.data(), static_cast<std::size_t>(n));
  EXPECT_EQ(std::strlen(full.data()), static_cast<std::size_t>(n));
  EXPECT_EQ(doc.front(), '{');
  EXPECT_EQ(doc.back(), '}');

  std::vector<char> exact(static_cast<std::size_t>(n), 'x');
  EXPECT_EQ(fn(exact.data(), n), n) << "cap == n is too small by the NUL: the caller retries with n + 1";
  EXPECT_EQ(std::string(exact.data()), doc.substr(0, static_cast<std::size_t>(n) - 1));

  char small[11];
  std::memset(small, 'x', sizeof small);
  EXPECT_EQ(fn(small, 10), n);
  EXPECT_EQ(small[9], '\0');
  EXPECT_EQ(small[10], 'x');
  EXPECT_EQ(std::string(small), doc.substr(0, 9));

  char one = 'x';
  EXPECT_EQ(fn(&one, 1), n);
  EXPECT_EQ(one, '\0');
}

TEST(Exports, JsonOutputsFollowSnprintf) {
  ExpectSnprintf("im_build_info", [](char *b, int32_t c) { return im_build_info(b, c); });
  ExpectSnprintf("im_abi_layout", [](char *b, int32_t c) { return im_abi_layout(b, c); });
}

TEST(Exports, LastErrorFollowsSnprintfAndStaysOnItsThread) {
  ASSERT_EQ(im_init(nullptr), IM_ERR_INVALID_ARG);
  const std::string detail = LastError();
  ASSERT_FALSE(detail.empty());
  const auto n = static_cast<int32_t>(detail.size());

  // Reading doesn't clear it, and the snprintf rule holds.
  EXPECT_EQ(im_last_error(nullptr, 0), n);
  char small[6];
  EXPECT_EQ(im_last_error(small, static_cast<int32_t>(sizeof small)), n);
  EXPECT_EQ(std::string(small), detail.substr(0, 5));
  EXPECT_TRUE(Fails(im_last_error(small, -1), IM_ERR_INVALID_ARG));
  EXPECT_EQ(LastError(), detail);

  // Another thread has its own (empty) detail.
  std::string other = "unset";
  std::thread([&] { other = LastError(); }).join();
  EXPECT_EQ(other, "");

  // The next export clears it.
  EXPECT_GT(im_build_info(nullptr, 0), 0);
  EXPECT_EQ(LastError(), "");
}

TEST_F(Engine, BuildInfo) {
  std::vector<char> buf(4096);
  const int32_t n = im_build_info(buf.data(), static_cast<int32_t>(buf.size()));
  ASSERT_GT(n, 0);
  ASSERT_LT(static_cast<std::size_t>(n), buf.size());
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  for (const char *field :
       {"\"engine\":\"", "\"abi\":65536,", "\"commit\":", "\"arch\":\"", "\"compiler\":\"",
        "\"ffmpeg\":{\"loaded\":", "\"dir\":\".\"", "\"tag\":", "\"opus\":", "\"openh264\":{\"loaded\":"}) {
    EXPECT_NE(doc.find(field), std::string::npos) << field << " missing from " << doc;
  }
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  EXPECT_NE(doc.find("\"testHooks\":true"), std::string::npos) << doc;
#else
  EXPECT_NE(doc.find("\"testHooks\":false"), std::string::npos) << doc;
#endif
#if defined(_WIN32)
  EXPECT_NE(doc.find("\"os\":\"windows\""), std::string::npos) << doc;
  EXPECT_EQ(doc.find(":\\"), std::string::npos) << "an absolute path in " << doc;
#endif
}

#if defined(_WIN32)
TEST(Exports, BuildInfoFfmpegDirIsRelative) {
  im_shutdown();
  const std::string module_dir = im::engine::os::ModuleDirectory();
  ASSERT_FALSE(module_dir.empty());
  const std::string ffmpeg_dir = module_dir + "\\ffmpeg\\bin";
  im_init_params p = InitParams();
  p.ffmpeg_dir = ffmpeg_dir.c_str();
  ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  std::vector<char> buf(4096);
  const int32_t n = im_build_info(buf.data(), static_cast<int32_t>(buf.size()));
  im_shutdown();
  ASSERT_GT(n, 0);
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  EXPECT_NE(doc.find("\"dir\":\"ffmpeg/bin\""), std::string::npos) << doc;
  EXPECT_EQ(doc.find(module_dir), std::string::npos) << "the absolute path leaked into " << doc;
}

TEST(Exports, BuildInfoFfmpegDirOutsideTheDllFolderIsNull) {
  im_shutdown();
  const std::string module_dir = im::engine::os::ModuleDirectory();
  ASSERT_FALSE(module_dir.empty());
  const std::size_t sep = module_dir.find_last_of("\\/");
  ASSERT_NE(sep, std::string::npos);
  // A sibling of the DLL's folder: neither its name nor a ".." may reach the document.
  const std::string ffmpeg_dir = module_dir.substr(0, sep) + "\\im-other-ffmpeg";
  im_init_params p = InitParams();
  p.ffmpeg_dir = ffmpeg_dir.c_str();
  ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  std::vector<char> buf(4096);
  const int32_t n = im_build_info(buf.data(), static_cast<int32_t>(buf.size()));
  im_shutdown();
  ASSERT_GT(n, 0);
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  EXPECT_NE(doc.find("\"dir\":null"), std::string::npos) << doc;
  EXPECT_EQ(doc.find("im-other-ffmpeg"), std::string::npos) << "a folder outside the DLL's leaked into " << doc;
  EXPECT_EQ(doc.find(".."), std::string::npos) << doc;
}
#endif

// ---------------------------------------------------------------------------------------------- im_clock

TEST(Exports, ClockIsMonotonicAndNearTheSystemTime) {
  int64_t last = INT64_MIN;
  for (int i = 0; i < 2000; ++i) {
    auto s = Sized<im_clock_sample>(sizeof(im_clock_sample));
    const int64_t before = im::engine::os::QpcNs();
    ASSERT_EQ(im_clock(&s), IM_OK) << LastError();
    const int64_t after = im::engine::os::QpcNs();
    ASSERT_EQ(s.struct_size, 32u);
    ASSERT_GE(s.mono_ns, last) << "sample " << i;
    ASSERT_GE(s.mono_ns, before);
    ASSERT_LE(s.mono_ns, after);
    ASSERT_GE(s.uncertainty_ns, 0);
    ASSERT_LE(s.uncertainty_ns, (after - before) / 2 + 1);
    last = s.mono_ns;
    if (i % 200 == 0) {
      const int64_t now_ns =
          duration_cast<std::chrono::nanoseconds>(std::chrono::system_clock::now().time_since_epoch()).count();
      EXPECT_LT(std::llabs(s.unix_ns - now_ns), 50'000'000LL) << "unix_ns is more than 50 ms off the system time";
    }
  }
}

// ---------------------------------------------------------------------------------------------- im_abi_layout

TEST(Exports, AbiLayoutEqualsTheGoldenFile) {
  const std::string path = NativeFile("common/tests/testdata/abi_layout.json");
  const std::string golden = ReadFile(path);
  ASSERT_FALSE(golden.empty()) << "cannot read " << path;
  std::string want;
  for (const char c : golden) {
    if (c != ' ' && c != '\n' && c != '\r' && c != '\t') {
      want.push_back(c); // no key or value in it holds a space
    }
  }
  const int32_t n = im_abi_layout(nullptr, 0);
  ASSERT_GT(n, 0);
  std::vector<char> buf(static_cast<std::size_t>(n) + 1);
  ASSERT_EQ(im_abi_layout(buf.data(), n + 1), n);
  EXPECT_EQ(std::string(buf.data(), static_cast<std::size_t>(n)), want);
}

// ---------------------------------------------------------------------------------------------- the worker

TEST_F(Engine, StalledWorkerGivesEngineTimeout) {
  // 04 §18.2's stalling hook: a job that holds the worker until released; its own poster gives up at once.
  std::promise<void> release;
  std::shared_future<void> released = release.get_future().share();
  ASSERT_TRUE(Fails(im::engine::TheWorker().Post("test: stall the worker", milliseconds(1),
                                                 [released]() -> int32_t {
                                                   released.wait();
                                                   return IM_OK;
                                                 }),
                    IM_ERR_TIMEOUT, "engine_timeout"));

  // im_list_sources runs on the worker: it waits its bound (50 ms) + 1 s, then gives up.
  char buf[64];
  const auto t0 = steady_clock::now();
  const int32_t st = im_list_sources(0, buf, static_cast<int32_t>(sizeof buf));
  const auto waited = duration_cast<milliseconds>(steady_clock::now() - t0).count();
  EXPECT_TRUE(Fails(st, IM_ERR_TIMEOUT, "engine_timeout", "im_list_sources did not finish within 1050 ms"));
  EXPECT_GE(waited, 1000);
  EXPECT_LT(waited, 3000);

  // im_probe's bound is 100 ms + 1 s for a quick probe. A timed-out call leaves the caller's struct alone (a write
  // would have zeroed these fields: the job's output starts zeroed).
  im_caps caps{};
  std::memset(&caps, 0xAB, sizeof caps);
  caps.struct_size = sizeof caps;
  EXPECT_TRUE(Fails(im_probe(IM_PROBE_QUICK, &caps), IM_ERR_TIMEOUT, "engine_timeout", "1100 ms"));
  EXPECT_EQ(caps.struct_size, sizeof(im_caps));
  EXPECT_EQ(caps.os_build, 0xABABABABu) << "a timed-out call wrote its output";
  EXPECT_EQ(caps.probe_ms, 0xABABABABu) << "a timed-out call wrote its output";

  // Released, the worker runs the late jobs (their results are discarded) and serves new calls.
  release.set_value();
  const int32_t again = im_list_sources(0, buf, static_cast<int32_t>(sizeof buf));
  EXPECT_NE(again, IM_ERR_TIMEOUT) << LastError();
}

TEST_F(Engine, PostedJobsPassTheirDetailToTheCaller) {
  const int32_t st = im::engine::TheWorker().Post("test: fail", milliseconds(2000), []() -> int32_t {
    return im::abi::Fail(IM_ERR_DEPENDENCY, im::abi::token::kMissing, "avcodec-62.dll is not in the folder");
  });
  EXPECT_TRUE(Fails(st, IM_ERR_DEPENDENCY, "missing", "avcodec-62.dll"));
  // The throw depends on a value the compiler can't see, so no code after it is unreachable (MSVC's C4702).
  static volatile bool always = true;
  const int32_t thrown = im::engine::TheWorker().Post("test: throw", milliseconds(2000), []() -> int32_t {
    if (always) {
      throw std::runtime_error("boom");
    }
    return IM_OK;
  });
  EXPECT_TRUE(Fails(thrown, IM_ERR_INTERNAL, nullptr, "boom"));
  EXPECT_EQ(im::engine::TheWorker().Post("test: ok", milliseconds(2000), []() -> int32_t { return 42; }), 42);
}

TEST(Exports, PostingWithoutAWorkerIsNotInitialized) {
  im_shutdown();
  EXPECT_TRUE(Fails(im::engine::TheWorker().Post("test", milliseconds(10), []() -> int32_t { return IM_OK; }),
                    IM_ERR_BAD_STATE, "not_initialized"));
}

// ---------------------------------------------------------------------------------------------- logs

TEST_F(Engine, ReadLogsDrainsWholeLines) {
  std::vector<char> buf(64 * 1024);
  while (im_read_logs(buf.data(), static_cast<int32_t>(buf.size())) > 0) {
  }
  im::log::Info("test", "exports_test marker", {{"n", 1}});
  const int32_t n = im_read_logs(buf.data(), static_cast<int32_t>(buf.size()));
  ASSERT_GT(n, 0) << LastError();
  const std::string out(buf.data(), static_cast<std::size_t>(n));
  EXPECT_EQ(out.back(), '\n');
  EXPECT_EQ(out.rfind("{\"t\":\"", 0), 0u) << out;
  EXPECT_NE(out.find("\"msg\":\"exports_test marker\""), std::string::npos) << out;
}

// ---------------------------------------------------------------------------------------------- the AudioEngine

// A fake AudioEngine that counts the calls reaching it. Init and Shutdown run on the engine worker, so their counters
// are atomic; init_hook (when set) decides what Init returns.
class CountingAudioEngine final : public im::audio::AudioEngine {
public:
  std::atomic<int> inits{0}, shutdowns{0};
  int rules = 0, watches = 0, modes = 0, mutes = 0, mixers = 0;
  uint32_t last_mode = 0xFFFFFFFF;
  std::atomic<uint32_t> init_flags{0xFFFFFFFF};
  std::function<im_status()> init_hook;

  im_status Init(uint32_t flags) override {
    ++inits;
    init_flags = flags;
    return init_hook ? init_hook() : IM_OK;
  }
  void Shutdown() override { ++shutdowns; }
  uint32_t ProbeFlags() override { return 0; }
  int32_t ListAppsJson(uint32_t, char *, int32_t) override { return IM_ERR_NOT_IMPLEMENTED; }
  im_status SetRules(const im_exclusions *) override {
    ++rules;
    return IM_OK;
  }
  im_status Watch(const im_exclusions *) override {
    ++watches;
    return IM_OK;
  }
  im_status Unwatch() override { return IM_OK; }
  im_status SetMode(uint32_t mode) override {
    ++modes;
    last_mode = mode;
    return IM_OK;
  }
  im_status SetMuted(bool) override {
    ++mutes;
    return IM_OK;
  }
  im_status LevelsGet(im_audio_levels *) override { return IM_ERR_BAD_STATE; }
  int32_t SoundCheck(uint32_t, int32_t, char *, int32_t) override { return IM_ERR_NOT_IMPLEMENTED; }
  std::unique_ptr<im::audio::Mixer> NewMixer() override {
    ++mixers;
    return nullptr;
  }
};

TEST(Exports, RefusedAudioArgumentsNeverReachTheAudioEngine) {
  im_shutdown();
  CountingAudioEngine fake;
  im::audio_bridge::SetEngineForTest(&fake);
  struct Restore {
    ~Restore() {
      im_shutdown();
      im::audio_bridge::SetEngineForTest(nullptr);
    }
  } restore;

  im_init_params p = InitParams();
  p.flags = 0;
  ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  EXPECT_EQ(fake.inits.load(), 1);
  EXPECT_EQ(fake.init_flags.load(), 0u);

  // IM_AUDIO_MODE_EVERYTHING is refused before any AudioEngine call (README §3 item 15).
  EXPECT_TRUE(Fails(im_audio_set_mode(IM_AUDIO_MODE_EVERYTHING), IM_ERR_INVALID_ARG));
  im_start_config cfg = StartConfig();
  cfg.audio.mode = IM_AUDIO_MODE_EVERYTHING;
  im_session s = 0;
  EXPECT_TRUE(Fails(im_start(&cfg, &s, nullptr), IM_ERR_INVALID_ARG, nullptr, "EVERYTHING"));
  const im_rule bad = Rule(IM_RULE_INSTANCE, "not-a-pid");
  const im_exclusions bad_ex = Exclusions(&bad, 1);
  EXPECT_TRUE(Fails(im_set_exclusions(&bad_ex), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_watch(&bad_ex), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_audio_set_muted(5), IM_ERR_INVALID_ARG));
  EXPECT_EQ(fake.modes, 0);
  EXPECT_EQ(fake.mixers, 0);
  EXPECT_EQ(fake.rules, 0);
  EXPECT_EQ(fake.watches, 0);
  EXPECT_EQ(fake.mutes, 0);

  // Valid calls reach it.
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_OK) << LastError();
  EXPECT_EQ(fake.modes, 1);
  EXPECT_EQ(fake.last_mode, IM_AUDIO_MODE_OFF);
  const im_rule good = Rule(IM_RULE_APP_EXCLUDE, "discord.exe");
  const im_exclusions ex = Exclusions(&good, 1);
  EXPECT_EQ(im_set_exclusions(&ex), IM_OK) << LastError();
  EXPECT_EQ(im_audio_watch(&ex), IM_OK) << LastError();
  EXPECT_EQ(im_audio_set_muted(1), IM_OK) << LastError();
  EXPECT_EQ(fake.rules, 1);
  EXPECT_EQ(fake.watches, 1);
  EXPECT_EQ(fake.mutes, 1);

  im_shutdown();
  EXPECT_EQ(fake.shutdowns.load(), 1);
}

// Audio never fails a call (04 §10.1): a failed AudioEngine::Init is logged, im_init succeeds without a detail, and
// im_shutdown still shuts the audio engine down.
TEST(Exports, FailedAudioInitDoesNotFailInit) {
  im_shutdown();
  CountingAudioEngine fake;
  fake.init_hook = []() -> im_status {
    return im::abi::Fail(IM_ERR_INTERNAL, {}, "test: the audio watcher can't start");
  };
  im::audio_bridge::SetEngineForTest(&fake);
  struct Restore {
    ~Restore() {
      im_shutdown();
      im::audio_bridge::SetEngineForTest(nullptr);
    }
  } restore;
  std::vector<char> logs(64 * 1024);
  while (im_read_logs(logs.data(), static_cast<int32_t>(logs.size())) > 0) {
  }

  im_init_params p = InitParams();
  p.flags = 0;
  ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  EXPECT_EQ(LastError(), "") << "a successful im_init left a detail";
  EXPECT_TRUE(im::engine::Initialized());
  EXPECT_EQ(fake.inits.load(), 1);
  const int32_t n = im_read_logs(logs.data(), static_cast<int32_t>(logs.size()));
  ASSERT_GT(n, 0) << LastError();
  const std::string out(logs.data(), static_cast<std::size_t>(n));
  EXPECT_NE(out.find("\"msg\":\"audio init failed\""), std::string::npos) << out;
  EXPECT_NE(out.find("the audio watcher can't start"), std::string::npos) << out;

  // Video calls keep working: im_probe reaches its area (the skeleton's stub answers whatever it answers).
  im_caps caps = Sized<im_caps>(sizeof(im_caps));
  EXPECT_NE(im_probe(IM_PROBE_QUICK, &caps), IM_ERR_BAD_STATE) << LastError();

  im_shutdown();
  EXPECT_EQ(fake.shutdowns.load(), 1);
}

// 04 §6.1: an AudioEngine::Init that outlasts im_init's bound (500 ms + 1 s) makes im_init fail with IM_ERR_TIMEOUT;
// when it finishes with IM_OK, the worker calls Shutdown itself, since no im_shutdown will.
TEST(Exports, LateAudioInitIsShutDownByTheWorker) {
  im_shutdown();
  // Static: the abandoned worker thread may reach the fake after a failed assertion returned from this test. The
  // promise is released at the latest when it is destroyed (a broken promise makes the future ready too).
  static CountingAudioEngine fake;
  fake.inits = 0;
  fake.shutdowns = 0;
  std::promise<void> release;
  std::shared_future<void> released = release.get_future().share();
  fake.init_hook = [released]() -> im_status {
    released.wait();
    return IM_OK;
  };
  im::audio_bridge::SetEngineForTest(&fake);
  struct Restore {
    ~Restore() {
      im_shutdown();
      im::audio_bridge::SetEngineForTest(nullptr);
    }
  } restore;

  im_init_params p = InitParams();
  p.flags = 0;
  const auto t0 = steady_clock::now();
  const im_status st = im_init(&p);
  const auto waited = duration_cast<milliseconds>(steady_clock::now() - t0).count();
  EXPECT_TRUE(Fails(st, IM_ERR_TIMEOUT, "engine_timeout", "AudioEngine::Init did not finish within 1500 ms"));
  EXPECT_GE(waited, 1400); // the bound, less a timer tick
  EXPECT_LT(waited, 5000); // the bound and the 1 s wait for the stuck worker
  EXPECT_FALSE(im::engine::Initialized());
  EXPECT_EQ(fake.shutdowns.load(), 0) << "Shutdown ran before the late Init finished";

  release.set_value(); // the late Init returns IM_OK on the abandoned worker
  const auto deadline = steady_clock::now() + std::chrono::seconds(10);
  while (fake.shutdowns.load() == 0 && steady_clock::now() < deadline) {
    std::this_thread::sleep_for(milliseconds(10));
  }
  EXPECT_EQ(fake.shutdowns.load(), 1) << "the worker didn't shut down the audio engine a late Init started";
  EXPECT_EQ(fake.inits.load(), 1);

  // The engine starts again afterwards, on a new worker.
  fake.init_hook = nullptr;
  ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  EXPECT_EQ(fake.inits.load(), 2);
  im_shutdown();
  EXPECT_EQ(fake.shutdowns.load(), 2);
}

} // namespace
