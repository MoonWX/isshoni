// The silent-stream check (docs/m2/05-windows-audio.md §8.5; "05" below), the Windows counterpart of macOS's
// zero-buffer rule: a heard app whose activity meter peak exceeds -60 dBFS for 1 s while all its include streams
// deliver digital silence for that second gets its streams reopened, at most once per 5 s per app (silentReopens, event
// silent_reopen). Two failed reopens in a row make it capture_failed (the banner capture-trouble while it plays).
//
// Portable C++ (no OS header), owned by the watcher thread, fed every 100 ms while a share is live with sound (the
// bus's silence flags make every channel silent, so the watcher doesn't feed it while muted, paused or off). Tests
// drive it on simulated time (native/windows/tests/audio_engine_control_test.cpp).
#pragma once

#include <cstdint>
#include <span>
#include <string>
#include <unordered_map>
#include <vector>

#include "im/audiomix/mix_engine.h"

namespace im::audio {

inline constexpr float kSilentCheckMeterDb = -60.0f;           // the session meter's peak above this: it plays
inline constexpr int64_t kSilentCheckSpanNs = 1'000'000'000;   // for 1 s
inline constexpr int64_t kSilentReopenEveryNs = 5'000'000'000; // at most once per 5 s per app
inline constexpr uint32_t kSilentReopensBeforeGivingUp = 2;    // two failed reopens in a row: capture_failed

// One app at one sample.
struct SilentSample {
  std::string app_id;
  float meter_peak_db = -120.0f; // the loudest of its render sessions' activity meters (05 §7.2)
  bool has_streams = false;      // it has running include streams
  bool streams_silent = false;   // every one of them delivered digital silence over the last sample
};

struct SilentActions {
  std::vector<std::string> reopen;  // reopen these apps' streams now (silent_reopen)
  std::vector<std::string> give_up; // these apps are capture_failed from now on
};

class SilentStreamCheck {
public:
  // One sample (every 100 ms). Apps missing from samples are forgotten.
  SilentActions Sample(int64_t now_ns, std::span<const SilentSample> samples);
  // A share ends: everything is forgotten.
  void Reset() noexcept { apps_.clear(); }

private:
  struct App {
    int64_t silent_since = -1; // the condition holds since then (-1: it doesn't)
    int64_t last_reopen = INT64_MIN / 2;
    uint32_t reopens = 0; // reopens in a row that didn't bring sound back
    bool given_up = false;
  };
  std::unordered_map<std::string, App> apps_;
};

// The same check on the fast path's EXCLUDE stream (05 §7.5: after a default render change "include and EXCLUDE
// streams stay, and the silent-stream check of §8.5 watches them"; it runs whenever the fast path does): the EXCLUDE
// stream carries every heard app, so a heard app's meter above -60 dBFS for 1 s while its channel delivers digital
// silence reopens it, at most once per 5 s. After two reopens that didn't bring sound back it is left alone (the
// engine has no capture_failed; give_up says so once) until it delivers sound or goes.
struct ExclusiveSilentAction {
  bool reopen = false;
  bool give_up = false;
};

class ExclusiveSilentCheck {
public:
  // One sample (every 100 ms): heard_meter_db, the loudest activity meter of the heard apps' render sessions;
  // levels, the mixer's last levels; tag, the EXCLUDE stream's channel (0: none joined).
  ExclusiveSilentAction Sample(int64_t now_ns, float heard_meter_db, const audiomix::LevelsSnapshot &levels,
                               uint64_t tag);
  void Reset() noexcept { check_.Reset(); }

private:
  SilentStreamCheck check_;
};

} // namespace im::audio
