#include "audio/silent_check.h"

#include <unordered_set>

#include "im/audiomix/constants.h"

namespace im::audio {

SilentActions SilentStreamCheck::Sample(int64_t now_ns, std::span<const SilentSample> samples) {
  SilentActions out;
  std::unordered_set<std::string> seen;
  for (const SilentSample &s : samples) {
    if (s.app_id.empty() || !seen.insert(s.app_id).second) {
      continue;
    }
    App &a = apps_[s.app_id];
    if (!s.has_streams) {
      a.silent_since = -1; // nothing to check (a reopen in progress keeps its count; one given up on may be tried
      a.given_up = false;  // again after a device or session event, and is given up on again if still silent)
      continue;
    }
    if (!s.streams_silent) {
      // Its streams deliver sound: the failures in a row start over, but not the 5 s between reopens (a flapping app
      // isn't reopened more often).
      const int64_t last = a.last_reopen;
      a = App{};
      a.last_reopen = last;
      continue;
    }
    if (s.meter_peak_db <= kSilentCheckMeterDb) {
      a.silent_since = -1; // silent streams of a silent app: nothing wrong
      continue;
    }
    if (a.silent_since < 0) {
      a.silent_since = now_ns;
    }
    if (now_ns - a.silent_since < kSilentCheckSpanNs) {
      continue;
    }
    if (a.reopens >= kSilentReopensBeforeGivingUp) {
      if (!a.given_up) {
        a.given_up = true;
        out.give_up.push_back(s.app_id);
      }
      continue;
    }
    if (now_ns - a.last_reopen < kSilentReopenEveryNs) {
      continue;
    }
    ++a.reopens;
    a.last_reopen = now_ns;
    a.silent_since = -1;
    out.reopen.push_back(s.app_id);
  }
  for (auto it = apps_.begin(); it != apps_.end();) {
    it = seen.count(it->first) == 0 ? apps_.erase(it) : std::next(it);
  }
  return out;
}

ExclusiveSilentAction ExclusiveSilentCheck::Sample(int64_t now_ns, float heard_meter_db,
                                                   const audiomix::LevelsSnapshot &levels, uint64_t tag) {
  SilentSample s{"exclude", heard_meter_db, false, true};
  for (uint32_t i = 0; tag != 0 && i < levels.count && i < audiomix::kMaxMixChannels; ++i) {
    const audiomix::ChannelLevel &ch = levels.channels[i];
    if (ch.tag == tag && ch.running) {
      s.has_streams = true;
      s.streams_silent = ch.peak_db <= audiomix::kLevelFloorDb;
    }
  }
  const SilentActions a = check_.Sample(now_ns, std::span<const SilentSample>(&s, 1));
  return ExclusiveSilentAction{!a.reopen.empty(), !a.give_up.empty()};
}

} // namespace im::audio
