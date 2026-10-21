#include "im/os/wake.h"

#include <array>
#include <chrono>
#include <condition_variable>
#include <cstddef>
#include <mutex>

#if defined(_WIN32)
#include <windows.h>
#elif defined(__linux__)
#include <cerrno>
#include <ctime>
#include <linux/futex.h>
#include <sys/syscall.h>
#include <unistd.h>
#elif defined(__APPLE__)
#if __has_include(<os/os_sync_wait_on_address.h>)
#include <cerrno>
#include <os/clock.h>
#include <os/os_sync_wait_on_address.h>
#define IM_HAVE_OS_SYNC 1
#endif
#endif

namespace im::os {

namespace detail {
namespace {

// A waiter checks the word under its bucket's mutex and waits on the bucket's condition variable; a waker changed the
// word before it takes the same mutex, so a wake can't fall between a waiter's check and its wait.
struct Bucket {
  std::mutex mu;
  std::condition_variable cv;
};

constexpr std::size_t kBuckets = 32;

Bucket &BucketFor(const void *addr) noexcept {
  static std::array<Bucket, kBuckets> buckets;
  const auto a = reinterpret_cast<std::uintptr_t>(addr);
  return buckets[(a >> 4) % kBuckets];
}

} // namespace

bool FallbackWaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept {
  Bucket &b = BucketFor(&word);
  const auto deadline = std::chrono::steady_clock::now() + std::chrono::milliseconds(timeout_ms);
  std::unique_lock<std::mutex> lock(b.mu);
  while (word.load(std::memory_order_acquire) == expected) {
    if (b.cv.wait_until(lock, deadline) == std::cv_status::timeout) {
      return word.load(std::memory_order_acquire) != expected;
    }
  }
  return true;
}

void FallbackWakeWordAll(const std::atomic<uint32_t> &word) noexcept {
  Bucket &b = BucketFor(&word);
  // Taking the mutex and releasing it before the notify (no bare block: clang-format 17 and 18 lay one out
  // differently) is what orders this wake after a waiter's check (see Bucket).
  std::unique_lock<std::mutex> lock(b.mu);
  lock.unlock();
  b.cv.notify_all();
}

} // namespace detail

#if defined(_WIN32)

bool WaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept {
  // WaitOnAddress compares the 4 bytes at the address with expected and sleeps while they are equal.
  auto *addr = const_cast<std::atomic<uint32_t> *>(&word);
  if (WaitOnAddress(addr, &expected, sizeof(expected), timeout_ms)) {
    return true;
  }
  return GetLastError() != ERROR_TIMEOUT;
}

void WakeWordOne(const std::atomic<uint32_t> &word) noexcept {
  WakeByAddressSingle(const_cast<std::atomic<uint32_t> *>(&word));
}

void WakeWordAll(const std::atomic<uint32_t> &word) noexcept {
  WakeByAddressAll(const_cast<std::atomic<uint32_t> *>(&word));
}

#elif defined(__linux__)

namespace {
long Futex(const std::atomic<uint32_t> &word, int op, uint32_t val, const timespec *ts) noexcept {
  // std::atomic<uint32_t> is a lock-free 32-bit object with the layout of uint32_t, which is what futex needs.
  return syscall(SYS_futex, reinterpret_cast<const uint32_t *>(&word), op, val, ts, nullptr, 0);
}
} // namespace

bool WaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept {
  timespec ts{};
  ts.tv_sec = static_cast<time_t>(timeout_ms / 1000);
  ts.tv_nsec = static_cast<long>(timeout_ms % 1000) * 1000000L;
  if (Futex(word, FUTEX_WAIT_PRIVATE, expected, &ts) == 0) {
    return true;
  }
  return errno != ETIMEDOUT;
}

void WakeWordOne(const std::atomic<uint32_t> &word) noexcept { Futex(word, FUTEX_WAKE_PRIVATE, 1, nullptr); }

void WakeWordAll(const std::atomic<uint32_t> &word) noexcept { Futex(word, FUTEX_WAKE_PRIVATE, 0x7FFFFFFF, nullptr); }

#elif defined(IM_HAVE_OS_SYNC)

bool WaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept {
  if (__builtin_available(macOS 14.4, *)) {
    auto *addr = const_cast<std::atomic<uint32_t> *>(&word);
    if (timeout_ms == 0) {
      return word.load(std::memory_order_acquire) != expected;
    }
    const uint64_t ns = static_cast<uint64_t>(timeout_ms) * 1000000u;
    if (os_sync_wait_on_address_with_timeout(addr, expected, sizeof(uint32_t), OS_SYNC_WAIT_ON_ADDRESS_NONE,
                                             OS_CLOCK_MACH_ABSOLUTE_TIME, ns) >= 0) {
      return true;
    }
    return errno != ETIMEDOUT;
  }
  return detail::FallbackWaitWord(word, expected, timeout_ms);
}

void WakeWordOne(const std::atomic<uint32_t> &word) noexcept {
  if (__builtin_available(macOS 14.4, *)) {
    os_sync_wake_by_address_any(const_cast<std::atomic<uint32_t> *>(&word), sizeof(uint32_t),
                                OS_SYNC_WAKE_BY_ADDRESS_NONE);
    return;
  }
  detail::FallbackWakeWordAll(word);
}

void WakeWordAll(const std::atomic<uint32_t> &word) noexcept {
  if (__builtin_available(macOS 14.4, *)) {
    os_sync_wake_by_address_all(const_cast<std::atomic<uint32_t> *>(&word), sizeof(uint32_t),
                                OS_SYNC_WAKE_BY_ADDRESS_NONE);
    return;
  }
  detail::FallbackWakeWordAll(word);
}

#else

bool WaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept {
  return detail::FallbackWaitWord(word, expected, timeout_ms);
}

void WakeWordOne(const std::atomic<uint32_t> &word) noexcept { detail::FallbackWakeWordAll(word); }

void WakeWordAll(const std::atomic<uint32_t> &word) noexcept { detail::FallbackWakeWordAll(word); }

#endif

} // namespace im::os
