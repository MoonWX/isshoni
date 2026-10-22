// Where the Opus stage and the PCM tap put their packets (docs/m2/04-windows-engine.md §6.2, §10.3): one writer per
// stream, the session's Opus ring (128 slots, 256 KiB) and, with IM_AUDIOF_TAP_PCM, its PCM ring (128 slots,
// 512 KiB). The session (04 E5) adapts its rings to this interface; the tests use recording fakes.
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::audio {

// One packet as it enters its stream's ring: the fields of im_packet that the ring stores. Audio and PCM packets are
// always layer IM_NO_LAYER, so the layer is not carried.
struct AudioPacket {
  uint32_t kind = 0;             // IM_PACKET_AUDIO (one 20 ms Opus packet) or IM_PACKET_PCM (one 10 ms chunk)
  uint32_t flags = 0;            // IM_PKT_REINIT (a new timestamp grid), IM_PKT_DISCONTINUITY (packets were dropped)
  int64_t capture_ts_ns = 0;     // engine clock, >= the session's epoch_ns
  uint32_t duration_us = 0;      // 20000 (Opus) or 10000 (PCM)
  const uint8_t *data = nullptr; // valid during Write only: the ring copies it
  uint32_t size = 0;             // Opus: at most kMaxPacketBytes (opus_stage.h); PCM: kChunkBytes (3840)
};

class PacketWriter {
public:
  virtual ~PacketWriter() = default;
  // Copies p into the stream's ring and returns true, or returns false when the ring is full and dropped it (04
  // §6.2: audio drops the newest packet). The writer counts the drop and raises the flag the supervisor turns into
  // RING_OVERFLOW; the Opus stage then sets IM_PKT_DISCONTINUITY on the stream's next packet and keeps the grid.
  // Packets the ring drops later, at dequeue (older than 1 s), are the ring's to flag. Called on the mixer or
  // test-tone thread: never blocks, never allocates.
  virtual bool Write(const AudioPacket &p) noexcept = 0;
};

} // namespace im::audio
