// IM_ENC_FAKE (docs/m2/04-windows-engine.md §18.2), the encoder of the IM_TEST_HOOKS builds: deterministic H.264
// access units without a GPU, so the CI tests (session_fake_test, Go's mediatest.Conformance and
// TestNativeIntoPublisher) run the whole session on windows-2025.
//
// - Every access unit carries a real SPS and PPS for the session profile and the layer size (High: profile_idc 100,
//   constraint byte 0x00; Constrained Baseline: profile_idc 66, constraint byte 0xE0; the level from size and frame
//   rate; cropping where the size isn't whole macroblocks), as an IDR AU (SPS, PPS, one NAL 5 slice) or a P AU (one
//   NAL 1 slice) — the SPS and PPS of a P AU are left out, as encoders do, and the normalizer (§9.4) puts them back in
//   front of every IDR.
// - Sizes: an AU is about target / fps bytes and an IDR about 10 times a P frame; the P size is lowered so that a GOP
//   (gop_ms) averages the target, so the 2 s average stays within ±10 % of it. Bitrate changes are live.
// - Each slice body starts with M1's 18-byte Marker (internal/media/fake/marker.go; emulation prevention applied, like
//   M1's synthetic slices): RID, Keyframe, Frame = the layer's AU index, CaptureNS = capture_ts_ns − epoch_ns, and
//   Flash on each layer's first frame at or after epoch_ns + k × 1 s (EncodeInput), so 01's Conformance checks 5 and 7
//   and its latency arithmetic work on native output; the rest of the body is seeded filler and the stop bit.
#pragma once

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <cstdint>
#include <memory>
#include <span>
#include <vector>

#include "isshoni_media.h"
#include "session/encoder.h"

namespace im::testhooks {

// The catalog entry of a fake-backed encoder reported as kind (IM_ENC_FAKE, or a fallback kind of Options): its
// IM_ENCF_* flags follow the kind's traits (im::encpolicy::Traits), status IM_OK.
session::EncoderEntry FakeEncoderEntry(uint32_t kind);

// The H.264 pieces, exposed for session_fake_test.
// The SPS NAL unit (header byte and emulation prevention included) of a layer of width × height at fps in profile
// (IM_PROFILE_HIGH or IM_PROFILE_CB). Empty when the size is above level 6.2.
std::vector<uint8_t> BuildSps(uint32_t profile, uint32_t width, uint32_t height, uint32_t fps);
// The PPS NAL unit: pic_parameter_set_id 0 on SPS 0, CAVLC.
std::vector<uint8_t> BuildPps();

// The fake's frame sizes in bytes: a P frame and an IDR for a layer at bitrate_bps, fps and gop_ms (0 = the layer's
// default, 4000 for layer 0 and 2000 for the others).
struct FakeSizes {
  uint32_t p = 0;
  uint32_t idr = 0;
};
FakeSizes FakeFrameSizes(uint32_t bitrate_bps, uint32_t fps, uint32_t gop_ms, uint32_t layer);

} // namespace im::testhooks

#endif // IM_TEST_HOOKS
