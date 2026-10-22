// What a session's video thread asks of a video encoder (docs/m2/04-windows-engine.md §9), and the list of encoders a
// session chooses from. The session owns the policy (encpolicy: the candidate order, one profile per session, live
// changes versus re-inits, the fallback, §9.1, §9.5–§9.8), the pacer (§8.4) and the access-unit normalizer (§9.4);
// an encoder only turns one frame into one Annex B access unit.
//
// Encoders today: IM_ENC_FAKE (src/testhooks/, IM_TEST_HOOKS builds only, 04 §18.2). README WA44 adds the libavcodec
// encoders and the probe's list behind EncoderCatalog, README WA51 OpenH264; the session code does not change.
//
// Threads: Open runs on the engine worker (im_start) or on the session's video thread (a re-init or a fallback);
// Encode and SetBitrate run on the video thread only.
#pragma once

#include <cstdint>
#include <memory>
#include <span>
#include <vector>

#include "isshoni_media.h"

namespace im::session {

// What an encoder is opened with: one layer of one session.
struct EncoderParams {
  uint32_t layer = 0;          // layer index ("f" = 0)
  char rid = 'f';              // the rid's first byte (the fake's Marker)
  uint32_t kind = IM_ENC_AUTO; // IM_ENC_*: the candidate being opened
  uint32_t profile = IM_PROFILE_HIGH;
  uint32_t width = 0; // output size, even, 64..4096 (im::video::ComputeOutputSize)
  uint32_t height = 0;
  uint32_t fps = 60;
  uint32_t bitrate_bps = 0;
  uint32_t gop_ms = 0;                     // 0 = the layer's default (04 §9.4: f 4000, others 2000)
  uint32_t content_hint = IM_CONTENT_AUTO; // IM_CONTENT_*: the tuning of 04 §9.3
  int64_t epoch_ns = 0;                    // im_session_info.epoch_ns
};

// One frame to encode, as the pacer decided it (im::pace::Encode).
struct EncodeInput {
  int64_t ts_ns = 0;    // the packet's capture_ts_ns (engine clock)
  bool idr = false;     // force an IDR
  bool refresh = false; // a re-encode of an unchanged frame (IM_PKT_REFRESH)
  bool black = false;   // no captured frame may be used yet: encode black (the first-keyframe fallback, §6.3)
  // Test-pattern facts the fake encoder writes into M1's Marker (04 §18.2); real encoders encode the pattern's pixels
  // instead: the layer's access-unit index (from 0, continuous across re-inits) and whether this is the layer's first
  // frame at or after epoch_ns + k × 1 s, the instants the test tone's beeps start (§10.3).
  uint32_t index = 0;
  bool flash = false;
};

// One encoded access unit. data stays valid until the encoder's next call.
struct EncodedAu {
  std::span<const uint8_t> data; // Annex B; empty when the encoder buffered the frame
  bool keyframe = false;         // the encoder's own key flag (the normalizer only cross-checks it, §9.4)
};

class VideoEncoder {
public:
  virtual ~VideoEncoder() = default;
  // Encodes one frame. IM_OK with out filled; IM_ERR_ENCODER_LOST when the device was removed or reset (the session
  // ends with token device_removed, §9.8); any other error is an encode error, which the session recovers from by
  // re-opening the encoder once, then the next candidate (§9.8).
  virtual im_status Encode(const EncodeInput &in, EncodedAu *out) noexcept = 0;
  // A live bitrate change (encoders with IM_ENCF_LIVE_BITRATE): applied before the next frame, no IDR (§9.5).
  virtual im_status SetBitrate(uint32_t bps) noexcept = 0;
};

// Opens an encoder of the entry's kind; IM_OK and *out set, IM_ERR_LIMIT for a session limit (NVENC's), or another
// error with the thread's detail set.
using OpenEncoderFn = im_status (*)(const EncoderParams &params, std::unique_ptr<VideoEncoder> *out);

// One encoder a session may choose: its probe entry (im_encoder_info: kind, adapter, IM_ENCF_* flags, status) and how
// to open it.
struct EncoderEntry {
  im_encoder_info info{};
  OpenEncoderFn open = nullptr;
};

// The encoders sessions choose from, in the probe's order (im_caps.encoders[]); encpolicy::BuildCandidates sorts
// them per session. Until README WA44's probe exists this is the test hooks' IM_ENC_FAKE (and the fake-backed
// fallback kinds a test asks for) in IM_TEST_HOOKS builds, and empty in a release build, where im_start then fails
// with IM_ERR_NO_ENCODER (layer_failed) for any video layer.
std::vector<EncoderEntry> EncoderCatalog();

} // namespace im::session
