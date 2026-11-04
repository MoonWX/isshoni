// The session watcher (docs/m2/05-windows-audio.md §7; "05" below): one thread in the COM multithreaded apartment
// that owns all WASAPI enumeration, the session and device notifications, the process table and the planner, and
// publishes "What friends hear" (§12.1) and the level meters (§12.2). It is a small event loop: requests from the
// audio exports, the callbacks' signals, and its ticks (100 ms levels, 1 s poll, 05 §7.3).
//
// It is the watcher half of 05's AudioEngine (README WA35) and, while a share is live, its capture (README WA45): the
// include set's streams (include_set.h) with their activator threads (activator.h), the capture thread
// (capture_thread.h) and the held process handles (process_hold.h), feeding the share's mixer engine (live_mix.h).
// While no share runs, or while its sound is off, the plan is the include set's prediction with engine "none" (05
// §6.5). README WA53 adds the other engines: the fast path (one EXCLUDE stream while no voice app runs, left at once
// on a kept-out process's session event, without the debounce and with the retroactive cut, 05 §6.4) and the endpoint
// engine (Windows 10 builds, a failed startup probe, process loopback failing twice in 10 s: engine_gate.h), the
// startup probe at each audio start (startup_probe.h), the endpoint stream following default-device changes (05 §7.5)
// and the silent-stream check (silent_check.h, 05 §8.5). The fast path follows the rules' IM_EXCLF_FAST_PATH, which Go
// sets from the settings key audio.fastPath (the kill switch).
//
// Every method may be called from any thread, concurrently. Calls that need the thread's COM work post a request and
// wait at most kWatcherWait, then fail with IM_ERR_TIMEOUT (05 §4.3); none calls CoInitializeEx on the caller's
// thread.
#pragma once

#include <atomic>
#include <chrono>
#include <cstdint>
#include <memory>
#include <thread>

#include "audio/live_mix.h"
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
  // im_audio_unwatch: stops watching (idempotent). Mic roots and lineage are kept, keyed by process key (05 §5.6,
  // §5.7: for the process's lifetime, whether or not the sheet stays open); every reconcile prunes the exited ones.
  im_status Unwatch();
  bool watching() const noexcept;

  // im_list_audio_apps (05 §12.1), snprintf rule: the latest report while watching; otherwise a one-shot snapshot
  // (state "stopped") taken on the thread with the remembered mic roots and lineage. The thread keeps a one-shot
  // document for a second, so the snprintf rule's retry with a larger buffer gets the same document, and drops it when
  // rules, watch or unwatch change what a report says.
  int32_t ListAppsJson(char *buf, int32_t cap);
  // im_audio_levels_get (05 §12.2): lock-free; IM_ERR_BAD_STATE (token not_watching) while not watching. out passed
  // CheckOut; it is written up to its struct_size.
  im_status LevelsGet(im_audio_levels *out);
  // im_caps.audio_flags (05 §11.1): computed on the thread the first time and after every device change, else cached.
  // 0 when the thread doesn't answer in time.
  uint32_t ProbeFlags();

  // A share's mixer started (Mixer::Start, 05 §10.5): from now on the include set's channels join mix's engine, the
  // watcher watches until DetachLive (whatever im_audio_unwatch says meanwhile), and the capture threads run. Returns
  // at once; the plan and the first activations follow on the thread. IM_ERR_BAD_STATE when the thread doesn't run.
  im_status AttachLive(std::shared_ptr<LiveMix> mix);
  // The share's mixer thread has stopped (Mixer::Stop): every channel and stream of mix goes. Waits at most wait for
  // the thread (IM_ERR_TIMEOUT then; the request keeps mix alive until the thread lets go of it).
  im_status DetachLive(const std::shared_ptr<LiveMix> &mix, std::chrono::milliseconds wait);
  // im_audio_set_mode and im_audio_set_muted while live: the report's mode, muted and events, and the plan (sound off
  // closes the streams, sound on plans as at start, 05 §6.5). The engine's silence flags are the caller's to set.
  // Return at once.
  void SetLiveMode(uint32_t mode);
  void SetLiveMuted(bool muted);

  struct State; // shared between the caller side and the thread

private:
  std::shared_ptr<State> st_;
  std::thread thread_;               // used by Start and Stop only (the engine's Init and Shutdown)
  std::atomic<bool> started_{false}; // what the other methods check: set once the thread runs
};

} // namespace im::audio
