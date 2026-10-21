// The session handle table (docs/m2/04-windows-engine.md §6.3, §18.1): generations, stale and foreign handles, the 16
// slots, the generation wrap-around and concurrent use.
#include <gtest/gtest.h>

#include <atomic>
#include <memory>
#include <set>
#include <thread>
#include <vector>

#include "im/abi/handles.h"

namespace {

struct Obj {
  int id;
};

using Table = im::abi::HandleTable<Obj>;

TEST(Handles, LayoutIsIndexAndGeneration) {
  static_assert(Table::kSlots == 16);
  static_assert(Table::MakeHandle(3, 7) == ((uint64_t{3} << 32) | 7));
  static_assert(Table::IndexOf(Table::MakeHandle(15, 1)) == 15);
  static_assert(Table::GenerationOf(Table::MakeHandle(15, 0xFFFFFFFFu)) == 0xFFFFFFFFu);

  Table t;
  const auto h = t.Insert(std::make_shared<Obj>(Obj{1}));
  EXPECT_EQ(Table::IndexOf(h), 0u);
  EXPECT_EQ(Table::GenerationOf(h), 1u);
  ASSERT_NE(t.Get(h), nullptr);
  EXPECT_EQ(t.Get(h)->id, 1);
}

TEST(Handles, ZeroIsNeverValid) {
  Table t;
  EXPECT_EQ(t.Get(0), nullptr);
  EXPECT_EQ(t.Remove(0), nullptr);
  for (int i = 0; i < 100; ++i) {
    const auto h = t.Insert(std::make_shared<Obj>(Obj{i}));
    EXPECT_NE(h, 0u);
    t.Remove(h);
  }
  EXPECT_EQ(t.Insert(nullptr), 0u);
}

TEST(Handles, StaleHandlesAreRejected) {
  Table t;
  const auto h1 = t.Insert(std::make_shared<Obj>(Obj{1}));
  ASSERT_NE(t.Remove(h1), nullptr);
  EXPECT_EQ(t.Get(h1), nullptr) << "removed";
  EXPECT_EQ(t.Remove(h1), nullptr) << "removed twice";

  // Reuse every slot until slot 0 comes round again: the old handle must stay dead though the index is the same.
  std::vector<uint64_t> live;
  uint64_t again = 0;
  for (uint32_t i = 0; i < Table::kSlots; ++i) {
    const auto h = t.Insert(std::make_shared<Obj>(Obj{static_cast<int>(i)}));
    ASSERT_NE(h, 0u);
    if (Table::IndexOf(h) == Table::IndexOf(h1)) {
      again = h;
    }
    live.push_back(h);
  }
  ASSERT_NE(again, 0u);
  EXPECT_NE(again, h1);
  EXPECT_EQ(Table::GenerationOf(again), Table::GenerationOf(h1) + 1);
  EXPECT_EQ(t.Get(h1), nullptr);
  EXPECT_NE(t.Get(again), nullptr);
}

TEST(Handles, ForeignHandlesAreRejected) {
  Table t;
  const auto h = t.Insert(std::make_shared<Obj>(Obj{1}));
  EXPECT_EQ(t.Get(Table::MakeHandle(Table::IndexOf(h), Table::GenerationOf(h) + 1)), nullptr) << "wrong generation";
  EXPECT_EQ(t.Get(Table::MakeHandle(Table::IndexOf(h), 0)), nullptr) << "generation 0";
  EXPECT_EQ(t.Get(Table::MakeHandle(1, Table::GenerationOf(h))), nullptr) << "empty slot";
  EXPECT_EQ(t.Get(Table::MakeHandle(16, 1)), nullptr) << "index past the table";
  EXPECT_EQ(t.Get(Table::MakeHandle(0xFFFFFFFFu, 1)), nullptr);
  EXPECT_EQ(t.Get(~uint64_t{0}), nullptr);
  EXPECT_EQ(t.Remove(Table::MakeHandle(16, 1)), nullptr);
  EXPECT_EQ(t.Count(), 1u);
}

TEST(Handles, SixteenSlotsAndTheLimit) {
  Table t;
  std::set<uint64_t> handles;
  for (uint32_t i = 0; i < Table::kSlots; ++i) {
    const auto h = t.Insert(std::make_shared<Obj>(Obj{static_cast<int>(i)}));
    ASSERT_NE(h, 0u) << i;
    handles.insert(h);
  }
  EXPECT_EQ(handles.size(), Table::kSlots);
  EXPECT_EQ(t.Count(), Table::kSlots);
  EXPECT_EQ(t.Insert(std::make_shared<Obj>(Obj{99})), 0u) << "the table is full";

  const auto one = *handles.begin();
  ASSERT_NE(t.Remove(one), nullptr);
  EXPECT_NE(t.Insert(std::make_shared<Obj>(Obj{100})), 0u) << "a freed slot is usable again";

  // The engine's session limit (4, 04 §6.4) is applied atomically with the insert.
  Table s;
  for (int i = 0; i < 4; ++i) {
    EXPECT_NE(s.Insert(std::make_shared<Obj>(Obj{i}), 4), 0u);
  }
  EXPECT_EQ(s.Insert(std::make_shared<Obj>(Obj{4}), 4), 0u) << "fifth session";
  EXPECT_EQ(s.Count(), 4u);
}

TEST(Handles, SlotsAreUsedRoundRobin) {
  Table t;
  const auto a = t.Insert(std::make_shared<Obj>(Obj{1}));
  t.Remove(a);
  const auto b = t.Insert(std::make_shared<Obj>(Obj{2}));
  EXPECT_NE(Table::IndexOf(a), Table::IndexOf(b)) << "a just-freed slot is not handed out again at once";
  t.Remove(b);
  uint32_t last = Table::IndexOf(b);
  for (int i = 0; i < 40; ++i) {
    const auto h = t.Insert(std::make_shared<Obj>(Obj{i}));
    EXPECT_EQ(Table::IndexOf(h), (last + 1) % Table::kSlots);
    last = Table::IndexOf(h);
    t.Remove(h);
  }
}

TEST(Handles, GenerationWrapsAroundPastZero) {
  static_assert(Table::NextGeneration(1) == 2);
  static_assert(Table::NextGeneration(0xFFFFFFFEu) == 0xFFFFFFFFu);
  static_assert(Table::NextGeneration(0xFFFFFFFFu) == 1);

  im::abi::HandleTable<Obj, 1> t(0xFFFFFFFEu); // one slot, so every insert reuses it
  const auto h1 = t.Insert(std::make_shared<Obj>(Obj{1}));
  EXPECT_EQ(decltype(t)::GenerationOf(h1), 0xFFFFFFFEu);
  t.Remove(h1);
  const auto h2 = t.Insert(std::make_shared<Obj>(Obj{2}));
  EXPECT_EQ(decltype(t)::GenerationOf(h2), 0xFFFFFFFFu);
  t.Remove(h2);
  const auto h3 = t.Insert(std::make_shared<Obj>(Obj{3}));
  EXPECT_EQ(decltype(t)::GenerationOf(h3), 1u) << "skips 0";
  EXPECT_NE(h3, 0u);
  EXPECT_EQ(t.Get(h1), nullptr);
  EXPECT_EQ(t.Get(h2), nullptr);
  EXPECT_EQ(t.Get(h3)->id, 3);
}

TEST(Handles, AnObjectOutlivesItsRemoval) {
  Table t;
  const auto h = t.Insert(std::make_shared<Obj>(Obj{7}));
  auto held = t.Get(h);            // a control call holding the session
  ASSERT_NE(t.Remove(h), nullptr); // im_stop on another thread
  EXPECT_EQ(held->id, 7);
  EXPECT_EQ(t.Get(h), nullptr);
}

TEST(Handles, RemoveAllAndSnapshot) {
  Table t;
  const auto a = t.Insert(std::make_shared<Obj>(Obj{1}));
  const auto b = t.Insert(std::make_shared<Obj>(Obj{2}));
  const auto snap = t.Snapshot();
  ASSERT_EQ(snap.size(), 2u);
  EXPECT_EQ(snap[0].first, a);
  EXPECT_EQ(snap[1].first, b);
  const auto all = t.RemoveAll();
  EXPECT_EQ(all.size(), 2u);
  EXPECT_EQ(t.Count(), 0u);
  EXPECT_EQ(t.Get(a), nullptr);
  EXPECT_EQ(t.Get(b), nullptr);
}

// Starts, stops and control calls racing from several threads (run under host-tsan too).
TEST(Handles, ConcurrentUse) {
  Table t;
  std::atomic<bool> stop{false};
  std::atomic<uint64_t> last{0};
  std::atomic<int> found{0};
  std::vector<std::thread> threads;
  for (int w = 0; w < 4; ++w) {
    threads.emplace_back([&, w] {
      for (int i = 0; i < 2000; ++i) {
        const auto h = t.Insert(std::make_shared<Obj>(Obj{w * 10000 + i}), 4);
        if (h == 0) {
          continue;
        }
        last.store(h, std::memory_order_relaxed);
        auto o = t.Get(h);
        EXPECT_NE(o, nullptr);
        EXPECT_NE(t.Remove(h), nullptr);
        EXPECT_EQ(t.Get(h), nullptr);
      }
    });
  }
  threads.emplace_back([&] {
    while (!stop.load()) {
      if (auto o = t.Get(last.load(std::memory_order_relaxed))) {
        found.fetch_add(o->id >= 0 ? 1 : 0, std::memory_order_relaxed);
      }
      EXPECT_LE(t.Count(), 4u);
    }
  });
  for (int w = 0; w < 4; ++w) {
    threads[static_cast<std::size_t>(w)].join();
  }
  stop.store(true);
  threads.back().join();
  EXPECT_EQ(t.Count(), 0u);
}

} // namespace
