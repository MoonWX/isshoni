// A session's event queue (docs/m2/04-windows-engine.md §6.2, §18.1): FIFO order and copies; 4 producers; overflow
// drops the oldest non-ENDED event and counts it; ENDED always delivered last; the 1 KiB JSON limit; the wake-up.
// Clean under ThreadSanitizer (host-tsan).
#include <gtest/gtest.h>

#include <atomic>
#include <cstdio>
#include <string>
#include <thread>
#include <vector>

#include "im/ring/event_queue.h"
#include "im/ring/signal.h"
#include "isshoni_media.h"

namespace {

using im::ring::EventPush;
using im::ring::EventQueue;
using im::ring::EventRecord;
using im::ring::kMaxEventJson;
using im::ring::WakeSignal;

TEST(EventQueue, FifoAndCopies) {
  EventQueue q;
  EXPECT_EQ(q.capacity(), 64u);
  std::string json = R"({"layer":"f","limit":"cpu","fps":31.5})";
  ASSERT_EQ(q.Push(IM_EVENT_LIMIT_CHANGED, IM_OK, 0, 100, json), EventPush::kOk);
  json[2] = 'X'; // the queue copied it
  ASSERT_EQ(q.Push(IM_EVENT_SOURCE_MINIMIZED, IM_OK, IM_NO_LAYER, 200, "{}"), EventPush::kOk);
  EXPECT_EQ(q.size(), 2u);
  EventRecord r;
  ASSERT_TRUE(q.Pop(&r));
  EXPECT_EQ(r.event, IM_EVENT_LIMIT_CHANGED);
  EXPECT_EQ(r.status, IM_OK);
  EXPECT_EQ(r.layer, 0u);
  EXPECT_EQ(r.ts_ns, 100);
  EXPECT_EQ(r.json_view(), R"({"layer":"f","limit":"cpu","fps":31.5})");
  ASSERT_TRUE(q.Pop(&r));
  EXPECT_EQ(r.event, IM_EVENT_SOURCE_MINIMIZED);
  EXPECT_EQ(r.layer, static_cast<uint32_t>(IM_NO_LAYER));
  EXPECT_FALSE(q.Pop(&r));
  EXPECT_FALSE(q.ended());
}

TEST(EventQueue, OverflowDropsTheOldest) {
  EventQueue q(nullptr, 4);
  for (int i = 0; i < 4; ++i) {
    ASSERT_EQ(q.Push(IM_EVENT_RING_OVERFLOW, IM_OK, IM_NO_LAYER, i, "{}"), EventPush::kOk);
  }
  EXPECT_EQ(q.Push(IM_EVENT_RING_OVERFLOW, IM_OK, IM_NO_LAYER, 4, "{}"), EventPush::kDroppedOldest);
  EXPECT_EQ(q.Push(IM_EVENT_RING_OVERFLOW, IM_OK, IM_NO_LAYER, 5, "{}"), EventPush::kDroppedOldest);
  EXPECT_EQ(q.TakeDropped(), 2u) << "counted for the next RING_OVERFLOW";
  EXPECT_EQ(q.TakeDropped(), 0u);
  EventRecord r;
  std::vector<int64_t> got;
  while (q.Pop(&r)) {
    got.push_back(r.ts_ns);
  }
  EXPECT_EQ(got, (std::vector<int64_t>{2, 3, 4, 5}));
}

TEST(EventQueue, EndedIsAlwaysDeliveredLast) {
  EventQueue q(nullptr, 4);
  for (int i = 0; i < 4; ++i) {
    q.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, i, "{}");
  }
  // A full queue makes room for ENDED by dropping the oldest event.
  EXPECT_EQ(q.Push(IM_EVENT_ENDED, IM_ERR_ENCODER_LOST, IM_NO_LAYER, 10,
                   R"({"reason":"encoder_lost","detail":"device_removed: DXGI_ERROR_DEVICE_REMOVED"})"),
            EventPush::kDroppedOldest);
  EXPECT_TRUE(q.ended());
  // Nothing is accepted after it, a second ENDED included.
  EXPECT_EQ(q.Push(IM_EVENT_CAPTURE_RESUMED, IM_OK, IM_NO_LAYER, 11, "{}"), EventPush::kAfterEnded);
  EXPECT_EQ(q.Push(IM_EVENT_ENDED, IM_OK, IM_NO_LAYER, 12, "{}"), EventPush::kAfterEnded);
  EventRecord r;
  std::vector<int32_t> events;
  while (q.Pop(&r)) {
    events.push_back(r.event);
  }
  ASSERT_EQ(events.size(), 4u);
  EXPECT_EQ(events.back(), IM_EVENT_ENDED);
  EXPECT_EQ(r.status, IM_ERR_ENCODER_LOST) << "the first cause wins";
  EXPECT_TRUE(q.ended_delivered());
  EXPECT_EQ(q.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, 13, "{}"), EventPush::kAfterEnded);
}

TEST(EventQueue, JsonLimit) {
  EventQueue q;
  const std::string max(kMaxEventJson, 'a');
  const std::string over(kMaxEventJson + 1, 'a');
  EXPECT_EQ(q.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, 1, max), EventPush::kOk);
  EXPECT_EQ(q.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, 2, over), EventPush::kTooLarge);
  EXPECT_EQ(q.size(), 1u) << "refused";
  // ENDED is never refused: its JSON becomes {} and the status still carries the cause.
  EXPECT_EQ(q.Push(IM_EVENT_ENDED, IM_ERR_INTERNAL, IM_NO_LAYER, 3, over), EventPush::kTooLarge);
  EventRecord r;
  ASSERT_TRUE(q.Pop(&r));
  EXPECT_EQ(r.size, kMaxEventJson);
  ASSERT_TRUE(q.Pop(&r));
  EXPECT_EQ(r.event, IM_EVENT_ENDED);
  EXPECT_EQ(r.status, IM_ERR_INTERNAL);
  EXPECT_EQ(r.json_view(), "{}");
}

TEST(EventQueue, PushWakesTheConsumer) {
  WakeSignal sig;
  EventQueue q(&sig);
  const uint32_t seen = sig.Snapshot();
  std::thread t([&] { q.Push(IM_EVENT_SOURCE_RESTORED, IM_OK, IM_NO_LAYER, 1, "{}"); });
  EXPECT_TRUE(sig.WaitFor(seen, 2000));
  t.join();
  EventRecord r;
  EXPECT_TRUE(q.Pop(&r));
}

TEST(EventQueue, FourProducersAndEndedLast) {
  constexpr int kProducers = 4;
  constexpr int kPerProducer = 20000;
  WakeSignal sig;
  EventQueue q(&sig);
  std::atomic<int> accepted{0};
  std::atomic<int> refused{0};
  std::atomic<bool> go{false};
  std::atomic<int> done{0};
  auto produce = [&](int id) {
    while (!go.load()) {
      std::this_thread::yield();
    }
    char json[64];
    for (int i = 0; i < kPerProducer; ++i) {
      const int n = std::snprintf(json, sizeof(json), R"({"p":%d,"i":%d})", id, i);
      // Producer 0 ends the session halfway; the others keep pushing.
      const bool end = id == 0 && i == kPerProducer / 2;
      const EventPush r = q.Push(end ? IM_EVENT_ENDED : IM_EVENT_RING_OVERFLOW, IM_OK, static_cast<uint32_t>(id), i,
                                 std::string_view(json, static_cast<std::size_t>(n)));
      if (r == EventPush::kOk || r == EventPush::kDroppedOldest) {
        accepted.fetch_add(1);
      } else {
        refused.fetch_add(1);
      }
    }
    done.fetch_add(1);
  };
  std::vector<std::thread> threads;
  for (int id = 0; id < kProducers; ++id) {
    threads.emplace_back(produce, id);
  }
  go = true;
  std::vector<int> last(kProducers, -1);
  int delivered = 0;
  int ended_at = -1;
  int out_of_order = 0;
  EventRecord r;
  for (;;) {
    const uint32_t seen = sig.Snapshot();
    if (q.Pop(&r)) {
      ASSERT_LT(ended_at, 0) << "an event after ENDED";
      if (r.event == IM_EVENT_ENDED) {
        ended_at = delivered;
      }
      const auto p = static_cast<std::size_t>(r.layer);
      if (r.ts_ns <= last[p]) {
        ++out_of_order;
      }
      last[p] = static_cast<int>(r.ts_ns);
      ++delivered;
      continue;
    }
    if (done.load() == kProducers && q.size() == 0) {
      break;
    }
    sig.WaitFor(seen, 10);
  }
  for (auto &t : threads) {
    t.join();
  }
  EXPECT_EQ(out_of_order, 0) << "each producer's events stay in order";
  ASSERT_GE(ended_at, 0) << "ENDED was delivered";
  EXPECT_EQ(ended_at, delivered - 1) << "ENDED is the last event";
  EXPECT_EQ(accepted.load() + refused.load(), kProducers * kPerProducer);
  EXPECT_EQ(static_cast<uint64_t>(delivered) + q.TakeDropped(), static_cast<uint64_t>(accepted.load()))
      << "every accepted event is delivered or counted as dropped";
}

} // namespace
