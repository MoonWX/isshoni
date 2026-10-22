// The test tone (IM_AUDIO_MODE_TEST_TONE, docs/m2/04-windows-engine.md §10.3): M1's fake audio signal, played into
// the session's PcmSink in place of 05's mixer, for isshoni-enginetest, the Go integration tests on CI runners without
// audio devices and the A/V checks. It captures nothing and ships in release builds.
//
// The signal is the one internal/media/fake/testdata/gentone.go wrote into M1's asset: both channels equal, periodic
// with a period of one second, a 440 Hz bed at 0.1 (-20 dBFS, 440 whole cycles per second) and, in the first 100 ms of
// each second, a 1 kHz beep at 0.32 (about -10 dBFS, 100 whole cycles) with 1 ms raised-cosine edges. Its phase is
// locked to the session's epoch_ns: a chunk's samples are the period's at (captureNs - epoch_ns), so the beeps start
// at epoch_ns + k * 1 s on the capture clock, where the video test pattern flashes.
//
// TestTone computes chunks; TestToneSource paces them on an exact 10 ms grid against a clock the caller reads. The
// Windows session (04 E5) runs a TestToneSource on its test-tone thread (MMCSS "Pro Audio", a high-resolution
// waitable timer): wait until the deadline Pump returned, read the engine clock, Pump again.
#pragma once

#include <atomic>
#include <cstdint>

#include "im/audio/pcm_sink.h"

namespace im::audio {

inline constexpr double kToneBedHz = 440.0;
inline constexpr double kToneBedLevel = 0.1;
inline constexpr double kToneBeepHz = 1000.0;
inline constexpr double kToneBeepLevel = 0.32;
inline constexpr uint32_t kToneBeepFrames = kRate / 10;   // 100 ms
inline constexpr uint32_t kToneEdgeFrames = kRate / 1000; // 1 ms raised-cosine edges
inline constexpr uint32_t kToneFadeFrames = kChunkFrames; // the pause and mute fade: 10 ms, like 05's mix bus

class TestTone {
public:
  explicit TestTone(int64_t epoch_ns) noexcept : epoch_ns_(epoch_ns) {}

  // Sample n (0 <= n < kRate) of the one-second period, without fade.
  static float PeriodSample(uint32_t n) noexcept;
  // The period's sample index at captureNs: (captureNs - epoch_ns) at 48 kHz, rounded to the nearest sample, mod kRate.
  uint32_t PeriodIndex(int64_t captureNs) const noexcept;

  // Writes the chunk whose first frame is at captureNs: kChunkFrames interleaved stereo frames. While paused or muted
  // the output fades to digital zero in 10 ms (a linear ramp, then exact zeros) and back in when neither is set. Call
  // it from one thread (the test tone's); never blocks, never allocates.
  void Fill(float *frames, int64_t captureNs) noexcept;

  // im_set_paused and the user's mute (im_audio_set_muted): the output is silent while either is set (effective mute
  // = user mute OR pause, as on 05's mix bus); each is set and cleared only by its own call. Any thread.
  void SetPaused(bool on) noexcept { paused_.store(on, std::memory_order_relaxed); }
  void SetMuted(bool on) noexcept { muted_.store(on, std::memory_order_relaxed); }
  bool paused() const noexcept { return paused_.load(std::memory_order_relaxed); }
  bool muted() const noexcept { return muted_.load(std::memory_order_relaxed); }

private:
  const int64_t epoch_ns_;
  std::atomic<bool> paused_{false};
  std::atomic<bool> muted_{false};
  uint32_t gain_steps_ = kToneFadeFrames; // the fade's position: kToneFadeFrames = full level, 0 = silent
};

// Feeds a PcmSink with the test tone on an exact 10 ms grid, as 05's mixer feeds it with the mix.
class TestToneSource {
public:
  // A chunk is handed over once its last frame is in the past: at its captureNs + 10 ms.
  static constexpr int64_t kDueAfterNs = kChunkNs;
  // A source that fell more than this many chunks behind (a stalled thread, a suspended PC) doesn't catch up: it
  // starts a new grid (OnGap) at one of its two newest due chunks, the one a whole number of 20 ms frames after
  // first_chunk_ns, so the frame phase is kept whatever chunk the stall began after.
  static constexpr int64_t kMaxLagChunks = 20;

  // epoch_ns: the session's; first_chunk_ns: the first chunk's captureNs. The session passes epoch_ns +
  // OpusStage::lookahead_ns(), so the Opus packets are stamped epoch_ns + k * 20 ms (the first one exactly at epoch_ns,
  // with IM_PKT_REINIT) and the beeps start on packet boundaries, like M1's fake audio. sink must outlive the source.
  TestToneSource(int64_t epoch_ns, int64_t first_chunk_ns, PcmSink *sink) noexcept
      : tone_(epoch_ns), sink_(sink), first_ns_(first_chunk_ns), next_ns_(first_chunk_ns) {}

  // Hands every chunk that is due at now_ns to the sink (OnGap first, before the very first chunk) and returns the
  // engine clock time at which the next chunk is due: the thread's next timer deadline. Never blocks, never allocates.
  int64_t Pump(int64_t now_ns) noexcept;

  TestTone &tone() noexcept { return tone_; }
  int64_t next_chunk_ns() const noexcept { return next_ns_; }
  uint64_t chunks() const noexcept { return chunks_; }
  uint64_t skips() const noexcept { return skips_; } // new grids after falling behind

private:
  TestTone tone_;
  PcmSink *const sink_;
  const int64_t first_ns_; // the grids' origin: every grid starts an even number of chunks after it
  int64_t next_ns_;
  bool started_ = false;
  uint64_t chunks_ = 0;
  uint64_t skips_ = 0;
  float buf_[kChunkSamples] = {};
};

} // namespace im::audio
