// The lock-free triple buffer behind the frame slots (docs/m2/04-windows-engine.md §8.2), as indices 0..2: one writer
// (the capture side) and one reader (the video thread) each own one slot, and the third is the latest published one.
//
// - The writer fills its slot, then Publish swaps it with the latest, marked fresh and numbered. If the latest was
//   still unread, the writer gets it back and overwrites it next time: the newest frame wins, the writer never waits.
// - The reader's Take swaps its slot with the latest only when that one is fresh; otherwise it keeps its slot, which
//   holds the last frame (re-encoded for refreshes and static keyframes, 04 §8.5).
//
// The swap is one atomic exchange with acquire-release order, so whatever the writer wrote into its slot (the frame
// metadata; the GPU copy is ordered by the shared immediate context, §8.2) is visible to the reader that takes it.
// Portable C++; Publish and Take never block or allocate.
#pragma once

#include <atomic>
#include <cstdint>

namespace im::gpu {

class TripleIndex {
public:
  static constexpr uint32_t kSlots = 3;

  // The slot the writer fills next, and how many frames it published so far (writer thread only).
  uint32_t write_index() const noexcept { return back_; }
  uint64_t published() const noexcept { return published_; }

  // Makes the writer's slot the latest, numbered seq = previous + 1, and gives the writer another one. Returns seq.
  uint64_t Publish() noexcept {
    const uint64_t seq = ++published_;
    const uint64_t old = latest_.exchange(back_ | kFresh | (seq << kSeqShift), std::memory_order_acq_rel);
    back_ = static_cast<uint32_t>(old & kIndexMask);
    return seq;
  }

  struct Taken {
    bool fresh = false;   // the reader moved to a newer frame
    uint32_t index = 0;   // the reader's slot (its frame is the newest one taken so far)
    uint64_t seq = 0;     // that frame's number (0: nothing taken yet)
    uint64_t skipped = 0; // frames published since the previous Take that the reader never saw
  };

  // Takes the latest frame when there is a newer one (reader thread only).
  Taken Take() noexcept {
    Taken t;
    if ((latest_.load(std::memory_order_acquire) & kFresh) != 0) {
      // Only the reader clears kFresh, so the exchange returns a fresh entry: this one or an even newer one.
      const uint64_t old = latest_.exchange(front_, std::memory_order_acq_rel);
      front_ = static_cast<uint32_t>(old & kIndexMask);
      const uint64_t seq = old >> kSeqShift;
      t.fresh = true;
      t.skipped = seq - seen_ - 1;
      seen_ = seq;
    }
    t.index = front_;
    t.seq = seen_;
    return t;
  }

  // The reader's slot and its frame's number (0: nothing taken yet), without taking (reader thread only).
  uint32_t read_index() const noexcept { return front_; }
  uint64_t read_seq() const noexcept { return seen_; }

private:
  static constexpr uint64_t kIndexMask = 0x3;
  static constexpr uint64_t kFresh = 0x4;
  static constexpr unsigned kSeqShift = 3;

  // The latest slot: bits 0–1 its index, bit 2 fresh (published and not taken), bits 3… its frame number.
  std::atomic<uint64_t> latest_{2};
  uint32_t back_ = 1;      // the writer's
  uint64_t published_ = 0; // the writer's
  uint32_t front_ = 0;     // the reader's
  uint64_t seen_ = 0;      // the reader's
};

} // namespace im::gpu
