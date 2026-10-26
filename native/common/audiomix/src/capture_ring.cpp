#include "im/audiomix/capture_ring.h"

#include <cstring>

namespace im::audiomix {

namespace {

// Relaxed atomic access to one frame word. GCC and clang get the builtins (one instruction, also in a debug build);
// MSVC gets std::atomic_ref.
#if defined(__GNUC__) || defined(__clang__)
inline void StoreWord(uint64_t *p, uint64_t v) noexcept { __atomic_store_n(p, v, __ATOMIC_RELAXED); }
inline uint64_t LoadWord(const uint64_t *p) noexcept { return __atomic_load_n(p, __ATOMIC_RELAXED); }
#else
inline void StoreWord(uint64_t *p, uint64_t v) noexcept {
  std::atomic_ref<uint64_t>(*p).store(v, std::memory_order_relaxed);
}
inline uint64_t LoadWord(const uint64_t *p) noexcept {
  return std::atomic_ref<uint64_t>(*const_cast<uint64_t *>(p)).load(std::memory_order_relaxed);
}
#endif

constexpr uint32_t kMaxCapacity = 1u << 24;
constexpr uint64_t kSideMask = CaptureRing::kSideEntries - 1;

uint64_t Pack(float l, float r) noexcept {
  uint32_t a = 0, b = 0;
  std::memcpy(&a, &l, sizeof a);
  std::memcpy(&b, &r, sizeof b);
  return uint64_t{a} | (uint64_t{b} << 32);
}

void Unpack(uint64_t v, float *out) noexcept {
  const auto a = static_cast<uint32_t>(v), b = static_cast<uint32_t>(v >> 32);
  std::memcpy(out, &a, sizeof a);
  std::memcpy(out + 1, &b, sizeof b);
}

} // namespace

CaptureRing::CaptureRing(uint32_t capacity_frames)
    : cap_(capacity_frames == 0 ? 1 : (capacity_frames > kMaxCapacity ? kMaxCapacity : capacity_frames)),
      storage_(new uint64_t[cap_]()), frames_(storage_.get()), side_first_(new std::atomic<uint64_t>[kSideEntries]()),
      side_qpc_(new std::atomic<int64_t>[kSideEntries]()) {}

void CaptureRing::Write(const float *frames, uint32_t n, int64_t qpc_ns, int64_t arrival_ns) noexcept {
  if (n == 0) {
    return;
  }
  // The packet's timing: its lag behind the mix and its length, in sums that decay with every packet's length (an
  // exponential average over kTimingAverageNs of audio; while less has come, the weights' sum makes it a plain mean).
  const double length = static_cast<double>(n) * static_cast<double>(kNsPerSecond) / kRate;
  double lag = static_cast<double>(arrival_ns) - static_cast<double>(qpc_ns) - length;
  lag = lag < 0 ? 0 : (lag > static_cast<double>(kMaxArrivalLagNs) ? static_cast<double>(kMaxArrivalLagNs) : lag);
  const double decay = static_cast<double>(kTimingAverageNs) / (static_cast<double>(kTimingAverageNs) + length);
  timing_weight_ = timing_weight_ * decay + length;
  lag_sum_ = lag_sum_ * decay + length * lag;
  packet_sum_ = packet_sum_ * decay + length * length;
  lag_pub_.store(static_cast<int64_t>(lag_sum_ / timing_weight_ + 0.5), std::memory_order_relaxed);
  packet_pub_.store(static_cast<int64_t>(packet_sum_ / timing_weight_ + 0.5), std::memory_order_relaxed);

  uint64_t first = w_.load(std::memory_order_relaxed);
  if (n > cap_) { // keep the packet's last cap_ frames
    const uint32_t drop = n - cap_;
    if (frames != nullptr) {
      frames += 2 * static_cast<std::size_t>(drop);
    }
    qpc_ns += static_cast<int64_t>(drop) * kNsPerSecond / kRate;
    first += drop;
    n = cap_;
  }
  const uint64_t end = first + n;

  // Make room: the oldest frames go. Moving r_ before overwriting their slots lets a consumer that read them notice
  // (its Consume finds r_ moved).
  uint64_t r = r_.load(std::memory_order_acquire);
  while (end - r > cap_) {
    const uint64_t to = end - cap_;
    if (r_.compare_exchange_weak(r, to, std::memory_order_acq_rel, std::memory_order_acquire)) {
      overflows_.fetch_add(1, std::memory_order_relaxed);
      overflow_frames_.fetch_add(to - r, std::memory_order_relaxed);
      break;
    }
  }

  auto pos = static_cast<uint32_t>(first % cap_);
  for (uint32_t i = 0; i < n; ++i) {
    StoreWord(frames_ + pos, frames != nullptr ? Pack(frames[2 * i], frames[2 * i + 1]) : 0);
    if (++pos == cap_) {
      pos = 0;
    }
  }

  // The packet's side entry; a full side ring drops its oldest entry the same way.
  const uint64_t sw = sw_.load(std::memory_order_relaxed);
  uint64_t sr = sr_.load(std::memory_order_acquire);
  while (sw + 1 - sr > kSideEntries) {
    if (sr_.compare_exchange_weak(sr, sw + 1 - kSideEntries, std::memory_order_acq_rel, std::memory_order_acquire)) {
      break;
    }
  }
  side_first_[sw & kSideMask].store(first, std::memory_order_relaxed);
  side_qpc_[sw & kSideMask].store(qpc_ns, std::memory_order_relaxed);
  sw_.store(sw + 1, std::memory_order_release);
  w_.store(end, std::memory_order_release);
}

uint32_t CaptureRing::Buffered() const noexcept {
  const uint64_t r = r_.load(std::memory_order_acquire);
  const uint64_t w = w_.load(std::memory_order_acquire); // after r: w >= r
  const uint64_t d = w - r;
  return d > cap_ ? cap_ : static_cast<uint32_t>(d);
}

uint32_t CaptureRing::Peek(float *dst, uint32_t n) noexcept {
  uint64_t r = r_.load(std::memory_order_acquire);
  const uint64_t w = w_.load(std::memory_order_acquire);
  uint64_t avail = w - r;
  if (avail > cap_) { // the producer moved past the r we loaded
    r = w - cap_;
    avail = cap_;
  }
  const uint32_t k = n < avail ? n : static_cast<uint32_t>(avail);
  auto pos = static_cast<uint32_t>(r % cap_);
  for (uint32_t i = 0; i < k; ++i) {
    Unpack(LoadWord(frames_ + pos), dst + 2 * static_cast<std::size_t>(i));
    if (++pos == cap_) {
      pos = 0;
    }
  }
  peek_r_ = r;
  peeked_ = true;
  return k;
}

bool CaptureRing::Consume(uint32_t n) noexcept {
  const uint64_t base = peeked_ ? peek_r_ : r_.load(std::memory_order_acquire);
  peeked_ = false;
  const uint64_t w = w_.load(std::memory_order_acquire);
  uint64_t target = base + n;
  if (target > w) {
    target = w;
  }
  // A read-modify-write either comes before the producer's overflow (whose overwrites then come after the frames
  // were read) or finds r_ moved.
  uint64_t r = base;
  bool clean = true;
  while (!r_.compare_exchange_weak(r, target, std::memory_order_acq_rel, std::memory_order_acquire)) {
    if (r != base) {
      clean = false;
    }
    if (r >= target) {
      break;
    }
  }
  return clean;
}

void CaptureRing::DiscardAll() noexcept {
  peeked_ = false;
  uint64_t r = r_.load(std::memory_order_acquire);
  const uint64_t w = w_.load(std::memory_order_acquire);
  while (r < w && !r_.compare_exchange_weak(r, w, std::memory_order_acq_rel, std::memory_order_acquire)) {
  }
}

int64_t CaptureRing::QpcOf(const Side &s, uint64_t i) noexcept {
  if (i >= s.first) {
    return s.qpc + static_cast<int64_t>((i - s.first) * static_cast<uint64_t>(kNsPerSecond) / kRate);
  }
  return s.qpc - static_cast<int64_t>((s.first - i) * static_cast<uint64_t>(kNsPerSecond) / kRate);
}

bool CaptureRing::SideFor(uint64_t r, Side *out) noexcept {
  for (int attempt = 0; attempt < 16; ++attempt) {
    const uint64_t sw = sw_.load(std::memory_order_acquire);
    const uint64_t sr = sr_.load(std::memory_order_acquire);
    if (sr >= sw) {
      return false; // never written
    }
    uint64_t idx = sr;
    while (idx + 1 < sw && side_first_[(idx + 1) & kSideMask].load(std::memory_order_relaxed) <= r) {
      ++idx;
    }
    const Side s{side_first_[idx & kSideMask].load(std::memory_order_relaxed),
                 side_qpc_[idx & kSideMask].load(std::memory_order_relaxed)};
    // Drop the entries before idx (their frames are consumed). The read-modify-write also proves that the producer
    // didn't overwrite entry idx while it was read: it either comes first (the overwrite comes after the read) or
    // finds sr_ past idx (read again).
    uint64_t cur = sr;
    bool valid = false;
    while (cur <= idx) {
      if (sr_.compare_exchange_weak(cur, idx, std::memory_order_acq_rel, std::memory_order_acquire)) {
        valid = true;
        break;
      }
    }
    if (valid) {
      *out = s;
      return true;
    }
  }
  return false;
}

bool CaptureRing::HeadQpc(int64_t *qpc_ns) noexcept {
  const uint64_t r = r_.load(std::memory_order_acquire);
  Side s{};
  if (!SideFor(r, &s)) {
    return false;
  }
  *qpc_ns = QpcOf(s, r);
  return true;
}

uint32_t CaptureRing::FramesBefore(int64_t qpc_ns) noexcept {
  const uint64_t r = r_.load(std::memory_order_acquire);
  const uint64_t w = w_.load(std::memory_order_acquire);
  const uint64_t buffered = (w - r) > cap_ ? cap_ : (w - r);
  if (buffered == 0) {
    return 0;
  }
  Side best{};
  if (!SideFor(r, &best) || QpcOf(best, r) >= qpc_ns) {
    return 0;
  }
  // Later packets that started at or before qpc_ns refine it (a gap in the capture shows in their times).
  const uint64_t sw = sw_.load(std::memory_order_acquire);
  for (uint64_t idx = sr_.load(std::memory_order_acquire) + 1; idx < sw; ++idx) {
    const Side s{side_first_[idx & kSideMask].load(std::memory_order_relaxed),
                 side_qpc_[idx & kSideMask].load(std::memory_order_relaxed)};
    if (s.first >= w || s.qpc > qpc_ns) {
      break;
    }
    if (s.first > best.first) {
      best = s;
    }
  }
  // The first frame at or after qpc_ns: ceil((qpc_ns - qpc) * rate / 1e9) frames after the packet's first (before it
  // when the head is older than the oldest entry the side ring still has).
  constexpr int64_t kFar = 100 * kNsPerSecond; // far beyond any ring: no overflow below
  const int64_t dt = qpc_ns - best.qpc;
  if (dt >= kFar) {
    return static_cast<uint32_t>(buffered);
  }
  if (dt <= -kFar) {
    return 0;
  }
  const int64_t num = dt * static_cast<int64_t>(kRate);
  const int64_t off = num >= 0 ? (num + kNsPerSecond - 1) / kNsPerSecond : -((-num) / kNsPerSecond);
  const int64_t i = static_cast<int64_t>(best.first) + off;
  if (i <= static_cast<int64_t>(r)) {
    return 0;
  }
  const auto ahead = static_cast<uint64_t>(i) - r;
  return static_cast<uint32_t>(ahead > buffered ? buffered : ahead);
}

} // namespace im::audiomix
