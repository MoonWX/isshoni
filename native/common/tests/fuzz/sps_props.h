// The properties the SPS parser, the normalizer's constraint rewrite and the emulation-prevention transforms
// (im/h264/sps.h, nal.h; docs/m2/04-windows-engine.md §9.4) hold on any input. Shared by the libFuzzer target sps_fuzz
// and sps_test's deterministic mutation run. The input is tried as a NAL unit as is and with an SPS header byte (0x67)
// in front.
#pragma once

#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

#include "im/h264/nal.h"
#include "im/h264/sps.h"

namespace im::h264::fuzzing {

// Whether data holds 00 00 00, 00 00 01 or 00 00 02.
inline bool HasStartCodeLike(std::span<const uint8_t> data) {
  for (std::size_t i = 0; i + 2 < data.size(); ++i) {
    if (data[i] == 0 && data[i + 1] == 0 && data[i + 2] <= 2) {
      return true;
    }
  }
  return false;
}

inline bool SameSps(const Sps &a, const Sps &b, bool ignore_constraint) {
  return a.profile_idc == b.profile_idc && (ignore_constraint || a.constraint_flags == b.constraint_flags) &&
         a.level_idc == b.level_idc && a.sps_id == b.sps_id && a.chroma_format_idc == b.chroma_format_idc &&
         a.frame_mbs_only == b.frame_mbs_only && a.coded_width == b.coded_width && a.coded_height == b.coded_height &&
         a.width == b.width && a.height == b.height && a.vui_present == b.vui_present;
}

// Checks one candidate SPS NAL unit; returns nullptr when every property holds.
inline const char *CheckSpsUnit(std::span<const uint8_t> nal) {
  Sps sps;
  const SpsError err = ParseSps(nal, &sps);
  if (err == SpsError::kOk) {
    if (sps.width == 0 || sps.height == 0 || sps.width > sps.coded_width || sps.height > sps.coded_height ||
        sps.coded_width > 32768 || sps.coded_height > 32768 || sps.coded_width % 16 != 0 ||
        sps.coded_height % 16 != 0) {
      return "a parsed SPS has an impossible size";
    }
    if (sps.profile_key() != ((uint32_t{sps.profile_idc} << 8) | sps.constraint_flags)) {
      return "profile_key must be profile_idc << 8 | constraint byte";
    }
  }
  if (nal.empty() || nal.size() > kMaxSpsBytes) {
    return nullptr;
  }
  // Escaping: Unescape(Escape(r)) == r for an RBSP that doesn't end in a zero byte, and Escape's output never holds
  // 00 00 00, 00 00 01 or 00 00 02.
  std::vector<uint8_t> rbsp(nal.size());
  rbsp.resize(UnescapeRbsp(nal, rbsp));
  std::vector<uint8_t> esc(MaxEscapedSize(rbsp.size()));
  esc.resize(EscapeRbsp(rbsp, esc));
  if (HasStartCodeLike(esc)) {
    return "escaped data holds a start-code-like sequence";
  }
  if (!esc.empty() && esc.back() == 0) {
    return "escaped data ends in a zero byte";
  }
  std::vector<uint8_t> back(esc.size());
  back.resize(UnescapeRbsp(esc, back));
  if (rbsp.empty() || rbsp.back() != 0) {
    if (back != rbsp) {
      return "Unescape(Escape(r)) != r";
    }
    // The canonical form parses exactly like the input.
    Sps again;
    const SpsError err2 = ParseSps(esc, &again);
    if (err2 != err || (err == SpsError::kOk && !SameSps(sps, again, false))) {
      return "re-escaping the SPS changed what it parses to";
    }
  }
  // The normalizer's constraint rewrite (SetConstraintByte, CB's e0 and High's 00) changes RBSP byte 2 (counting the
  // header byte) and nothing else, so it parses to the same fields; it fits in kMaxSpsBytes and adds no start-code-like
  // sequence.
  if (err == SpsError::kOk) {
    const bool clean = !HasStartCodeLike(nal);
    for (const uint8_t value : {uint8_t{0xE0}, uint8_t{0x00}}) {
      std::vector<uint8_t> out(kMaxSpsBytes);
      out.resize(SetConstraintByte(nal, value, out));
      if (out.empty()) {
        return "SetConstraintByte refused an SPS that parses (or its result exceeds kMaxSpsBytes)";
      }
      std::vector<uint8_t> changed = rbsp;
      changed[2] = value;
      std::vector<uint8_t> out_rbsp(out.size());
      out_rbsp.resize(UnescapeRbsp(out, out_rbsp));
      if (out_rbsp != changed) {
        return "rewriting the constraint byte changed another byte of the RBSP";
      }
      Sps rewritten;
      if (ParseSps(out, &rewritten) != SpsError::kOk || rewritten.constraint_flags != value ||
          !SameSps(sps, rewritten, true)) {
        return "rewriting the constraint byte changed another field";
      }
      if (clean && HasStartCodeLike(out)) {
        return "rewriting the constraint byte made a start-code-like sequence";
      }
    }
  }
  return nullptr;
}

inline const char *CheckSps(std::span<const uint8_t> data) {
  if (const char *e = CheckSpsUnit(data)) {
    return e;
  }
  std::vector<uint8_t> with_header;
  with_header.reserve(data.size() + 1);
  with_header.push_back(0x67);
  with_header.insert(with_header.end(), data.begin(), data.end());
  return CheckSpsUnit(with_header);
}

} // namespace im::h264::fuzzing
