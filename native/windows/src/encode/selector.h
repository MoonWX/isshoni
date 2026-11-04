// The encoders sessions choose from (docs/m2/04-windows-engine.md §9.1, §9.8, §9.10; "04" below): the selector table
// that maps each IM_ENC_* kind to its open function, and the probe's cached result as session::EncoderEntry values.
//
// - The table: the libavcodec kinds (NVENC, QSV, AMF, MF hardware and software) open through FfmpegEncoder::Open;
//   OpenH264 through OpenOpenH264 (README WA51 replaces openh264_stub.cpp; until then its entry is "disabled").
// - The cache: the last full probe's encoders[] with the LUID of each one's adapter. CatalogEntries hands them to a
//   session in the probe's order: FrameInput::kNv12, the device on the encoder's adapter (a software encoder on the
//   default adapter), the open function of its kind. A session that ended on a removed device marks the cache stale
//   (session::MarkProbeCacheStale, 04 §9.8); CatalogEntries then probes the encoders again before the next start
//   relies on them, and clears the mark. With no full probe yet it runs one as well (lazily, 04 §9.8), so a start
//   never needs im_probe first.
// - The session's encoder catalog (session/encoder.h's EncoderCatalog) lists these after the test hooks' entries
//   (README WA44's wiring of session/encoder_catalog.cpp); the probe's IM_ENC_FAKE entry (test-hooks builds) is left
//   out here, since the catalog adds the fake itself.
//
// Threads: every function here is thread-safe; CatalogEntries may run a probe, so it is called on the engine worker
// (im_start).
#pragma once

#include <cstdint>
#include <memory>
#include <span>
#include <vector>

#include "isshoni_media.h"
#include "session/encoder.h"

namespace im::encode {

// The open function of a kind, or nullptr for a kind the engine can't open (IM_ENC_AUTO, later kinds, IM_ENC_FAKE:
// the test hooks open that one).
session::OpenEncoderFn OpenFunctionFor(uint32_t kind) noexcept;

// Stores a full probe's result (caps.encoders[] and caps.adapters[]' LUIDs) for CatalogEntries.
void StoreProbe(const im_caps &caps) noexcept;

// Forgets the stored probe (im_shutdown, tests): the next CatalogEntries probes again.
void ForgetProbe() noexcept;

// The encoders a session may choose from, in the probe's order (see above). Probes first when nothing is stored or
// the cache is stale.
std::vector<session::EncoderEntry> CatalogEntries();

// OpenH264 (README WA51 fills these in openh264_stub.cpp's place): its probe entry (status IM_ERR_DEPENDENCY
// "disabled" while it is off) and its open function.
void ProbeOpenH264(bool full, im_encoder_info &info) noexcept;
im_status OpenOpenH264(const session::EncoderParams &params, std::unique_ptr<session::VideoEncoder> *out) noexcept;

} // namespace im::encode
