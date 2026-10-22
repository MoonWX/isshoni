#include "audio/levels.h"

#include <algorithm>
#include <cmath>
#include <cstring>
#include <string>
#include <thread>

#include "im/abi/utf8.h"

namespace im::audio {

LevelStore::LevelStore() noexcept {
  im_audio_levels empty{};
  empty.mix_peak_db = kSilenceDb;
  empty.mix_rms_db = kSilenceDb;
  uint32_t w[kWords] = {};
  std::memcpy(w, &empty, sizeof empty);
  for (std::size_t i = 0; i < kWords; ++i) {
    words_[i].store(w[i], std::memory_order_relaxed);
  }
}

void LevelStore::Publish(const im_audio_levels &v) noexcept {
  uint32_t w[kWords] = {};
  std::memcpy(w, &v, sizeof v);
  const uint32_t s = seq_.load(std::memory_order_relaxed);
  seq_.store(s + 1, std::memory_order_relaxed); // odd: a write is in progress
  std::atomic_thread_fence(std::memory_order_release);
  for (std::size_t i = 0; i < kWords; ++i) {
    words_[i].store(w[i], std::memory_order_relaxed);
  }
  seq_.store(s + 2, std::memory_order_release);
}

im_audio_levels LevelStore::Read() const noexcept {
  uint32_t w[kWords] = {};
  for (unsigned attempt = 0;; ++attempt) {
    const uint32_t before = seq_.load(std::memory_order_acquire);
    if ((before & 1u) == 0) {
      for (std::size_t i = 0; i < kWords; ++i) {
        w[i] = words_[i].load(std::memory_order_relaxed);
      }
      std::atomic_thread_fence(std::memory_order_acquire);
      if (seq_.load(std::memory_order_relaxed) == before) {
        break;
      }
    }
    if (attempt > 64) {
      std::this_thread::yield(); // a writer was preempted mid-sample: let it finish
    }
  }
  im_audio_levels out;
  std::memcpy(&out, w, sizeof out);
  out.struct_size = sizeof(im_audio_levels);
  return out;
}

float PeakToDb(float peak) noexcept {
  if (!(peak > 0.0f)) { // also NaN
    return kSilenceDb;
  }
  const float db = 20.0f * std::log10(peak);
  return std::max(db, kSilenceDb);
}

im_audio_levels MakeLevels(uint32_t report_seq, int64_t qpc_ns, float mix_peak_db, float mix_rms_db,
                           std::span<const AppActivity> apps) {
  im_audio_levels out{};
  out.struct_size = sizeof(im_audio_levels);
  out.report_seq = report_seq;
  out.qpc_ns = qpc_ns;
  out.mix_peak_db = mix_peak_db;
  out.mix_rms_db = mix_rms_db;

  struct Merged {
    std::string_view id;
    float peak_db;
    int64_t last;
    bool captured;
  };
  std::vector<Merged> merged;
  merged.reserve(apps.size());
  for (const AppActivity &a : apps) {
    if (a.app_id.empty()) {
      continue;
    }
    auto it = std::find_if(merged.begin(), merged.end(), [&](const Merged &m) { return m.id == a.app_id; });
    if (it == merged.end()) {
      merged.push_back(Merged{a.app_id, a.peak_db, a.last_active_ns, a.captured});
    } else {
      it->peak_db = std::max(it->peak_db, a.peak_db);
      it->last = std::max(it->last, a.last_active_ns);
      it->captured = it->captured || a.captured;
    }
  }
  std::stable_sort(merged.begin(), merged.end(), [](const Merged &a, const Merged &b) { return a.last > b.last; });
  const std::size_t n = std::min<std::size_t>(merged.size(), IM_MAX_LEVEL_APPS);
  for (std::size_t i = 0; i < n; ++i) {
    im_audio_level &l = out.apps[i];
    abi::CopyFixed(l.app_id, sizeof l.app_id, merged[i].id);
    l.peak_db = std::max(merged[i].peak_db, kSilenceDb);
    l.rms_db = l.peak_db; // activity: = peak_db (05 §4.1); captured RMS comes with the mixer (README WA45)
    l.source = merged[i].captured ? 0u : 1u;
  }
  out.count = static_cast<uint32_t>(n);
  return out;
}

} // namespace im::audio
