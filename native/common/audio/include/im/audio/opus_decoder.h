// An Opus decoder over the DLL's libopus: 48 kHz stereo float, one packet per call, keeping its state between calls
// as a receiver does. For the stage's tests and as the building block of 05's test export im_test_opus_decode
// (docs/m2/05-windows-audio.md §4.1, whose semantics are 05's); the shipped engine decodes nothing else.
#pragma once

#include <cstdint>
#include <memory>

#include "isshoni_media.h"

struct OpusDecoder; // libopus

namespace im::audio {

// The most frames per channel one Opus packet decodes to: 120 ms at 48 kHz.
inline constexpr int32_t kMaxDecodeFrames = 5760;

class OpusPacketDecoder {
public:
  // IM_ERR_INVALID_ARG for a NULL out, IM_ERR_INTERNAL when libopus fails. Allocates.
  static im_status Create(std::unique_ptr<OpusPacketDecoder> *out) noexcept;
  ~OpusPacketDecoder();
  OpusPacketDecoder(const OpusPacketDecoder &) = delete;
  OpusPacketDecoder &operator=(const OpusPacketDecoder &) = delete;

  // Decodes pkt (len bytes) into pcm, interleaved stereo float with room for max_frames frames per channel, and
  // returns the frames per channel written. Negative: IM_ERR_INVALID_ARG for a NULL or empty pkt, a NULL pcm, a
  // max_frames too small for the packet, or a malformed packet; IM_ERR_INTERNAL for any other libopus failure.
  int32_t Decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames) noexcept;
  // Forgets the state (OPUS_RESET_STATE): the next packet decodes as a stream's first.
  void Reset() noexcept;

private:
  explicit OpusPacketDecoder(OpusDecoder *dec) noexcept : dec_(dec) {}
  OpusDecoder *dec_;
};

} // namespace im::audio
