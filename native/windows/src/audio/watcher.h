// The session watcher (docs/m2/05-windows-audio.md §7; "05" below): one thread in the COM multithreaded apartment
// that owns all WASAPI enumeration, the session and device notifications, the process table and the planner, and
// publishes "What friends hear" (§12.1) and the level meters (§12.2). It is a small event loop: requests from the
// audio exports, the callbacks' signals, and its ticks (100 ms levels, 1 s poll, 05 §7.3).
//
// This is the watcher half of 05's AudioEngine (README WA35): it captures nothing. While no share runs, the plan is
// the include set's prediction with engine "none" (05 §6.5). README WA45 adds the capture streams and the mixer.
//
// Every method may be called from any thread, concurrently. Calls that need the thread's COM work post a request and
// wait at most kWatcherWait, then fail with IM_ERR_TIMEOUT (05 §4.3); none calls CoInitializeEx on the caller's
// thread.
#pragma once

#include <chrono>
#include <cstdint>
#include <memory>
#include <thread>

#include "im/audiopolicy/exclusions.h"
#include "isshoni_media.h"

namespace im::audio {

// How long an ABI call waits for the watcher thread before IM_ERR_TIMEOUT (05 §4.3).
inline constexpr std::chrono::milliseconds kWatcherWait{3000};
// How long Stop waits for the thread before it abandons it with a log record, never killing it (05 §10.5).
inline constexpr std::chrono::milliseconds kWatcherJoin{2000};

class Watcher {
public:
  Watcher();
  ~Watcher(); // Stop
  Watcher(const Watcher &) = delete;
  Watcher &operator=(const Watcher &) = delete;

  // Starts the thread (COM MTA): it creates the device enumerator and registers the device notifications, then
  // waits for work. IM_ERR_INTERNAL when the thread can't start.
  im_status Start();
  // Stops the thread: unregisters every notification and releases every COM object on it. Waits at most
  // kWatcherJoin; a thread stuck in a driver call is abandoned (its state is shared, so nothing dangles).
  void Stop() noexcept;

  // im_set_exclusions (05 §4.3): the rules (already validated and copied, the implicit instance rule included) for
  // the watcher, applied on its thread within 50 ms; returns at once.
  void SetRules(audiopolicy::Exclusions ex);
  // im_audio_watch: replaces the rules and starts watching (idempotent). Returns once the first report is out.
  im_status Watch(audiopolicy::Exclusions ex);
  // im_audio_unwatch: stops watching (idempotent), forgets mic and lineage state (audiopolicy::Planner::Reset).
  im_status Unwatch();
  bool watching() const noexcept;

  // im_list_audio_apps (05 §12.1), snprintf rule: the latest report while watching; otherwise a one-shot snapshot
  // (state "stopped") taken on the thread. A one-shot document is kept for a second, so the snprintf rule's retry with
  // a larger buffer gets the same document.
  int32_t ListAppsJson(char *buf, int32_t cap);
  // im_audio_levels_get (05 §12.2): lock-free; IM_ERR_BAD_STATE (token not_watching) while not watching. out passed
  // CheckOut; it is written up to its struct_size.
  im_status LevelsGet(im_audio_levels *out);
  // im_caps.audio_flags (05 §11.1): computed on the thread the first time and after every device change, else cached.
  // 0 when the thread doesn't answer in time.
  uint32_t ProbeFlags();

  struct State; // shared between the caller side and the thread

private:
  std::shared_ptr<State> st_;
  std::thread thread_;
};

} // namespace im::audio
