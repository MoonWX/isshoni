#include "im/encpolicy/candidates.h"

#include <algorithm>

#include "im/abi/status.h"
#include "im/encpolicy/encoders.h"

namespace im::encpolicy {

namespace {

// The groups of the order, in order.
enum Group : uint32_t {
  kFakeFirst = 0,
  kVendorCapture = 1,
  kMfHwCapture = 2,
  kVendorOther = 3,
  kMfHwOther = 4,
  kMfSw = 5,
  kOpenH264 = 6,
  kRest = 7, // kinds of other platforms, in probe order
};

uint32_t KindRank(uint32_t kind) noexcept {
  switch (kind) {
  case IM_ENC_NVENC:
    return 0;
  case IM_ENC_QSV:
    return 1;
  case IM_ENC_AMF:
    return 2;
  default:
    return 3;
  }
}

struct Keyed {
  uint64_t key = 0;
  uint32_t index = 0;
  bool cross = false;
};

constexpr std::size_t kMaxProbeEntries = 64;

} // namespace

std::size_t BuildCandidates(std::span<const im_encoder_info> probe, const CandidateQuery &query,
                            std::span<Candidate> out) noexcept {
  std::array<Keyed, kMaxProbeEntries> keyed{};
  std::size_t count = 0;
  const bool dd = query.capture_path == IM_CAPTURE_DD;
  for (std::size_t i = 0; i < probe.size() && count < kMaxProbeEntries; ++i) {
    const im_encoder_info &e = probe[i];
    if (e.status != IM_OK) {
      continue;
    }
    if (query.forced != IM_ENC_AUTO && e.kind != query.forced) {
      continue;
    }
    const bool on_capture = query.capture_adapter != kNoAdapter && e.adapter_index == query.capture_adapter;
    uint32_t group = kRest;
    uint32_t adapter_rank = 0;
    bool cross = false;
    const EncoderTraits &t = Traits(e.kind);
    if (e.kind == IM_ENC_FAKE) {
      if (!query.test_source && query.forced != IM_ENC_FAKE) {
        continue; // the fake encoder is chosen only for a test: source or when forced
      }
      group = kFakeFirst;
    } else if (e.kind == IM_ENC_NVENC || e.kind == IM_ENC_QSV || e.kind == IM_ENC_AMF) {
      group = on_capture ? kVendorCapture : kVendorOther;
    } else if (e.kind == IM_ENC_MF_HW) {
      group = on_capture ? kMfHwCapture : kMfHwOther;
    } else if (e.kind == IM_ENC_MF_SW) {
      group = kMfSw;
    } else if (e.kind == IM_ENC_OPENH264) {
      group = kOpenH264;
    } else if (t.token.empty()) {
      continue; // unknown kind
    }
    if (group == kVendorOther || group == kMfHwOther) {
      adapter_rank = std::min<uint32_t>(e.adapter_index, 0xFFFF);
      cross = dd && query.capture_adapter != kNoAdapter;
    }
    Keyed &k = keyed[count++];
    k.key = (uint64_t{group} << 48) | (uint64_t{adapter_rank} << 32) | (uint64_t{KindRank(e.kind)} << 24) | i;
    k.index = static_cast<uint32_t>(i);
    k.cross = cross;
  }
  std::sort(keyed.begin(), keyed.begin() + static_cast<std::ptrdiff_t>(count),
            [](const Keyed &a, const Keyed &b) { return a.key < b.key; });
  const std::size_t n = std::min(count, out.size());
  for (std::size_t i = 0; i < n; ++i) {
    const im_encoder_info &e = probe[keyed[i].index];
    Candidate c;
    c.kind = e.kind;
    c.adapter_index = e.adapter_index;
    c.flags = e.flags;
    c.cross_adapter_copy = keyed[i].cross;
    c.probe_index = keyed[i].index;
    out[i] = c;
  }
  return n;
}

bool Supports(const Candidate &c, uint32_t profile) noexcept {
  const bool probed = (c.flags & (IM_ENCF_HIGH | IM_ENCF_CB)) != 0;
  const EncoderTraits &t = Traits(c.kind);
  if (profile == IM_PROFILE_HIGH) {
    return probed ? (c.flags & IM_ENCF_HIGH) != 0 : t.high;
  }
  if (profile == IM_PROFILE_CB) {
    return probed ? (c.flags & IM_ENCF_CB) != 0 : t.cb;
  }
  return true;
}

namespace {

// Opens the layer with the first candidate that supports the profile and opens; further layers get one more round
// after a wait. Returns the candidate index, or candidates.size().
std::size_t OpenLayer(std::span<const Candidate> candidates, uint32_t layer, uint32_t profile, Opener &opener) {
  const int rounds = layer == 0 ? 1 : 2;
  for (int round = 0; round < rounds; ++round) {
    bool any = false;
    for (std::size_t i = 0; i < candidates.size(); ++i) {
      if (!Supports(candidates[i], profile)) {
        continue;
      }
      any = true;
      if (opener.Open(layer, candidates[i], profile) == IM_OK) {
        return i;
      }
    }
    if (!any) {
      break; // nothing to retry
    }
    if (round + 1 < rounds) {
      opener.Wait(kLayerRetryWaitMs);
    }
  }
  return candidates.size();
}

} // namespace

Selection SelectEncoders(std::span<const Candidate> candidates, uint32_t profile, uint32_t layer_count, bool forced,
                         Opener &opener) {
  Selection sel;
  sel.layer_count = std::min<uint32_t>(layer_count, IM_MAX_LAYERS);
  sel.profile = profile;
  if (sel.layer_count == 0) {
    return sel; // audio only
  }
  // High first; CB when some candidate can do CB but not High (OpenH264), so the whole session can still run.
  std::array<uint32_t, 2> profiles{profile, 0};
  std::size_t profile_count = 1;
  if (profile == IM_PROFILE_HIGH) {
    for (const Candidate &c : candidates) {
      if (Supports(c, IM_PROFILE_CB) && !Supports(c, IM_PROFILE_HIGH)) {
        profiles[profile_count++] = IM_PROFILE_CB;
        break;
      }
    }
  }
  uint32_t failed = 0;
  for (std::size_t p = 0; p < profile_count; ++p) {
    uint32_t opened = 0;
    bool ok = true;
    for (uint32_t l = 0; l < sel.layer_count; ++l) {
      const std::size_t i = OpenLayer(candidates, l, profiles[p], opener);
      if (i == candidates.size()) {
        ok = false;
        failed = l;
        break;
      }
      sel.layers[l].candidate = i;
      sel.layers[l].kind = candidates[i].kind;
      opened = l + 1;
    }
    if (ok) {
      sel.profile = profiles[p];
      return sel;
    }
    for (uint32_t l = 0; l < opened; ++l) {
      opener.Close(l);
    }
  }
  sel.status = IM_ERR_NO_ENCODER;
  sel.token = forced ? abi::token::kForcedFailed : abi::token::kLayerFailed;
  sel.failed_layer = failed;
  sel.layers = {};
  return sel;
}

Recovery RecoverLayer(std::span<const Candidate> candidates, std::size_t current, uint32_t layer, uint32_t profile,
                      Failure why, im_status cause, Opener &opener) {
  Recovery r;
  if (current >= candidates.size()) {
    return r; // kExhausted
  }
  const uint32_t old_kind = candidates[current].kind;
  r.candidate = current;
  r.kind = old_kind;
  switch (why) {
  case Failure::kEncodeError: {
    const im_status st = opener.Open(layer, candidates[current], profile);
    if (st == IM_OK) {
      r.outcome = Recovery::Outcome::kSame;
      return r;
    }
    r.reason = st == IM_ERR_LIMIT ? abi::token::kSessionLimit : std::string_view("encode_error");
    break;
  }
  case Failure::kBadStream:
    r.reason = "encode_error";
    break;
  case Failure::kReopenFailed:
    r.reason = cause == IM_ERR_LIMIT ? abi::token::kSessionLimit : abi::token::kOpenFailed;
    break;
  }
  for (std::size_t i = current + 1; i < candidates.size(); ++i) {
    if (!Supports(candidates[i], profile)) {
      continue;
    }
    if (opener.Open(layer, candidates[i], profile) == IM_OK) {
      r.outcome = Recovery::Outcome::kChanged;
      r.candidate = i;
      r.kind = candidates[i].kind;
      r.kind_changed = r.kind != old_kind;
      return r;
    }
  }
  r.outcome = Recovery::Outcome::kExhausted;
  return r;
}

} // namespace im::encpolicy
