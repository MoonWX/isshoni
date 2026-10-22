// A capture stream's ring (docs/m2/05-windows-audio.md §8.3): float stereo frames from the capture thread to the
// mixer, with a side ring of {first frame index, QPC time} per captured packet, so the mixer knows when the audio
// engine mixed the frame at the ring's head (§10.3) and the fast path can cut the ring at a QPC time (§6.4).
//
// One producer thread (the capture thread) and one consumer thread (the mixer), lock-free. On overflow the producer
// drops the oldest frames: it moves the read index forward itself, so a consumer whose Peek raced with that learns it
// from Consume's result. Each frame (left and right float) is one 64-bit word read and written atomically (relaxed),
// so a raced read is stale, never undefined behaviour. Construction allocates; nothing else does.
#pragma once

#include <atomic>
#include <cstdint>
#include <memory>

#include "im/audiomix/constants.h"

namespace im::audiomix {

class CaptureRing {
public:
  static constexpr uint32_t kDefaultFrames = kRate / 2; // 500 ms (§8.3, §17: 192 KiB of float stereo)
  static constexpr uint32_t kSideEntries = 1024;        // packets the side ring remembers (power of two)

  // capacity_frames: 1 .. 2^24 (clamped). Not on a real-time thread: it allocates.
  explicit CaptureRing(uint32_t capacity_frames = kDefaultFrames);
  CaptureRing(const CaptureRing &) = delete;
  CaptureRing &operator=(const CaptureRing &) = delete;

  uint32_t capacity() const noexcept { return cap_; }

  // ---- producer (the capture thread)

  // Appends one captured packet: n interleaved stereo frames whose first frame the audio engine mixed at qpc_ns
  // (pu64QPCPosition in ns, or the read time minus the frames already buffered when the position is missing, §8.3).
  // frames == nullptr appends zeros (AUDCLNT_BUFFERFLAGS_SILENT). A packet longer than the ring keeps its last
  // capacity() frames. When the ring is full the oldest frames are dropped (overflows, overflow_frames).
  void Write(const float *frames, uint32_t n, int64_t qpc_ns) noexcept;

  // ---- consumer (the mixer)

  // Frames available at the head.
  uint32_t Buffered() const noexcept;
  // Copies up to n frames from the head into dst (2 floats each) without consuming them; returns how many.
  uint32_t Peek(float *dst, uint32_t n) noexcept;
  // Drops n frames from the head (fewer when fewer are buffered). After a Peek it counts from the frame that Peek
  // started at. Returns false when the producer dropped frames from under that Peek (an overflow raced with it, so
  // the peeked frames may be stale); the head is then past both.
  bool Consume(uint32_t n) noexcept;
  // Drops everything buffered.
  void DiscardAll() noexcept;
  // The QPC time at which the audio engine mixed the head frame (the next frame Peek returns, or the next one written
  // when nothing is buffered). False before the first Write.
  bool HeadQpc(int64_t *qpc_ns) noexcept;
  // How many buffered frames lie before the first frame captured at or after qpc_ns: 0 when the head is already
  // that late, Buffered() when no buffered frame is.
  uint32_t FramesBefore(int64_t qpc_ns) noexcept;

  // ---- counters (any thread)
  uint64_t overflows() const noexcept { return overflows_.load(std::memory_order_relaxed); }
  uint64_t overflow_frames() const noexcept { return overflow_frames_.load(std::memory_order_relaxed); }
  uint64_t written_frames() const noexcept { return w_.load(std::memory_order_relaxed); }

private:
  struct Side {
    uint64_t first; // frame index of the packet's first frame
    int64_t qpc;    // its QPC time
  };
  // The newest side entry that starts at or before frame index r (or the oldest one), with sr_ advanced past the
  // entries before it. False when there is none.
  bool SideFor(uint64_t r, Side *out) noexcept;
  // The QPC time of frame index i, given the side entry it belongs to.
  static int64_t QpcOf(const Side &s, uint64_t i) noexcept;

  const uint32_t cap_;
  std::unique_ptr<uint64_t[]> storage_; // cap_ packed frames, accessed only through atomic loads and stores
  uint64_t *frames_;
  std::atomic<uint64_t> w_{0}; // frames ever written (producer)
  std::atomic<uint64_t> r_{0}; // frames ever consumed or dropped (consumer; producer on overflow)
  std::unique_ptr<std::atomic<uint64_t>[]> side_first_;
  std::unique_ptr<std::atomic<int64_t>[]> side_qpc_;
  std::atomic<uint64_t> sw_{0}; // side entries ever written (producer)
  std::atomic<uint64_t> sr_{0}; // the oldest live side entry (consumer; producer on overflow)
  std::atomic<uint64_t> overflows_{0};
  std::atomic<uint64_t> overflow_frames_{0};
  uint64_t peek_r_ = 0; // consumer only: where the last Peek started
  bool peeked_ = false;
};

} // namespace im::audiomix
