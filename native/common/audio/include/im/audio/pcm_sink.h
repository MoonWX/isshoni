// The boundary between 05's mixer and 04's Opus stage (docs/m2/04-windows-engine.md §10.2, 05 §10.5): 05's mixer
// thread, or 04's test tone standing in for it, hands the mix to a PcmSink in 10 ms chunks of 48 kHz stereo float. The
// Opus stage (opus_stage.h) is the sink of every session with audio; it also runs the PCM tap.
#pragma once

#include <cstddef>
#include <cstdint>

namespace im::audio {

inline constexpr uint32_t kRate = 48000, kChannels = 2, kChunkFrames = 480; // 10 ms

inline constexpr int64_t kChunkNs = 10'000'000;                                  // one chunk on the engine clock
inline constexpr uint32_t kChunkSamples = kChunkFrames * kChannels;              // floats per chunk (960)
inline constexpr uint32_t kChunkBytes = kChunkSamples * uint32_t{sizeof(float)}; // 3840: one IM_PACKET_PCM
inline constexpr int64_t kNsPerSecond = 1'000'000'000;

class PcmSink {
public:
  virtual ~PcmSink() = default;
  // Every 10 ms on the mixer thread: exactly kChunkFrames interleaved stereo float frames in [-1, 1], after the mix
  // bus (what friends hear, muted = zeros); captureNs = engine clock (QPC ns) of the first frame. Within one grid,
  // consecutive chunks are exactly 10 000 000 ns apart. Real-time safe: never blocks, never allocates.
  virtual void OnChunk(const float *frames, int64_t captureNs) noexcept = 0;
  // A new timestamp grid starts (first chunk, mixer resync, silence re-anchor; 05 §10.4): the next OnChunk's
  // captureNs may be any value. Device changes and engine switches keep the grid (05) and don't call it.
  virtual void OnGap(int64_t captureNs) noexcept = 0;
};

} // namespace im::audio
