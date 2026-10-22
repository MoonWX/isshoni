// What im_start's source_id resolves to (docs/m2/04-windows-engine.md §7.2, §6.4): pick tokens and listed sources.
//
// Pick tokens ("pick:<n>", 04 §7.2 step 5): single use, a successful im_start consumes one and a failed one leaves it
// valid; a token expires 5 min after its pick (counted from the pick, not from its last use); at most 4 unused tokens
// are kept and a fifth pick evicts the oldest; im_start with a consumed, expired or evicted token is
// IM_ERR_SOURCE_GONE with token pick_expired; im_release_source forgets one at once. The picker (README WA50, the
// capture area) adds its picks here with AddPick and forwards im_release_source to Release; until then only the test
// hooks' fake picker adds picks (src/testhooks/).
//
// Listed sources need IM_START_ALLOW_LISTED_SOURCE: "test:pattern" (the test pattern, 04 §8; this slice's CPU
// pattern, a GPU one in README WA33) and the monitors of im_list_sources ("mon:…", README WA60).
//
// Thread-safe. im_start resolves and consumes on the engine worker, so two starts never race for one token.
#pragma once

#include <cstddef>
#include <cstdint>
#include <deque>
#include <memory>
#include <mutex>
#include <string>
#include <string_view>

#include "isshoni_media.h"

namespace im::session {

inline constexpr std::string_view kTestPatternId = "test:pattern";
inline constexpr int64_t kPickLifetimeNs = 5LL * 60 * 1'000'000'000; // 04 §6.4
inline constexpr std::size_t kMaxPicks = 4;

// How the CPU test pattern behaves (the test hooks choose; a session uses kMoving otherwise).
enum class Pattern : uint8_t {
  kMoving, // a new frame every 1/hz on a grid anchored at epoch_ns (a moving pattern)
  kStatic, // only the first frame, at epoch_ns (a static screen: WGC and DD deliver an initial frame)
  kNone,   // no frame at all (the first-keyframe fallback: black at 300 ms, §6.3)
};

// A resolved source.
struct SourceSpec {
  uint32_t kind = IM_SOURCE_TEST_PATTERN;  // IM_SOURCE_*
  uint32_t capture_path = IM_CAPTURE_TEST; // IM_CAPTURE_*: what the session captures with
  uint32_t width = 1920;                   // physical pixels at pick time
  uint32_t height = 1080;
  uint32_t flags = 0; // IM_SRCF_*
  bool test = true;   // a test source: IM_ENC_FAKE comes first under test hooks (04 §18.2)
  Pattern pattern = Pattern::kMoving;
  uint32_t pattern_hz = 60;
  // The capture area's object for this source (a GraphicsCaptureItem, README WA50); opaque here.
  std::shared_ptr<void> item;
};

class SourceRegistry {
public:
  // Stores spec under a new token "pick:<n>" and returns it; the oldest unused pick goes when kMaxPicks are kept.
  std::string AddPick(SourceSpec spec);

  // Resolves id for im_start: a live pick token, or a listed id when allow_listed. On success *serial is the pick's
  // serial (0 for a listed source), for Consume. Failures set the thread's detail: IM_ERR_SOURCE_GONE (pick_expired)
  // for a pick token that was consumed, expired or evicted; IM_ERR_INVALID_ARG for a listed id without allow_listed
  // or an id of no known form; IM_ERR_NOT_IMPLEMENTED for monitors until README WA60.
  im_status Resolve(std::string_view id, bool allow_listed, SourceSpec *out, uint64_t *serial);

  // A successful im_start used the pick: it can't be used again. No-op for 0 or a pick that is gone.
  void Consume(uint64_t serial) noexcept;

  // im_release_source: forgets an unused pick; other ids are ignored.
  void Release(std::string_view id) noexcept;

  // im_shutdown: forgets every pick.
  void Clear() noexcept;

  // Unused picks kept now (tests).
  std::size_t PickCount();

private:
  struct Pick {
    uint64_t serial = 0;
    int64_t picked_ns = 0;
    SourceSpec spec;
  };
  void ExpireLocked(int64_t now) noexcept;

  std::mutex mu_;
  std::deque<Pick> picks_; // oldest first
  uint64_t next_serial_ = 1;
};

// The process-wide registry (never destroyed, like the engine).
SourceRegistry &Sources() noexcept;

// The registry's clock: the engine clock plus the test hooks' offset (04 §18.2's "clock hook at 5 min after the
// pick"); the engine clock alone in a release build.
int64_t PickClockNs() noexcept;

} // namespace im::session
