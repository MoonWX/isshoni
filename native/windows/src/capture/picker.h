// The picker thread (docs/m2/04-windows-engine.md §6.1, §7.2): an STA with a message loop and a hidden owner window,
// created on the first pick and kept until im_shutdown. im_pick_source never shows the picker on its caller's thread:
// a blocked Wails UI thread would freeze the window for as long as the user takes, and an STA UI thread may not wait
// on an IAsyncOperation.
//
//   caller (any thread)            picker thread (STA)
//   Pick ── request ─────────────► GraphicsCapturePicker, IInitializeWithWindow::Initialize(parent or the owner
//     │                              window), PickSingleItemAsync, pumping messages (MsgWaitForMultipleObjectsEx) until
//     │                              it completes, Cancel() or the timeout (both call the operation's Cancel())
//     │                            the item's identity (window_match.h) and the borderless answer (winrt_support.h)
//     ◄──────────────────── done ─ the result (status, detail and the PickedItem)
//
// One pick at a time: a second Pick while one is open is IM_ERR_LIMIT with token pick_busy (04 §5.3). The owner
// window, used when params.parent_window is NULL (tools: isshoni-enginetest pick), is a 1×1 fully transparent
// layered tool window at the centre of the primary monitor, shown (without activation) only while its pick is open,
// so the picker is modal over it and centred on the primary monitor.
#pragma once

#include <windows.h>

#include <cstdint>
#include <memory>
#include <mutex>
#include <string>

#include "capture/picked_item.h"
#include "isshoni_media.h"

namespace im::capture {

inline constexpr uint32_t kDefaultPickTimeoutMs = 120'000; // 04 §6.4
inline constexpr uint32_t kMaxPickTimeoutMs = 600'000;
// How long a cancelled or timed-out pick waits for the operation to report its cancellation.
inline constexpr uint32_t kCancelGraceMs = 2'000;

struct PickResult {
  std::shared_ptr<PickedItem> picked;
  uint32_t kind = IM_SOURCE_WINDOW; // IM_SOURCE_* (§7.2 step 4)
  bool borderless = false;          // BorderlessAllowed() at pick time: no IM_SRCF_OS_BORDER
  bool self = false;                // IM_SRCF_SELF
  uint32_t width = 0;               // item.Size()
  uint32_t height = 0;
  std::string name; // DisplayName() in UTF-8 (shown locally, never logged)
};

class Picker {
public:
  Picker();
  ~Picker();
  Picker(const Picker &) = delete;
  Picker &operator=(const Picker &) = delete;

  // Shows the picker owned by parent (NULL: the owner window) and blocks until the user picks, cancels or timeout_ms
  // passes (0 = kDefaultPickTimeoutMs). Failures set the caller's detail: IM_ERR_CANCELLED (closed, Cancel, timed out),
  // IM_ERR_LIMIT (pick_busy), IM_ERR_INVALID_ARG (parent is not a window), IM_ERR_UNSUPPORTED_OS (no WGC),
  // IM_ERR_PERMISSION_DENIED (capture_denied), IM_ERR_INTERNAL.
  im_status Pick(HWND parent, uint32_t timeout_ms, PickResult *out);

  // Makes an open Pick return IM_ERR_CANCELLED; nothing when none is open.
  void Cancel() noexcept;

  // Cancels an open pick, waits for it (≤ kCancelGraceMs plus the pick's own wind-down) and ends the thread; the next
  // Pick starts a new one.
  void Shutdown() noexcept;

private:
  struct Request;
  struct Thread;

  std::mutex mu_;
  std::shared_ptr<Thread> thread_; // the running picker thread, or null
  bool busy_ = false;              // a Pick is open
};

// The process's picker (never destroyed, like the engine).
Picker &ThePicker() noexcept;

} // namespace im::capture
