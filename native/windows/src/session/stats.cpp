#include "session/stats.h"

#include <algorithm>
#include <cmath>

#include "im/json/writer.h"

namespace im::session {

void ByteWindow::Add(int64_t t_ns, uint32_t bytes) noexcept {
  if (count_ == kCap) {
    head_ = (head_ + 1) % kCap;
    --count_;
  }
  e_[(head_ + count_) % kCap] = Entry{t_ns, bytes};
  ++count_;
}

uint64_t ByteWindow::BitsPerSecond(int64_t now_ns) const noexcept {
  uint64_t bytes = 0;
  int64_t first = now_ns;
  for (std::size_t i = 0; i < count_; ++i) {
    const Entry &e = e_[(head_ + i) % kCap];
    if (now_ns - e.t > kStatsWindowNs) {
      continue;
    }
    bytes += e.bytes;
    first = std::min(first, e.t);
  }
  if (bytes == 0) {
    return 0;
  }
  // Over the whole window once it is full, over the time covered while the session is younger than that.
  const int64_t span = std::max<int64_t>(now_ns - first, kStatsWindowNs / 4);
  const double seconds = static_cast<double>(std::min(span, kStatsWindowNs)) / 1e9;
  return static_cast<uint64_t>(static_cast<double>(bytes) * 8.0 / seconds);
}

void Samples::Add(int64_t ns) noexcept {
  v_[next_] = ns;
  next_ = (next_ + 1) % kCap;
  count_ = std::min(count_ + 1, kCap);
}

double Samples::PercentileMs(double q) const noexcept {
  if (count_ == 0) {
    return 0;
  }
  std::array<int64_t, kCap> sorted{};
  std::copy_n(v_.begin(), count_, sorted.begin());
  std::sort(sorted.begin(), sorted.begin() + static_cast<std::ptrdiff_t>(count_));
  const double rank = std::clamp(q, 0.0, 100.0) / 100.0 * static_cast<double>(count_ - 1);
  const auto i = static_cast<std::size_t>(std::lround(rank));
  return static_cast<double>(sorted[std::min(i, count_ - 1)]) / 1e6;
}

std::string_view AudioStateName(uint32_t state) noexcept {
  switch (state) {
  case IM_AUDIO_STATE_STARTING:
    return "starting";
  case IM_AUDIO_STATE_OK:
    return "ok";
  case IM_AUDIO_STATE_DEGRADED:
    return "degraded";
  case IM_AUDIO_STATE_NO_DEVICE:
    return "no-device";
  default:
    return "off";
  }
}

std::string_view CapturePathName(uint32_t path) noexcept {
  switch (path) {
  case IM_CAPTURE_WGC:
    return "wgc";
  case IM_CAPTURE_DD:
    return "dd";
  case IM_CAPTURE_TEST:
    return "test";
  case IM_CAPTURE_SCK:
    return "sck";
  case IM_CAPTURE_PORTAL:
    return "portal";
  case IM_CAPTURE_X11:
    return "x11";
  default:
    return "";
  }
}

std::string StatsJson(const StatsSnapshot &s) {
  json::Writer w;
  w.BeginObject().Key("layers").BeginArray();
  for (uint32_t i = 0; i < s.layer_count && i < s.layers.size(); ++i) {
    const LayerStatsSnapshot &l = s.layers[i];
    w.BeginObject();
    const auto rid_len = static_cast<std::size_t>(std::find(l.rid, l.rid + sizeof l.rid, '\0') - l.rid);
    w.Key("rid").String(std::string_view(l.rid, rid_len));
    w.Key("encoder").String(l.encoder);
    w.Key("codec").String(l.codec);
    w.Key("hw").Bool(l.hw);
    w.Key("width").Uint(l.width);
    w.Key("height").Uint(l.height);
    w.Key("active").Bool(l.active);
    w.Key("fpsIn").Fixed(l.fps_in, 1);
    w.Key("fpsOut").Fixed(l.fps_out, 1);
    w.Key("bitrateBps").Uint(l.bitrate_bps);
    w.Key("targetBps").Uint(l.target_bps);
    w.Key("keyframes").Uint(l.keyframes);
    w.Key("refreshes").Uint(l.refreshes);
    w.Key("reinits").Uint(l.reinits);
    w.Key("drops").BeginObject();
    w.Key("pacer").Uint(l.drops_pacer).Key("ring").Uint(l.drops_ring).Key("stale").Uint(l.drops_stale);
    w.EndObject();
    w.Key("encodeMs").BeginObject();
    w.Key("p50").Fixed(l.encode_p50, 1).Key("p95").Fixed(l.encode_p95, 1).Key("p99").Fixed(l.encode_p99, 1);
    w.EndObject();
    w.Key("latencyMs").BeginObject();
    w.Key("p50").Fixed(l.latency_p50, 1).Key("p95").Fixed(l.latency_p95, 1);
    w.EndObject();
    w.Key("limit").String(l.limit);
    w.EndObject();
  }
  w.EndArray();
  w.Key("capture").BeginObject();
  w.Key("path").String(s.capture_path);
  w.Key("sourceWidth").Uint(s.source_width);
  w.Key("sourceHeight").Uint(s.source_height);
  w.Key("frames").Uint(s.frames);
  w.Key("interrupted").Bool(s.interrupted);
  w.EndObject();
  w.Key("audio").BeginObject();
  w.Key("state").String(s.audio_state);
  w.Key("packets").Uint(s.audio_packets);
  w.Key("drops").Uint(s.audio_drops);
  w.Key("bitrateBps").Uint(s.audio_bitrate_bps);
  w.EndObject();
  w.Key("paused").Bool(s.paused);
  w.EndObject();
  return w.Take();
}

} // namespace im::session
