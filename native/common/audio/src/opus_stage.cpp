// The Opus stage and the PCM tap (opus_stage.h; docs/m2/04-windows-engine.md §10.3, 05 §10.1-§10.4).
#include "im/audio/opus_stage.h"

#include <opus.h>

#include <cstring>
#include <new>
#include <string_view>

#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

constexpr std::string_view kComponent = "audio";

bool ValidBitrate(uint32_t bps) noexcept { return bps >= kMinBitrate && bps <= kMaxBitrate; }

// 05 §10.1's settings, after opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO). Returns the first libopus error.
int ApplySettings(OpusEncoder *enc, uint32_t bitrate) noexcept {
  const int results[] = {
      opus_encoder_ctl(enc, OPUS_SET_BITRATE(static_cast<opus_int32>(bitrate))),
      opus_encoder_ctl(enc, OPUS_SET_VBR(1)),
      opus_encoder_ctl(enc, OPUS_SET_VBR_CONSTRAINT(1)),
      opus_encoder_ctl(enc, OPUS_SET_COMPLEXITY(10)),
      opus_encoder_ctl(enc, OPUS_SET_SIGNAL(OPUS_SIGNAL_MUSIC)),
      opus_encoder_ctl(enc, OPUS_SET_MAX_BANDWIDTH(OPUS_BANDWIDTH_FULLBAND)),
      // FEC stays off: these bitrates stay in CELT, where in-band FEC does nothing (plan "Audio").
      opus_encoder_ctl(enc, OPUS_SET_INBAND_FEC(0)),
      opus_encoder_ctl(enc, OPUS_SET_PACKET_LOSS_PERC(0)),
      // DTX stays off: continuous 20 ms frames keep the capture-time grid and the receivers' jitter buffers steady.
      opus_encoder_ctl(enc, OPUS_SET_DTX(0)),
      opus_encoder_ctl(enc, OPUS_SET_LSB_DEPTH(24)),
  };
  for (const int r : results) {
    if (r != OPUS_OK) {
      return r;
    }
  }
  return OPUS_OK;
}

} // namespace

im_status OpusStage::Create(const OpusStageConfig &cfg, std::unique_ptr<OpusStage> *out) noexcept {
  if (out == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus stage: out is NULL");
  }
  out->reset();
  const uint32_t bitrate = cfg.bitrate_bps == 0 ? kDefaultBitrate : cfg.bitrate_bps;
  if (!ValidBitrate(bitrate)) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "audio.bitrate_bps %u is outside %u..%u (0 = %u)", cfg.bitrate_bps,
                      kMinBitrate, kMaxBitrate, kDefaultBitrate);
  }
  if (cfg.frame_ms != 0 && cfg.frame_ms != kFrameMs) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "audio.frame_ms %u: only %u (or 0) is supported", cfg.frame_ms, kFrameMs);
  }
  if (cfg.audio == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus stage: no audio ring");
  }
  const bool tap = (cfg.flags & IM_AUDIOF_TAP_PCM) != 0;
  if (tap && cfg.pcm == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus stage: IM_AUDIOF_TAP_PCM without a PCM ring");
  }

  int err = OPUS_OK;
  OpusEncoder *enc =
      opus_encoder_create(static_cast<opus_int32>(kRate), static_cast<int>(kChannels), OPUS_APPLICATION_AUDIO, &err);
  if (enc == nullptr || err != OPUS_OK) {
    if (enc != nullptr) {
      opus_encoder_destroy(enc);
    }
    return abi::Failf(IM_ERR_INTERNAL, {}, "opus_encoder_create: %s", opus_strerror(err));
  }
  if ((err = ApplySettings(enc, bitrate)) != OPUS_OK) {
    opus_encoder_destroy(enc);
    return abi::Failf(IM_ERR_INTERNAL, {}, "opus_encoder_ctl: %s", opus_strerror(err));
  }
  // The lookahead is read once (312 samples for this application); it never changes with the bitrate.
  opus_int32 lookahead = 0;
  if ((err = opus_encoder_ctl(enc, OPUS_GET_LOOKAHEAD(&lookahead))) != OPUS_OK || lookahead < 0 ||
      lookahead >= static_cast<opus_int32>(kFrameFrames)) {
    opus_encoder_destroy(enc);
    return abi::Failf(IM_ERR_INTERNAL, {}, "OPUS_GET_LOOKAHEAD: %s, %d samples", opus_strerror(err),
                      static_cast<int>(lookahead));
  }

  OpusStageConfig c = cfg;
  c.bitrate_bps = bitrate;
  c.pcm = tap ? cfg.pcm : nullptr;
  auto *stage = new (std::nothrow) OpusStage(c, enc, static_cast<int32_t>(lookahead));
  if (stage == nullptr) {
    opus_encoder_destroy(enc);
    return abi::Fail(IM_ERR_INTERNAL, {}, "opus stage: out of memory");
  }
  out->reset(stage);
  log::Info(
      kComponent, "opus stage ready",
      {{"bitrate", bitrate}, {"lookahead", static_cast<int32_t>(lookahead)}, {"tap_pcm", tap}, {"marker", cfg.marker}});
  return IM_OK;
}

OpusStage::OpusStage(const OpusStageConfig &cfg, OpusEncoder *enc, int32_t lookahead) noexcept
    : enc_(enc), epoch_ns_(cfg.epoch_ns), lookahead_samples_(lookahead),
      lookahead_ns_(int64_t{lookahead} * kNsPerSecond / int64_t{kRate}), audio_(cfg.audio), pcm_(cfg.pcm),
      marker_(cfg.marker), applied_bitrate_(cfg.bitrate_bps), next_beep_ns_(cfg.epoch_ns),
      target_bitrate_(cfg.bitrate_bps) {}

OpusStage::~OpusStage() { opus_encoder_destroy(enc_); }

void OpusStage::OnChunk(const float *frames, int64_t captureNs) noexcept {
  if (frames == nullptr) {
    return;
  }
  // 05 keeps consecutive chunks of a grid exactly 10 ms apart and calls OnGap before any other step. A chunk off the
  // grid is taken as a new grid, so packets within a grid always stay exactly 20 ms apart.
  if (grid_open_ && captureNs != last_chunk_ns_ + kChunkNs) {
    NewGrid();
  }
  grid_open_ = true;
  last_chunk_ns_ = captureNs;

  if (pcm_ != nullptr) {
    Tap(frames, captureNs);
  }

  if (half_) {
    std::memcpy(frame_ + kChunkSamples, frames, kChunkBytes);
    half_ = false;
    EncodeFrame(half_ns_ - lookahead_ns_);
    return;
  }
  // This chunk would start a frame: keep it only if that packet is stamped at or after epoch_ns and at least 20 ms
  // after the previous packet.
  const int64_t ts = captureNs - lookahead_ns_;
  if (ts < epoch_ns_ || (packet_seen_ && ts < last_packet_ns_ + kFrameNs)) {
    early_chunks_.fetch_add(1, std::memory_order_relaxed);
    return;
  }
  std::memcpy(frame_, frames, kChunkBytes);
  half_ = true;
  half_ns_ = captureNs;
}

void OpusStage::OnGap(int64_t captureNs) noexcept {
  // The next chunk may have any time; it is checked against epoch_ns and the previous packet when it comes.
  (void)captureNs;
  NewGrid();
}

void OpusStage::NewGrid() noexcept {
  half_ = false; // a half-filled frame is dropped
  grid_open_ = false;
  opus_flags_ |= IM_PKT_REINIT;
  pcm_flags_ |= IM_PKT_REINIT;
  gaps_.fetch_add(1, std::memory_order_relaxed);
}

void OpusStage::Tap(const float *frames, int64_t ts) noexcept {
  if (ts < epoch_ns_ || (pcm_seen_ && ts < last_pcm_ns_ + kChunkNs)) {
    return;
  }
  pcm_seen_ = true;
  last_pcm_ns_ = ts;
  AudioPacket p;
  p.kind = IM_PACKET_PCM;
  p.flags = pcm_flags_;
  p.capture_ts_ns = ts;
  p.duration_us = kPcmDurationUs;
  p.data = reinterpret_cast<const uint8_t *>(frames);
  p.size = kChunkBytes;
  if (pcm_->Write(p)) {
    pcm_flags_ = 0;
    pcm_packets_.fetch_add(1, std::memory_order_relaxed);
  } else {
    pcm_flags_ |= IM_PKT_DISCONTINUITY;
    pcm_drops_.fetch_add(1, std::memory_order_relaxed);
  }
}

void OpusStage::EncodeFrame(int64_t ts) noexcept {
  // im_set_audio_bitrate takes effect from this frame on: live, no reset, no REINIT.
  const uint32_t target = target_bitrate_.load(std::memory_order_relaxed);
  if (target != applied_bitrate_ &&
      opus_encoder_ctl(enc_, OPUS_SET_BITRATE(static_cast<opus_int32>(target))) == OPUS_OK) {
    applied_bitrate_ = target;
  }
  packet_seen_ = true;
  last_packet_ns_ = ts;
  const uint32_t index = packet_index_++;

  const opus_int32 n = opus_encode_float(enc_, frame_, static_cast<int>(kFrameFrames), encoded_,
                                         static_cast<opus_int32>(kMaxEncodeBytes));
  if (n <= 0) {
    // Never seen with valid input; the frame is lost like a ring drop and the grid goes on.
    opus_flags_ |= IM_PKT_DISCONTINUITY;
    encode_errors_.fetch_add(1, std::memory_order_relaxed);
    if (!logged_encode_error_) {
      logged_encode_error_ = true;
      log::Error(kComponent, "opus encode failed", {{"status", static_cast<int32_t>(n)}});
    }
    return;
  }

  const uint8_t *data = encoded_;
  auto size = static_cast<std::size_t>(n);
  if (marker_) {
    Marker m;
    m.frame = index;
    m.capture_ns = ts - epoch_ns_;
    if (ts >= next_beep_ns_) { // the first packet at or after each beep start
      m.beep = true;
      next_beep_ns_ += ((ts - next_beep_ns_) / kNsPerSecond + 1) * kNsPerSecond;
    }
    const std::size_t framed = WriteWithMarker(std::span<const uint8_t>(encoded_, size), m, framed_);
    if (framed > 0) {
      data = framed_;
      size = framed;
    }
  }

  AudioPacket p;
  p.kind = IM_PACKET_AUDIO;
  p.flags = opus_flags_;
  p.capture_ts_ns = ts;
  p.duration_us = kOpusDurationUs;
  p.data = data;
  p.size = static_cast<uint32_t>(size);
  if (audio_->Write(p)) {
    opus_flags_ = 0;
    packets_.fetch_add(1, std::memory_order_relaxed);
  } else {
    opus_flags_ |= IM_PKT_DISCONTINUITY;
    drops_.fetch_add(1, std::memory_order_relaxed);
  }
}

im_status OpusStage::SetBitrate(uint32_t bps) noexcept {
  if (!ValidBitrate(bps)) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "audio bitrate %u is outside %u..%u", bps, kMinBitrate, kMaxBitrate);
  }
  target_bitrate_.store(bps, std::memory_order_relaxed);
  return IM_OK;
}

im_status OpusStage::ReadSettings(OpusSettings *out) const noexcept {
  if (out == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus stage: settings out is NULL");
  }
  OpusSettings s;
  opus_int32 v[13] = {};
  const int results[] = {
      opus_encoder_ctl(enc_, OPUS_GET_APPLICATION(&v[0])),    opus_encoder_ctl(enc_, OPUS_GET_SAMPLE_RATE(&v[1])),
      opus_encoder_ctl(enc_, OPUS_GET_BITRATE(&v[2])),        opus_encoder_ctl(enc_, OPUS_GET_VBR(&v[3])),
      opus_encoder_ctl(enc_, OPUS_GET_VBR_CONSTRAINT(&v[4])), opus_encoder_ctl(enc_, OPUS_GET_COMPLEXITY(&v[5])),
      opus_encoder_ctl(enc_, OPUS_GET_SIGNAL(&v[6])),         opus_encoder_ctl(enc_, OPUS_GET_MAX_BANDWIDTH(&v[7])),
      opus_encoder_ctl(enc_, OPUS_GET_INBAND_FEC(&v[8])),     opus_encoder_ctl(enc_, OPUS_GET_PACKET_LOSS_PERC(&v[9])),
      opus_encoder_ctl(enc_, OPUS_GET_DTX(&v[10])),           opus_encoder_ctl(enc_, OPUS_GET_LSB_DEPTH(&v[11])),
      opus_encoder_ctl(enc_, OPUS_GET_LOOKAHEAD(&v[12])),
  };
  for (const int r : results) {
    if (r != OPUS_OK) {
      return abi::Failf(IM_ERR_INTERNAL, {}, "opus_encoder_ctl: %s", opus_strerror(r));
    }
  }
  s.application = v[0];
  s.sample_rate = v[1];
  s.bitrate = v[2];
  s.vbr = v[3];
  s.vbr_constraint = v[4];
  s.complexity = v[5];
  s.signal = v[6];
  s.max_bandwidth = v[7];
  s.inband_fec = v[8];
  s.packet_loss_perc = v[9];
  s.dtx = v[10];
  s.lsb_depth = v[11];
  s.lookahead = v[12];
  *out = s;
  return IM_OK;
}

OpusStageStats OpusStage::stats() const noexcept {
  OpusStageStats s;
  s.packets = packets_.load(std::memory_order_relaxed);
  s.drops = drops_.load(std::memory_order_relaxed);
  s.pcm_packets = pcm_packets_.load(std::memory_order_relaxed);
  s.pcm_drops = pcm_drops_.load(std::memory_order_relaxed);
  s.early_chunks = early_chunks_.load(std::memory_order_relaxed);
  s.gaps = gaps_.load(std::memory_order_relaxed);
  s.encode_errors = encode_errors_.load(std::memory_order_relaxed);
  s.bitrate_bps = target_bitrate_.load(std::memory_order_relaxed);
  return s;
}

} // namespace im::audio
