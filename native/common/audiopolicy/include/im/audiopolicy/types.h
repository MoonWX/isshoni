// The vocabulary of the audio exclusion policy (docs/m2/05-windows-audio.md §5, §6, §12.1): process keys, verdicts,
// reasons, engines and the names the report JSON uses for them.
//
// im_audiopolicy is portable C++20 without OS headers (05 §2 "Policy code"): the Windows watcher (native/windows/
// src/audio/) feeds it process snapshots and session lists and acts on its plans; the library itself opens nothing,
// logs nothing and keeps no global state, so every object may live on any one thread (the watcher's).
#pragma once

#include <compare>
#include <cstddef>
#include <cstdint>
#include <string_view>

namespace im::audiopolicy {

// A process key (05 §5.1): the pid and the creation time as a FILETIME (100 ns units since 1601), 0 when the process
// couldn't be opened. Every map of the policy is keyed by process key, never by pid alone.
struct ProcessKey {
  uint32_t pid = 0;
  uint64_t created = 0;

  friend constexpr bool operator==(const ProcessKey &, const ProcessKey &) = default;
  friend constexpr auto operator<=>(const ProcessKey &, const ProcessKey &) = default;
};

struct ProcessKeyHash {
  std::size_t operator()(const ProcessKey &k) const noexcept {
    const uint64_t h = (k.created * 0x9E3779B97F4A7C15ull) ^ (uint64_t{k.pid} * 0xC2B2AE3D27D4EB4Full);
    return static_cast<std::size_t>(h ^ (h >> 29));
  }
};

// apps[].verdict and sessions[].verdict (05 §12.1).
enum class Verdict : uint8_t {
  kHeard,       // captured (or, while no capture runs, would be)
  kKeptOut,     // rows 1-4 and 6 of 05 §5.3
  kNotCaptured, // row 7 and the planner's reasons (05 §6.2)
  kPending,     // the session's process is not in the snapshot yet: never captured
};

// apps[].reason (05 §12.1). Kept out: builtin, user, mic, viewer, isshoni. Not captured: launcher, tree, pid_reuse,
// limit, capture_failed. Heard and pending: none ("").
enum class Reason : uint8_t {
  kNone,
  kBuiltin,
  kUser,
  kMic,
  kViewer,
  kIsshoni,
  kLauncher,
  kTree,
  kPidReuse,
  kLimit,
  kCaptureFailed,
};

// The report's "engine" (05 §6.1).
enum class Engine : uint8_t {
  kNone,        // mode off (or a mode without capture): no stream
  kEndpoint,    // classic loopback of the default render endpoint: nothing is kept out (probe failed)
  kExcludeSelf, // the fast path: one EXCLUDE stream on the tree of R (05 §6.4)
  kIncludeSet,  // one INCLUDE stream per allowed target, mixed (05 §6.2)
};

// sessions[].note (05 §12.1).
enum class Note : uint8_t {
  kNone,
  kCovered,  // captured by an ancestor's stream (05 §6.2 step 3)
  kNoHandle, // an include target running without a held process handle (05 §5.7)
};

enum class Flow : uint8_t { kRender, kCapture };

enum class SessionState : uint8_t { kInactive, kActive }; // expired sessions are never passed in (05 §7.2)

// The streams a capture engine holds (05 §8): one per include target, or the one exclusive stream.
enum class StreamKind : uint8_t {
  kInclude,  // PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE on the target
  kExclude,  // PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE on R (the fast path)
  kEndpoint, // the default render endpoint's loopback
};

// Why a live stream has to go (Plan::remove). The watcher fades each one out over 10 ms of its buffer (05 §9.4).
enum class RemovalReason : uint8_t {
  kExited,      // its process is gone from the snapshot
  kKeyMismatch, // the pid is there with another creation time (or, unknown, another exe or parent): a reused PID
  kKeptOut,     // the target's own app became kept out
  kLauncher,    // the target is a launcher (I2)
  kUnsafe,      // a kept-out process appeared in its OS tree (I1, 05 §6.2 step 6)
  kSessionGone, // its process lives, but it has had no render session for the 3 s grace (S1's 3 polls)
  kCovered,     // an ancestor's stream covers it; fade it when the covering stream runs (05 §6.2 step 3)
  kLimit,       // more live include streams than the cap (never in practice: live streams keep their slots)
  kEngine,      // the engine changed (include set <-> exclude-self <-> endpoint, or off)
  kTarget,      // an exclusive stream whose target is no longer the fast path's R
};

// Per-process warning bits; app rows OR them (05 §12.1 apps[].warnings).
inline constexpr uint32_t kWarnSharedAppUsesMic = 1u << 0;    // "shared_app_uses_mic" (05 §5.8)
inline constexpr uint32_t kWarnSharedAppKeptOut = 1u << 1;    // "shared_app_kept_out" (05 §5.8)
inline constexpr uint32_t kWarnIncludedUsesMic = 1u << 2;     // "included_uses_mic": a row-5 allow beat row 6
inline constexpr uint32_t kWarnMultiProcessSession = 1u << 3; // "multi_process_session" (05 §7.2)

// The report's strings (05 §12.1); "" for kNone.
std::string_view VerdictName(Verdict v) noexcept;
std::string_view ReasonName(Reason r) noexcept;
std::string_view EngineName(Engine e) noexcept;
std::string_view NoteName(Note n) noexcept;
std::string_view FlowName(Flow f) noexcept;
std::string_view RemovalReasonName(RemovalReason r) noexcept; // for counters and tests; never a report field
// The name of one warning bit ("shared_app_uses_mic"), "" for anything else.
std::string_view WarningName(uint32_t bit) noexcept;

} // namespace im::audiopolicy
