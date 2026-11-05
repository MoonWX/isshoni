// The encoders sessions choose from (docs/m2/04-windows-engine.md §9.1, §9.8, §9.10; "04" below): the selector table
// that maps each IM_ENC_* kind to its open function, and the probe's cached result as session::EncoderEntry values.
//
// - The table: the libavcodec kinds (NVENC, QSV, AMF, MF hardware and software) open through FfmpegEncoder::Open;
//   OpenH264 through OpenOpenH264 (openh264_encoder.cpp, README WA51; its entry is "disabled" until the user turns it
//   on, im_set_openh264_path).
// - The cache: the last full probe's encoders[] with the LUID of each one's adapter. CatalogEntries hands them to a
//   session in the probe's order: FrameInput::kNv12, the device on the encoder's adapter (a software encoder on the
//   default adapter), the open function of its kind. A session that ended on a removed device marks the cache stale
//   (session::MarkProbeCacheStale, 04 §9.8); CatalogEntries then probes the encoders again before the next start
//   relies on them, and clears the mark. With no full probe yet it runs one as well (lazily, 04 §9.8), so a start
//   never needs im_probe first.
// - The capture adapter (README WA61): PrimaryAdapterIndex, step 1 of 04 §9.1's order when the source's monitor isn't
//   known.
// - The session's encoder catalog (session/encoder.h's EncoderCatalog) lists these after the test hooks' entries
//   (session/encoder_catalog.cpp, wired by README WF10); the probe's IM_ENC_FAKE entry (test-hooks builds) is left
//   out here, since the catalog adds the fake itself.
//
// Threads: every function here is thread-safe; CatalogEntries may run a probe, so it is called on the engine worker
// (im_start).
#pragma once

#include <cstdint>
#include <memory>
#include <optional>
#include <span>
#include <vector>

#include "isshoni_media.h"
#include "session/encoder.h"

namespace im::encode {

// The open function of a kind, or nullptr for a kind the engine can't open (IM_ENC_AUTO, later kinds, IM_ENC_FAKE:
// the test hooks open that one).
session::OpenEncoderFn OpenFunctionFor(uint32_t kind) noexcept;

// Stores a full probe's result (caps.encoders[] and caps.adapters[]' LUIDs) for CatalogEntries. complete: caps is
// im_probe(IM_PROBE_FULL)'s whole answer (the OS and audio flags too), not ProbeForCatalog's encoders only.
void StoreProbe(const im_caps &caps, bool complete) noexcept;

// The stored full probe when it is still current (same FFmpeg folder, cache not stale), for IM_PROBE_QUICK.
struct StoredCaps {
  im_caps caps{};
  bool complete = false; // see StoreProbe
};
std::optional<StoredCaps> StoredProbe();

// Forgets the stored probe (im_shutdown, tests): the next CatalogEntries probes again.
void ForgetProbe() noexcept;

// The encoders a session may choose from, in the probe's order (see above). Probes first when nothing is stored or
// the cache is stale.
std::vector<session::EncoderEntry> CatalogEntries();

// The capture adapter of 04 §9.1's order (step 1: the vendor encoder, then MF hardware, on it) when the source's
// monitor isn't known (README WA61): the adapter of the primary monitor, which DXGI lists first (EnumAdapters1(0), the
// probe's EnumAdapterByGpuPreference(UNSPECIFIED) order), as an index into the stored probe's adapters[] (the space of
// im_encoder_info.adapter_index). encpolicy::kNoAdapter without a stored probe, or when the first adapter has no
// outputs (every hardware encoder then counts as "another adapter"). A session asks it right after CatalogEntries,
// which stored the probe. A DD source's output adapter is AdapterIndexOfLuid's; WGC's "monitor that holds most of the
// source" isn't reported yet.
uint32_t PrimaryAdapterIndex() noexcept;

// The index in the stored probe's adapters[] of the adapter with luid (im_adapter_info.luid), or encpolicy::kNoAdapter
// when no probe is stored or none has it: the capture adapter of a Desktop Duplication source, its output's adapter
// (capture::OutputAdapterLuid, 04 §7.4, §9.1 step 1).
uint32_t AdapterIndexOfLuid(uint64_t luid) noexcept;

// OpenH264 (openh264_encoder.cpp, README WA51): its probe entry and its open function. The entry is IM_ERR_DEPENDENCY
// while it is off or its file doesn't load (disabled, missing, version, blocked_by_policy); a full probe test-encodes
// it (04 §9.10, with the live-bitrate check) and a quick one (full false) reports the last full probe's result for the
// same library, or IM_OK without IM_ENCF_AVAILABLE when none ran since it was loaded. CatalogEntries and StoredProbe
// read the entry again, so a changed setting takes effect at the next start and quick probe (04 §5.3). deadline_ns
// (engine::os::QpcNs) is the probe's encoder budget (probe.h kEncoderBudgetMs, 04 §9.10 "As built") and matters only
// when full: past it the test encode doesn't start (IM_ERR_TIMEOUT, engine_timeout), and a test encode past it after
// its IDR skips the live-bitrate check (the table's flag kept, logged). kNoProbeDeadline: no budget.
inline constexpr int64_t kNoProbeDeadline = INT64_MAX;
void ProbeOpenH264(bool full, int64_t deadline_ns, im_encoder_info &info) noexcept;
im_status OpenOpenH264(const session::EncoderParams &params, std::unique_ptr<session::VideoEncoder> *out) noexcept;

} // namespace im::encode
