// The os/ shim's wake-up on a 32-bit word (docs/m2/04-windows-engine.md §6.2): producers bump a sequence word and
// wake it, the consumer waits on it with a timeout. No lock on the hot path. Windows maps it to WaitOnAddress and
// WakeByAddressSingle/All, Linux to futex, macOS to os_sync_wait_on_address (macOS 14.4 and later); other systems,
// and older macOS at run time, use a portable fallback (a mutex and a condition variable per address bucket).
//
// These are the only functions of native/common that touch an OS header (src/os/wake.cpp).
#pragma once

#include <atomic>
#include <cstdint>

namespace im::os {

// Blocks while word == expected, for at most timeout_ms (0 = don't block). Returns false when the timeout passed and
// true otherwise: woken, or word != expected already. It may also return true without a change (a spurious wake), so
// callers loop on their own condition and deadline.
bool WaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept;

// Wakes one or every thread blocked in WaitWord on word. Call it after changing word.
void WakeWordOne(const std::atomic<uint32_t> &word) noexcept;
void WakeWordAll(const std::atomic<uint32_t> &word) noexcept;

namespace detail {
// The portable fallback, exposed so the tests run it on every OS.
bool FallbackWaitWord(const std::atomic<uint32_t> &word, uint32_t expected, uint32_t timeout_ms) noexcept;
void FallbackWakeWordAll(const std::atomic<uint32_t> &word) noexcept;
} // namespace detail

} // namespace im::os
