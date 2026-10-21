// The engine's log ring (docs/m2/04-windows-engine.md §5.6, §6.1): a bounded lock-free MPSC queue of 512 records that
// any thread, real-time ones included, pushes to without blocking or allocating, and that im_read_logs drains as
// newline-delimited JSON:
//
//   {"t":"2026-10-21T12:00:00.000Z","level":"info","component":"encode","msg":"encoder opened",
//    "attrs":{"layer":"f","encoder":"nvenc","w":1920,"h":1080}}
//
// A push into a full ring drops the record; the next record that gets in carries the count as "dropped":N (after
// "msg"). Records above the ring's level are not recorded. Strings longer than the limits below are cut on a UTF-8
// boundary.
//
// Privacy (04 §5.6, README §3 item 18): no record at any level, debug included, may contain a window title, a share
// label or a source name, or name an executable, a process or an app. Callers log ids, sizes, tokens and codes.
#pragma once

#include <atomic>
#include <concepts>
#include <cstddef>
#include <cstdint>
#include <initializer_list>
#include <memory>
#include <mutex>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::log {

inline constexpr uint32_t kCapacity = 512; // records (04 §6.4)
inline constexpr std::size_t kMaxComponentBytes = 23;
inline constexpr std::size_t kMaxMsgBytes = 159;
inline constexpr std::size_t kMaxAttrs = 8; // more are left out
inline constexpr std::size_t kMaxKeyBytes = 23;
inline constexpr std::size_t kMaxStringBytes = 63; // string attribute values
// The longest formatted record, newline included. Drain needs cap > kMaxLineBytes.
inline constexpr int32_t kMaxLineBytes = 8192;

// One attribute of a record: a key and an integer, a float, a bool or a string. It only views its strings; Push
// copies them.
class Attr {
public:
  enum class Kind : uint8_t { kInt, kUint, kDouble, kBool, kString };

  Attr(std::string_view key, std::string_view v) noexcept : key_(key), kind_(Kind::kString), str_(v) {}
  Attr(std::string_view key, const char *v) noexcept : Attr(key, std::string_view(v ? v : "")) {}
  Attr(std::string_view key, bool v) noexcept : key_(key), kind_(Kind::kBool), b_(v) {}
  template <std::signed_integral T>
    requires(!std::same_as<T, bool>)
  Attr(std::string_view key, T v) noexcept : key_(key), kind_(Kind::kInt), i_(static_cast<int64_t>(v)) {}
  template <std::unsigned_integral T>
    requires(!std::same_as<T, bool>)
  Attr(std::string_view key, T v) noexcept : key_(key), kind_(Kind::kUint), u_(static_cast<uint64_t>(v)) {}
  template <std::floating_point T>
  Attr(std::string_view key, T v) noexcept : key_(key), kind_(Kind::kDouble), d_(static_cast<double>(v)) {}

  std::string_view key() const noexcept { return key_; }
  Kind kind() const noexcept { return kind_; }
  int64_t int_value() const noexcept { return i_; }
  uint64_t uint_value() const noexcept { return u_; }
  double double_value() const noexcept { return d_; }
  bool bool_value() const noexcept { return b_; }
  std::string_view string_value() const noexcept { return str_; }

private:
  std::string_view key_;
  Kind kind_;
  union {
    int64_t i_ = 0;
    uint64_t u_;
    double d_;
    bool b_;
  };
  std::string_view str_;
};

class Ring {
public:
  // capacity must be a power of two (tests use small rings; the engine's is kCapacity).
  explicit Ring(uint32_t capacity = kCapacity);
  ~Ring();
  Ring(const Ring &) = delete;
  Ring &operator=(const Ring &) = delete;

  // IM_LOG_* (im_init's log_level); values above IM_LOG_DEBUG mean debug. The default is IM_LOG_INFO.
  void SetLevel(uint32_t level) noexcept;
  uint32_t level() const noexcept { return level_.load(std::memory_order_relaxed); }
  bool Enabled(uint32_t level) const noexcept { return level <= this->level(); }

  // Records a log line stamped with the wall clock now. Returns false when the level is filtered or the ring is full
  // (then the drop is counted). Lock-free, never allocates: safe on real-time threads.
  bool Push(uint32_t level, std::string_view component, std::string_view msg,
            std::initializer_list<Attr> attrs = {}) noexcept;
  // Push with an explicit wall-clock time (Unix ns), for tests.
  bool PushAt(int64_t unix_ns, uint32_t level, std::string_view component, std::string_view msg,
              std::initializer_list<Attr> attrs = {}) noexcept;

  // im_read_logs: writes whole NDJSON lines, oldest first, at most cap - 1 bytes plus a NUL, and returns the bytes
  // written (0 = nothing queued). Records that don't fit stay queued for the next call. A NULL buf or a cap of at most
  // kMaxLineBytes (so that a record could never fit) is IM_ERR_INVALID_ARG. Concurrent drains are serialized.
  int32_t Drain(char *buf, int32_t cap);

  // Drops counted since the last record that got in (they show in the next one).
  uint64_t pending_drops() const noexcept { return dropped_.load(std::memory_order_relaxed); }
  uint32_t capacity() const noexcept { return mask_ + 1; }

private:
  struct Record;
  struct Cell;

  std::unique_ptr<Cell[]> cells_;
  uint32_t mask_;
  std::atomic<uint32_t> level_{IM_LOG_INFO};
  std::atomic<uint64_t> enqueue_pos_{0};
  std::atomic<uint64_t> dropped_{0};
  std::mutex drain_mu_;
  uint64_t dequeue_pos_ = 0; // guarded by drain_mu_
  std::string line_;         // guarded by drain_mu_
};

// The process-wide ring that im_read_logs drains. Never destroyed (threads may log during process exit).
Ring &Default() noexcept;

inline bool Error(std::string_view component, std::string_view msg, std::initializer_list<Attr> attrs = {}) noexcept {
  return Default().Push(IM_LOG_ERROR, component, msg, attrs);
}
inline bool Warn(std::string_view component, std::string_view msg, std::initializer_list<Attr> attrs = {}) noexcept {
  return Default().Push(IM_LOG_WARN, component, msg, attrs);
}
inline bool Info(std::string_view component, std::string_view msg, std::initializer_list<Attr> attrs = {}) noexcept {
  return Default().Push(IM_LOG_INFO, component, msg, attrs);
}
inline bool Debug(std::string_view component, std::string_view msg, std::initializer_list<Attr> attrs = {}) noexcept {
  return Default().Push(IM_LOG_DEBUG, component, msg, attrs);
}

// "2026-10-21T12:00:00.000Z" for a Unix time in ns (UTC, milliseconds; times before 1970 are written as 1970).
std::string FormatTimestamp(int64_t unix_ns);

} // namespace im::log
