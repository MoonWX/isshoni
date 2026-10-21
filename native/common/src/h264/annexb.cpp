#include "im/h264/annexb.h"

#include "im/h264/nal.h"
#include "isshoni_media.h"

namespace im::h264 {

namespace {

constexpr uint8_t kStartCode[4] = {0, 0, 0, 1};

// Whether a NAL unit of this type stays in the stream (see the header: everything else is dropped).
bool Kept(uint8_t type) noexcept {
  switch (type) {
  case nal::kSlice:
  case nal::kSliceDpa:
  case nal::kSliceDpb:
  case nal::kSliceDpc:
  case nal::kIdr:
  case nal::kSps:
  case nal::kPps:
  case nal::kSpsExt:
  case nal::kPrefix:
  case nal::kSubsetSps:
  case nal::kAuxSlice:
  case nal::kSliceExt:
    return true;
  default:
    return false;
  }
}

bool Forbidden(std::span<const uint8_t> nal) noexcept { return (nal[0] & 0x80) != 0; }

void Assign(std::vector<uint8_t> &v, std::span<const uint8_t> s) { v.assign(s.begin(), s.end()); }

// The constraint byte a session profile wants for an SPS with this profile_idc, or -1 to leave it.
int WantedConstraint(uint32_t session_profile, uint8_t profile_idc) noexcept {
  if (session_profile == IM_PROFILE_CB && profile_idc == kProfileBaseline) {
    return 0xE0;
  }
  if (session_profile == IM_PROFILE_HIGH && profile_idc == kProfileHigh) {
    return 0x00;
  }
  return -1;
}

bool ProfileMatches(uint32_t session_profile, uint8_t profile_idc) noexcept {
  if (session_profile == IM_PROFILE_HIGH) {
    return profile_idc == kProfileHigh;
  }
  if (session_profile == IM_PROFILE_CB) {
    return profile_idc == kProfileBaseline;
  }
  return true;
}

} // namespace

std::size_t Normalizer::MaxOutputSize(std::size_t n) noexcept {
  // The smallest unit is a 3-byte start code and one byte: it grows by a quarter. Plus an inserted SPS and PPS.
  return n + n / 4 + 2 * (sizeof(kStartCode) + MaxEscapedSize(kMaxSpsBytes)) + 8;
}

void Normalizer::Reserve(std::size_t au_bytes) {
  out_.reserve(MaxOutputSize(au_bytes));
  sps_.reserve(MaxEscapedSize(kMaxSpsBytes));
  pps_.reserve(kMaxSpsBytes);
  scratch_.reserve(MaxEscapedSize(kMaxSpsBytes));
}

void Normalizer::Reset() noexcept {
  sps_.clear();
  pps_.clear();
  sps_info_ = Sps{};
  sps_ok_ = false;
}

void Normalizer::Append(std::span<const uint8_t> nal) {
  out_.insert(out_.end(), std::begin(kStartCode), std::end(kStartCode));
  out_.insert(out_.end(), nal.begin(), nal.end());
}

void Normalizer::TakeSps(std::span<const uint8_t> nal, const NormalizeOptions &options, AuInfo &info) {
  Sps parsed;
  if (ParseSps(nal, &parsed) != SpsError::kOk) {
    Assign(sps_, nal);
    sps_info_ = Sps{};
    sps_ok_ = false;
    info.sps_invalid = true;
    return;
  }
  const int want = WantedConstraint(options.session_profile, parsed.profile_idc);
  if (want < 0 || parsed.constraint_flags == static_cast<uint8_t>(want)) {
    Assign(sps_, nal);
    sps_info_ = parsed;
    sps_ok_ = true;
    return;
  }
  // Unescape the whole unit (the header byte is never 00, so the RBSP's escaping is unchanged by including it), set the
  // constraint byte (header, profile_idc, constraint: index 2) and escape it again.
  scratch_.resize(nal.size());
  const std::size_t n = UnescapeRbsp(nal, std::span<uint8_t>(scratch_));
  scratch_.resize(n);
  scratch_[2] = static_cast<uint8_t>(want);
  sps_.resize(MaxEscapedSize(n));
  sps_.resize(EscapeRbsp(std::span<const uint8_t>(scratch_), std::span<uint8_t>(sps_)));
  parsed.constraint_flags = static_cast<uint8_t>(want);
  sps_info_ = parsed;
  sps_ok_ = true;
  info.sps_rewritten = true;
}

std::span<const uint8_t> Normalizer::Normalize(std::span<const uint8_t> au, const NormalizeOptions &options,
                                               AuInfo *info_out) {
  AuInfo info;
  out_.clear();

  // Pass 1: what the AU holds. The last SPS and PPS win; the earlier ones are duplicates.
  std::span<const uint8_t> last_sps;
  std::span<const uint8_t> last_pps;
  uint32_t sps_count = 0;
  uint32_t pps_count = 0;
  const std::size_t leading = ForEachNal(au, [&](std::span<const uint8_t> unit) {
    const uint8_t type = NalType(unit);
    if (Forbidden(unit) || !Kept(type)) {
      ++info.dropped_nals;
      return true;
    }
    if (type == nal::kSps) {
      last_sps = unit;
      ++sps_count;
    } else if (type == nal::kPps) {
      last_pps = unit;
      ++pps_count;
    } else if (IsVcl(type)) {
      info.has_vcl = true;
      if (type == nal::kIdr) {
        info.keyframe = true;
      }
    }
    return true;
  });
  info.leading_bytes = static_cast<uint32_t>(leading);
  if (sps_count > 1) {
    info.dropped_nals += sps_count - 1;
  }
  if (pps_count > 1) {
    info.dropped_nals += pps_count - 1;
  }

  // The cache follows the stream, also for an AU that carries only parameter sets.
  if (!last_sps.empty()) {
    TakeSps(last_sps, options, info);
  }
  if (!last_pps.empty()) {
    Assign(pps_, last_pps);
  }
  if (sps_ok_) {
    info.profile = sps_info_.profile_key();
    info.width = static_cast<uint16_t>(sps_info_.width);
    info.height = static_cast<uint16_t>(sps_info_.height);
    info.profile_mismatch = !ProfileMatches(options.session_profile, sps_info_.profile_idc);
  }
  if (options.encoder_keyframe >= 0) {
    info.key_mismatch = (options.encoder_keyframe != 0) != info.keyframe;
  }
  if (!info.has_vcl) {
    // Parameter sets (or nothing) only: no picture to send. The next IDR gets them from the cache.
    info.dropped_nals += (last_sps.empty() ? 0u : 1u) + (last_pps.empty() ? 0u : 1u);
    if (info_out != nullptr) {
      *info_out = info;
    }
    return {};
  }

  // Pass 2: SPS, PPS, then everything else in order.
  if (!last_sps.empty() || info.keyframe) {
    if (!sps_.empty()) {
      Append(sps_);
      ++info.nal_count;
      info.inserted_parameter_sets = info.inserted_parameter_sets || last_sps.empty();
    } else {
      info.missing_parameter_sets = true;
    }
  }
  if (!last_pps.empty() || info.keyframe) {
    if (!pps_.empty()) {
      Append(pps_);
      ++info.nal_count;
      info.inserted_parameter_sets = info.inserted_parameter_sets || last_pps.empty();
    } else {
      info.missing_parameter_sets = true;
    }
  }
  ForEachNal(au, [&](std::span<const uint8_t> unit) {
    const uint8_t type = NalType(unit);
    if (Forbidden(unit) || !Kept(type) || type == nal::kSps || type == nal::kPps) {
      return true;
    }
    Append(unit);
    ++info.nal_count;
    return true;
  });
  if (info_out != nullptr) {
    *info_out = info;
  }
  return std::span<const uint8_t>(out_);
}

} // namespace im::h264
