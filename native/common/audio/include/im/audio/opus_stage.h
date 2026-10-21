// The Opus stage and the PCM tap (docs/m2/04-windows-engine.md §10.3, 05 §10.1-§10.4): the PcmSink of every session
// with audio. It runs inline on the mixer thread (or the test tone's), pairs the 10 ms chunks into 20 ms frames,
// encodes each with libopus and writes one IM_PACKET_AUDIO per frame into the session's Opus ring; with
// IM_AUDIOF_TAP_PCM it also writes each chunk as one IM_PACKET_PCM into the PCM ring.
//
// Timestamps. The encoder delays its output by its lookahead La (OPUS_GET_LOOKAHEAD, 312 samples = 6.5 ms for this
// application), so a packet's capture_ts_ns is its first chunk's captureNs - La * 1e9 / 48000: the time of the first
// sample a receiver decodes from it. Consecutive packets of one grid are therefore exactly 20 000 000 ns apart and Go's
// RTP timestamps advance by exactly 960 (05 §10.3). PCM packets carry their chunk's captureNs (no lookahead).
//
// Flags. The first packet of each stream and its first packet after a new grid (OnGap, or a chunk that is not 10 ms
// after the previous one, which 05 never sends) carry IM_PKT_REINIT; a new grid drops a half-filled frame. A packet
// the ring refused (PacketWriter::Write false) or a frame libopus failed to encode sets IM_PKT_DISCONTINUITY on the
// stream's next packet, and the grid is unchanged. A flag stays pending until a packet carrying it got into the ring.
//
// Start at epoch_ns. 05 stamps chunks about 50 ms in the past, so the first chunks after im_start are older than the
// session's epoch_ns: the stage drops chunks while the packet they would start would be stamped before epoch_ns, and
// the tap drops chunks stamped before it. Every packet of either kind is thus stamped at or after epoch_ns, and the
// first one carries IM_PKT_REINIT. Packet times never decrease: a chunk that would start a frame less than 20 ms
// after the previous packet (or, for the tap, a chunk less than 10 ms after the previous one) is dropped too.
//
// Settings (05 §10.1, exactly): opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO), OPUS_SET_BITRATE (0 ->
// 128 000), VBR 1 with VBR_CONSTRAINT 1 (constrained VBR), COMPLEXITY 10, SIGNAL_MUSIC, MAX_BANDWIDTH FULLBAND,
// INBAND_FEC 0, PACKET_LOSS_PERC 0, DTX 0 (digital silence keeps flowing at 50 packets/s), LSB_DEPTH 24.
//
// Threads. OnChunk and OnGap run on one thread at a time (the mixer's). SetBitrate and stats() may be called from any
// thread at any time; ReadSettings only while no OnChunk runs. Real-time safe: after Create nothing allocates, locks
// or blocks; libopus encodes into the stage's own buffers.
#pragma once

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>

#include "im/audio/opus_packet.h"
#include "im/audio/packet_writer.h"
#include "im/audio/pcm_sink.h"
#include "isshoni_media.h"

struct OpusEncoder; // libopus

namespace im::audio {

inline constexpr uint32_t kDefaultBitrate = 128000; // im_audio_config.bitrate_bps 0
inline constexpr uint32_t kMinBitrate = 32000;      // im_audio_config.bitrate_bps, im_set_audio_bitrate
inline constexpr uint32_t kMaxBitrate = 510000;
inline constexpr uint32_t kFrameMs = 20;                              // im_audio_config.frame_ms (0 = 20; M2 only 20)
inline constexpr uint32_t kFrameChunks = 2;                           // chunks per Opus frame
inline constexpr uint32_t kFrameFrames = kChunkFrames * kFrameChunks; // 960 samples per channel
inline constexpr int64_t kFrameNs = kChunkNs * kFrameChunks;          // 20 000 000
inline constexpr uint32_t kOpusDurationUs = 20000;                    // im_packet.duration_us of IM_PACKET_AUDIO
inline constexpr uint32_t kPcmDurationUs = 10000;                     // and of IM_PACKET_PCM
inline constexpr int32_t kMaxEncodeBytes = 1275;                      // opus_encode_float's buffer (05 §10.1)
// The largest IM_PACKET_AUDIO: an encoded packet plus the Marker framing's count byte, padding length and Marker.
inline constexpr std::size_t kMaxPacketBytes = kMaxEncodeBytes + 2 + kMarkerSize;

struct OpusStageConfig {
  int64_t epoch_ns = 0;          // im_session_info.epoch_ns: no packet is stamped before it
  uint32_t bitrate_bps = 0;      // im_audio_config.bitrate_bps: 0 = 128 000, else 32 000..510 000
  uint32_t frame_ms = 0;         // im_audio_config.frame_ms: 0 or 20
  uint32_t flags = 0;            // im_audio_config.flags: IM_AUDIOF_TAP_PCM adds the PCM tap; other bits are ignored
  PacketWriter *audio = nullptr; // the Opus ring (required); must outlive the stage
  PacketWriter *pcm = nullptr;   // the PCM ring (required with IM_AUDIOF_TAP_PCM, else unused); must outlive the stage
  // The test tone under IM_TEST_HOOKS with IM_ENC_FAKE (the session decides): every Opus packet is re-framed with
  // M1's Marker in its code 3 padding (opus_packet.h). Off for real audio.
  bool marker = false;
};

// The encoder's settings as libopus reports them (OPUS_GET_*), for tests and the debug log.
struct OpusSettings {
  int32_t application = 0;      // OPUS_APPLICATION_AUDIO
  int32_t sample_rate = 0;      // 48000
  int32_t bitrate = 0;          // the target in bit/s
  int32_t vbr = 0;              // 1
  int32_t vbr_constraint = 0;   // 1
  int32_t complexity = 0;       // 10
  int32_t signal = 0;           // OPUS_SIGNAL_MUSIC
  int32_t max_bandwidth = 0;    // OPUS_BANDWIDTH_FULLBAND
  int32_t inband_fec = 0;       // 0
  int32_t packet_loss_perc = 0; // 0
  int32_t dtx = 0;              // 0
  int32_t lsb_depth = 0;        // 24
  int32_t lookahead = 0;        // 312 samples
};

// Counters for im_get_stats ("audio":{"packets","drops","bitrateBps"}) and the supervisor's RING_OVERFLOW.
struct OpusStageStats {
  uint64_t packets = 0;       // Opus packets the audio ring took
  uint64_t drops = 0;         // Opus packets the audio ring refused (full)
  uint64_t pcm_packets = 0;   // PCM packets the PCM ring took
  uint64_t pcm_drops = 0;     // PCM packets the PCM ring refused
  uint64_t early_chunks = 0;  // chunks the Opus side dropped: before epoch_ns, or going back in time
  uint64_t gaps = 0;          // new grids: OnGap calls and chunks off the 10 ms grid
  uint64_t encode_errors = 0; // frames libopus failed to encode (dropped, DISCONTINUITY on the next packet)
  uint32_t bitrate_bps = 0;   // the current target
};

class OpusStage final : public PcmSink {
public:
  // Creates the stage and its encoder with 05 §10.1's settings. IM_ERR_INVALID_ARG (with im_last_error's detail) for
  // a bitrate or frame length out of range, a missing writer or a NULL out; IM_ERR_INTERNAL when libopus fails.
  // Allocates: call it before the mixer starts (inside im_start).
  static im_status Create(const OpusStageConfig &cfg, std::unique_ptr<OpusStage> *out) noexcept;
  ~OpusStage() override;
  OpusStage(const OpusStage &) = delete;
  OpusStage &operator=(const OpusStage &) = delete;

  void OnChunk(const float *frames, int64_t captureNs) noexcept override;
  void OnGap(int64_t captureNs) noexcept override;

  // im_set_audio_bitrate: 32 000..510 000 (else IM_ERR_INVALID_ARG and nothing changes). The stage applies it with
  // OPUS_SET_BITRATE before its next frame: live, no reset, no REINIT. Any thread.
  im_status SetBitrate(uint32_t bps) noexcept;

  // The encoder's lookahead La in samples (312) and as ns on the engine clock (La * 1e9 / 48000 = 6 500 000), which
  // packet times subtract. The test tone starts its chunks at epoch_ns + lookahead_ns(), so its packets are stamped
  // epoch_ns + k * 20 ms, like M1's fake audio.
  int32_t lookahead_samples() const noexcept { return lookahead_samples_; }
  int64_t lookahead_ns() const noexcept { return lookahead_ns_; }
  bool tap_pcm() const noexcept { return pcm_ != nullptr; }

  // Reads the encoder's settings back with OPUS_GET_*. Not while OnChunk runs (tests, diagnostics).
  im_status ReadSettings(OpusSettings *out) const noexcept;

  OpusStageStats stats() const noexcept;

private:
  OpusStage(const OpusStageConfig &cfg, OpusEncoder *enc, int32_t lookahead) noexcept;

  void NewGrid() noexcept;
  void Tap(const float *frames, int64_t ts) noexcept;
  void EncodeFrame(int64_t ts) noexcept;

  OpusEncoder *enc_;
  const int64_t epoch_ns_;
  const int32_t lookahead_samples_;
  const int64_t lookahead_ns_;
  PacketWriter *const audio_;
  PacketWriter *const pcm_; // nullptr without the tap
  const bool marker_;

  // Mixer-thread state.
  bool grid_open_ = false;              // a chunk of the current grid was seen
  int64_t last_chunk_ns_ = 0;           // its captureNs
  bool half_ = false;                   // frame_ holds the first chunk of a frame
  int64_t half_ns_ = 0;                 // that chunk's captureNs
  bool packet_seen_ = false;            // an Opus packet was produced
  int64_t last_packet_ns_ = 0;          // its capture_ts_ns
  bool pcm_seen_ = false;               // a PCM packet was produced
  int64_t last_pcm_ns_ = 0;             // its capture_ts_ns
  uint32_t opus_flags_ = IM_PKT_REINIT; // pending for the next Opus packet
  uint32_t pcm_flags_ = IM_PKT_REINIT;  // pending for the next PCM packet
  uint32_t applied_bitrate_;
  uint32_t packet_index_ = 0; // Marker.frame of the next packet
  int64_t next_beep_ns_;      // the next beep start (epoch_ns + k * 1 s) for the Marker
  bool logged_encode_error_ = false;
  float frame_[kFrameFrames * kChannels] = {};
  uint8_t encoded_[kMaxEncodeBytes] = {};
  uint8_t framed_[kMaxPacketBytes] = {};

  // Shared with other threads.
  std::atomic<uint32_t> target_bitrate_;
  std::atomic<uint64_t> packets_{0}, drops_{0}, pcm_packets_{0}, pcm_drops_{0}, early_chunks_{0}, gaps_{0},
      encode_errors_{0};
};

} // namespace im::audio
