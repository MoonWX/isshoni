// The audio mixer core (docs/m2/05-windows-audio.md §9, §10.3, §10.4, §17; README WA36): the portable part of 05's
// mixer, target im_audiomix. It holds the capture rings with their QPC side rings, the per-stream fill controller and
// resampler, the channel states with every fade, the mix bus with its silence flags and levels, the chunk timestamps
// and their grid, and the writer of the report JSON (05 §12.1). Windows' capture streams, the waitable timer, MMCSS and
// the watcher are native/windows/src/audio/'s (README WA35, WA45); they feed and drive what this library declares.
//
// This header holds the constants every part shares and the sink the mixer feeds.
#pragma once

#include <cstdint>

namespace im::audiomix {

// The mix format: 48 kHz float stereo, interleaved, in 10 ms chunks (05 §9.1). The same values as 04's
// im::audio::kRate, kChannels and kChunkFrames (native/common/audio/include/im/audio/pcm_sink.h).
inline constexpr uint32_t kRate = 48000;
inline constexpr uint32_t kChannels = 2;
inline constexpr uint32_t kChunkFrames = 480;                       // 10 ms
inline constexpr uint32_t kChunkSamples = kChunkFrames * kChannels; // floats per chunk
inline constexpr int64_t kChunkNs = 10'000'000;
inline constexpr int64_t kNsPerMs = 1'000'000;
inline constexpr int64_t kNsPerSecond = 1'000'000'000;
inline constexpr uint32_t kFramesPerMs = kRate / 1000; // 48

// Digital silence: |x| below this on both channels (S1's 1e-6, -120 dBFS). The onset rule, the silent bypass and the
// re-anchor's "the mix has been digital silence" all use it.
inline constexpr float kSilenceThreshold = 1e-6f;
// The floor of every level in dBFS (05 §9.6, im_audio_level: -120 = silence).
inline constexpr float kLevelFloorDb = -120.0f;

// Whether a stereo frame is digital silence.
constexpr bool IsSilentFrame(float l, float r) noexcept {
  return (l < kSilenceThreshold && l > -kSilenceThreshold) && (r < kSilenceThreshold && r > -kSilenceThreshold);
}

// The sink the mixer feeds every 10 ms, on the mixer thread. It has exactly the methods and rules of 04's
// im::audio::PcmSink (pcm_sink.h): this library doesn't link 04's im_audio, so native/windows/src/audio/ (README WA45)
// hands the session's Opus stage over through a two-line adapter that forwards both calls.
class ChunkSink {
public:
  virtual ~ChunkSink() = default;
  // kChunkFrames interleaved stereo float frames in [-1, 1] after the mix bus (zeros while silenced); capture_ns =
  // the engine clock (QPC ns) of the first frame. Within one grid, consecutive chunks are exactly kChunkNs apart.
  // Real-time safe: never blocks, never allocates.
  virtual void OnChunk(const float *frames, int64_t capture_ns) noexcept = 0;
  // A new timestamp grid starts with the next chunk, whose capture_ns this is (05 §10.4): the first chunk after
  // MixEngine::Begin, a resync and a silence re-anchor. Never for device changes, engine switches or sound off/on.
  virtual void OnGap(int64_t capture_ns) noexcept = 0;
};

} // namespace im::audiomix
