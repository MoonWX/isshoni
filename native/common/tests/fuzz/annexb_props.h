// The properties the access-unit normalizer (im/h264/annexb.h, docs/m2/04-windows-engine.md §9.4) holds on any
// input. Shared by the libFuzzer target annexb_fuzz and annexb_test's deterministic mutation run, so every host and
// CI configuration checks them, not only the fuzzing builds.
//
// Input layout: byte 0 picks the options (bits 0-1: session profile none/High/CB/none; bits 2-3: the encoder's key
// flag -1/0/1/-1); byte 1's bits 0-1 split the rest into 1..4 access units, normalized in order by one normalizer
// (so the SPS/PPS cache is exercised).
#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

#include "im/h264/annexb.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"

namespace im::h264::fuzzing {

inline NormalizeOptions OptionsFromByte(uint8_t b) {
  NormalizeOptions o;
  switch (b & 3u) {
  case 1:
    o.session_profile = IM_PROFILE_HIGH;
    break;
  case 2:
    o.session_profile = IM_PROFILE_CB;
    break;
  default:
    o.session_profile = 0;
    break;
  }
  switch ((b >> 2) & 3u) {
  case 1:
    o.encoder_keyframe = 0;
    break;
  case 2:
    o.encoder_keyframe = 1;
    break;
  default:
    o.encoder_keyframe = -1;
    break;
  }
  return o;
}

// Checks one normalized access unit; returns nullptr when every property holds.
inline const char *CheckOutput(std::span<const uint8_t> out, const AuInfo &info, const NormalizeOptions &o) {
  if (out.empty() != !info.has_vcl) {
    return "output must be empty exactly when the AU has no slice";
  }
  if (out.empty()) {
    return nullptr;
  }
  // Canonical framing: re-framing the units with 4-byte start codes gives the same bytes.
  std::vector<std::vector<uint8_t>> units;
  const std::size_t leading = ForEachNal(out, [&](std::span<const uint8_t> nal) {
    units.emplace_back(nal.begin(), nal.end());
    return true;
  });
  if (leading != 0) {
    return "output has bytes before its first start code";
  }
  std::vector<uint8_t> reframed;
  for (const auto &u : units) {
    reframed.insert(reframed.end(), {0, 0, 0, 1});
    reframed.insert(reframed.end(), u.begin(), u.end());
  }
  if (reframed.size() != out.size() || !std::equal(reframed.begin(), reframed.end(), out.begin())) {
    return "output is not canonical Annex B (4-byte start codes, no stray or trailing zero bytes)";
  }
  if (units.size() != info.nal_count) {
    return "nal_count differs from the units written";
  }
  bool idr = false;
  std::size_t sps_at = units.size();
  std::size_t pps_at = units.size();
  for (std::size_t i = 0; i < units.size(); ++i) {
    const uint8_t type = NalType(units[i]);
    if ((units[i][0] & 0x80) != 0) {
      return "a unit with forbidden_zero_bit set was kept";
    }
    switch (type) {
    case nal::kSei:
    case nal::kAud:
    case nal::kEndOfSeq:
    case nal::kEndOfStream:
    case nal::kFiller:
      return "a dropped unit type (AUD, SEI, filler, end of sequence or stream) was kept";
    case nal::kSps:
      if (sps_at != units.size()) {
        return "two SPS in one AU";
      }
      sps_at = i;
      break;
    case nal::kPps:
      if (pps_at != units.size()) {
        return "two PPS in one AU";
      }
      pps_at = i;
      break;
    case nal::kIdr:
      idr = true;
      break;
    default:
      if (type == 0 || (type >= 16 && type <= 18) || type >= 21) {
        return "a reserved or unspecified unit type was kept";
      }
      break;
    }
  }
  if (idr != info.keyframe) {
    return "keyframe must equal 'contains an IDR slice'";
  }
  if (sps_at != units.size() && sps_at != 0) {
    return "the SPS must come first";
  }
  if (pps_at != units.size() && pps_at != (sps_at == units.size() ? 0u : 1u)) {
    return "the PPS must follow the SPS (or come first without one)";
  }
  if (idr && !info.missing_parameter_sets && (sps_at != 0 || pps_at != 1)) {
    return "an IDR AU must start with SPS, PPS";
  }
  if (o.encoder_keyframe >= 0 && info.key_mismatch != ((o.encoder_keyframe != 0) != info.keyframe)) {
    return "key_mismatch must cross-check the encoder's flag";
  }
  if (sps_at != units.size() && o.session_profile != 0) {
    Sps sps;
    if (ParseSps(units[sps_at], &sps) == SpsError::kOk) {
      if (o.session_profile == IM_PROFILE_CB && sps.profile_idc == kProfileBaseline && sps.constraint_flags != 0xE0) {
        return "a CB session's baseline SPS must carry constraint byte e0";
      }
      if (o.session_profile == IM_PROFILE_HIGH && sps.profile_idc == kProfileHigh && sps.constraint_flags != 0x00) {
        return "a High session's SPS must carry constraint byte 00";
      }
    }
  }
  return nullptr;
}

// Runs the whole input; returns nullptr when every property holds.
inline const char *CheckAnnexB(std::span<const uint8_t> data) {
  if (data.size() < 2) {
    return nullptr;
  }
  const NormalizeOptions o = OptionsFromByte(data[0]);
  const std::size_t parts = (data[1] & 3u) + 1;
  const std::span<const uint8_t> stream = data.subspan(2);
  Normalizer n;
  std::vector<uint8_t> first;
  for (std::size_t p = 0; p < parts; ++p) {
    const std::size_t begin = stream.size() * p / parts;
    const std::size_t end = stream.size() * (p + 1) / parts;
    AuInfo info;
    const std::span<const uint8_t> au = stream.subspan(begin, end - begin);
    const auto out = n.Normalize(au, o, &info);
    if (out.size() > Normalizer::MaxOutputSize(au.size())) {
      return "the output is larger than MaxOutputSize";
    }
    if (const char *err = CheckOutput(out, info, o)) {
      return err;
    }
    // Idempotence: a fresh normalizer with the same options reproduces the output byte for byte.
    first.assign(out.begin(), out.end());
    Normalizer again;
    AuInfo info2;
    const auto out2 = again.Normalize(first, o, &info2);
    if (out2.size() != first.size() || !std::equal(first.begin(), first.end(), out2.begin())) {
      return "not idempotent: normalizing the output again changed it";
    }
    if (info2.keyframe != info.keyframe) {
      return "not idempotent: the output's keyframe flag differs";
    }
    // An output with an SPS describes itself: profile and size must come out the same.
    if (again.has_sps() &&
        (info2.profile != info.profile || info2.width != info.width || info2.height != info.height)) {
      return "not idempotent: the output's profile or size differ";
    }
  }
  return nullptr;
}

} // namespace im::h264::fuzzing
