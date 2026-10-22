#include "session/sources.h"

#include <charconv>
#include <utility>

#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"
#include "testhooks/testhooks.h"

namespace im::session {
namespace {

constexpr std::string_view kPickPrefix = "pick:";
constexpr std::string_view kTestPrefix = "test:";
constexpr std::string_view kMonitorPrefix = "mon:";

bool StartsWith(std::string_view s, std::string_view prefix) noexcept { return s.substr(0, prefix.size()) == prefix; }

// The serial of "pick:<n>", or 0 when id is not of that form.
uint64_t PickSerial(std::string_view id) noexcept {
  if (!StartsWith(id, kPickPrefix)) {
    return 0;
  }
  const std::string_view digits = id.substr(kPickPrefix.size());
  uint64_t n = 0;
  const auto [end, ec] = std::from_chars(digits.data(), digits.data() + digits.size(), n);
  if (ec != std::errc() || end != digits.data() + digits.size() || digits.empty() || digits[0] == '0') {
    return 0;
  }
  return n;
}

} // namespace

int64_t PickClockNs() noexcept {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  return engine::os::QpcNs() + testhooks::PickClockOffsetNs();
#else
  return engine::os::QpcNs();
#endif
}

void SourceRegistry::ExpireLocked(int64_t now) noexcept {
  while (!picks_.empty() && now - picks_.front().picked_ns >= kPickLifetimeNs) {
    picks_.pop_front(); // picks are kept in pick order: the oldest expires first
  }
}

std::string SourceRegistry::AddPick(SourceSpec spec) {
  const int64_t now = PickClockNs();
  std::lock_guard<std::mutex> lock(mu_);
  ExpireLocked(now);
  while (picks_.size() >= kMaxPicks) {
    picks_.pop_front(); // a fifth pick evicts the oldest unused one (04 §7.2)
  }
  Pick p;
  p.serial = next_serial_++;
  p.picked_ns = now;
  p.spec = std::move(spec);
  picks_.push_back(std::move(p));
  return std::string(kPickPrefix) + std::to_string(picks_.back().serial);
}

im_status SourceRegistry::Resolve(std::string_view id, bool allow_listed, SourceSpec *out, uint64_t *serial) {
  *serial = 0;
  if (const uint64_t n = PickSerial(id); n != 0) {
    const int64_t now = PickClockNs();
    std::lock_guard<std::mutex> lock(mu_);
    ExpireLocked(now);
    for (const Pick &p : picks_) {
      if (p.serial == n) {
        *out = p.spec;
        *serial = n;
        return IM_OK;
      }
    }
    return abi::Failf(IM_ERR_SOURCE_GONE, abi::token::kPickExpired,
                      "im_start: the pick token was used, expired (5 min) or evicted (4 kept)");
  }
  if (StartsWith(id, kTestPrefix) || StartsWith(id, kMonitorPrefix)) {
    if (!allow_listed) {
      return abi::Fail(IM_ERR_INVALID_ARG, {},
                       "im_start: a listed source needs IM_START_ALLOW_LISTED_SOURCE (the app passes picks only)");
    }
    if (StartsWith(id, kMonitorPrefix)) {
      return abi::Fail(IM_ERR_NOT_IMPLEMENTED, {}, "im_start: monitor capture is not implemented yet (README WA60)");
    }
    if (id != kTestPatternId) {
      return abi::Fail(IM_ERR_SOURCE_GONE, {}, "im_start: unknown test source (only test:pattern)");
    }
    SourceSpec spec;
    spec.kind = IM_SOURCE_TEST_PATTERN;
    spec.capture_path = IM_CAPTURE_TEST;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    const testhooks::Options o = testhooks::Current();
    spec.width = o.pattern_width;
    spec.height = o.pattern_height;
    spec.pattern = o.pattern;
    spec.pattern_hz = o.pattern_hz;
#endif
    *out = std::move(spec);
    return IM_OK;
  }
  return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_start: source_id is neither a pick token nor a listed source");
}

void SourceRegistry::Consume(uint64_t serial) noexcept {
  if (serial == 0) {
    return;
  }
  std::lock_guard<std::mutex> lock(mu_);
  for (auto it = picks_.begin(); it != picks_.end(); ++it) {
    if (it->serial == serial) {
      picks_.erase(it);
      return;
    }
  }
}

void SourceRegistry::Release(std::string_view id) noexcept {
  const uint64_t n = PickSerial(id);
  if (n != 0) {
    Consume(n);
  }
}

void SourceRegistry::Clear() noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  picks_.clear();
}

std::size_t SourceRegistry::PickCount() {
  const int64_t now = PickClockNs();
  std::lock_guard<std::mutex> lock(mu_);
  ExpireLocked(now);
  return picks_.size();
}

SourceRegistry &Sources() noexcept {
  static SourceRegistry *const r = new SourceRegistry(); // never destroyed, like the engine
  return *r;
}

} // namespace im::session
