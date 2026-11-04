// im_probe (docs/m2/04-windows-engine.md §9.10; "04" below), on the engine worker. encode.h's Probe runs the steps:
//   1. the OS: RtlGetVersion's build, the UBR from the registry, and the IM_OS_* flags (from the build where only
//      C++/WinRT could tell more: WGC's IsSupported and its properties; the borderless access request is README
//      WA50's and stays clear until it lands; a trial DuplicateOutput1 per output; MFStartup through the System32-only
//      delay-load hook; advanced color per active path);
//   2. the adapters: IDXGIFactory6::EnumAdapterByGpuPreference (EnumAdapters1 without it), up to IM_MAX_ADAPTERS,
//      with vendor, device, the UMD version (CheckInterfaceSupport) and whether one has outputs;
//   3. the encoders in 04 §9.1's order (per hardware adapter: its vendor encoder, then MF hardware; then MF software,
//      then OpenH264; test-hooks builds add IM_ENC_FAKE last). IM_PROBE_QUICK only says which ones the FFmpeg build
//      has (no test encode, no IM_ENCF_AVAILABLE). IM_PROBE_FULL opens each one at 1920×1080 on a device on its
//      adapter, encodes one mid-gray NV12 frame as an IDR and checks for SPS, PPS and an IDR slice; records the wall
//      time of open + IDR + close (test_encode_us) and the flags (zero copy from the open, High and CB from trial
//      opens, live bitrate and forced IDR from 04 §9.1's table); then, for an encoder the table marks live, the
//      live-bitrate check: two P frames, a 50 % bitrate change as 04 §9.5 applies it, two more frames; an IDR among
//      those two clears IM_ENCF_LIVE_BITRATE (logged at info, "live_bitrate_idr"). An open over 1 s marks the
//      encoder unavailable (slow_open); every unavailable encoder carries its status and 04 §5.7 token;
//   4. the audio flags: 05's AudioEngine::ProbeFlags through audio_bridge::Engine() (it posts to 05's watcher).
// A full probe's encoders are stored for the sessions (selector.h).
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::encode {

// Steps 2 and 3 of a full probe only, into caps (adapters and encoders): what CatalogEntries runs before a start
// when no full probe is stored or the cache is stale.
im_status ProbeForCatalog(im_caps &caps) noexcept;

// The longest an encoder's open may take before the probe marks it unavailable (token slow_open).
inline constexpr uint32_t kSlowOpenUs = 1'000'000;
// The test encode's size and rate.
inline constexpr uint32_t kTestWidth = 1920;
inline constexpr uint32_t kTestHeight = 1080;
inline constexpr uint32_t kTestFps = 60;
inline constexpr uint32_t kTestBitrate = 8'000'000;
// After this much probe time, the encoders not yet tested are reported as timed out (IM_ERR_TIMEOUT, engine_timeout)
// instead of opened, so im_probe answers within its bound (04 §5.3: 2 s, posted with 1 s of slack).
inline constexpr uint32_t kEncoderBudgetMs = 2500;

} // namespace im::encode
