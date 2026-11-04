// The activator threads (docs/m2/05-windows-audio.md §8.1, §10.5; README WA45) with a fake opener: an activation that
// never completes times out after the pool's timeout (05: 3 s) and the pool goes on with the next request; a stop
// aborts every pending wait at once; at most 8 requests are queued or running; a cancel drops a queued request and
// aborts a running one; a completion handler that fires after the activator gave up, or after the pool is gone,
// touches nothing freed. Portable: no OS call, so it also builds and runs on the Mac.
//
// Waits are bounded and never compare clock readings shorter than Windows' 15.6 ms tick (README §4 "Testing").
#include <gtest/gtest.h>

#include <atomic>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <thread>
#include <vector>

#include "audio/activator.h"

namespace im::audio {
namespace {

using namespace std::chrono_literals;

constexpr int32_t kTimeoutHr = static_cast<int32_t>(0x800705B4u); // HRESULT_FROM_WIN32(ERROR_TIMEOUT)
constexpr int32_t kAbortHr = static_cast<int32_t>(0x80004004u);   // E_ABORT

class FakeClient final : public CaptureClient {
public:
  void *event() const noexcept override { return nullptr; }
  int32_t Start() noexcept override { return 0; }
  int32_t Drain(audiomix::CaptureRing &) noexcept override { return 0; }
  uint64_t discontinuities() const noexcept override { return 0; }
};

// An opener whose activations complete when the test says so (or never), like ActivateAudioInterfaceAsync's handler.
class FakeOpener final : public StreamOpener {
public:
  OpenResult Open(const OpenRequest &req, ActivationContext &ctx) override {
    auto c = std::make_shared<Completion>();
    {
      std::lock_guard<std::mutex> l(mu);
      pending.push_back(c);
      ++started;
      if (complete_at_once) {
        c->Complete();
      }
    }
    cv.notify_all();
    OpenResult r;
    r.id = req.id;
    const WaitResult w = ctx.Wait(c);
    {
      std::lock_guard<std::mutex> l(mu);
      waits.push_back(w);
    }
    cv.notify_all();
    switch (w) {
    case WaitResult::kDone:
      r.client = std::make_unique<FakeClient>();
      r.format = "float32";
      break;
    case WaitResult::kTimeout:
      r.hr = kTimeoutHr;
      r.timed_out = true;
      break;
    case WaitResult::kAborted:
      r.hr = kAbortHr;
      r.aborted = true;
      break;
    }
    return r;
  }

  // Waits until n activations started (bounded).
  bool WaitStarted(uint32_t n) {
    std::unique_lock<std::mutex> l(mu);
    return cv.wait_for(l, 5s, [&] { return started >= n; });
  }
  bool WaitWaits(std::size_t n) {
    std::unique_lock<std::mutex> l(mu);
    return cv.wait_for(l, 10s, [&] { return waits.size() >= n; });
  }
  void CompleteAll() {
    std::lock_guard<std::mutex> l(mu);
    for (auto &c : pending) {
      c->Complete();
    }
  }

  std::mutex mu;
  std::condition_variable cv;
  std::vector<std::shared_ptr<Completion>> pending;
  std::vector<WaitResult> waits;
  uint32_t started = 0;
  bool complete_at_once = false;
};

// The pool's results, waiting for at least n (bounded).
std::vector<OpenResult> Results(ActivatorPool &pool, std::size_t n) {
  std::vector<OpenResult> all;
  const auto end = std::chrono::steady_clock::now() + 10s;
  while (all.size() < n && std::chrono::steady_clock::now() < end) {
    for (auto &r : pool.TakeResults()) {
      all.push_back(std::move(r));
    }
    if (all.size() < n) {
      std::this_thread::sleep_for(2ms);
    }
  }
  return all;
}

TEST(AudioActivator, TheDefaultsAreTheSpecs) {
  EXPECT_EQ(kActivationTimeout, 3000ms);
  EXPECT_EQ(kActivatorThreads, 2u);
  EXPECT_EQ(kMaxActivationsInFlight, 8u);
  const ActivatorOptions o;
  EXPECT_EQ(o.timeout, kActivationTimeout);
  EXPECT_EQ(o.threads, kActivatorThreads);
}

TEST(AudioActivator, AnActivationThatNeverCompletesTimesOutAndThePoolGoesOn) {
  auto opener = std::make_shared<FakeOpener>();
  ActivatorOptions o;
  o.timeout = 200ms;
  ActivatorPool pool(opener, o);
  ASSERT_EQ(pool.Start(), IM_OK);
  const auto t0 = std::chrono::steady_clock::now();
  ASSERT_TRUE(pool.Submit(OpenRequest{1, audiopolicy::StreamKind::kInclude, 4242}));
  auto r = Results(pool, 1);
  const auto took = std::chrono::steady_clock::now() - t0;
  ASSERT_EQ(r.size(), 1u);
  EXPECT_EQ(r[0].id, 1u);
  EXPECT_TRUE(r[0].timed_out);
  EXPECT_FALSE(r[0].aborted);
  EXPECT_EQ(r[0].hr, kTimeoutHr);
  EXPECT_EQ(r[0].client, nullptr);
  EXPECT_GE(took, 150ms); // it waited for the timeout (with room for a coarse clock), not for ever
  EXPECT_LT(took, 5s);
  EXPECT_EQ(pool.in_flight(), 0u);

  // The pool goes on: the next activation completes.
  opener->complete_at_once = true;
  ASSERT_TRUE(pool.Submit(OpenRequest{2, audiopolicy::StreamKind::kInclude, 4243}));
  r = Results(pool, 1);
  ASSERT_EQ(r.size(), 1u);
  EXPECT_EQ(r[0].id, 2u);
  EXPECT_EQ(r[0].hr, 0);
  EXPECT_NE(r[0].client, nullptr);
  EXPECT_EQ(r[0].format, "float32");
}

// 05 §8.1's 3 s, with the default options: a request whose activation never completes comes back timed out.
TEST(AudioActivator, TheDefaultTimeoutIsThreeSeconds) {
  auto opener = std::make_shared<FakeOpener>();
  ActivatorPool pool(opener);
  ASSERT_EQ(pool.Start(), IM_OK);
  const auto t0 = std::chrono::steady_clock::now();
  ASSERT_TRUE(pool.Submit(OpenRequest{7, audiopolicy::StreamKind::kInclude, 1}));
  ASSERT_TRUE(opener->WaitStarted(1));
  std::this_thread::sleep_for(1s);
  EXPECT_TRUE(pool.TakeResults().empty()) << "gave up before 3 s";
  const auto r = Results(pool, 1);
  ASSERT_EQ(r.size(), 1u);
  EXPECT_TRUE(r[0].timed_out);
  EXPECT_GE(std::chrono::steady_clock::now() - t0, 2900ms);
}

TEST(AudioActivator, AStopAbortsEveryPendingWait) {
  auto opener = std::make_shared<FakeOpener>();
  ActivatorOptions o;
  o.timeout = 60s;
  ActivatorPool pool(opener, o);
  ASSERT_EQ(pool.Start(), IM_OK);
  ASSERT_TRUE(pool.Submit(OpenRequest{1, audiopolicy::StreamKind::kInclude, 1}));
  ASSERT_TRUE(pool.Submit(OpenRequest{2, audiopolicy::StreamKind::kInclude, 2}));
  ASSERT_TRUE(pool.Submit(OpenRequest{3, audiopolicy::StreamKind::kInclude, 3})); // queued behind the two threads
  ASSERT_TRUE(opener->WaitStarted(2));
  const auto t0 = std::chrono::steady_clock::now();
  pool.Stop();
  EXPECT_LT(std::chrono::steady_clock::now() - t0, 1s); // not the 60 s timeout
  EXPECT_FALSE(pool.running());
  ASSERT_TRUE(opener->WaitWaits(2));
  for (const WaitResult w : opener->waits) {
    EXPECT_EQ(w, WaitResult::kAborted);
  }
  EXPECT_EQ(opener->started, 2u); // the queued request never ran
  EXPECT_FALSE(pool.Submit(OpenRequest{4, audiopolicy::StreamKind::kInclude, 4})) << "a stopped pool takes nothing";
  // Start may follow.
  opener->complete_at_once = true;
  ASSERT_EQ(pool.Start(), IM_OK);
  ASSERT_TRUE(pool.Submit(OpenRequest{5, audiopolicy::StreamKind::kInclude, 5}));
  const auto r = Results(pool, 1);
  ASSERT_EQ(r.size(), 1u);
  EXPECT_EQ(r[0].id, 5u);
  EXPECT_EQ(r[0].hr, 0);
}

TEST(AudioActivator, AtMostEightInFlight) {
  auto opener = std::make_shared<FakeOpener>();
  ActivatorOptions o;
  o.timeout = 60s;
  ActivatorPool pool(opener, o);
  ASSERT_EQ(pool.Start(), IM_OK);
  for (uint64_t id = 1; id <= kMaxActivationsInFlight; ++id) {
    ASSERT_TRUE(pool.Submit(OpenRequest{id, audiopolicy::StreamKind::kInclude, static_cast<uint32_t>(id)})) << id;
  }
  EXPECT_EQ(pool.in_flight(), kMaxActivationsInFlight);
  EXPECT_FALSE(pool.Submit(OpenRequest{99, audiopolicy::StreamKind::kInclude, 99}));
  ASSERT_TRUE(opener->WaitStarted(2));
  EXPECT_EQ(opener->started, 2u) << "two activator threads";
  // Completing what runs lets the queue drain, one result per request.
  opener->complete_at_once = true;
  opener->CompleteAll();
  const auto r = Results(pool, kMaxActivationsInFlight);
  EXPECT_EQ(r.size(), kMaxActivationsInFlight);
  for (const auto &x : r) {
    EXPECT_EQ(x.hr, 0) << x.id;
  }
  EXPECT_EQ(pool.in_flight(), 0u);
  EXPECT_TRUE(pool.Submit(OpenRequest{99, audiopolicy::StreamKind::kInclude, 99}));
}

TEST(AudioActivator, CancelDropsAQueuedRequestAndAbortsARunningOne) {
  auto opener = std::make_shared<FakeOpener>();
  ActivatorOptions o;
  o.timeout = 60s;
  o.threads = 1;
  ActivatorPool pool(opener, o);
  ASSERT_EQ(pool.Start(), IM_OK);
  ASSERT_TRUE(pool.Submit(OpenRequest{1, audiopolicy::StreamKind::kInclude, 1}));
  ASSERT_TRUE(pool.Submit(OpenRequest{2, audiopolicy::StreamKind::kInclude, 2}));
  ASSERT_TRUE(opener->WaitStarted(1));
  pool.Cancel(2); // queued: dropped without a result
  pool.Cancel(1); // running: its wait is aborted
  const auto r = Results(pool, 1);
  ASSERT_EQ(r.size(), 1u);
  EXPECT_EQ(r[0].id, 1u);
  EXPECT_TRUE(r[0].aborted);
  EXPECT_EQ(r[0].client, nullptr);
  std::this_thread::sleep_for(50ms);
  EXPECT_TRUE(pool.TakeResults().empty());
  EXPECT_EQ(opener->started, 1u);
  EXPECT_EQ(pool.in_flight(), 0u);
}

TEST(AudioActivator, ACompletionAfterTheActivatorGaveUpIsHarmless) {
  auto opener = std::make_shared<FakeOpener>();
  std::shared_ptr<Completion> late;
  {
    ActivatorOptions o;
    o.timeout = 50ms;
    ActivatorPool pool(opener, o);
    ASSERT_EQ(pool.Start(), IM_OK);
    ASSERT_TRUE(pool.Submit(OpenRequest{1, audiopolicy::StreamKind::kInclude, 1}));
    const auto r = Results(pool, 1);
    ASSERT_EQ(r.size(), 1u);
    EXPECT_TRUE(r[0].timed_out);
    std::lock_guard<std::mutex> l(opener->mu);
    late = opener->pending.at(0); // what Windows' handler would still hold
  }
  // The pool is gone; the handler completes now, from another thread.
  std::thread t([late] { late->Complete(); });
  t.join();
  EXPECT_TRUE(late->done());
}

TEST(AudioActivator, AHandlerCompletingFromAnotherThreadWakesTheWait) {
  // An opener whose handler completes 30 ms later from a thread of its own (Windows calls it on an MTA thread).
  class AsyncOpener final : public StreamOpener {
  public:
    OpenResult Open(const OpenRequest &req, ActivationContext &ctx) override {
      auto c = std::make_shared<Completion>();
      std::thread([c] {
        std::this_thread::sleep_for(30ms);
        c->Complete();
      }).detach();
      OpenResult r;
      r.id = req.id;
      if (ctx.Wait(c) == WaitResult::kDone) {
        r.client = std::make_unique<FakeClient>();
      } else {
        r.hr = -1;
      }
      return r;
    }
  };
  std::atomic<int> wakes{0};
  ActivatorOptions o;
  o.on_result = [](void *ctx) { static_cast<std::atomic<int> *>(ctx)->fetch_add(1); };
  o.result_ctx = &wakes;
  ActivatorPool pool(std::make_shared<AsyncOpener>(), o);
  ASSERT_EQ(pool.Start(), IM_OK);
  ASSERT_TRUE(pool.Submit(OpenRequest{1, audiopolicy::StreamKind::kInclude, 1}));
  const auto r = Results(pool, 1);
  ASSERT_EQ(r.size(), 1u);
  EXPECT_EQ(r[0].hr, 0);
  EXPECT_EQ(wakes.load(), 1);
}

TEST(AudioActivator, ThreadHooksRunOnEveryThread) {
  struct Count {
    std::atomic<int> starting{0}, leaving{0};
  } count;
  ActivatorOptions o;
  o.thread_hook = [](void *ctx, bool starting) {
    auto *c = static_cast<Count *>(ctx);
    (starting ? c->starting : c->leaving).fetch_add(1);
  };
  o.hook_ctx = &count;
  {
    ActivatorPool pool(std::make_shared<FakeOpener>(), o);
    ASSERT_EQ(pool.Start(), IM_OK);
  }
  EXPECT_EQ(count.starting.load(), 2);
  EXPECT_EQ(count.leaving.load(), 2);
}

} // namespace
} // namespace im::audio
