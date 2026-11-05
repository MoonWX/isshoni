// The portable parts of the OpenH264 backend (docs/m2/04-windows-engine.md §9.3, §9.9; "04" below; README WA51):
// Cisco's headers (third_party/openh264/, the pinned release's), the encoder settings of 04 §9.3, the version check,
// the NV12 → I420 chroma split and the access unit of one EncodeFrame. Header-only, no OS header, so the backend's
// arithmetic also builds and runs on the Mac (README WA51's scratch checks); openh264_api.cpp and
// openh264_encoder.cpp use it on Windows.
#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

#include "encode/openh264_pin.h"

#if defined(_MSC_VER)
#pragma warning(push, 0) // Cisco's headers are not ours: /W4 /WX stays off them
#endif
#include "openh264/codec_api.h"
#include "openh264/codec_app_def.h"
#include "openh264/codec_def.h"
#include "openh264/codec_ver.h"
#if defined(_MSC_VER)
#pragma warning(pop)
#endif

namespace im::encode::openh264 {

static_assert(OPENH264_MAJOR == kPinMajor && OPENH264_MINOR == kPinMinor && OPENH264_REVISION == kPinRevision,
              "third_party/openh264's headers are not the pinned release (openh264_pin.h)");

// 04 §9.1: OpenH264 is capped at 1080p30. A larger picture fails the open (the next candidate takes the layer); a
// higher frame rate is encoded with the rate control of 30 fps (fMaxFrameRate), and frames that come faster are
// skipped by OpenH264's rate control rather than refused.
inline constexpr uint32_t kMaxPixels = 1920 * 1088;
inline constexpr uint32_t kMaxFps = 30;
// 04 §9.3: up to 4 threads, 4 fixed slices.
inline constexpr uint32_t kMaxThreads = 4;
inline constexpr uint32_t kSlices = 4;

// What one layer's encoder is opened with.
struct Settings {
  uint32_t width = 0;
  uint32_t height = 0;
  uint32_t fps = 30;
  uint32_t bitrate_bps = 0;
  uint32_t cores = 1; // std::thread::hardware_concurrency(), 0 read as 1
};

// Whether the picture fits 04 §9.1's cap (and OpenH264's: even sides, at least 16).
constexpr bool SizeSupported(uint32_t width, uint32_t height) noexcept {
  return width >= 16 && height >= 16 && width % 2 == 0 && height % 2 == 0 &&
         static_cast<uint64_t>(width) * height <= kMaxPixels;
}

// 04 §9.3's SEncParamExt, applied over GetDefaultParams' values: screen content in real time, bitrate rate control
// at the target (target = max), no periodic IDR (the session forces IDRs, 04 §9.4), constant SPS/PPS ids, CAVLC
// (Constrained Baseline, the only profile OpenH264 gives the engine), frame skipping allowed, min(4, cores) threads
// with 4 fixed slices, medium complexity; one spatial and one temporal layer of the layer's size.
inline void ApplySettings(const Settings &s, SEncParamExt *p) noexcept {
  const float fps = static_cast<float>(std::clamp<uint32_t>(s.fps, 1, kMaxFps));
  const int bps = static_cast<int>(std::min<uint32_t>(s.bitrate_bps, 0x7FFFFFFF));
  p->iUsageType = SCREEN_CONTENT_REAL_TIME;
  p->iPicWidth = static_cast<int>(s.width);
  p->iPicHeight = static_cast<int>(s.height);
  p->iTargetBitrate = bps;
  p->iMaxBitrate = bps;
  p->iRCMode = RC_BITRATE_MODE;
  p->fMaxFrameRate = fps;
  p->iTemporalLayerNum = 1;
  p->iSpatialLayerNum = 1;
  p->iComplexityMode = MEDIUM_COMPLEXITY;
  p->uiIntraPeriod = 0;
  p->eSpsPpsIdStrategy = CONSTANT_ID;
  p->bPrefixNalAddingCtrl = false;
  p->bEnableSSEI = false;
  p->bSimulcastAVC = false;
  p->iPaddingFlag = 0;
  p->iEntropyCodingModeFlag = 0;
  p->bEnableFrameSkip = true;
  p->bEnableLongTermReference = false;
  const uint32_t cores = std::max<uint32_t>(s.cores, 1);
  p->iMultipleThreadIdc = static_cast<unsigned short>(std::min(kMaxThreads, cores));
  p->bUseLoadBalancing = false;
  SSpatialLayerConfig &l = p->sSpatialLayers[0];
  l.iVideoWidth = static_cast<int>(s.width);
  l.iVideoHeight = static_cast<int>(s.height);
  l.fFrameRate = fps;
  l.iSpatialBitrate = bps;
  l.iMaxSpatialBitrate = bps;
  l.uiProfileIdc = PRO_BASELINE;
  l.uiLevelIdc = LEVEL_UNKNOWN; // OpenH264 derives the level from size, rate and bitrate
  l.sSliceArgument.uiSliceMode = SM_FIXEDSLCNUM_SLICE;
  l.sSliceArgument.uiSliceNum = kSlices;
  // Full range off, BT.709 in the VUI, like the hardware encoders (04 §9.3's color fields).
  l.bVideoSignalTypePresent = true;
  l.uiVideoFormat = static_cast<unsigned char>(VF_UNDEF);
  l.bFullRange = false;
  l.bColorDescriptionPresent = true;
  l.uiColorPrimaries = static_cast<unsigned char>(CP_BT709);
  l.uiTransferCharacteristics = static_cast<unsigned char>(TRC_BT709);
  l.uiColorMatrix = static_cast<unsigned char>(CM_BT709);
}

// Whether the loaded library is the pinned release (major, minor and revision; the build number is not compared).
constexpr bool VersionMatches(const OpenH264Version &v) noexcept {
  return v.uMajor == kPinMajor && v.uMinor == kPinMinor && v.uRevision == kPinRevision;
}

// Splits the interleaved Cb/Cr plane of an NV12 picture (width × height/2 bytes, rows uv_stride apart) into the U and
// V planes of I420 (width/2 × height/2 each, rows width/2 apart).
inline void SplitChroma(const uint8_t *uv, std::size_t uv_stride, uint32_t width, uint32_t height, uint8_t *u,
                        uint8_t *v) noexcept {
  const std::size_t cw = width / 2;
  for (uint32_t y = 0; y < height / 2; ++y) {
    const uint8_t *row = uv + static_cast<std::size_t>(y) * uv_stride;
    uint8_t *ur = u + static_cast<std::size_t>(y) * cw;
    uint8_t *vr = v + static_cast<std::size_t>(y) * cw;
    for (std::size_t x = 0; x < cw; ++x) {
      ur[x] = row[2 * x];
      vr[x] = row[2 * x + 1];
    }
  }
}

// The access unit of one EncodeFrame: every layer's NAL units (already Annex B, with start codes) in order, appended
// to out (cleared first). Empty for a skipped frame (videoFrameTypeSkip) or when no layer holds bytes.
inline void CollectAu(const SFrameBSInfo &info, std::vector<uint8_t> *out) {
  out->clear();
  if (info.eFrameType == videoFrameTypeSkip || info.eFrameType == videoFrameTypeInvalid) {
    return;
  }
  const int layers = std::clamp(info.iLayerNum, 0, static_cast<int>(MAX_LAYER_NUM_OF_FRAME));
  for (int i = 0; i < layers; ++i) {
    const SLayerBSInfo &l = info.sLayerInfo[i];
    if (l.pBsBuf == nullptr || l.pNalLengthInByte == nullptr) {
      continue;
    }
    std::size_t bytes = 0;
    for (int n = 0; n < l.iNalCount; ++n) {
      bytes += static_cast<std::size_t>(std::max(l.pNalLengthInByte[n], 0));
    }
    out->insert(out->end(), l.pBsBuf, l.pBsBuf + bytes);
  }
}

// Whether EncodeFrame said it wrote an IDR (the session's normalizer decides KEYFRAME from the NAL types, 04 §9.4;
// this is the encoder's own key flag, which it only cross-checks).
constexpr bool IsIdr(const SFrameBSInfo &info) noexcept { return info.eFrameType == videoFrameTypeIDR; }

} // namespace im::encode::openh264
