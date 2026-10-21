// The real-time rule of the mixer and test-tone threads (docs/m2/04-windows-engine.md §6.1, §10.2): after Create,
// OnChunk and OnGap of the Opus stage (with the PCM tap and the Marker framing) and TestToneSource::Pump allocate
// nothing. This executable replaces the global operator new to count the C++ allocations made on the calling thread
// while the stage runs (libopus itself encodes on the stack and in the encoder's state, allocated by Create).
#include <gtest/gtest.h>

#include <cstdlib>
#include <memory>
#include <new>

#include "im/audio/opus_stage.h"
#include "im/audio/test_tone.h"

namespace {
thread_local bool t_counting = false;
thread_local long t_allocations = 0;

void *Allocate(std::size_t n) {
  if (t_counting) {
    t_allocations++;
  }
  if (void *p = std::malloc(n == 0 ? 1 : n)) {
    return p;
  }
  throw std::bad_alloc();
}
} // namespace

// The replacements carry the SAL annotations of vcruntime_new.h's declarations: without them MSVC reports C28251
// ("Inconsistent annotation for 'new'"), which /WX makes an error.
#if defined(_MSC_VER)
#define IM_NEW_RESULT(n)         _Ret_notnull_ _Post_writable_byte_size_(n)
#define IM_NEW_NOTHROW_RESULT(n) _Ret_maybenull_ _Success_(return != NULL) _Post_writable_byte_size_(n)
#else
#define IM_NEW_RESULT(n)
#define IM_NEW_NOTHROW_RESULT(n)
#endif

IM_NEW_RESULT(n) void *operator new(std::size_t n) { return Allocate(n); }
IM_NEW_RESULT(n) void *operator new[](std::size_t n) { return Allocate(n); }
IM_NEW_NOTHROW_RESULT(n) void *operator new(std::size_t n, const std::nothrow_t &) noexcept {
  if (t_counting) {
    t_allocations++;
  }
  return std::malloc(n == 0 ? 1 : n);
}
IM_NEW_NOTHROW_RESULT(n) void *operator new[](std::size_t n, const std::nothrow_t &) noexcept {
  if (t_counting) {
    t_allocations++;
  }
  return std::malloc(n == 0 ? 1 : n);
}
void operator delete(void *p) noexcept { std::free(p); }
void operator delete[](void *p) noexcept { std::free(p); }
void operator delete(void *p, std::size_t) noexcept { std::free(p); }
void operator delete[](void *p, std::size_t) noexcept { std::free(p); }
void operator delete(void *p, const std::nothrow_t &) noexcept { std::free(p); }
void operator delete[](void *p, const std::nothrow_t &) noexcept { std::free(p); }

namespace {

// A ring that takes everything and keeps nothing.
class Counter final : public im::audio::PacketWriter {
public:
  bool Write(const im::audio::AudioPacket &p) noexcept override {
    bytes += p.size;
    return (++calls % 7) != 0; // and refuses some, to run the DISCONTINUITY path
  }
  long calls = 0;
  uint64_t bytes = 0;
};

TEST(OpusStageRealTime, NoAllocationAfterCreate) {
  Counter audio, pcm;
  std::unique_ptr<im::audio::OpusStage> stage;
  ASSERT_EQ(
      im::audio::OpusStage::Create({.flags = IM_AUDIOF_TAP_PCM, .audio = &audio, .pcm = &pcm, .marker = true}, &stage),
      IM_OK);
  im::audio::TestToneSource src(0, stage->lookahead_ns(), stage.get());

  t_counting = true;
  t_allocations = 0;
  for (int64_t now = 0; now < 5'000'000'000; now += 2'000'000) {
    (void)src.Pump(now);
    if (now == 2'000'000'000) {
      src.tone().SetPaused(true);
      (void)stage->SetBitrate(256000);
      stage->OnGap(now); // a new grid mid-run
    }
    if (now == 3'000'000'000) {
      src.tone().SetPaused(false);
    }
  }
  (void)stage->stats();
  t_counting = false;

  EXPECT_EQ(t_allocations, 0);
  EXPECT_GT(audio.calls, 200);
  EXPECT_GT(pcm.calls, 400);
  EXPECT_GT(stage->stats().drops, 0u);
  EXPECT_EQ(stage->stats().gaps, 2u); // the source's first grid and the one mid-run
  EXPECT_EQ(stage->stats().bitrate_bps, 256000u);
}

TEST(OpusStageRealTime, TheCounterWorks) {
  t_counting = true;
  t_allocations = 0;
  auto p = std::make_unique<int>(1);
  t_counting = false;
  EXPECT_EQ(t_allocations, 1);
  EXPECT_EQ(*p, 1);
}

} // namespace
