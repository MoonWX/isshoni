#include "im/pace/pacer.h"

#include <algorithm>

namespace im::pace {

std::string_view LimitToken(Limit limit) noexcept {
  switch (limit) {
  case Limit::kCpu:
    return "cpu";
  case Limit::kGpu:
    return "gpu";
  case Limit::kNone:
    break;
  }
  return "none";
}

Pacer::Pacer(int64_t epoch_ns, std::span<const LayerConfig> layers) noexcept : epoch_(epoch_ns) {
  layer_count_ = static_cast<uint32_t>(std::min<std::size_t>(layers.size(), kMaxLayers));
  for (uint32_t i = 0; i < layer_count_; ++i) {
    Configure(layers_[i], layers[i], i);
    layers_[i].measure_since = epoch_ns;
    layers_[i].stats.active = layers[i].active;
  }
}

void Pacer::Configure(Layer &l, const LayerConfig &cfg, uint32_t index) noexcept {
  l.cfg = cfg;
  const uint32_t fps = std::clamp<uint32_t>(cfg.fps, 1, 240);
  l.cfg.fps = fps;
  l.interval = 1'000'000'000 / fps;
  l.gop_ns = int64_t{cfg.gop_ms == 0 ? DefaultGopMs(index) : cfg.gop_ms} * 1'000'000;
}

template <class Fn> void Pacer::ForLayers(uint32_t layer, Fn &&fn) noexcept {
  if (layer == IM_ALL_LAYERS) {
    for (uint32_t i = 0; i < layer_count_; ++i) {
      fn(layers_[i]);
    }
  } else if (layer < layer_count_) {
    fn(layers_[layer]);
  }
}

void Pacer::OnFrame(int64_t capture_ts_ns, int64_t now_ns, uint32_t skipped) noexcept {
  if (!paused_) {
    for (uint32_t i = 0; i < layer_count_; ++i) {
      Layer &l = layers_[i];
      // Frames go unencoded although the layer is due: the encoder fell behind (thinning a faster source to the
      // target happens while the layer isn't due and isn't counted).
      if (l.cfg.active && l.encoded_any && now_ns >= l.next_due - kToleranceNs) {
        l.stats.pacer_drops += skipped + (l.encoded_frame < latest_frame_ ? 1u : 0u);
      }
    }
  }
  ++latest_frame_;
  latest_ts_ = capture_ts_ns;
  latest_arrival_ = now_ns;
  for (uint32_t i = 0; i <= skipped && i < 1024; ++i) {
    in_meter_.Add(now_ns); // fpsIn counts every captured frame, seen or not
  }
}

void Pacer::RequestKeyframe(uint32_t layer) noexcept {
  ForLayers(layer, [](Layer &l) { l.idr_requested = true; });
}

void Pacer::RequireKeyframe(uint32_t layer) noexcept {
  ForLayers(layer, [](Layer &l) { l.must_idr = true; });
}

void Pacer::ResetMeasure(Layer &l, int64_t now) noexcept {
  l.measure_since = now;
  l.below_since = kNever;
  l.above_since = kNever;
}

void Pacer::Reopen(uint32_t layer, int64_t now_ns) noexcept {
  ForLayers(layer, [&](Layer &l) {
    l.next_due = kMinusInf;
    l.must_idr = true;
    ResetMeasure(l, now_ns);
  });
}

void Pacer::SetFps(uint32_t layer, uint32_t fps, int64_t now_ns) noexcept {
  if (layer >= layer_count_) {
    return;
  }
  LayerConfig cfg = layers_[layer].cfg;
  cfg.fps = fps;
  Configure(layers_[layer], cfg, layer);
  ResetMeasure(layers_[layer], now_ns);
}

void Pacer::SetGop(uint32_t layer, uint32_t gop_ms) noexcept {
  if (layer >= layer_count_) {
    return;
  }
  LayerConfig cfg = layers_[layer].cfg;
  cfg.gop_ms = gop_ms;
  Configure(layers_[layer], cfg, layer);
}

void Pacer::SetLayerActive(uint32_t layer, bool active, int64_t now_ns) noexcept {
  if (layer >= layer_count_) {
    return;
  }
  Layer &l = layers_[layer];
  if (active && !l.cfg.active) {
    l.next_due = kMinusInf;
    l.must_idr = true; // the first frame after reactivation is an IDR
    ResetMeasure(l, now_ns);
  }
  l.cfg.active = active;
  l.stats.active = active;
}

void Pacer::SetPaused(bool on, int64_t now_ns) noexcept {
  if (on == paused_) {
    return;
  }
  paused_ = on;
  for (uint32_t i = 0; i < layer_count_; ++i) {
    ResetMeasure(layers_[i], now_ns);
    if (!on) {
      layers_[i].next_due = kMinusInf; // resume: the newest captured frame is encoded at once
    }
  }
}

void Pacer::SetHardware(uint32_t layer, bool hardware) noexcept {
  if (layer < layer_count_) {
    layers_[layer].cfg.hardware = hardware;
  }
}

int64_t Pacer::Stamp(Layer &l, int64_t ts) const noexcept {
  ts = std::max(ts, epoch_);
  if (ts <= l.last_ts) {
    ts = l.last_ts + kStampStepNs;
  }
  return ts;
}

bool Pacer::Decide(uint32_t index, int64_t now, Encode *e) noexcept {
  Layer &l = layers_[index];
  if (!l.cfg.active) {
    return false;
  }
  const bool due = now >= l.next_due - kToleranceNs;
  const bool new_frame = !paused_ && latest_frame_ > l.encoded_frame;
  const bool idr_wanted = l.must_idr || (l.idr_requested && now >= l.last_idr + kIdrSpacingNs);
  const int64_t interval0 = layers_[0].interval;
  // A frame that waited this long is a static picture: stamped now, like a refresh.
  const int64_t frame_ts = now - latest_ts_ > kStaticFrameNs ? now : latest_ts_;

  *e = Encode{};
  e->layer = index;
  if (new_frame && due) {
    e->input = Input::kLatest;
    e->frame = latest_frame_;
    e->idr = idr_wanted || now - l.last_idr >= l.gop_ns; // a new frame is a content change: the time-based GOP
    e->ts_ns = frame_ts;
    return true;
  }
  // A wanted IDR rides on the next new frame (branch above, when due). It goes without one when the source is static
  // (no new frame for one interval of layer 0): the waiting frame of a layer that isn't due yet, or the last frame
  // again; while paused (frames keep arriving but are ignored), the last frame as soon as the layer is due.
  const bool static_source = now - latest_arrival_ >= interval0;
  if (idr_wanted && (new_frame || l.encoded_any) && ((new_frame || !paused_) ? static_source : due)) {
    e->idr = true;
    if (new_frame) {
      e->input = Input::kLatest;
      e->frame = latest_frame_;
      e->ts_ns = frame_ts;
    } else {
      e->input = Input::kLast;
      e->frame = l.encoded_frame;
      e->ts_ns = now;
    }
    return true;
  }
  if (!new_frame && l.encoded_any && now >= l.refresh_at) {
    e->input = Input::kLast;
    e->frame = l.encoded_frame;
    e->refresh = true;
    e->ts_ns = now;
    return true;
  }
  // First keyframe: nothing captured 300 ms after the start (or a paused start, which may show nothing): black.
  if (!l.encoded_any && !new_frame && (paused_ || (latest_frame_ == 0 && now >= epoch_ + kFirstFrameFallbackNs))) {
    e->input = Input::kLast;
    e->frame = 0;
    e->idr = true;
    e->ts_ns = now;
    return true;
  }
  return false;
}

void Pacer::Apply(Layer &l, const Encode &e, int64_t now) noexcept {
  // advance(l): the nominal schedule, with the resync when more than one interval behind.
  if (now - l.next_due > l.interval) {
    l.next_due = now + l.interval;
  } else {
    l.next_due += l.interval;
  }
  if (e.input == Input::kLatest) {
    l.encoded_frame = e.frame;
    l.out_meter.Add(now);
    l.refresh_at = now + kRefreshAfterNs;
  } else if (e.refresh) {
    l.refresh_at = now + kRefreshEveryNs;
    ++l.stats.refreshes;
  } else if (e.frame == 0) {
    l.refresh_at = now + kRefreshAfterNs; // black: refreshed like any picture until a real frame arrives
  }
  if (e.idr && e.input == Input::kLast && e.frame != 0) {
    // A static IDR is a full picture: the next refresh comes 200 ms after it at the earliest.
    l.refresh_at = std::max(l.refresh_at, now + kRefreshAfterNs);
  }
  if (e.idr) {
    l.last_idr = now;
    l.must_idr = false;
    l.idr_requested = false;
    ++l.stats.keyframes;
  }
  l.encoded_any = true;
  l.last_ts = e.ts_ns;
  ++l.stats.encodes;
}

void Pacer::PushLimitChange(const LimitChange &c) noexcept {
  if (change_count_ == kMaxLimitChanges) {
    std::move(changes_.begin() + 1, changes_.end(), changes_.begin());
    --change_count_;
  }
  changes_[change_count_++] = c;
}

void Pacer::EvaluateLimit(uint32_t index, int64_t now) noexcept {
  Layer &l = layers_[index];
  if (!l.cfg.active || paused_) {
    l.below_since = kNever;
    l.above_since = kNever;
    return;
  }
  const int64_t from = std::max(now - kRateWindowNs, l.measure_since);
  const int64_t window = now - from;
  if (window < kMinRateWindowNs) {
    return;
  }
  const uint32_t in = in_meter_.CountSince(from);
  const uint32_t out = l.out_meter.CountSince(from);
  if (in < 2) {
    // A static source: no evidence either way.
    l.below_since = kNever;
    l.above_since = kNever;
    return;
  }
  const double seconds = static_cast<double>(window) / 1e9;
  const double expected = std::min(static_cast<double>(l.cfg.fps) * seconds, static_cast<double>(in));
  const double ratio = static_cast<double>(out) / expected;
  const double fps_out = static_cast<double>(out) / seconds;
  if (ratio < kOverloadRatio) {
    l.above_since = kNever;
    if (l.below_since == kNever) {
      l.below_since = now;
    }
    if (l.stats.limit == Limit::kNone && now - l.below_since >= kOverloadAfterNs) {
      l.stats.limit = l.cfg.hardware ? Limit::kGpu : Limit::kCpu;
      PushLimitChange(LimitChange{index, l.stats.limit, fps_out});
    }
  } else if (ratio > kRecoverRatio) {
    l.below_since = kNever;
    if (l.above_since == kNever) {
      l.above_since = now;
    }
    if (l.stats.limit != Limit::kNone && now - l.above_since >= kRecoverAfterNs) {
      l.stats.limit = Limit::kNone;
      PushLimitChange(LimitChange{index, Limit::kNone, fps_out});
    }
  } else {
    l.below_since = kNever;
    l.above_since = kNever;
  }
}

std::size_t Pacer::Poll(int64_t now_ns, std::span<Encode> out) noexcept {
  std::size_t n = 0;
  for (uint32_t i = 0; i < layer_count_; ++i) {
    Encode e;
    if (n < out.size() && Decide(i, now_ns, &e)) {
      e.ts_ns = Stamp(layers_[i], e.ts_ns);
      Apply(layers_[i], e, now_ns);
      out[n++] = e;
    }
    EvaluateLimit(i, now_ns);
  }
  return n;
}

int64_t Pacer::NextWakeNs() const noexcept {
  int64_t wake = kNever;
  const int64_t interval0 = layer_count_ > 0 ? layers_[0].interval : 0;
  for (uint32_t i = 0; i < layer_count_; ++i) {
    const Layer &l = layers_[i];
    if (!l.cfg.active) {
      continue;
    }
    const bool new_frame = !paused_ && latest_frame_ > l.encoded_frame;
    if (new_frame) {
      wake = std::min(wake, l.next_due - kToleranceNs);
    }
    if ((l.must_idr || l.idr_requested) && (new_frame || l.encoded_any)) {
      const int64_t eligible = l.must_idr ? kMinusInf : l.last_idr + kIdrSpacingNs;
      const int64_t timely = (!new_frame && paused_) ? l.next_due - kToleranceNs : latest_arrival_ + interval0;
      wake = std::min(wake, std::max(eligible, timely));
    }
    if (!new_frame && l.encoded_any) {
      wake = std::min(wake, l.refresh_at);
    }
    if (!l.encoded_any && !new_frame) {
      wake = std::min(wake, paused_ ? epoch_ : epoch_ + kFirstFrameFallbackNs);
    }
  }
  return wake;
}

std::size_t Pacer::TakeLimitChanges(std::span<LimitChange> out) noexcept {
  const std::size_t n = std::min(out.size(), change_count_);
  std::copy_n(changes_.begin(), n, out.begin());
  std::move(changes_.begin() + static_cast<std::ptrdiff_t>(n),
            changes_.begin() + static_cast<std::ptrdiff_t>(change_count_), changes_.begin());
  change_count_ -= n;
  return n;
}

LayerStats Pacer::Stats(uint32_t layer, int64_t now_ns) const noexcept {
  if (layer >= layer_count_) {
    return LayerStats{};
  }
  const Layer &l = layers_[layer];
  LayerStats s = l.stats;
  const int64_t from = std::max(now_ns - kRateWindowNs, epoch_);
  const int64_t window = now_ns - from;
  if (window > 0) {
    const double seconds = static_cast<double>(window) / 1e9;
    s.fps_in = static_cast<double>(in_meter_.CountSince(from)) / seconds;
    s.fps_out = static_cast<double>(l.out_meter.CountSince(from)) / seconds;
  }
  return s;
}

} // namespace im::pace
