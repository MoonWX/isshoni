// The session watcher through the exports (docs/m2/05-windows-audio.md §4.3, §7, §11.1, §12, §19.2; README WA35):
// im_init with and without IM_INIT_NO_AUDIO, watch and unwatch, the report and the levels, the probe flags, the
// no-device state of a machine without an audio endpoint (CI's hosted runners), and 100 watcher start/stop cycles
// with the process's handle count back to its baseline. Runs anywhere: a test that needs a render endpoint, or its
// absence, says so and skips.
#include <windows.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <string>
#include <thread>
#include <vector>

#include <gtest/gtest.h>

#include "audio/mixer.h"
#include "audio/probe_flags.h"
#include "audio/win_audio.h"
#include "audio_bridge/audio_bridge.h"
#include "isshoni_media.h"

namespace im::audio {
namespace {

std::string LastError() {
  char buf[1024];
  const int32_t n = im_last_error(buf, sizeof buf);
  return n > 0 ? std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1)) : std::string();
}

im_status Init(uint32_t flags) {
  im_init_params p{};
  p.struct_size = sizeof p;
  p.abi_version = IM_ABI_VERSION;
  p.flags = flags;
  p.log_level = IM_LOG_DEBUG;
  return im_init(&p);
}

// The rules a share sheet passes, plus a built-in rule on a process every Windows runs (svchost.exe), so the report
// has a "kept out" row without any audio session.
struct Rules {
  im_rule rules[3] = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "svchost.exe", "test-voice", "Test Voice"},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
      {IM_RULE_SHARED_RUNTIME, IM_RULE_SRC_DATA, "msedgewebview2.exe", nullptr, nullptr},
  };
  im_exclusions ex{};
  explicit Rules(uint32_t count = 3) {
    ex.struct_size = sizeof ex;
    ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH;
    ex.rules = rules;
    ex.rule_count = count;
  }
};

// The report under the snprintf rule (05 §4.3): the length first, then the document. A watcher may publish a new
// report between the two calls (its 1 s poll), so a length that changed is asked again.
std::string Report() {
  for (int attempt = 0; attempt < 5; ++attempt) {
    const int32_t n = im_list_audio_apps(0, nullptr, 0);
    if (n <= 0) {
      ADD_FAILURE() << "im_list_audio_apps: " << n << " " << LastError();
      return {};
    }
    std::string doc(static_cast<std::size_t>(n) + 1, '\0');
    const int32_t m = im_list_audio_apps(0, doc.data(), n + 1);
    if (m == n) {
      doc.resize(static_cast<std::size_t>(n));
      return doc;
    }
  }
  ADD_FAILURE() << "the report changed length five times in a row";
  return {};
}

bool Has(const std::string &doc, const std::string &fragment) { return doc.find(fragment) != std::string::npos; }

// The report's "seq".
uint32_t Seq(const std::string &doc) {
  const auto at = doc.find("\"seq\":");
  return at == std::string::npos ? 0 : static_cast<uint32_t>(std::stoul(doc.substr(at + 6)));
}

// Active endpoints of a flow, counted on a thread of the test's own (COM MTA), independently of the engine.
uint32_t Endpoints(EDataFlow flow) {
  uint32_t n = 0;
  std::thread([&] {
    const win::ComApartment com;
    win::ComPtr<IMMDeviceEnumerator> en;
    if (SUCCEEDED(com.hr()) && SUCCEEDED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL,
                                                          __uuidof(IMMDeviceEnumerator), en.PutVoid()))) {
      n = ActiveEndpoints(en.Get(), flow);
    }
  }).join();
  return n;
}

class AudioWatcher : public ::testing::Test {
protected:
  void TearDown() override { im_shutdown(); }
};

// IM_INIT_NO_AUDIO (05 §4.3): no audio API is touched; the watcher's calls answer IM_ERR_BAD_STATE, im_set_exclusions
// keeps the rules, unwatch is a no-op, and audio_flags is 0.
TEST_F(AudioWatcher, NoAudioInit) {
  ASSERT_EQ(Init(IM_INIT_NO_AUDIO), IM_OK) << LastError();
  Rules r;
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  EXPECT_EQ(im_audio_watch(&r.ex), IM_ERR_BAD_STATE);
  EXPECT_NE(LastError().find("IM_INIT_NO_AUDIO"), std::string::npos) << LastError();
  EXPECT_EQ(im_list_audio_apps(0, nullptr, 0), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_sound_check(0, 1000, nullptr, 0), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_muted(1), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_set_exclusions(&r.ex), IM_OK) << LastError();
  EXPECT_EQ(im_audio_unwatch(), IM_OK) << LastError();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), 0u);
  EXPECT_FALSE(audio_bridge::Engine().NewMixer());
}

// Not watching: the levels are IM_ERR_BAD_STATE (not_watching) and the report is a one-shot snapshot ("stopped");
// watching: both work and the built-in row shows; after unwatch the levels are refused again. Mode and mute need a
// running audio session, which the watcher alone isn't (05 §4.3).
TEST_F(AudioWatcher, WatchAndUnwatch) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_EQ(LastError().rfind("not_watching: ", 0), 0u) << LastError();
  std::string doc = Report();
  EXPECT_TRUE(Has(doc, "{\"v\":1,\"seq\":")) << doc.substr(0, 200);
  EXPECT_TRUE(Has(doc, "\"state\":\"stopped\""));
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc, "\"live\":false"));

  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << "idempotent: " << LastError();
  doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"ok\"") || Has(doc, "\"state\":\"no-device\"")) << doc.substr(0, 300);
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc,
                  "{\"id\":\"test-voice\",\"name\":\"Test Voice\",\"exe\":\"svchost.exe\",\"verdict\":\"kept_out\","
                  "\"reason\":\"builtin\""))
      << "a running process matched by a built-in rule has its row (05 §12.1)";
  EXPECT_FALSE(Has(doc, "\"pids\":0,")) << "every row counts its processes";
  ASSERT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
  EXPECT_EQ(levels.struct_size, sizeof levels);
  // The levels carry the report's seq of their sample (published every 100 ms, so they may lag a new report).
  EXPECT_GE(levels.report_seq, 1u);
  EXPECT_LE(levels.report_seq, Seq(Report()));
  EXPECT_FLOAT_EQ(levels.mix_peak_db, -120.0f) << "not live";
  EXPECT_LE(levels.count, IM_MAX_LEVEL_APPS);
  for (uint32_t i = 0; i < levels.count; ++i) {
    EXPECT_EQ(levels.apps[i].source, 1u) << "only activity meters while not live";
  }
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_muted(1), IM_ERR_BAD_STATE);

  // New rules apply within 50 ms (05 §4.1); the row goes, and seq moves.
  const uint32_t before = Seq(doc);
  Rules fewer(2);
  fewer.rules[0] = fewer.rules[1];
  fewer.rules[1] = fewer.rules[2];
  ASSERT_EQ(im_set_exclusions(&fewer.ex), IM_OK) << LastError();
  bool gone = false;
  for (int i = 0; i < 40 && !gone; ++i) {
    std::this_thread::sleep_for(std::chrono::milliseconds(50));
    doc = Report();
    gone = !Has(doc, "\"id\":\"test-voice\"");
  }
  EXPECT_TRUE(gone) << "the row of a removed rule stays";
  EXPECT_GT(Seq(doc), before);
  EXPECT_TRUE(Has(doc, "\"code\":\"rules_changed\""));

  ASSERT_EQ(im_audio_unwatch(), IM_OK) << LastError();
  ASSERT_EQ(im_audio_unwatch(), IM_OK) << "idempotent: " << LastError();
  EXPECT_EQ(im_audio_levels_get(&levels), IM_ERR_BAD_STATE);
  EXPECT_TRUE(Has(Report(), "\"state\":\"stopped\""));
}

// A machine without an active render endpoint (CI's hosted runners, 05 §7.5, §19.2): state no-device, no crash, the
// probe skipped, the levels empty, and neither loopback nor session-events flag.
TEST_F(AudioWatcher, NoDeviceState) {
  if (Endpoints(eRender) > 0) {
    GTEST_SKIP() << "this machine has an active render endpoint";
  }
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  const std::string doc = Report();
  EXPECT_TRUE(Has(doc, "\"state\":\"no-device\"")) << doc.substr(0, 400);
  EXPECT_TRUE(Has(doc, "\"device\":{\"present\":false,\"renderEndpoints\":0,"));
  EXPECT_TRUE(Has(doc, "\"probe\":{\"result\":\"skipped\""));
  EXPECT_TRUE(Has(doc, "\"engine\":\"none\""));
  EXPECT_TRUE(Has(doc, "\"sessions\":[]"));
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  ASSERT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
  EXPECT_EQ(levels.count, 0u);
  const uint32_t flags = audio_bridge::Engine().ProbeFlags();
  EXPECT_EQ(flags & (IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_ENDPOINT_LOOPBACK | IM_AUDIO_SESSION_EVENTS), 0u) << flags;
  EXPECT_EQ((flags & IM_AUDIO_MIC_DETECTION) != 0, Endpoints(eCapture) > 0) << flags;
}

// im_caps.audio_flags (05 §11.1): only the four bits; the mic bit exactly when a capture endpoint is active; without
// a render endpoint no loopback or session-events bit; cached (the second answer is the same and quick).
TEST_F(AudioWatcher, ProbeFlags) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  const uint32_t render = Endpoints(eRender);
  const uint32_t capture = Endpoints(eCapture);
  const uint32_t flags = audio_bridge::Engine().ProbeFlags();
  EXPECT_EQ(flags & ~0xFu, 0u) << flags;
  EXPECT_EQ((flags & IM_AUDIO_MIC_DETECTION) != 0, capture > 0) << flags;
  if (render == 0) {
    EXPECT_EQ(flags & (IM_AUDIO_PROCESS_LOOPBACK | IM_AUDIO_ENDPOINT_LOOPBACK | IM_AUDIO_SESSION_EVENTS), 0u);
  } else {
    EXPECT_NE(flags & IM_AUDIO_SESSION_EVENTS, 0u) << "session notifications register on a render endpoint";
  }
  const auto t0 = std::chrono::steady_clock::now();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), flags);
  EXPECT_LT(std::chrono::steady_clock::now() - t0, std::chrono::milliseconds(500)) << "the flags are cached";
  // Watching changes nothing about them.
  Rules r;
  ASSERT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
  EXPECT_EQ(audio_bridge::Engine().ProbeFlags(), flags);
}

// 100 watcher start/stop cycles (im_init, watch, report, levels, unwatch, im_shutdown) leave the process's handle
// count where it was: the watcher thread, its events, the enumerator, the notifications and the process handles of
// the table are all released (05 §19.2).
TEST_F(AudioWatcher, StartStopCyclesKeepHandles) {
  auto cycle = [] {
    if (Init(0) != IM_OK) {
      ADD_FAILURE() << "im_init: " << LastError();
      return;
    }
    Rules r;
    EXPECT_EQ(im_audio_watch(&r.ex), IM_OK) << LastError();
    EXPECT_GT(im_list_audio_apps(0, nullptr, 0), 0) << LastError();
    im_audio_levels levels{};
    levels.struct_size = sizeof levels;
    EXPECT_EQ(im_audio_levels_get(&levels), IM_OK) << LastError();
    EXPECT_EQ(im_audio_unwatch(), IM_OK) << LastError();
    im_shutdown();
  };
  auto handles = [] {
    DWORD n = 0;
    GetProcessHandleCount(GetCurrentProcess(), &n);
    return static_cast<int64_t>(n);
  };
  // Warm up: the first cycles load mmdevapi and COM's own lasting state.
  for (int i = 0; i < 3; ++i) {
    cycle();
  }
  const int64_t baseline = handles();
  int64_t at10 = 0;
  for (int i = 0; i < 100; ++i) {
    cycle();
    if (i == 9) {
      at10 = handles();
    }
  }
  const int64_t after = handles();
  // The thread pool's own threads come and go, so a little slack, but no growth with the cycles.
  EXPECT_LE(after, baseline + 32) << "baseline " << baseline << ", after 100 cycles " << after;
  EXPECT_LE(after - at10, 16) << "after 10 cycles " << at10 << ", after 100 " << after;
}

} // namespace
} // namespace im::audio
