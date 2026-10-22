// The mixer thread on a real clock (docs/m2/05-windows-audio.md §9.1, §10.5): Start returns with the clock running,
// chunks come every 10 ms on the grid with one OnGap at the start, Stop joins and nothing comes after it, a second
// Start begins a new grid. Also the watcher's calls from another thread while it runs (clean under host-tsan).
#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <thread>

#include "im/audiomix/mixer.h"
#include "mix_test_support.h"

namespace im::audiomix {
namespace {

// Counts chunks and checks the grid; atomics, because the test thread reads while the mixer thread writes.
class CountingSink final : public ChunkSink {
public:
  void OnChunk(const float *frames, int64_t t) noexcept override {
    if (have_ && t - last_ != kChunkNs) {
      off_grid.fetch_add(1, std::memory_order_relaxed);
    }
    have_ = true;
    last_ = t;
    for (uint32_t i = 0; i < kChunkSamples; ++i) {
      if (frames[i] != 0.0f) {
        loud.fetch_add(1, std::memory_order_relaxed);
        break;
      }
    }
    chunks.fetch_add(1, std::memory_order_release);
  }
  void OnGap(int64_t) noexcept override {
    have_ = false;
    gaps.fetch_add(1, std::memory_order_relaxed);
  }
  std::atomic<uint64_t> chunks{0}, gaps{0}, off_grid{0}, loud{0};

private:
  bool have_ = false;
  int64_t last_ = 0;
};

std::atomic<int> g_hook_calls{0};
void Hook(void *, bool starting) { g_hook_calls.fetch_add(starting ? 1 : 100); }

TEST(Mixer, CreateNeedsASink) {
  std::unique_ptr<Mixer> m;
  EXPECT_EQ(Mixer::Create(MixerOptions{}, &m), IM_ERR_INVALID_ARG);
  CountingSink sink;
  MixerOptions o;
  o.sink = &sink;
  EXPECT_EQ(Mixer::Create(o, nullptr), IM_ERR_INVALID_ARG);
}

TEST(Mixer, RunsOnTheSteadyClockAndStops) {
  CountingSink sink;
  MixerOptions o;
  o.sink = &sink;
  o.thread_hook = &Hook;
  std::unique_ptr<Mixer> m;
  ASSERT_EQ(Mixer::Create(o, &m), IM_OK);
  g_hook_calls = 0;
  const auto t0 = std::chrono::steady_clock::now();
  ASSERT_EQ(m->Start(), IM_OK);
  EXPECT_TRUE(m->running());
  EXPECT_EQ(m->Start(), IM_ERR_BAD_STATE);
  EXPECT_LT(std::chrono::steady_clock::now() - t0, std::chrono::milliseconds(100)); // Mixer::Start <= 100 ms
  std::this_thread::sleep_for(std::chrono::milliseconds(300));
  const auto s0 = std::chrono::steady_clock::now();
  m->Stop();
  EXPECT_LT(std::chrono::steady_clock::now() - s0, std::chrono::milliseconds(500)); // Stop <= 500 ms
  const auto elapsed_ms =
      std::chrono::duration_cast<std::chrono::milliseconds>(std::chrono::steady_clock::now() - t0).count();
  const uint64_t chunks = sink.chunks.load();
  EXPECT_GE(chunks, 20u);
  EXPECT_LE(chunks, static_cast<uint64_t>(elapsed_ms / 10 + 2));
  EXPECT_EQ(sink.gaps.load(), 1u);
  EXPECT_EQ(sink.off_grid.load(), 0u);
  EXPECT_EQ(g_hook_calls.load(), 101);
  std::this_thread::sleep_for(std::chrono::milliseconds(30));
  EXPECT_EQ(sink.chunks.load(), chunks); // nothing after Stop
  EXPECT_GT(m->busy_ns(), 0);
  // Again: a new grid.
  ASSERT_EQ(m->Start(), IM_OK);
  std::this_thread::sleep_for(std::chrono::milliseconds(50));
  m->Stop();
  EXPECT_EQ(sink.gaps.load(), 2u);
  EXPECT_EQ(sink.off_grid.load(), 0u);
}

// A stream on the real clock, fed by a thread standing in for the capture thread, while the test thread does the
// watcher's work: levels, flags, events, then the stream leaves.
TEST(Mixer, ACaptureThreadAndTheWatcherAtOnce) {
  CountingSink sink;
  MixerOptions o;
  o.sink = &sink;
  std::unique_ptr<Mixer> m;
  ASSERT_EQ(Mixer::Create(o, &m), IM_OK);
  CaptureRing ring;
  ChannelOptions co;
  co.ring = &ring;
  co.tag = 9;
  std::unique_ptr<Channel> ch;
  ASSERT_EQ(Channel::Create(std::move(co), &ch), IM_OK);
  ASSERT_TRUE(m->engine().Add(ch.get()));

  std::atomic<bool> stop{false};
  SteadyTickClock clock;
  std::thread capture([&] {
    test::Tone tone(1000, 0.5f);
    std::vector<float> buf(2 * kChunkFrames);
    uint64_t frame = 0;
    int64_t next = clock.NowNs();
    while (!stop.load()) {
      tone.Fill(frame, kChunkFrames, buf.data());
      ring.Write(buf.data(), kChunkFrames, next);
      frame += kChunkFrames;
      next += kChunkNs;
      std::this_thread::sleep_until(std::chrono::steady_clock::time_point(std::chrono::nanoseconds(next + kChunkNs)));
    }
  });
  ASSERT_EQ(m->Start(), IM_OK);
  LevelsSnapshot levels;
  std::vector<MixEvent> events(64);
  bool running_seen = false;
  for (int i = 0; i < 40; ++i) {
    std::this_thread::sleep_for(std::chrono::milliseconds(10));
    (void)m->engine().ReadLevels(&levels);
    for (uint32_t n = m->engine().DrainEvents(events.data(), 64), k = 0; k < n; ++k) {
      running_seen = running_seen || (events[k].code == MixEventCode::kStreamRunning && events[k].tag == 9);
    }
    m->engine().SetMuted(i == 20);
  }
  ch->Remove();
  for (int i = 0; i < 100 && !ch->released(); ++i) {
    std::this_thread::sleep_for(std::chrono::milliseconds(5));
  }
  EXPECT_TRUE(ch->released());
  m->Stop();
  stop = true;
  capture.join();
  EXPECT_TRUE(running_seen);
  EXPECT_GT(sink.loud.load(), 0u);
  EXPECT_EQ(sink.off_grid.load(), 0u);
}

} // namespace
} // namespace im::audiomix
