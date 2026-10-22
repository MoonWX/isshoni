#include "im/capture/monitor_map.h"

#include <cstddef>

namespace im::capture {

namespace {

bool SizeMatches(const OutputDesc &o, uint32_t w, uint32_t h) noexcept {
  const int64_t ow = int64_t{o.right} - o.left;
  const int64_t oh = int64_t{o.bottom} - o.top;
  return ow == int64_t{w} && oh == int64_t{h};
}

bool NameMatches(const OutputDesc &o, std::string_view name) noexcept {
  if (name.empty()) {
    return false;
  }
  return o.device_name == name || (!o.friendly_name.empty() && o.friendly_name == name);
}

} // namespace

MapResult MapMonitor(uint32_t item_width, uint32_t item_height, std::string_view item_name,
                     std::span<const OutputDesc> outputs) noexcept {
  MapResult r;
  int32_t by_size = -1;
  int32_t by_name = -1;
  for (std::size_t i = 0; i < outputs.size(); ++i) {
    const OutputDesc &o = outputs[i];
    if (!SizeMatches(o, item_width, item_height)) {
      continue; // step 2
    }
    ++r.size_matches;
    by_size = static_cast<int32_t>(i);
    if (NameMatches(o, item_name)) {
      ++r.name_matches;
      by_name = static_cast<int32_t>(i);
    }
  }
  if (r.size_matches == 0) {
    r.outcome = MapResult::Outcome::kNoSizeMatch;
    return r;
  }
  if (r.size_matches == 1) {
    r.outcome = MapResult::Outcome::kBySize;
    r.output = by_size;
    return r;
  }
  if (r.name_matches == 1) {
    r.outcome = MapResult::Outcome::kByName;
    r.output = by_name;
    return r;
  }
  r.outcome = MapResult::Outcome::kAmbiguous;
  return r;
}

} // namespace im::capture
