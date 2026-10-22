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
  // The smallest unit is a 3-byte start code and one byte: it grows by a quarter. Plus an SPS and a PPS: the cache
  // holds none larger than kMaxSpsBytes, and a rewritten SPS fits in it too (SetConstraintByte).
  return n + n / 4 + 2 * (sizeof(kStartCode) + kMaxSpsBytes) + 8;
}

void Normalizer::Reserve(std::size_t au_bytes) {
  out_.reserve(MaxOutputSize(au_bytes));
  sps_.reserve(kMaxSpsBytes);
  pps_.reserve(kMaxSpsBytes);
}

void Normalizer::Reset() noexcept {
  ForgetSps();
  pps_.clear();
}

void Normalizer::ForgetSps() noexcept {
  sps_.clear();
  sps_info_ = Sps{};
  sps_ok_ = false;
}

void Normalizer::Append(std::span<const uint8_t> nal) {
  out_.insert(out_.end(), std::begin(kStartCode), std::end(kStartCode));
  out_.insert(out_.end(), nal.begin(), nal.end());
}

bool Normalizer::TakeSps(std::span<const uint8_t> nal, const NormalizeOptions &options, AuInfo &info) {
  if (nal.size() > kMaxSpsBytes) {
    // Larger than the cache, MaxOutputSize and Reserve allow (it doesn't parse either): dropped.
    ForgetSps();
    info.sps_invalid = true;
    return false;
  }
  Sps parsed;
  if (ParseSps(nal, &parsed) != SpsError::kOk) {
    Assign(sps_, nal);
    sps_info_ = Sps{};
    sps_ok_ = false;
    info.sps_invalid = true;
    return true;
  }
  const int want = WantedConstraint(options.session_profile, parsed.profile_idc);
  if (want < 0 || parsed.constraint_flags == static_cast<uint8_t>(want)) {
    Assign(sps_, nal);
    sps_info_ = parsed;
    sps_ok_ = true;
    return true;
  }
  // Set the constraint byte, then check that the result parses with the new byte (SetConstraintByte keeps the rest
  // of the RBSP, so it does); the AU's profile and size come from the rewritten SPS that goes out.
  sps_.resize(kMaxSpsBytes);
  sps_.resize(SetConstraintByte(nal, static_cast<uint8_t>(want), std::span<uint8_t>(sps_)));
  Sps rewritten;
  if (sps_.empty() || ParseSps(sps_, &rewritten) != SpsError::kOk || rewritten.profile_idc != parsed.profile_idc ||
      rewritten.constraint_flags != static_cast<uint8_t>(want)) {
    ForgetSps();
    info.sps_invalid = true;
    return false;
  }
  sps_info_ = rewritten;
  sps_ok_ = true;
  info.sps_rewritten = true;
  return true;
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

  // The cache follows the stream, also for an AU that carries only parameter sets. An SPS or PPS that can't be carried
  // is dropped and leaves its cache entry empty: inserting the one it replaced before the next IDR would be wrong.
  if (!last_sps.empty() && !TakeSps(last_sps, options, info)) {
    ++info.dropped_nals;
    last_sps = {};
  }
  if (last_pps.size() > kMaxSpsBytes) {
    pps_.clear();
    ++info.dropped_nals;
    last_pps = {};
  } else if (!last_pps.empty()) {
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
