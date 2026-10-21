// The Opus decoder (opus_decoder.h).
#include "im/audio/opus_decoder.h"

#include <opus.h>

#include <new>

#include "im/abi/status.h"
#include "im/audio/pcm_sink.h"

namespace im::audio {

im_status OpusPacketDecoder::Create(std::unique_ptr<OpusPacketDecoder> *out) noexcept {
  if (out == nullptr) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus decoder: out is NULL");
  }
  out->reset();
  int err = OPUS_OK;
  OpusDecoder *dec = opus_decoder_create(static_cast<opus_int32>(kRate), static_cast<int>(kChannels), &err);
  if (dec == nullptr || err != OPUS_OK) {
    if (dec != nullptr) {
      opus_decoder_destroy(dec);
    }
    return abi::Failf(IM_ERR_INTERNAL, {}, "opus_decoder_create: %s", opus_strerror(err));
  }
  auto *d = new (std::nothrow) OpusPacketDecoder(dec);
  if (d == nullptr) {
    opus_decoder_destroy(dec);
    return abi::Fail(IM_ERR_INTERNAL, {}, "opus decoder: out of memory");
  }
  out->reset(d);
  return IM_OK;
}

OpusPacketDecoder::~OpusPacketDecoder() { opus_decoder_destroy(dec_); }

int32_t OpusPacketDecoder::Decode(const uint8_t *pkt, int32_t len, float *pcm, int32_t max_frames) noexcept {
  if (pkt == nullptr || len <= 0 || pcm == nullptr || max_frames <= 0) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "opus decode: NULL or empty packet or output");
  }
  const int n = opus_decode_float(dec_, pkt, len, pcm, max_frames, 0);
  if (n >= 0) {
    return n;
  }
  if (n == OPUS_BAD_ARG || n == OPUS_BUFFER_TOO_SMALL || n == OPUS_INVALID_PACKET) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "opus_decode_float: %s", opus_strerror(n));
  }
  return abi::Failf(IM_ERR_INTERNAL, {}, "opus_decode_float: %s", opus_strerror(n));
}

void OpusPacketDecoder::Reset() noexcept { opus_decoder_ctl(dec_, OPUS_RESET_STATE); }

} // namespace im::audio
