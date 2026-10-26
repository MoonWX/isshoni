// No allocation on the mixer thread after Mixer::Start (docs/m2/05-windows-audio.md §19.1, 04 §6.1): 10 simulated
// minutes of the real mixer thread with speexdsp, ratio changes, onsets, silent-bypass exits, a stream leaving and the
// silence flags, while an interposer counts malloc, calloc, realloc and free (and the C++ operator new and delete)
// called from that thread. The count must be 0.
//
// Where it counts: on Linux with glibc the malloc interposer sees every call of the process (it replaces glibc's
// symbols); on macOS it sees the code linked into this executable (speexdsp, im_audiomix, the test), and libc++'s own
// allocations go through the replaced operator new. Under a sanitizer the runtime owns malloc and operator new (a
// second definition fails to link), and MSVC's static CRT can't be interposed: those builds run the same simulation
// without counting and say so in a log line.
#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <cstdio>
#include <cstdlib>
#include <new>
#include <thread>

#include "im/audiomix/mixer.h"
#include "mix_test_support.h"

#if defined(__SANITIZE_ADDRESS__) || defined(__SANITIZE_THREAD__)
#define IM_SANITIZED 1
#elif defined(__has_feature)
#if __has_feature(address_sanitizer) || __has_feature(thread_sanitizer)
#define IM_SANITIZED 1
#endif
#endif

namespace {

// The thread whose calls are counted (none: a default id) and the count.
std::atomic<std::thread::id> g_watched{};
std::atomic<long> g_calls{0};

[[maybe_unused]] inline void Note() noexcept { // unused when nothing is counted
  if (g_watched.load(std::memory_order_relaxed) == std::this_thread::get_id()) {
    g_calls.fetch_add(1, std::memory_order_relaxed);
  }
}

} // namespace

#if !defined(IM_SANITIZED) && defined(__linux__) && defined(__GLIBC__)
#define IM_MALLOC_COUNTED 1
extern "C" {
void *__libc_malloc(size_t);
void *__libc_calloc(size_t, size_t);
void *__libc_realloc(void *, size_t);
void __libc_free(void *);
void *malloc(size_t n) noexcept {
  Note();
  return __libc_malloc(n);
}
void *calloc(size_t a, size_t b) noexcept {
  Note();
  return __libc_calloc(a, b);
}
void *realloc(void *p, size_t n) noexcept {
  Note();
  return __libc_realloc(p, n);
}
void free(void *p) noexcept {
  if (p != nullptr) {
    Note();
  }
  __libc_free(p);
}
}
#elif !defined(IM_SANITIZED) && defined(__APPLE__)
#define IM_MALLOC_COUNTED 1
extern "C" {
struct _malloc_zone_t;
_malloc_zone_t *malloc_default_zone(void);
_malloc_zone_t *malloc_zone_from_ptr(const void *);
void *malloc_zone_malloc(_malloc_zone_t *, size_t);
void *malloc_zone_calloc(_malloc_zone_t *, size_t, size_t);
void *malloc_zone_realloc(_malloc_zone_t *, void *, size_t);
void malloc_zone_free(_malloc_zone_t *, void *);
void *malloc(size_t n) {
  Note();
  return malloc_zone_malloc(malloc_default_zone(), n);
}
void *calloc(size_t a, size_t b) {
  Note();
  return malloc_zone_calloc(malloc_default_zone(), a, b);
}
void *realloc(void *p, size_t n) {
  Note();
  if (p == nullptr) {
    return malloc_zone_malloc(malloc_default_zone(), n);
  }
  return malloc_zone_realloc(malloc_zone_from_ptr(p), p, n);
}
void free(void *p) {
  if (p == nullptr) {
    return;
  }
  Note();
  if (_malloc_zone_t *z = malloc_zone_from_ptr(p); z != nullptr) {
    malloc_zone_free(z, p);
  }
}
}
#endif

// The C++ allocations, as opus_stage_rt_test counts them (with vcruntime's SAL annotations for MSVC's /W4 /WX).
#if defined(_MSC_VER)
#define IM_NEW_RESULT(n)         _Ret_notnull_ _Post_writable_byte_size_(n)
#define IM_NEW_NOTHROW_RESULT(n) _Ret_maybenull_ _Success_(return != NULL) _Post_writable_byte_size_(n)
#else
#define IM_NEW_RESULT(n)
#define IM_NEW_NOTHROW_RESULT(n)
#endif

#if !defined(IM_SANITIZED)
#define IM_NEW_COUNTED 1
namespace {
void *Allocate(std::size_t n) {
  Note();
  if (void *p = std::malloc(n == 0 ? 1 : n)) {
    return p;
  }
  throw std::bad_alloc();
}
void Release(void *p) noexcept {
  if (p != nullptr) {
    Note();
  }
  std::free(p);
}
} // namespace
IM_NEW_RESULT(n) void *operator new(std::size_t n) { return Allocate(n); }
IM_NEW_RESULT(n) void *operator new[](std::size_t n) { return Allocate(n); }
IM_NEW_NOTHROW_RESULT(n) void *operator new(std::size_t n, const std::nothrow_t &) noexcept {
  Note();
  return std::malloc(n == 0 ? 1 : n);
}
IM_NEW_NOTHROW_RESULT(n) void *operator new[](std::size_t n, const std::nothrow_t &) noexcept {
  Note();
  return std::malloc(n == 0 ? 1 : n);
}
void operator delete(void *p) noexcept { Release(p); }
void operator delete[](void *p) noexcept { Release(p); }
void operator delete(void *p, std::size_t) noexcept { Release(p); }
void operator delete[](void *p, std::size_t) noexcept { Release(p); }
void operator delete(void *p, const std::nothrow_t &) noexcept { Release(p); }
void operator delete[](void *p, const std::nothrow_t &) noexcept { Release(p); }
#endif

namespace im::audiomix {
namespace {

constexpr int64_t kMs = kNsPerMs;
constexpr int64_t kSecond = kNsPerSecond;

// A sink that keeps nothing.
class NullSink final : public ChunkSink {
public:
  void OnChunk(const float *, int64_t t) noexcept override {
    if (have_ && t - last_ != kChunkNs) {
      ++off_grid;
    }
    have_ = true;
    last_ = t;
    ++chunks;
  }
  void OnGap(int64_t) noexcept override { ++gaps; }
  uint64_t chunks = 0, gaps = 0, off_grid = 0;

private:
  bool have_ = false;
  int64_t last_ = 0;
};

// Simulated time for the mixer thread: each wait jumps to the deadline and lets the simulated captures write what
// arrived by then, on the mixer thread itself (so their work is counted too). At the end it raises done and waits
// for Stop.
class SimClock final : public TickClock {
public:
  int64_t NowNs() noexcept override { return now_; }
  void WaitUntil(int64_t deadline_ns, const std::atomic<bool> &stop) noexcept override {
    if (now_ < deadline_ns) {
      now_ = deadline_ns;
    }
    for (test::Producer *p : producers) {
      if (p != nullptr) {
        p->RunUntil(now_);
      }
    }
    if (observe) {
      observe(observe_ctx);
    }
    if (now_ >= end) {
      done.store(true, std::memory_order_release);
      while (!stop.load(std::memory_order_acquire)) {
        std::this_thread::yield();
      }
    }
  }
  void Interrupt() noexcept override {}

  test::Producer *producers[4] = {};
  void (*observe)(void *) = nullptr;
  void *observe_ctx = nullptr;
  int64_t end = 0;
  std::atomic<bool> done{false};

private:
  int64_t now_ = 0;
};

void Watch(void *, bool starting) {
  if (starting) {
    g_calls.store(0, std::memory_order_relaxed);
    g_watched.store(std::this_thread::get_id(), std::memory_order_relaxed);
  } else {
    g_watched.store(std::thread::id{}, std::memory_order_relaxed);
  }
}

// What the mixer thread saw (written only there, read after the join).
struct Seen {
  Channel *a = nullptr;
  int32_t last_ppm = 0;
  int ppm_changes = 0;
  int bypass_exits = 0;
  bool bypassed = false;
};

void Observe(void *ctx) {
  auto *s = static_cast<Seen *>(ctx);
  if (s->a->released()) {
    return;
  }
  const int32_t ppm = s->a->ppm();
  s->ppm_changes += ppm != s->last_ppm;
  s->last_ppm = ppm;
  const bool b = s->a->bypassed();
  s->bypass_exits += s->bypassed && !b;
  s->bypassed = b;
}

TEST(MixerAllocation, NoAllocationOnTheMixerThreadAfterStart) {
  constexpr bool kCounted =
#if defined(IM_MALLOC_COUNTED) || defined(IM_NEW_COUNTED)
      true;
#else
      false;
#endif
#if defined(IM_SANITIZED)
  const int64_t minutes = 2;
#else
  const int64_t minutes = 10;
#endif

  // The counter itself works: calls on a watched thread are seen.
  Watch(nullptr, true);
  {
    void *p = std::malloc(64);
    std::free(p);
    auto q = std::make_unique<int[]>(16);
    std::unique_ptr<Resampler> r;
    ASSERT_EQ(NewSpeexResampler(&r), IM_OK); // speexdsp's calloc
  }
  const long control = g_calls.load();
  (void)control; // unchecked when nothing is counted
  Watch(nullptr, false);
#if defined(IM_MALLOC_COUNTED)
  EXPECT_GE(control, 6); // malloc, free, new[], delete[], speexdsp's callocs and frees
#elif defined(IM_NEW_COUNTED)
  EXPECT_GE(control, 2);
#endif
#if !defined(IM_MALLOC_COUNTED)
  std::printf("note: this build counts %s on the mixer thread (malloc is counted on Linux with glibc and on macOS "
              "without a sanitizer)\n",
              kCounted ? "C++ allocations only" : "nothing");
#endif

  // Stream A, from a device 300 ppm fast: a tone for the first minute (the ratio converges through many changes),
  // then 300 ms tone bursts every 2 s (onsets after silence, silent-bypass exits). Stream B: silent, a tone in its
  // fourth minute, then silent again; it leaves at minute 6 (at minute 1 of a sanitizer build's 2-minute run).
  test::Segments sa, sb;
  for (int64_t s = 0; s < minutes * 60; s += 2) {
    const auto from = static_cast<uint64_t>(s * kRate) + 1000;
    sa.Add(from, from + 300 * kFramesPerMs, s % 4 == 0 ? 1000 : 440, 0.5f);
    if (s / 2 >= 15) {
      break; // 16 segments at most: the rest repeats below
    }
  }
  struct Repeat final : public test::Signal {
    test::Tone *steady;
    test::Segments *inner;
    uint64_t steady_frames;
    uint64_t period;
    void Fill(uint64_t first, uint32_t n, float *out) override {
      for (uint32_t k = 0; k < n; ++k) {
        if (first + k < steady_frames) {
          steady->Fill(first + k, 1, out + 2 * k);
        } else {
          inner->Fill((first + k - steady_frames) % period, 1, out + 2 * k);
        }
      }
    }
  } ra;
  test::Tone steady(1000, 0.5f);
  ra.steady = &steady;
  ra.steady_frames = 60 * kRate;
  ra.inner = &sa;
  ra.period = 32 * kRate; // the 16 bursts repeat every 32 s
  sb.Add(static_cast<uint64_t>(180 * kRate), static_cast<uint64_t>(240 * kRate), 1000, 0.3f);

  CaptureRing ring_a, ring_b;
  test::ProducerConfig pa;
  pa.drift_ppm = 300;
  pa.jitter_ns = 3 * kMs;
  pa.seed = 1;
  test::ProducerConfig pb = pa;
  pb.drift_ppm = -200;
  pb.seed = 2;
  test::Producer prod_a(&ring_a, &ra, pa), prod_b(&ring_b, &sb, pb);

  NullSink sink;
  SimClock clock;
  clock.producers[0] = &prod_a;
  clock.producers[1] = &prod_b;
  clock.end = minutes * 60 * kSecond;
  MixerOptions mo;
  mo.sink = &sink;
  mo.clock = &clock;
  mo.thread_hook = &Watch;
  std::unique_ptr<Mixer> mixer;
  ASSERT_EQ(Mixer::Create(mo, &mixer), IM_OK);
  ChannelOptions oa, ob;
  oa.ring = &ring_a;
  oa.tag = 1;
  ob.ring = &ring_b;
  ob.tag = 2;
  std::unique_ptr<Channel> a, b;
  ASSERT_EQ(Channel::Create(std::move(oa), &a), IM_OK);
  ASSERT_EQ(Channel::Create(std::move(ob), &b), IM_OK);
  ASSERT_TRUE(mixer->engine().Add(a.get()));
  ASSERT_TRUE(mixer->engine().Add(b.get()));
  Seen seen;
  seen.a = a.get();
  clock.observe = &Observe;
  clock.observe_ctx = &seen;

  ASSERT_EQ(mixer->Start(), IM_OK);
  // The watcher's side meanwhile: levels, the silence flags, B leaving.
  LevelsSnapshot levels;
  const int64_t leave_chunks = minutes * 6 / 10 * 60 * 100;
  bool removed = false;
  while (!clock.done.load(std::memory_order_acquire)) {
    (void)mixer->engine().ReadLevels(&levels);
    const int64_t chunks = static_cast<int64_t>(mixer->engine().counters().chunks);
    mixer->engine().SetPaused(chunks % 9000 > 8800); // a 2 s pause every 90 s
    if (!removed && chunks > leave_chunks) {
      b->Remove();
      removed = true;
    }
    std::this_thread::sleep_for(std::chrono::milliseconds(1));
  }
  mixer->Stop();
  const long calls = g_calls.load();

  EXPECT_TRUE(b->released());
  EXPECT_GE(sink.chunks, static_cast<uint64_t>(minutes * 60 * 100));
  EXPECT_EQ(sink.gaps, 1u);
  EXPECT_EQ(sink.off_grid, 0u);
  EXPECT_GT(seen.ppm_changes, 10);
  EXPECT_GT(seen.bypass_exits, (minutes - 1) * 60 / 2 / 2);
  if constexpr (kCounted) {
    EXPECT_EQ(calls, 0) << "allocations on the mixer thread after Mixer::Start";
  } else {
    std::printf("note: allocation count skipped (sanitizer or MSVC build): the simulation ran without counting\n");
  }
}

} // namespace
} // namespace im::audiomix
