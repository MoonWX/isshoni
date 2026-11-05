// The held handles of include targets (docs/m2/05-windows-audio.md §5.7; "05" below): a SYNCHRONIZE |
// PROCESS_QUERY_LIMITED_INFORMATION handle taken before a stream's activation and held while it lives, so its PID
// can't be reused under it, with a one-shot wait that tells the watcher when the process exits. Portable interface;
// process_hold_win.cpp is Windows' (OpenProcess, GetProcessTimes, RegisterWaitForSingleObject), tests use fakes.
#pragma once

#include <cstdint>
#include <memory>

#include "im/audiopolicy/types.h"

namespace im::audio {

// A held process. Destroying it releases the wait and the handle (it waits for a running exit callback to finish).
class ProcessHold {
public:
  virtual ~ProcessHold() = default;
  // The process exited (the wait fired). Any thread.
  virtual bool exited() const noexcept = 0;
};

enum class HoldStatus : uint8_t {
  kHeld,     // *out holds the process
  kNoHandle, // OpenProcess refused (an anti-cheat driver or a protected process stripping rights): activate anyway,
             // I3 is checked by snapshot (05 §5.7, the note no_handle)
  kMismatch, // the pid belongs to another process now (another creation time) or is gone: plan again
};

class ProcessGate {
public:
  virtual ~ProcessGate() = default;
  // Opens key.pid and compares its creation time with key.created (0: unknown, accepted). When a held process exits,
  // the gate wakes the watcher from an OS thread, and the hold's exited() turns true.
  virtual HoldStatus Hold(const audiopolicy::ProcessKey &key, std::unique_ptr<ProcessHold> *out) = 0;
};

// Windows' gate (process_hold_win.cpp). wake(ctx) is called from the OS's wait thread when a held process exits; ctx
// must outlive every hold (the watcher passes its Signals).
std::unique_ptr<ProcessGate> NewWindowsProcessGate(void (*wake)(void *ctx), void *ctx);

} // namespace im::audio
