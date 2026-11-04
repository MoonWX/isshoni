// session_log_test (docs/m2/04-windows-engine.md §5.6 "Privacy", E14; README §3 item 18, §4 "Logging", WA52): the
// log-capture test. No record of the engine's log ring, at info level and at debug level, contains a window title or
// names an executable, a process or an app.
//
// The test plants such names wherever a session meets them, then drives sessions through what they log (start, the
// encoders, pause and resume, keyframes, stats, the audio start's failures, stop) and drains the ring with
// im_read_logs:
// - the test process's own executable (its file name, with and without ".exe");
// - a top-level window of the test with a distinctive title, handed to im_protect_window;
// - the exclusion rules of im_start (an executable, its app id and its display name), which a fake AudioEngine
//   (audio_bridge::SetEngineForTest, 04 §18.2) refuses with a detail that repeats them, as 05's text may;
// - a Mixer whose Start fails with a detail that names another app's executable.
// The session logs only the token of such a detail (session.h's DetailToken), so the check also finds the
// tokens the fake used ("set_rules", "missing") and the records that carry them.
#include <gtest/gtest.h>

#include <windows.h>

#include <algorithm>
#include <cctype>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <memory>
#include <mutex>
#include <string>
#include <vector>

#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "im/abi/status.h"
#include "isshoni_media.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
#include "testhooks/testhooks.h"
#endif

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

// The names the test plants (lower case: the search ignores case).
constexpr const char *kGameExe = "wa52secretgame.exe";
constexpr const char *kGameAppId = "exe:wa52secretgame.exe";
constexpr const char *kGameName = "WA52 Secret Game";
constexpr const char *kVoiceExe = "wa52secretvoice.exe";
constexpr const wchar_t *kWindowTitle = L"WA52 secret window title";

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

std::string Lower(std::string s) {
  std::transform(s.begin(), s.end(), s.begin(), [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
  return s;
}

// The test executable's file name, lower case ASCII ("session_log_test.exe"), "" when it can't be read.
std::string OwnExeName() {
  wchar_t path[MAX_PATH] = {};
  const DWORD n = GetModuleFileNameW(nullptr, path, MAX_PATH);
  if (n == 0 || n >= MAX_PATH) {
    return {};
  }
  const wchar_t *base = path;
  for (const wchar_t *p = path; *p != L'\0'; ++p) {
    if (*p == L'\\' || *p == L'/') {
      base = p + 1;
    }
  }
  std::string out;
  for (const wchar_t *p = base; *p != L'\0'; ++p) {
    out.push_back(*p < 128 ? static_cast<char>(std::tolower(static_cast<int>(*p))) : '?');
  }
  return out;
}

// ---------------------------------------------------------------------------------------------- the fake audio

// A Mixer that starts (or refuses with a detail naming kVoiceExe) and produces nothing: the session's audio state is
// ok and its stage gets no chunk.
class QuietMixer final : public im::audio::Mixer {
public:
  explicit QuietMixer(bool refuse) noexcept : refuse_(refuse) {}
  im_status Start(const im::audio::MixerConfig &) override {
    if (refuse_) {
      return im::abi::Failf(IM_ERR_INTERNAL, im::abi::token::kMissing, "%s could not be captured", kVoiceExe);
    }
    return IM_OK;
  }
  void Stop() override {}
  void SetPaused(bool) override {}
  uint32_t State() const override { return IM_AUDIO_STATE_OK; }

private:
  const bool refuse_;
};

// 05's AudioEngine as far as a session needs it: SetRules refuses with a detail that repeats the rule, NewMixer makes
// the mixer the test asks for. Any thread.
class FakeAudio final : public im::audio::AudioEngine {
public:
  enum class MixerKind : uint8_t { kNone, kRefusing, kQuiet };

  static FakeAudio &Instance() {
    static FakeAudio *const a = new FakeAudio(); // never destroyed: an abandoned thread may still call it
    return *a;
  }

  void Configure(bool refuse_rules, MixerKind mixer) {
    std::lock_guard<std::mutex> lock(mu_);
    refuse_rules_ = refuse_rules;
    mixer_ = mixer;
  }

  im_status Init(uint32_t) override { return IM_OK; }
  void Shutdown() override {}
  uint32_t ProbeFlags() override { return 0; }
  int32_t ListAppsJson(uint32_t, char *buf, int32_t cap) override { return im::abi::CopyOut("{}", buf, cap); }
  im_status SetRules(const im_exclusions *ex) override {
    std::lock_guard<std::mutex> lock(mu_);
    if (!refuse_rules_ || ex == nullptr || ex->rule_count == 0) {
      return IM_OK;
    }
    const im_rule &r = ex->rules[0];
    return im::abi::Failf(IM_ERR_INVALID_ARG, {}, "set_rules: rule 0 (%s, %s, %s) is refused", r.value ? r.value : "",
                          r.app_id ? r.app_id : "", r.name ? r.name : "");
  }
  im_status Watch(const im_exclusions *) override { return IM_OK; }
  im_status Unwatch() override { return IM_OK; }
  im_status SetMode(uint32_t) override { return IM_OK; }
  im_status SetMuted(bool) override { return IM_OK; }
  im_status LevelsGet(im_audio_levels *) override {
    return im::abi::Fail(IM_ERR_BAD_STATE, im::abi::token::kNotWatching, "fake: not watching");
  }
  int32_t SoundCheck(uint32_t, int32_t, char *, int32_t) override {
    return im::abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "fake: no sound check");
  }
  std::unique_ptr<im::audio::Mixer> NewMixer() override {
    std::lock_guard<std::mutex> lock(mu_);
    switch (mixer_) {
    case MixerKind::kRefusing:
      return std::make_unique<QuietMixer>(true);
    case MixerKind::kQuiet:
      return std::make_unique<QuietMixer>(false);
    default:
      return nullptr;
    }
  }

private:
  FakeAudio() = default;
  std::mutex mu_;
  bool refuse_rules_ = false;
  MixerKind mixer_ = MixerKind::kNone;
};

// ---------------------------------------------------------------------------------------------- the log

// Every record drained so far.
std::vector<std::string> DrainLogs() {
  std::vector<std::string> lines;
  std::vector<char> buf(64 * 1024);
  for (int i = 0; i < 1000; ++i) { // the ring holds 512 records: a few calls empty it
    const int32_t n = im_read_logs(buf.data(), static_cast<int32_t>(buf.size()));
    if (n <= 0) {
      EXPECT_EQ(n, 0) << LastError();
      break;
    }
    std::size_t start = 0;
    const std::string chunk(buf.data(), static_cast<std::size_t>(n));
    while (start < chunk.size()) {
      std::size_t end = chunk.find('\n', start);
      if (end == std::string::npos) {
        end = chunk.size();
      }
      if (end > start) {
        lines.push_back(chunk.substr(start, end - start));
      }
      start = end + 1;
    }
  }
  return lines;
}

void Pull(im_session s, milliseconds d) {
  const auto end = Clock::now() + d;
  while (Clock::now() < end) {
    im_packet p{};
    p.struct_size = sizeof p;
    const im_status st = im_next_packet(s, 50, &p);
    if (st != IM_OK && st != IM_TIMEOUT) {
      break;
    }
  }
}

std::string StatsOf(im_session s) {
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  return n > 0 ? std::string(buf.data(), static_cast<std::size_t>(std::min<int32_t>(n, 16383))) : std::string();
}

class SessionLog : public ::testing::TestWithParam<uint32_t> {
protected:
  void SetUp() override {
    im_shutdown();
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::testhooks::Reset();
#endif
    FakeAudio::Instance().Configure(false, FakeAudio::MixerKind::kNone);
    im::audio_bridge::SetEngineForTest(&FakeAudio::Instance());
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = 0; // audio on: the fake AudioEngine answers
    p.log_level = GetParam();
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
    window_ = CreateWindowExW(0, L"STATIC", kWindowTitle, WS_OVERLAPPEDWINDOW, 0, 0, 320, 200, nullptr, nullptr,
                              GetModuleHandleW(nullptr), nullptr);
  }
  void TearDown() override {
    im_shutdown();
    im::audio_bridge::SetEngineForTest(nullptr);
    if (window_ != nullptr) {
      DestroyWindow(window_);
    }
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::testhooks::Reset();
#endif
  }

  // Starts cfg, runs it for a while with pause, keyframes and stats, and stops it.
  void Run(const im_start_config &cfg, bool video) {
    im_session s = 0;
    im_session_info info{};
    info.struct_size = sizeof info;
    ASSERT_EQ(im_start(&cfg, &s, &info), IM_OK) << LastError();
    Pull(s, milliseconds(400));
    EXPECT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
    if (video) {
      EXPECT_EQ(im_request_keyframe(s, IM_ALL_LAYERS), IM_OK) << LastError();
    }
    Pull(s, milliseconds(300));
    EXPECT_FALSE(StatsOf(s).empty());
    EXPECT_EQ(im_audio_set_muted(1), IM_OK) << LastError();
    EXPECT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
    EXPECT_EQ(im_audio_set_muted(0), IM_OK) << LastError();
    Pull(s, milliseconds(300));
    EXPECT_EQ(im_stop(s), IM_OK);
    Collect(); // the ring holds 512 records: drained after each session, so none is dropped
  }

  void Collect() {
    for (std::string &l : DrainLogs()) {
      lines_.push_back(std::move(l));
    }
  }

  HWND window_ = nullptr;
  std::vector<std::string> lines_;
};

im_exclusions Exclusions(const im_rule *rules) {
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.rules = rules;
  ex.rule_count = 1;
  return ex;
}

im_start_config AudioOnly(const im_exclusions *ex) {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.audio.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  c.exclusions = ex;
  return c;
}

TEST_P(SessionLog, NoRecordNamesAWindowOrAnApp) {
  ASSERT_NE(window_, nullptr) << "CreateWindowExW failed: " << GetLastError();
  Collect(); // what im_init logged is checked with the rest
  const im_rule rules[] = {im_rule{IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, kGameExe, kGameAppId, kGameName}};
  const im_exclusions ex = Exclusions(rules);

  // Our own window, as the app protects its windows (04 §11); the result doesn't matter here.
  (void)im_protect_window(window_, 0);
  (void)im_protect_window(window_, IM_PROTECT_OFF);

  // The audio start's failures: refused rules and no mixer, then a mixer that doesn't start. Audio never fails a share.
  FakeAudio::Instance().Configure(true, FakeAudio::MixerKind::kNone);
  Run(AudioOnly(&ex), false);
  FakeAudio::Instance().Configure(false, FakeAudio::MixerKind::kRefusing);
  Run(AudioOnly(&ex), false);
  FakeAudio::Instance().Configure(false, FakeAudio::MixerKind::kQuiet);
  Run(AudioOnly(&ex), false);
  // The test tone.
  im_start_config tone{};
  tone.struct_size = sizeof tone;
  tone.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  tone.audio.flags = IM_AUDIOF_TAP_PCM;
  Run(tone, false);

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // Video through IM_ENC_FAKE: a picked window (the fake picker) with 05's audio and its rules, and test:pattern.
  const std::string pick = im::testhooks::FakePick(IM_SOURCE_WINDOW, 1280, 720);
  im_start_config video{};
  video.struct_size = sizeof video;
  video.source_id = pick.c_str();
  video.profile = IM_PROFILE_HIGH;
  video.content_hint = IM_CONTENT_DETAIL;
  video.encoder = IM_ENC_AUTO;
  video.layer_count = 2;
  std::memcpy(video.layers[0].rid, "f", 2);
  video.layers[0].max_pixels = 1280 * 720;
  video.layers[0].fps = 30;
  video.layers[0].bitrate_bps = 2'000'000;
  std::memcpy(video.layers[1].rid, "q", 2);
  video.layers[1].max_pixels = 640 * 360;
  video.layers[1].fps = 15;
  video.layers[1].bitrate_bps = 300'000;
  video.audio.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  video.exclusions = &ex;
  FakeAudio::Instance().Configure(true, FakeAudio::MixerKind::kRefusing);
  Run(video, true);
  video.flags = IM_START_ALLOW_LISTED_SOURCE;
  video.source_id = "test:pattern";
  video.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  video.exclusions = nullptr;
  Run(video, true);
#endif

  Collect();
  const std::vector<std::string> &lines = lines_;
  ASSERT_GE(lines.size(), 8u) << "the sessions logged their starts and stops";
  const std::string own = OwnExeName();
  ASSERT_GT(own.size(), 4u) << "the test's own executable name";
  const std::string own_stem = own.substr(0, own.size() - 4);
  std::vector<std::string> forbidden = {Lower(kGameExe),
                                        "wa52secretgame",
                                        Lower(kGameName),
                                        Lower(kVoiceExe),
                                        "wa52secretvoice",
                                        "wa52 secret window",
                                        own,
                                        own_stem,
                                        ".exe"};
  bool rules_token = false, mixer_token = false;
  for (const std::string &line : lines) {
    const std::string l = Lower(line);
    for (const std::string &f : forbidden) {
      EXPECT_EQ(l.find(f), std::string::npos) << "a record names \"" << f << "\": " << line;
    }
    rules_token = rules_token || (l.find("audio rules not applied") != std::string::npos &&
                                  l.find("\"token\":\"set_rules\"") != std::string::npos);
    mixer_token = mixer_token || (l.find("audio mixer did not start") != std::string::npos &&
                                  l.find("\"token\":\"missing\"") != std::string::npos);
  }
  // The failures were logged, with their tokens only (warn level: recorded at info and at debug).
  EXPECT_TRUE(rules_token) << "no \"audio rules not applied\" record with its token";
  EXPECT_TRUE(mixer_token) << "no \"audio mixer did not start\" record with its token";
}

INSTANTIATE_TEST_SUITE_P(Levels, SessionLog, ::testing::Values(IM_LOG_INFO, IM_LOG_DEBUG),
                         [](const ::testing::TestParamInfo<uint32_t> &p) {
                           return std::string(p.param == IM_LOG_DEBUG ? "Debug" : "Info");
                         });

} // namespace
