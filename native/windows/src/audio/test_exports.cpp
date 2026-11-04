// The shipped audio test exports (docs/m2/05-windows-audio.md §4.1, §13.2; README WA53): im_test_render_tone and
// im_test_render_bursts render on the DLL's own render client (render_win.h) from a DLL thread in COM's MTA, and
// capture nothing. The signals and the onset prediction are signal.h's.
#include "audio/test_exports.h"

#include <algorithm>
#include <cmath>
#include <memory>
#include <string>
#include <string_view>

#include "audio/render_win.h"
#include "audio/signal.h"
#include "im/abi/status.h"
#include "im/abi/utf8.h"

namespace im::audio {
namespace {

constexpr int32_t kMaxToneMs = 600'000;  // 10 minutes
constexpr int32_t kMaxPeriodMs = 60'000; // a burst at most once a minute
constexpr int32_t kMaxBursts = 100'000;
constexpr uint32_t kToneRampMs = 10; // raised-cosine ramps, so a tone starts and stops without a click
constexpr std::size_t kMaxEndpointId = 1024;

im_status CheckSignal(const char *fn, const char *endpoint_id, float hz, float amp) {
  if (!std::isfinite(hz) || hz <= 0.0f || hz >= static_cast<float>(kSignalRate) / 2) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: hz must be above 0 and below 24000", fn);
  }
  if (!std::isfinite(amp) || amp < 0.0f || amp > 1.0f) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: amp must be 0..1", fn);
  }
  if (endpoint_id != nullptr) {
    const std::string_view id(endpoint_id);
    if (id.size() >= kMaxEndpointId || !abi::IsValidUtf8(id)) {
      return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: endpoint_id is too long or not UTF-8", fn);
    }
  }
  return IM_OK;
}

// Opens the render client on endpoint_id ("" or NULL: the default) on the current (MTA) thread.
im_status OpenRender(const char *fn, const char *endpoint_id, std::unique_ptr<WasapiRender> *out, std::string *detail) {
  const std::wstring id = endpoint_id != nullptr ? win::Wide(endpoint_id) : std::wstring();
  const HRESULT hr = WasapiRender::Open(id, out);
  if (SUCCEEDED(hr)) {
    return IM_OK;
  }
  *detail = std::string(fn) + ": the render endpoint did not open (" + win::Hr(hr) + ")";
  if (hr == HRESULT_FROM_WIN32(ERROR_NOT_FOUND)) {
    return IM_ERR_INVALID_ARG; // no such endpoint (or none at all)
  }
  return IM_ERR_INTERNAL;
}

} // namespace

im_status TestRenderTone(const char *endpoint_id, float hz, float amp, int32_t ms) {
  if (const im_status st = CheckSignal("im_test_render_tone", endpoint_id, hz, amp); st != IM_OK) {
    return st;
  }
  if (ms < 1 || ms > kMaxToneMs) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_test_render_tone: ms %d is outside 1..%d", static_cast<int>(ms),
                      static_cast<int>(kMaxToneMs));
  }
  return RunOnMta("isshoni audio test tone", [&](std::string *detail) -> im_status {
    std::unique_ptr<WasapiRender> render;
    if (const im_status st = OpenRender("im_test_render_tone", endpoint_id, &render, detail); st != IM_OK) {
      return st;
    }
    Tone t;
    t.hz = hz;
    t.amp = amp;
    t.length = static_cast<uint64_t>(ms) * (kSignalRate / 1000);
    t.ramp = static_cast<uint32_t>(std::min<uint64_t>(kToneRampMs * (kSignalRate / 1000), t.length / 2));
    const im_status st = PlayTone(*render, t, 0, 0, nullptr);
    if (st != IM_OK) {
      *detail = std::string(abi::LastErrorText());
    }
    return st;
  });
}

int32_t TestRenderBursts(const char *endpoint_id, float hz, float amp, int32_t burst_ms, int32_t period_ms,
                         int32_t count, int64_t *onset_ns, int32_t cap) {
  if (const im_status st = CheckSignal("im_test_render_bursts", endpoint_id, hz, amp); st != IM_OK) {
    return st;
  }
  if (period_ms < 1 || period_ms > kMaxPeriodMs || burst_ms < 1 || burst_ms > period_ms || count < 1 ||
      count > kMaxBursts) {
    return abi::Fail(IM_ERR_INVALID_ARG, {},
                     "im_test_render_bursts: burst_ms, period_ms or count out of range (1 <= burst_ms <= period_ms "
                     "<= 60000, 1 <= count <= 100000)");
  }
  int32_t written = 0;
  const im_status st = RunOnMta("isshoni audio test bursts", [&](std::string *detail) -> im_status {
    std::unique_ptr<WasapiRender> render;
    if (const im_status o = OpenRender("im_test_render_bursts", endpoint_id, &render, detail); o != IM_OK) {
      return o;
    }
    Bursts b;
    b.hz = hz;
    b.amp = amp;
    b.burst_ms = static_cast<uint32_t>(burst_ms);
    b.period_ms = static_cast<uint32_t>(period_ms);
    b.count = static_cast<uint32_t>(count);
    const int32_t n = PlayBursts(*render, b, onset_ns, cap, nullptr);
    if (n < 0) {
      *detail = std::string(abi::LastErrorText());
      return n;
    }
    written = n;
    return IM_OK;
  });
  return st != IM_OK ? st : written;
}

} // namespace im::audio
