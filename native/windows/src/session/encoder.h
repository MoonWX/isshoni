// What a session's video thread asks of a video encoder (docs/m2/04-windows-engine.md §9), and the list of encoders a
// session chooses from. The session owns the policy (encpolicy: the candidate order, one profile per session, live
// changes versus re-inits, the fallback, §9.1, §9.5–§9.8), the pacer (§8.4), the GPU path (§8: the frame slots, the
// converters, session/frames.h) and the access-unit normalizer (§9.4); an encoder only turns one picture into one
// Annex B access unit.
//
// Encoders today: IM_ENC_FAKE (src/testhooks/, IM_TEST_HOOKS builds only, 04 §18.2). README WA44 adds the libavcodec
// encoders and the probe's list behind EncoderCatalog, README WA51 OpenH264; the session code does not change.
//
// The picture (04 §8.3, §9.2; README WF2). An encoder that takes pixels (FrameInput::kNv12: every real one) is opened
// with the session's gpu::Device. Before each Encode the video thread asks it for the NV12 target of the next frame
// (NextTarget: a slice of its FFmpeg frame pool for a zero-copy encoder, or its own gpu::Nv12Surface that a CPU
// encoder reads back with gpu::Nv12Readback), converts the frame into it with the layer's gpu::Converter, and passes
// it as EncodeInput::picture. IM_ENC_FAKE takes nothing (FrameInput::kNone): it writes M1's Marker from EncodeInput's
// facts, and a session whose encoders all take nothing runs no GPU path at all.
//
// Threads: Open runs on the engine worker (im_start) or on the session's video thread (a re-init or a fallback);
// NextTarget, Encode and SetBitrate run on the video thread only, without the device's context lock held (an encoder
// takes it around its own D3D calls; FFmpeg through AVD3D11VADeviceContext.lock, 04 §9.2).
#pragma once

#include <cstdint>
#include <memory>
#include <span>
#include <vector>

#include "gpu/device.h"
#include "gpu/frame.h"
#include "isshoni_media.h"

namespace im::session {

// What an encoder takes per frame. Known from its catalog entry before it opens, because a session creates its device
// (on the encoder's adapter, 04 §8.1) before it opens any encoder.
enum class FrameInput : uint8_t {
  // The layer's NV12 picture on the session's device, converted into the target NextTarget hands out.
  kNv12,
  // Nothing but EncodeInput's facts (the test hooks' IM_ENC_FAKE and its fake-backed fallback kinds). A session whose
  // candidates all take nothing gets no device, frame slots or converters: its frames are capture times only, so
  // session_fake_test and Go's Conformance stay off WARP and keep their timing (session/frames.h).
  kNone,
};

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
  // FrameInput::kNv12: the session's device, which outlives the encoder (FFmpeg's hardware device wraps it with its
  // context lock, 04 §9.2). Null for an encoder that takes nothing.
  gpu::Device *device = nullptr;
};

// One frame to encode, as the pacer decided it (im::pace::Encode).
struct EncodeInput {
  int64_t ts_ns = 0;    // the packet's capture_ts_ns (engine clock)
  bool idr = false;     // force an IDR
  bool refresh = false; // a re-encode of an unchanged frame (IM_PKT_REFRESH)
  bool black = false;   // no captured frame may be used yet: encode black (the first-keyframe fallback, §6.3)
  // FrameInput::kNv12: the target NextTarget handed out, now holding this frame's picture (the newest captured frame,
  // the layer's last one again for a refresh or a static keyframe, or black), converted and letterboxed to the layer's
  // size (04 §8.3). Null for an encoder that takes nothing.
  const gpu::Nv12Target *picture = nullptr;
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
  // FrameInput::kNv12: where the session converts the next frame, an NV12 picture of the layer's output size on the
  // session's device (gpu::Nv12Target): a slice of the encoder's FFmpeg frame pool (zero copy, 04 §9.2), or the
  // encoder's own gpu::Nv12Surface, which Encode reads back for a CPU encoder. Called right before each Encode; the
  // target stays the encoder's, valid until that Encode returns. When the conversion fails, no Encode follows: the
  // session recovers the layer (a re-open, §9.8) or ends, and a later NextTarget may hand out the same target again.
  // IM_ERR_ENCODER_LOST on device loss (the session ends, device_removed); any other error is an encode error, unless
  // the session's device says it was removed (04 §8.1: then it is device loss too). The default, for encoders that
  // take nothing, is IM_ERR_NOT_IMPLEMENTED.
  virtual im_status NextTarget(gpu::Nv12Target *out) noexcept;
  // Encodes one frame. IM_OK with out filled; IM_ERR_ENCODER_LOST when the device was removed or reset (the session
  // ends with token device_removed, §9.8); any other error is an encode error, which the session recovers from by
  // re-opening the encoder once, then the next candidate (§9.8), unless the session's device says it was removed (an
  // ordinary error after a TDR, 04 §8.1: the session ends as device_removed).
  virtual im_status Encode(const EncodeInput &in, EncodedAu *out) noexcept = 0;
  // A live bitrate change (encoders with IM_ENCF_LIVE_BITRATE): applied before the next frame, no IDR (§9.5).
  virtual im_status SetBitrate(uint32_t bps) noexcept = 0;
};

// Opens an encoder of the entry's kind; IM_OK and *out set, IM_ERR_LIMIT for a session limit (NVENC's), or another
// error with the thread's detail set.
using OpenEncoderFn = im_status (*)(const EncoderParams &params, std::unique_ptr<VideoEncoder> *out);

// One encoder a session may choose: its probe entry (im_encoder_info: kind, adapter, IM_ENCF_* flags, status), how to
// open it, what it takes per frame and the adapter its device must be on.
struct EncoderEntry {
  im_encoder_info info{};
  OpenEncoderFn open = nullptr;
  FrameInput input = FrameInput::kNv12;
  // FrameInput::kNv12: the device a session using this encoder creates (04 §8.1). README WA44's probe sets the LUID of
  // info.adapter_index's adapter (im_caps.adapters[]); a software encoder takes the default adapter (on a CI runner
  // without a GPU that is the Basic Render Driver, WARP).
  gpu::DeviceRequest device;
};

// The encoders sessions choose from, in the probe's order (im_caps.encoders[]); encpolicy::BuildCandidates sorts
// them per session. Until README WA44's probe exists this is the test hooks' IM_ENC_FAKE (and the fake-backed
// fallback kinds a test asks for), all FrameInput::kNone, in IM_TEST_HOOKS builds, and empty in a release build,
// where im_start then fails with IM_ERR_NO_ENCODER (layer_failed) for any video layer.
std::vector<EncoderEntry> EncoderCatalog();

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// Test hooks (session_gpu_test): entries EncoderCatalog lists before the test hooks' fake encoders, for sessions
// started from now on; an empty list removes them (testhooks::Reset doesn't: the test that sets them clears them).
// An entry with FrameInput::kNv12 on a WARP device runs a session's GPU path before README WA44's encoders exist.
void SetCatalogPrefixForTest(std::vector<EncoderEntry> entries);
#endif

} // namespace im::session
