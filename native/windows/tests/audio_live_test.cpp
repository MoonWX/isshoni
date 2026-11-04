// A share's mixer from 05's real AudioEngine (docs/m2/05-windows-audio.md §4.3, §9.1, §9.6, §10.5, §12.1; README WA45)
// on whatever audio the machine has (CI's hosted runners have no render endpoint): NewMixer gives a mixer once the
// watcher runs; Start returns with the clock running and chunks reach the PcmSink every 10 ms on one grid, the first
// after an OnGap; the report says live, with mode, mute and pause; im_audio_set_mode and im_audio_set_muted work while
// live and answer IM_ERR_BAD_STATE after; Stop ends the chunks; a mixer whose Start failed is destroyed without Stop;
// im_shutdown stops a mixer still live; start/stop cycles leak no handles.
//
// Calls are ordered by counters and the chunks' own times, never by comparing clock readings shorter than Windows'
// tick; every wait is bounded (README §4 "Testing").
#include <windows.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <vector>

#include <gtest/gtest.h>

#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "im/audio/pcm_sink.h"
#include "isshoni_media.h"

namespace im::audio {
namespace {

using namespace std::chrono_literals;

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

// Polls the report until it has fragment (bounded).
bool ReportHas(const std::string &fragment) {
  for (int i = 0; i < 100; ++i) {
    if (Has(Report(), fragment)) {
      return true;
    }
    std::this_thread::sleep_for(20ms);
  }
  return false;
}

// What the mixer hands the session: chunks with their times, the gaps, and the order of both.
class RecordingSink final : public PcmSink {
public:
  void OnChunk(const float *frames, int64_t capture_ns) noexcept override {
    std::lock_guard<std::mutex> l(mu);
    for (uint32_t i = 0; i < kChunkSamples; ++i) {
      peak = std::max(peak, frames[i] < 0 ? -frames[i] : frames[i]);
    }
    times.push_back(capture_ns);
    order.push_back('c');
  }
  void OnGap(int64_t) noexcept override {
    std::lock_guard<std::mutex> l(mu);
    order.push_back('g');
  }
  std::size_t chunks() {
    std::lock_guard<std::mutex> l(mu);
    return times.size();
  }
  bool WaitChunks(std::size_t n) {
    for (int i = 0; i < 500 && chunks() < n; ++i) {
      std::this_thread::sleep_for(10ms);
    }
    return chunks() >= n;
  }

  std::mutex mu;
  std::vector<int64_t> times;
  std::string order;
  float peak = 0;
};

// The rules a share sheet passes: a built-in voice rule and a launcher.
struct Rules {
  im_rule rules[2] = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "discord.exe", "discord", "Discord"},
      {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", nullptr, nullptr},
  };
  im_exclusions ex{};
  Rules() {
    ex.struct_size = sizeof ex;
    ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH;
    ex.rules = rules;
    ex.rule_count = 2;
  }
};

class AudioLive : public ::testing::Test {
protected:
  void TearDown() override { im_shutdown(); }
};

TEST_F(AudioLive, AMixerFeedsTheGridReportsLiveAndStops) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  Rules r;
  ASSERT_EQ(im_set_exclusions(&r.ex), IM_OK) << LastError();
  std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
  ASSERT_TRUE(m) << "the watcher runs: a share gets a mixer";
  EXPECT_EQ(m->State(), IM_AUDIO_STATE_OFF) << "not started";
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE) << "no running audio session yet";

  RecordingSink sink;
  ASSERT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}), IM_OK) << LastError();
  ASSERT_TRUE(sink.WaitChunks(30)) << "chunks every 10 ms";
  {
    std::lock_guard<std::mutex> l(sink.mu);
    ASSERT_EQ(sink.order.substr(0, 2), "gc") << "the first chunk starts a grid";
    // One grid unless a resync came between (a loaded runner): consecutive chunks exactly 10 ms apart within it.
    std::size_t idx = 0, steps = 0;
    char prev = 0;
    for (const char c : sink.order) {
      if (c == 'c') {
        if (prev == 'c') { // not the first chunk of a grid
          EXPECT_EQ(sink.times[idx] - sink.times[idx - 1], kChunkNs) << "chunk " << idx;
          ++steps;
        }
        ++idx;
      }
      prev = c;
    }
    EXPECT_GT(steps, 20u);
    EXPECT_EQ(sink.peak, 0.0f) << "nothing captured on this machine's silence";
  }
  // The state leaves "starting" with the first plan: ok with a render endpoint, no-device without.
  uint32_t st = m->State();
  for (int i = 0; i < 100 && st == IM_AUDIO_STATE_STARTING; ++i) {
    std::this_thread::sleep_for(20ms);
    st = m->State();
  }
  EXPECT_TRUE(st == IM_AUDIO_STATE_OK || st == IM_AUDIO_STATE_NO_DEVICE) << st;
  EXPECT_TRUE(ReportHas("\"live\":true")) << Report().substr(0, 300);
  const std::string doc = Report();
  EXPECT_TRUE(Has(doc, "\"mode\":\"exclude-apps\",\"engine\":\"include-set\"")) << doc.substr(0, 300);
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  EXPECT_EQ(im_audio_levels_get(&levels), IM_OK) << "live: the levels work without im_audio_watch";

  // The silence flags while live (05 §9.6), each in the report.
  ASSERT_EQ(im_audio_set_muted(1), IM_OK) << LastError();
  EXPECT_TRUE(ReportHas("\"muted\":true"));
  EXPECT_TRUE(ReportHas("\"code\":\"muted\""));
  ASSERT_EQ(im_audio_set_muted(0), IM_OK) << LastError();
  EXPECT_TRUE(ReportHas("\"muted\":false"));
  m->SetPaused(true);
  EXPECT_TRUE(ReportHas("\"paused\":true"));
  m->SetPaused(false);
  ASSERT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_OK) << LastError();
  EXPECT_TRUE(ReportHas("\"mode\":\"off\",\"engine\":\"none\"")) << "sound off: no capture engine (05 §6.5)";
  EXPECT_TRUE(ReportHas("\"code\":\"mode_changed\""));
  ASSERT_EQ(im_audio_set_mode(IM_AUDIO_MODE_EXCLUDE_APPS), IM_OK) << LastError();
  EXPECT_TRUE(ReportHas("\"mode\":\"exclude-apps\",\"engine\":\"include-set\""));
  const std::size_t before_flags = sink.chunks();
  ASSERT_TRUE(sink.WaitChunks(before_flags + 5)) << "the grid goes on through the flags";

  m->Stop();
  const std::size_t after_stop = sink.chunks();
  std::this_thread::sleep_for(60ms);
  EXPECT_EQ(sink.chunks(), after_stop) << "no OnChunk after Stop";
  EXPECT_EQ(m->State(), IM_AUDIO_STATE_OFF);
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_ERR_BAD_STATE);
  EXPECT_EQ(im_audio_set_muted(1), IM_ERR_BAD_STATE);
  EXPECT_TRUE(ReportHas("\"live\":false"));
  EXPECT_TRUE(ReportHas("\"state\":\"stopped\"")) << "nobody watches after the share";
  m->Stop(); // idempotent
}

// 05 §10.5: a Mixer must cope with being destroyed without Stop after a failed Start (the session then runs without
// it); and a second Start is refused while one runs.
TEST_F(AudioLive, AFailedStartLeavesAMixerThatCanBeDestroyed) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  {
    std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
    ASSERT_TRUE(m);
    EXPECT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, nullptr}), IM_ERR_INVALID_ARG);
    EXPECT_EQ(m->State(), IM_AUDIO_STATE_OFF);
  }
  {
    RecordingSink sink;
    std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
    ASSERT_TRUE(m);
    EXPECT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_OFF, 0, &sink}), IM_ERR_INVALID_ARG) << "mode off makes no mixer";
    ASSERT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}), IM_OK) << LastError();
    EXPECT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}), IM_ERR_BAD_STATE);
    ASSERT_TRUE(sink.WaitChunks(3));
  } // destroyed while running: the destructor stops it
  EXPECT_TRUE(ReportHas("\"live\":false"));
}

// IM_START_PAUSED reaches SetPaused before Start (04 §10.2): the first report already says paused.
TEST_F(AudioLive, PausedBeforeStart) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  RecordingSink sink;
  std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
  ASSERT_TRUE(m);
  m->SetPaused(true);
  ASSERT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}), IM_OK) << LastError();
  EXPECT_TRUE(ReportHas("\"paused\":true"));
  m->Stop();
}

// im_shutdown with a mixer still live (04 stops its sessions first; this is the backstop): the chunks end, and the
// mixer's own Stop and destruction afterwards are harmless.
TEST_F(AudioLive, ShutdownStopsALiveMixer) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  RecordingSink sink;
  std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
  ASSERT_TRUE(m);
  ASSERT_EQ(m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}), IM_OK) << LastError();
  ASSERT_TRUE(sink.WaitChunks(3));
  im_shutdown();
  const std::size_t n = sink.chunks();
  std::this_thread::sleep_for(60ms);
  EXPECT_EQ(sink.chunks(), n);
  m->Stop();
  m.reset();
}

// Shares come and go: the mixer, capture and activator threads with them, no handle left behind.
TEST_F(AudioLive, StartStopCyclesKeepHandles) {
  ASSERT_EQ(Init(0), IM_OK) << LastError();
  auto handles = [] {
    DWORD n = 0;
    GetProcessHandleCount(GetCurrentProcess(), &n);
    return static_cast<int64_t>(n);
  };
  auto cycle = [] {
    RecordingSink sink;
    std::unique_ptr<Mixer> m = audio_bridge::Engine().NewMixer();
    if (!m || m->Start(MixerConfig{IM_AUDIO_MODE_EXCLUDE_APPS, 0, &sink}) != IM_OK) {
      ADD_FAILURE() << "start: " << LastError();
      return;
    }
    EXPECT_TRUE(sink.WaitChunks(2));
    m->Stop();
  };
  for (int i = 0; i < 3; ++i) {
    cycle();
  }
  const int64_t baseline = handles();
  for (int i = 0; i < 30; ++i) {
    cycle();
  }
  // The thread pool's own threads come and go, so a little slack, but no growth with the cycles.
  EXPECT_LE(handles(), baseline + 32) << "baseline " << baseline;
}

} // namespace
} // namespace im::audio
