// The engine's test hooks (docs/m2/04-windows-engine.md §2 "Test builds", §18.2): IM_ENC_FAKE (fake_encoder.h), fault
// injection, the fake picker and the test pattern's modes. They exist only in IM_TEST_HOOKS builds (the CMake
// presets win-x64-ci and win-x64-asan), never in a shipped DLL: in any other build this header declares nothing and
// the folder's sources compile to nothing.
//
// The hooks are C++ functions, not exports: the Windows tests link the engine's objects (im_engine) and call them
// directly; Go's DLL tests reach IM_ENC_FAKE through im_start (encoder IM_ENC_FAKE, or IM_ENC_AUTO on a test:
// source) and need no hook. Every hook may be called from any thread; Configure, FailOpens and Reset apply to
// sessions started afterwards (and FailOpens also to a running session's next re-open).
#pragma once

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <cstdint>
#include <string>
#include <vector>

#include "isshoni_media.h"
#include "session/sources.h"

namespace im::testhooks {

struct Options {
  // The test pattern of "test:pattern" and of fake picks (session/sources.h): drawn on the GPU, or only its frame
  // times for IM_ENC_FAKE sessions (session/frames.h, session/frame_times.h).
  session::Pattern pattern = session::Pattern::kMoving;
  uint32_t pattern_width = 1920;
  uint32_t pattern_height = 1080;
  uint32_t pattern_hz = 60;
  // Two of every three frames of the moving pattern (k mod 3 ≠ 0) reach the session this long after their grid
  // instants (capture times unchanged; clamped below half an interval): a capture whose frames arrive with jitter.
  int64_t pattern_late_ns = 0;
  // Encoder kinds the catalog lists after IM_ENC_FAKE (in this order), each backed by the fake encoder but reported
  // as its own kind with that kind's traits (hardware, live bitrate, profiles): the next candidates of the fallback
  // cases (04 §9.8). Kinds without traits are ignored.
  std::vector<uint32_t> fallback_kinds;
};

// Sets the options for sessions started from now on.
void Configure(const Options &options);
Options Current();

// Back to the defaults: Options{}, no failing opens, the pick clock's offset 0.
void Reset();

// While fail is set, opening an encoder of kind fails with IM_ERR_NO_ENCODER (open_failed): a start without any
// candidate left (forced_failed, layer_failed), a re-open that fails during a recovery (04 §9.8).
void FailOpens(uint32_t kind, bool fail);
bool OpensFail(uint32_t kind) noexcept;

// The fake picker (04 §18.2's pick-token cases): adds a pick of a test pattern of the given size to the token table,
// as README WA50's picker adds a real one, and returns its token ("pick:<n>"). kind is IM_SOURCE_WINDOW or
// IM_SOURCE_MONITOR (the capture path stays IM_CAPTURE_TEST).
std::string FakePick(uint32_t kind = IM_SOURCE_WINDOW, uint32_t width = 1280, uint32_t height = 720);

// The pick tokens' clock hook: moves the token table's clock forward by ns (expiry is 5 min after the pick).
void AdvancePickClock(int64_t ns) noexcept;
int64_t PickClockOffsetNs() noexcept;

// Faults on a running session, as its encoders would report them (04 §9.8, §18.2): the layer's next encode fails
// (an encode error: the session re-opens the encoder once, then tries the next candidate), or reports a removed
// device (the session ends with ENDED{IM_ERR_ENCODER_LOST}, device_removed). IM_ERR_INVALID_ARG for an unknown
// handle or a layer the session doesn't have.
enum class Fault : uint8_t { kEncodeError, kDeviceRemoved };
im_status Inject(im_session s, uint32_t layer, Fault fault);

// The device a session runs on (an id that grows with every new device; 0 for an unknown handle). Every session opens
// its own (04 §8.1), so a session started after a device removal runs on a new one (04 §9.8).
uint64_t SessionDevice(im_session s);

// Whether a device removal marked the probe's cache stale since the last Reset (04 §9.8).
bool ProbeCacheStale() noexcept;

} // namespace im::testhooks

#endif // IM_TEST_HOOKS
