#include "audio/signal.h"

#include <algorithm>
#include <cmath>
#include <numbers>

#include "im/abi/status.h"

namespace im::audio {
namespace {

constexpr uint32_t kFramesPerMs = kSignalRate / 1000;
constexpr uint32_t kWaitMs = 5;            // between buffer writes
constexpr uint32_t kStallMs = 2000;        // the device position stops moving this long: give up
constexpr uint32_t kMaxWriteFrames = 4800; // 100 ms per write at most
constexpr uint32_t kBurstTailMs = 50;      // zeros after the last burst, so it has played when the call returns

double DbToAmp(double db) noexcept { return std::pow(10.0, db / 20.0); }

// The gain of a raised-cosine ramp of `ramp` frames at frame i of a sound of `length` frames: 0 → 1 over the first
// ramp frames, 1 → 0 over the last ones.
double RampGain(uint64_t i, uint64_t length, uint32_t ramp) noexcept {
  if (ramp == 0) {
    return 1.0;
  }
  const uint64_t from_end = length - 1 - i;
  const uint64_t edge = std::min(i, from_end);
  if (edge >= ramp) {
    return 1.0;
  }
  const double x = static_cast<double>(edge) / static_cast<double>(ramp);
  return 0.5 - 0.5 * std::cos(std::numbers::pi * x);
}

// The play loop of PlayBursts and PlayTone: writes frames [0, total) made by gen as the buffer takes them, reads the
// device position after every write (on_pos) and returns once they have played.
template <class Gen, class OnPos>
im_status Play(RenderTarget &t, uint64_t total, Gen &&gen, OnPos &&on_pos, const std::atomic<bool> *abort,
               void (*between)(void *), void *ctx) noexcept {
  std::vector<float> buf;
  try {
    buf.resize(2 * kMaxWriteFrames);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "audio signal: out of memory");
  }
  uint64_t written = 0;
  bool started = false;
  uint64_t last_pos = 0;
  uint32_t still_ms = 0;
  for (;;) {
    if (abort != nullptr && abort->load(std::memory_order_acquire)) {
      return abi::Fail(IM_ERR_CANCELLED, {}, "audio signal: stopped");
    }
    if (written < total) {
      uint32_t n = 0;
      if (const int32_t hr = t.Writable(&n); hr < 0) {
        return abi::Failf(IM_ERR_INTERNAL, {}, "audio signal: the render buffer failed (0x%08X)",
                          static_cast<unsigned>(hr));
      }
      n = static_cast<uint32_t>(std::min<uint64_t>({n, total - written, kMaxWriteFrames}));
      if (n > 0) {
        gen(written, n, buf.data());
        if (const int32_t hr = t.Write(buf.data(), n); hr < 0) {
          return abi::Failf(IM_ERR_INTERNAL, {}, "audio signal: the render write failed (0x%08X)",
                            static_cast<unsigned>(hr));
        }
        written += n;
      }
      if (!started) {
        if (const int32_t hr = t.Start(); hr < 0) {
          return abi::Failf(IM_ERR_INTERNAL, {}, "audio signal: the render client did not start (0x%08X)",
                            static_cast<unsigned>(hr));
        }
        started = true;
      }
    }
    uint64_t pos = 0;
    int64_t qpc = 0;
    const bool have = t.Position(&pos, &qpc);
    if (have) {
      on_pos(pos, qpc);
      if (written >= total && pos >= total) {
        return IM_OK;
      }
    }
    if (between != nullptr) {
      between(ctx);
    }
    t.Wait(kWaitMs);
    if (have && pos != last_pos) {
      last_pos = pos;
      still_ms = 0;
    } else {
      still_ms += kWaitMs;
    }
    if (still_ms >= kStallMs) {
      if (written >= total) {
        return IM_OK; // all written; a device that stops its clock at the end of the data has played it
      }
      return abi::Fail(IM_ERR_TIMEOUT, {}, "audio signal: the render position did not move for 2 s");
    }
  }
}

} // namespace

double ToneLevelDb(const float *mono, std::size_t n, double rate, double hz) noexcept {
  if (mono == nullptr || n < 2 || rate <= 0) {
    return kSignalFloorDb;
  }
  const double coeff = 2.0 * std::cos(2.0 * std::numbers::pi * hz / rate);
  double s1 = 0, s2 = 0, sum_w = 0;
  for (std::size_t i = 0; i < n; ++i) {
    const double w = 0.5 - 0.5 * std::cos(2.0 * std::numbers::pi * static_cast<double>(i) / static_cast<double>(n - 1));
    sum_w += w;
    const double s0 = static_cast<double>(mono[i]) * w + coeff * s1 - s2;
    s2 = s1;
    s1 = s0;
  }
  const double power = s1 * s1 + s2 * s2 - coeff * s1 * s2;
  const double amp = 2.0 * std::sqrt(std::max(power, 0.0)) / sum_w;
  if (amp <= 1e-10) {
    return kSignalFloorDb;
  }
  return 20.0 * std::log10(amp);
}

double MaxWindowLevelDb(const float *mono, std::size_t n, double rate, double hz, std::size_t window,
                        std::size_t hop) noexcept {
  if (window < 2 || hop < 1 || n < window) {
    return kSignalFloorDb;
  }
  double best = kSignalFloorDb;
  for (std::size_t start = 0; start + window <= n; start += hop) {
    best = std::max(best, ToneLevelDb(mono + start, window, rate, hz));
  }
  return best;
}

void AppendMono(const float *stereo, std::size_t frames, std::vector<float> &out) {
  out.reserve(out.size() + frames);
  for (std::size_t i = 0; i < frames; ++i) {
    out.push_back(0.5f * (stereo[2 * i] + stereo[2 * i + 1]));
  }
}

void ToneFrames(const Tone &tone, uint64_t first, uint32_t n, float *out) noexcept {
  const double w1 = 2.0 * std::numbers::pi * tone.hz / kSignalRate;
  const double w2 = 2.0 * std::numbers::pi * tone.hz2 / kSignalRate;
  for (uint32_t k = 0; k < n; ++k) {
    const uint64_t i = first + k;
    float v = 0.0f;
    if (i < tone.length) {
      const double t = static_cast<double>(i);
      double s = std::sin(w1 * t);
      if (tone.hz2 > 0) {
        s += std::sin(w2 * t);
      }
      v = static_cast<float>(tone.amp * s * RampGain(i, tone.length, tone.ramp));
    }
    out[2 * k] = v;
    out[2 * k + 1] = v;
  }
}

Tone ProbeTone() noexcept {
  Tone t;
  t.hz = 997;
  t.amp = DbToAmp(-60.0);
  t.length = 200 * kFramesPerMs;
  t.ramp = 10 * kFramesPerMs;
  return t;
}

Tone Chime() noexcept {
  Tone t;
  t.hz = 880;
  t.hz2 = 1320;
  t.amp = DbToAmp(-14.0) / 2.0; // the two partials' peaks add up to -14 dBFS
  t.length = 800 * kFramesPerMs;
  t.ramp = 10 * kFramesPerMs;
  return t;
}

ProbeLevel ClassifyProbeLevel(double level_db) noexcept {
  if (level_db >= -66.0 && level_db <= -54.0) {
    return ProbeLevel::kOk;
  }
  if (level_db < -80.0) {
    return ProbeLevel::kSilent;
  }
  return ProbeLevel::kMismatch;
}

int64_t PredictFrameNs(uint64_t f, uint64_t pos, int64_t qpc_ns) noexcept {
  // d x 1e9 would overflow int64 for frames more than about 53 h away: whole seconds and the rest apart (the same
  // result, both parts truncated toward zero with the same sign).
  constexpr int64_t kRate = kSignalRate;
  const int64_t d = static_cast<int64_t>(f) - static_cast<int64_t>(pos);
  return qpc_ns + d / kRate * 1'000'000'000 + d % kRate * 1'000'000'000 / kRate;
}

int32_t PlayBursts(RenderTarget &target, const Bursts &b, int64_t *onset_ns, int32_t cap,
                   const std::atomic<bool> *abort) noexcept {
  if (b.count == 0 || b.burst_ms == 0 || b.period_ms < b.burst_ms) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "audio bursts: count, burst_ms or period_ms out of range");
  }
  const uint64_t lead = uint64_t{b.lead_ms} * kFramesPerMs;
  const uint64_t period = uint64_t{b.period_ms} * kFramesPerMs;
  Tone burst;
  burst.hz = b.hz;
  burst.amp = b.amp;
  burst.length = uint64_t{b.burst_ms} * kFramesPerMs;
  burst.ramp = static_cast<uint32_t>(std::min<uint64_t>(kFramesPerMs, burst.length / 2)); // 1 ms ramps
  const uint64_t total = lead + (b.count - 1) * period + burst.length + kBurstTailMs * kFramesPerMs;
  const uint32_t want = static_cast<uint32_t>(std::min<int64_t>(b.count, std::max(cap, 0)));
  std::vector<int64_t> onset;
  std::vector<uint8_t> have;
  try {
    onset.assign(b.count, 0);
    have.assign(b.count, 0);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "audio bursts: out of memory");
  }
  const auto first_of = [&](uint32_t k) { return lead + k * period; };
  const auto gen = [&](uint64_t from, uint32_t n, float *out) {
    for (uint32_t i = 0; i < n; ++i) {
      const uint64_t f = from + i;
      float v = 0.0f;
      if (f >= lead) {
        const uint64_t rel = (f - lead) % period;
        const uint64_t k = (f - lead) / period;
        if (k < b.count && rel < burst.length) {
          ToneFrames(burst, rel, 1, out + 2 * i);
          continue;
        }
      }
      out[2 * i] = v;
      out[2 * i + 1] = v;
    }
  };
  uint32_t next = 0; // the first burst whose first frame may still be ahead of the position
  const auto on_pos = [&](uint64_t pos, int64_t qpc) {
    for (uint32_t k = next; k < b.count; ++k) {
      const uint64_t f = first_of(k);
      if (f > pos + kSignalRate) {
        break; // more than 1 s ahead (and so are the rest): a later sample, nearer to it, predicts it
      }
      if (pos <= f) {
        onset[k] = PredictFrameNs(f, pos, qpc); // the last sample before its first frame plays
        have[k] = 1;
      } else {
        if (!have[k]) {
          onset[k] = PredictFrameNs(f, pos, qpc); // no earlier sample (a slow first read): the nearest one
          have[k] = 1;
        }
        next = k + 1;
      }
    }
  };
  if (const im_status st = Play(target, total, gen, on_pos, abort, nullptr, nullptr); st != IM_OK) {
    return st;
  }
  for (uint32_t k = 0; k < want; ++k) {
    onset_ns[k] = onset[k];
  }
  return static_cast<int32_t>(want);
}

im_status PlayTone(RenderTarget &target, const Tone &tone, uint32_t lead_frames, uint32_t tail_frames,
                   const std::atomic<bool> *abort, void (*between)(void *ctx), void *ctx) noexcept {
  const uint64_t total = lead_frames + tone.length + tail_frames;
  const auto gen = [&](uint64_t from, uint32_t n, float *out) {
    for (uint32_t i = 0; i < n; ++i) {
      const uint64_t f = from + i;
      if (f < lead_frames) {
        out[2 * i] = 0.0f;
        out[2 * i + 1] = 0.0f;
      } else {
        ToneFrames(tone, f - lead_frames, 1, out + 2 * i);
      }
    }
  };
  return Play(target, total, gen, [](uint64_t, int64_t) {}, abort, between, ctx);
}

} // namespace im::audio
