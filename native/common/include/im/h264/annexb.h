// The access-unit normalizer (docs/m2/04-windows-engine.md §9.4). Every access unit of every encoder passes through
// it before it enters a ring, so what leaves the engine looks the same whatever the encoder:
//
// - NAL units are split on 3- and 4-byte start codes and re-emitted with 4-byte start codes (00 00 00 01).
// - Dropped: access unit delimiters (9), end of sequence (10), end of stream (11), filler (12), SEI (6), and NAL
//   units no conforming H.264 stream of ours carries (types 0, 16..18, 21..31 — 24..31 would also collide with RTP's
//   STAP/FU types), units with forbidden_zero_bit set, and bytes before the first start code.
// - The latest SPS and PPS are cached. SPS and PPS go first, SPS then PPS, one of each: duplicates are removed, and an
//   IDR access unit always goes out as SPS, PPS, then the slices, the missing headers inserted from the cache (the
//   plan's "SPS/PPS in-band before every IDR"). An SPS or PPS larger than kMaxSpsBytes is dropped and empties its
//   cache entry (the encoder replaced what was cached), so the output stays within MaxOutputSize and the buffers
//   Reserve sized.
// - The SPS is parsed (sps.h): profile, constraint byte, level and the size after cropping fill the AU's profile key,
//   width and height (the cached SPS's for an AU that carries none).
// - The constraint byte is normalized for the session profile: Constrained Baseline (profile_idc 66) gets 0xE0, so the
//   stream's key is 42e0 as the SFU's table expects; High (profile_idc 100) gets 0x00, so the key is 6400 (04 §12.2:
//   the engine never sets constraint_set4/5; clearing a constraint flag only withdraws a promise, so the stream stays
//   valid). Only that byte changes (SetConstraintByte keeps the rest of the RBSP, emulation prevention included), and
//   the normalizer parses the rewritten SPS again before it uses it.
// - A session-profile mismatch (a High session whose SPS isn't profile_idc 100, a CB session whose SPS isn't 66) is
//   flagged: the caller closes that encoder and tries the next one (ENCODER_CHANGED, reason encode_error).
// - KEYFRAME = the AU contains an IDR slice (type 5); the encoder's own key flag is only cross-checked.
// - An AU without a slice (parameter sets only) produces no output: its SPS/PPS go into the cache for the next IDR.
//
// The output is idempotent: normalizing it again with a fresh normalizer and the same options gives the same bytes.
// Fuzzed (annexb_fuzz).
//
// The normalizer keeps its output in a buffer it reuses: after Reserve (or the first AUs of each size) it doesn't
// allocate, so the video thread can run it. One normalizer per layer; not thread-safe.
#pragma once

#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

#include "im/h264/sps.h"

namespace im::h264 {

struct NormalizeOptions {
  uint32_t session_profile = 0; // IM_PROFILE_HIGH or IM_PROFILE_CB: rewrite and check; 0: leave the SPS alone
  int encoder_keyframe = -1;    // the encoder's key flag (0/1) to cross-check, or -1
};

struct AuInfo {
  bool keyframe = false;                // contains an IDR slice
  bool has_vcl = false;                 // contains a slice (types 1..5); false: nothing was output
  bool inserted_parameter_sets = false; // an IDR got SPS and/or PPS from the cache
  bool missing_parameter_sets = false;  // an IDR without SPS or PPS and none cached: output without them
  bool profile_mismatch = false;        // the SPS doesn't match the session profile
  bool sps_invalid = false;             // an SPS that doesn't parse: passed through unchanged, or dropped (too large)
  bool sps_rewritten = false;           // the constraint byte was changed
  bool key_mismatch = false;            // encoder_keyframe disagrees with keyframe
  uint32_t profile = 0;                 // the active SPS's key (0x6400, 0x42E0), 0 when none is known
  uint16_t width = 0;                   // the active SPS's size after cropping, 0 when none is known
  uint16_t height = 0;
  uint32_t nal_count = 0;     // NAL units written
  uint32_t dropped_nals = 0;  // NAL units dropped (by type, forbidden bit, duplicates, size)
  uint32_t leading_bytes = 0; // bytes before the first start code (dropped)
};

class Normalizer {
public:
  Normalizer() = default;

  // Grows the output buffer for access units of up to au_bytes (the layer's arena size), so later calls don't
  // allocate.
  void Reserve(std::size_t au_bytes);

  // Normalizes one access unit. The result stays valid until the next call. info may be null.
  std::span<const uint8_t> Normalize(std::span<const uint8_t> au, const NormalizeOptions &options, AuInfo *info);

  // Forgets the cached SPS and PPS. Call it when the encoder re-opens or the session profile changes: the cache holds
  // the SPS as rewritten for the options it came with.
  void Reset() noexcept;

  bool has_sps() const noexcept { return !sps_.empty(); }
  bool has_pps() const noexcept { return !pps_.empty(); }
  // The active SPS (valid when has_sps() and it parsed).
  const Sps &sps() const noexcept { return sps_info_; }

  // The worst-case output size for an input of n bytes: start codes grow from 3 to 4 bytes, and an SPS and a PPS of
  // up to kMaxSpsBytes each may come from the cache or from a rewrite.
  static std::size_t MaxOutputSize(std::size_t n) noexcept;

private:
  // Caches the AU's SPS, rewritten for the session profile. False when it can't be carried (larger than kMaxSpsBytes,
  // or a rewrite that doesn't parse back): the cache is emptied and the caller drops the SPS.
  bool TakeSps(std::span<const uint8_t> nal, const NormalizeOptions &options, AuInfo &info);
  void ForgetSps() noexcept;
  void Append(std::span<const uint8_t> nal);

  std::vector<uint8_t> out_;
  std::vector<uint8_t> sps_; // cached SPS NAL (after rewrite)
  std::vector<uint8_t> pps_; // cached PPS NAL
  Sps sps_info_;
  bool sps_ok_ = false; // sps_ parsed
};

} // namespace im::h264
