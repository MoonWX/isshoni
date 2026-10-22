// The level meters' store (docs/m2/05-windows-audio.md §12.2, §4.1: "lock-free read, < 50 us"): the watcher thread
// publishes an im_audio_levels sample every 100 ms, and im_audio_levels_get copies the latest one from any thread.
// Portable C++ (no OS header), so its tests run anywhere.
#pragma once

#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <span>
#include <string_view>
#include <vector>

#include "isshoni_media.h"

namespace im::audio {

// A sequence lock over the sample's 32-bit words: one writer (the watcher), any number of readers, none of which
// ever blocks the writer or takes a lock. The words are atomics, so a reader racing a write reads no torn value in
// the C++ memory model; it sees the sequence change and copies again.
class LevelStore {
public:
  LevelStore() noexcept;
  LevelStore(const LevelStore &) = delete;
  LevelStore &operator=(const LevelStore &) = delete;

  // The writer: makes v the latest sample (its struct_size is ignored).
  void Publish(const im_audio_levels &v) noexcept;
  // Any thread: the latest sample, with struct_size = sizeof(im_audio_levels). Before the first Publish: an empty
  // sample (report_seq 0, no apps, mix at -120 dBFS).
  im_audio_levels Read() const noexcept;

private:
  static constexpr std::size_t kWords = sizeof(im_audio_levels) / sizeof(uint32_t);
  static_assert(sizeof(im_audio_levels) % sizeof(uint32_t) == 0, "im_audio_levels is whole 32-bit words");
  std::atomic<uint32_t> seq_{0};
  std::array<std::atomic<uint32_t>, kWords> words_;
};

// The level of silence (05 §4.1: "-120 = silence").
inline constexpr float kSilenceDb = -120.0f;

// A linear peak (0..1) as dBFS, floored at kSilenceDb.
float PeakToDb(float peak) noexcept;

// One app's activity for a sample (05 §12.2): the peak of its sessions' meters over the last 100 ms.
struct AppActivity {
  std::string_view app_id; // matches apps[].id of the report
  float peak_db = kSilenceDb;
  int64_t last_active_ns = 0; // engine clock; the sample orders apps by it, most recent first
  bool captured = false;      // source 0 (a running stream while live) or 1 (the session's activity meter)
};

// Builds a sample (05 §12.2): one entry per app id (entries of one id are merged: the loudest peak, the latest
// activity), most recently active first, at most IM_MAX_LEVEL_APPS; rms_db = peak_db for activity entries; the mix at
// mix_peak_db / mix_rms_db (kSilenceDb while not live).
im_audio_levels MakeLevels(uint32_t report_seq, int64_t qpc_ns, float mix_peak_db, float mix_rms_db,
                           std::span<const AppActivity> apps);

} // namespace im::audio
