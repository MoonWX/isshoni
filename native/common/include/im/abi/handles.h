// The session handle table (docs/m2/04-windows-engine.md §2, §6.3): a handle is index << 32 | generation into a fixed
// table of 16 slots. Every insert into a slot takes the slot's next generation (1..0xFFFFFFFF, wrapping past 0), and
// free slots are used round-robin, so a stale handle (a stopped session's) or a foreign one (any other value) never
// reaches a live object: Get returns nullptr and the export answers IM_ERR_INVALID_ARG. 0 is never a valid handle.
//
// Thread-safe: every call may come from any thread. The table holds shared_ptrs, so an object found by Get stays alive
// while the caller uses it, even if another thread removes it meanwhile (im_stop racing a control call).
#pragma once

#include <array>
#include <cstdint>
#include <memory>
#include <mutex>
#include <utility>
#include <vector>

namespace im::abi {

using Handle = uint64_t; // im_session

template <class T, uint32_t N = 16> class HandleTable {
  static_assert(N >= 1);

public:
  static constexpr uint32_t kSlots = N;

  // first_generation is the generation every slot hands out first (tests start near the wrap-around); 0 means 1.
  explicit HandleTable(uint32_t first_generation = 1) noexcept {
    const uint32_t first = first_generation == 0 ? 1 : first_generation;
    for (auto &s : slots_) {
      s.generation = first - 1; // Insert advances it first; first - 1 may be 0, which NextGeneration skips
    }
  }

  HandleTable(const HandleTable &) = delete;
  HandleTable &operator=(const HandleTable &) = delete;

  static constexpr Handle MakeHandle(uint32_t index, uint32_t generation) noexcept {
    return (static_cast<Handle>(index) << 32) | generation;
  }
  static constexpr uint32_t IndexOf(Handle h) noexcept { return static_cast<uint32_t>(h >> 32); }
  static constexpr uint32_t GenerationOf(Handle h) noexcept { return static_cast<uint32_t>(h & 0xFFFFFFFFu); }

  // Stores obj and returns its handle, or 0 when obj is null or `limit` objects (at most N) are already stored. The
  // limit lets the engine cap live sessions (4, 04 §6.4) atomically with the insert.
  Handle Insert(std::shared_ptr<T> obj, uint32_t limit = N) {
    if (!obj) {
      return 0;
    }
    std::lock_guard<std::mutex> lock(mu_);
    if (count_ >= limit || count_ >= N) {
      return 0;
    }
    for (uint32_t k = 0; k < N; ++k) {
      const uint32_t i = (next_ + k) % N;
      Slot &s = slots_[i];
      if (s.obj) {
        continue;
      }
      s.generation = NextGeneration(s.generation);
      s.obj = std::move(obj);
      ++count_;
      next_ = (i + 1) % N;
      return MakeHandle(i, s.generation);
    }
    return 0; // unreachable: count_ < N means a slot is free
  }

  // The object behind h, or nullptr for 0, a stale or a foreign handle.
  std::shared_ptr<T> Get(Handle h) const {
    std::lock_guard<std::mutex> lock(mu_);
    const Slot *s = Find(*this, h);
    return s ? s->obj : nullptr;
  }

  // Removes the object behind h and returns it (the caller stops it outside the table's lock), or nullptr when h is
  // not live. After it returns, h is stale.
  std::shared_ptr<T> Remove(Handle h) {
    std::lock_guard<std::mutex> lock(mu_);
    Slot *s = Find(*this, h);
    if (!s) {
      return nullptr;
    }
    --count_;
    return std::exchange(s->obj, nullptr);
  }

  // Removes every object (im_shutdown) and returns them with their handles.
  std::vector<std::pair<Handle, std::shared_ptr<T>>> RemoveAll() {
    std::vector<std::pair<Handle, std::shared_ptr<T>>> out;
    std::lock_guard<std::mutex> lock(mu_);
    for (uint32_t i = 0; i < N; ++i) {
      if (slots_[i].obj) {
        out.emplace_back(MakeHandle(i, slots_[i].generation), std::exchange(slots_[i].obj, nullptr));
      }
    }
    count_ = 0;
    return out;
  }

  // Every live object with its handle, at one instant.
  std::vector<std::pair<Handle, std::shared_ptr<T>>> Snapshot() const {
    std::vector<std::pair<Handle, std::shared_ptr<T>>> out;
    std::lock_guard<std::mutex> lock(mu_);
    for (uint32_t i = 0; i < N; ++i) {
      if (slots_[i].obj) {
        out.emplace_back(MakeHandle(i, slots_[i].generation), slots_[i].obj);
      }
    }
    return out;
  }

  uint32_t Count() const {
    std::lock_guard<std::mutex> lock(mu_);
    return count_;
  }

  // 1..0xFFFFFFFF, wrapping past 0 (a generation of 0 would let handle 0 be valid).
  static constexpr uint32_t NextGeneration(uint32_t g) noexcept { return g == 0xFFFFFFFFu ? 1 : g + 1; }

private:
  struct Slot {
    std::shared_ptr<T> obj;
    uint32_t generation = 0;
  };

  // The live slot behind h (const or not, like self), or nullptr. Called with mu_ held.
  template <class Self> static auto Find(Self &self, Handle h) noexcept -> decltype(&self.slots_[0]) {
    const uint32_t i = IndexOf(h);
    const uint32_t g = GenerationOf(h);
    if (h == 0 || g == 0 || i >= N) {
      return nullptr;
    }
    auto &s = self.slots_[i];
    return (s.obj && s.generation == g) ? &s : nullptr;
  }

  mutable std::mutex mu_;
  std::array<Slot, N> slots_{};
  uint32_t count_ = 0;
  uint32_t next_ = 0;
};

} // namespace im::abi
