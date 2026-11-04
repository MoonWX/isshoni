#include "encode/selector.h"

#include <algorithm>
#include <mutex>
#include <optional>
#include <string>
#include <utility>

#include "encode/ffmpeg_api.h"
#include "encode/ffmpeg_encoder.h"
#include "encode/probe.h"
#include "im/encpolicy/candidates.h"
#include "im/log/ring.h"
#include "session/session.h"

namespace im::encode {
namespace {

struct Cache {
  std::mutex mu;
  std::optional<im_caps> caps; // the last full probe
  bool complete = false;       // caps came from im_probe(IM_PROBE_FULL), the OS and audio flags included
  std::string ffmpeg_dir;      // the FFmpeg folder it ran with: another im_init's folder needs a new probe
};

Cache &TheCache() noexcept {
  static Cache *const c = new Cache(); // never destroyed, like the engine
  return *c;
}

} // namespace

session::OpenEncoderFn OpenFunctionFor(uint32_t kind) noexcept {
  switch (kind) {
  case IM_ENC_NVENC:
  case IM_ENC_QSV:
  case IM_ENC_AMF:
  case IM_ENC_MF_HW:
  case IM_ENC_MF_SW:
    return &FfmpegEncoder::Open;
  case IM_ENC_OPENH264:
    return &OpenOpenH264;
  default:
    return nullptr;
  }
}

void StoreProbe(const im_caps &caps, bool complete) noexcept {
  Cache &c = TheCache();
  try {
    std::string dir = ffmpeg::DefaultDir();
    std::lock_guard<std::mutex> lock(c.mu);
    c.caps = caps;
    c.complete = complete;
    c.ffmpeg_dir = std::move(dir);
  } catch (...) {
    std::lock_guard<std::mutex> lock(c.mu);
    c.caps.reset(); // out of memory: the next start probes again
  }
}

void ForgetProbe() noexcept {
  Cache &c = TheCache();
  std::lock_guard<std::mutex> lock(c.mu);
  c.caps.reset();
}

std::optional<StoredCaps> StoredProbe() {
  const std::string dir = ffmpeg::DefaultDir();
  Cache &c = TheCache();
  std::lock_guard<std::mutex> lock(c.mu);
  if (!c.caps || session::ProbeCacheStale() || c.ffmpeg_dir != dir) {
    return std::nullopt;
  }
  return StoredCaps{*c.caps, c.complete};
}

std::vector<session::EncoderEntry> CatalogEntries() {
  std::optional<im_caps> caps;
  {
    const std::string dir = ffmpeg::DefaultDir();
    Cache &c = TheCache();
    std::lock_guard<std::mutex> lock(c.mu);
    if (c.caps && !session::ProbeCacheStale() && c.ffmpeg_dir == dir) {
      caps = c.caps;
    }
  }
  if (!caps) {
    // 04 §9.8: after a removed device the adapters (and their LUIDs) may have changed; and a start may come before
    // any im_probe. The encoders are probed again here, on the worker, before this start relies on them.
    im_caps fresh{};
    if (ProbeForCatalog(fresh) == IM_OK) {
      StoreProbe(fresh, false);
      session::ResetProbeCacheStale();
      caps = fresh;
    } else {
      log::Warn("encode", "the encoder probe for a start failed");
      return {};
    }
  }
  std::vector<session::EncoderEntry> out;
  const uint32_t n = std::min<uint32_t>(caps->encoder_count, IM_MAX_ENCODERS);
  out.reserve(n);
  for (uint32_t i = 0; i < n; ++i) {
    const im_encoder_info &info = caps->encoders[i];
    if (info.kind == IM_ENC_FAKE) {
      continue; // the catalog adds the test hooks' fake itself
    }
    session::EncoderEntry e;
    e.info = info;
    e.open = OpenFunctionFor(info.kind);
    e.input = session::FrameInput::kNv12;
    if (info.adapter_index != encpolicy::kNoAdapter && info.adapter_index < caps->adapter_count) {
      e.device.adapter = gpu::DeviceRequest::Adapter::kLuid;
      e.device.luid = caps->adapters[info.adapter_index].luid;
    } else {
      e.device.adapter = gpu::DeviceRequest::Adapter::kDefault; // a software encoder: any device reads back
    }
    out.push_back(e);
  }
  return out;
}

} // namespace im::encode
