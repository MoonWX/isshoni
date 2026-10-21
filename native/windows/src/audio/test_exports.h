// The shipped audio test exports' area (docs/m2/05-windows-audio.md §4.1, §13.2, §18; for cmd/isshoni-audiotest):
// im_test_render_tone and im_test_render_bursts render test signals on a render endpoint and capture nothing.
// test_exports_stub.cpp is the skeleton's version; 05 replaces it (README WA53). Arguments arrive checked
// (src/api/exports.cpp); failures set the thread's detail.
#pragma once

#include <cstdint>

#include "isshoni_media.h"

namespace im::audio {

// endpoint_id: an IMMDevice id (UTF-8); NULL or "" = the default render endpoint (eConsole).
im_status TestRenderTone(const char *endpoint_id, float hz, float amp, int32_t ms);

// Returns the number of onsets written to onset_ns (at most cap), or a negative status.
int32_t TestRenderBursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms, int32_t period_ms,
                         int32_t count, int64_t *onset_ns, int32_t cap);

} // namespace im::audio
