// The OS calls of the engine core (docs/m2/04-windows-engine.md §6.1, §12.3, §13.3), behind one header so the rest of
// src/engine/ and src/api/ is plain C++20. Implemented for Windows in os_windows.cpp.
#pragma once

#include <cstdint>
#include <limits>
#include <memory>
#include <string>
#include <string_view>

namespace im::engine::os {

// The engine clock: QueryPerformanceCounter in ns (04 §12.3), the clock of every capture_ts_ns and of Go's qpcNS().
int64_t QpcNs() noexcept;

// A thread's wake-up with a deadline on the engine clock (04 §8.4's timer, §10.3's tone thread). On Windows the
// thread waits on an auto-reset event and a high-resolution waitable timer (CREATE_WAITABLE_TIMER_HIGH_RESOLUTION),
// so a deadline is met within about half a millisecond rather than at the next 15.6 ms system tick. One thread waits;
// any thread may wake it, real-time threads included (Wake never blocks or allocates).
class Waiter {
public:
  static constexpr int64_t kNoDeadline = std::numeric_limits<int64_t>::max();

  Waiter();
  ~Waiter();
  Waiter(const Waiter &) = delete;
  Waiter &operator=(const Waiter &) = delete;

  // Wakes the waiting thread, or makes its next wait return at once.
  void Wake() noexcept;
  // Waits until Wake is called or the engine clock reaches deadline_ns (kNoDeadline: no timeout). Returns true when
  // woken. A deadline in the past returns at once (still consuming a pending Wake).
  bool WaitUntil(int64_t deadline_ns) noexcept;

private:
  struct State;
  std::unique_ptr<State> state_;
};

// What a session thread runs as (04 §6.1): the video thread is MMCSS "Capture" at THREAD_PRIORITY_ABOVE_NORMAL, the
// test tone MMCSS "Pro Audio", the supervisor a normal thread. MMCSS comes from avrt.dll, loaded from System32 at run
// time; without it the thread just keeps its priority.
enum class ThreadClass : uint8_t { kNormal, kCapture, kProAudio };

// Called first on a session thread: names it (ASCII, for debuggers and crash dumps; never a source or app name) and
// joins its scheduling class. Returns a token for LeaveThread (0 when nothing needs undoing).
uintptr_t EnterThread(const char *name, ThreadClass cls) noexcept;
// Called last on that thread with EnterThread's token.
void LeaveThread(uintptr_t token) noexcept;

// The wall clock in Unix ns (GetSystemTimePreciseAsFileTime).
int64_t UnixNs() noexcept;

// Clears ONEVPL_SEARCH_PATH and ONEVPL_PRIORITY_PATH from the process environment (04 §13.3 layer 4): libvpl's
// dispatcher inside avcodec reads them, and FFmpeg's static CRT copies the environment when the engine first loads
// it, after im_init.
void ClearLibvplSearchPaths() noexcept;

// Called first and last on the engine worker's thread (04 §6.1): COM in the multithreaded apartment and the thread's
// name. EnterWorkerThread returns 0 (S_OK) or the failing HRESULT; LeaveWorkerThread is called only after a success.
int32_t EnterWorkerThread() noexcept;
void LeaveWorkerThread() noexcept;

// The folder of isshoni-media.dll (or of the test executable the engine is linked into), UTF-8, without a trailing
// separator; "" when Windows can't tell.
std::string ModuleDirectory();

// "windows" (im_build_info's "os").
std::string_view OsName() noexcept;

} // namespace im::engine::os
