// Test helpers of the audiomix tests: S1's click detector, a deterministic random source, test signals, a simulated
// capture stream that writes a CaptureRing on a simulated clock (device drift, packet bursts with jitter), and a sink
// that records what the mixer hands over. Nothing here allocates after construction, so the allocation test can run
// the simulation on the mixer thread.
#pragma once

#include <algorithm>
#include <climits>
#include <cmath>
#include <cstdint>
#include <functional>
#include <memory>
#include <vector>

#include "im/audiomix/capture_ring.h"
#include "im/audiomix/channel.h"
#include "im/audiomix/constants.h"
#include "im/audiomix/mix_engine.h"
#include "im/audiomix/resampler.h"

namespace im::audiomix::test {

inline constexpr double kPi = 3.14159265358979323846;

// S1's click detector (spikes/s1-win-audio/analyze.go clickEvents, the audiotest analyzer's base): a click is a sample
// whose second difference exceeds 0.03 on either channel; detections within 10 ms of each other are one click. A
// -6 dBFS 1 kHz tone's second difference peaks at 0.0086, a step of its size at 0.5. Returns the clicks' frame
// indices.
inline std::vector<uint64_t> ClickFrames(const std::vector<float> &interleaved, double threshold = 0.03) {
  std::vector<uint64_t> clicks;
  const uint64_t frames = interleaved.size() / 2;
  for (uint32_t c = 0; c < 2; ++c) {
    int64_t last = -1;
    for (uint64_t i = 2; i < frames; ++i) {
      const double d2 = std::fabs(static_cast<double>(interleaved[2 * i + c]) - 2.0 * interleaved[2 * (i - 1) + c] +
                                  interleaved[2 * (i - 2) + c]);
      if (d2 > threshold && (last < 0 || static_cast<int64_t>(i) - last > static_cast<int64_t>(kRate / 100))) {
        bool dup = false;
        for (const uint64_t o : clicks) {
          dup = dup || (o > i ? o - i : i - o) < kRate / 100;
        }
        if (!dup) {
          clicks.push_back(i);
        }
        last = static_cast<int64_t>(i);
      }
    }
  }
  return clicks;
}

// splitmix64: the same numbers on every platform (std's distributions are implementation-defined).
class Rng {
public:
  explicit Rng(uint64_t seed) : s_(seed) {}
  uint64_t Next() {
    uint64_t z = (s_ += 0x9E3779B97F4A7C15ull);
    z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9ull;
    z = (z ^ (z >> 27)) * 0x94D049BB133111EBull;
    return z ^ (z >> 31);
  }
  double Uniform(double lo, double hi) {
    return lo + (hi - lo) * static_cast<double>(Next() >> 11) / 9007199254740992.0;
  }
  int64_t UniformInt(int64_t lo, int64_t hi) { // inclusive
    return lo + static_cast<int64_t>(Next() % static_cast<uint64_t>(hi - lo + 1));
  }

private:
  uint64_t s_;
};

// A capture stream's content: Fill writes frames [first, first + n) as interleaved stereo.
class Signal {
public:
  virtual ~Signal() = default;
  virtual void Fill(uint64_t first, uint32_t n, float *out) = 0;
};

// A sine with an integer frequency: exact phase at any frame index, also hours in (one period is tabulated). Both
// channels equal.
class Tone final : public Signal {
public:
  Tone(uint32_t hz, float amplitude) : hz_(hz), amp_(amplitude) {
    uint32_t a = kRate, b = hz;
    while (b != 0) {
      const uint32_t t = a % b;
      a = b;
      b = t;
    }
    period_ = kRate / a; // frames until the phase repeats
    table_.resize(period_);
    for (uint32_t i = 0; i < period_; ++i) {
      const uint64_t p = static_cast<uint64_t>(i) * hz_ % kRate;
      table_[i] = amp_ * static_cast<float>(std::sin(2.0 * kPi * static_cast<double>(p) / kRate));
    }
  }
  float At(uint64_t i) const { return table_[i % period_]; }
  void Fill(uint64_t first, uint32_t n, float *out) override {
    uint64_t pos = first % period_;
    for (uint32_t k = 0; k < n; ++k) {
      out[2 * k] = out[2 * k + 1] = table_[pos];
      if (++pos == period_) {
        pos = 0;
      }
    }
  }

private:
  uint32_t hz_;
  float amp_;
  uint32_t period_ = 1;
  std::vector<float> table_;
};

// Silence everywhere.
class Zeros final : public Signal {
public:
  void Fill(uint64_t, uint32_t n, float *out) override {
    for (uint32_t k = 0; k < 2 * n; ++k) {
      out[k] = 0.0f;
    }
  }
};

// A piecewise signal: up to 16 segments [from, to) in frames, each a tone (hz > 0) or a constant level (hz == 0;
// amplitude 0 is silence); silence outside them. Tone phases count from frame 0, so a tone resumes mid-cycle.
class Segments final : public Signal {
public:
  void Add(uint64_t from, uint64_t to, uint32_t hz, float amplitude) {
    if (n_ < 16) {
      seg_[n_++] = {from, to, hz, amplitude};
    }
  }
  float At(uint64_t i) const {
    for (uint32_t s = 0; s < n_; ++s) {
      if (i >= seg_[s].from && i < seg_[s].to) {
        if (seg_[s].hz == 0) {
          return seg_[s].amp;
        }
        const uint64_t p = (i % kRate) * seg_[s].hz % kRate;
        return seg_[s].amp * static_cast<float>(std::sin(2.0 * kPi * static_cast<double>(p) / kRate));
      }
    }
    return 0.0f;
  }
  void Fill(uint64_t first, uint32_t n, float *out) override {
    for (uint32_t k = 0; k < n; ++k) {
      out[2 * k] = out[2 * k + 1] = At(first + k);
    }
  }

private:
  struct Seg {
    uint64_t from, to;
    uint32_t hz;
    float amp;
  };
  Seg seg_[16] = {};
  uint32_t n_ = 0;
};

// A constant level on both channels (DC): exact sample values through the model resampler.
class Level final : public Signal {
public:
  explicit Level(float v) : v_(v) {}
  void Fill(uint64_t, uint32_t n, float *out) override {
    for (uint32_t k = 0; k < 2 * n; ++k) {
      out[k] = v_;
    }
  }

private:
  float v_;
};

struct ProducerConfig {
  double drift_ppm = 0; // the device clock against QPC: positive = the device runs fast (fills the ring)
  uint32_t packet_frames = kChunkFrames;
  int64_t start_ns = 0;              // QPC time of the stream's first frame
  int64_t latency_ns = 3 * kNsPerMs; // a packet reaches the capture thread this long after its last frame was mixed
  int64_t jitter_ns = 0;             // ... plus or minus up to this (uniform), never before an earlier packet
  int64_t qpc_noise_ns = 0;          // plus or minus up to this on the reported QPC position
  uint64_t seed = 1;
};

// A simulated capture stream: packets of the signal arrive in the ring at their simulated times.
class Producer {
public:
  Producer(CaptureRing *ring, Signal *signal, const ProducerConfig &config)
      : ring_(ring), signal_(signal), c_(config), rng_(config.seed),
        buf_(2 * static_cast<std::size_t>(config.packet_frames)) {
    ScheduleNext();
  }
  // The QPC time at which the audio engine mixed frame i.
  int64_t FrameQpc(uint64_t i) const {
    return c_.start_ns + static_cast<int64_t>(std::llround(static_cast<double>(i) * kNsPerSecond /
                                                           (kRate * (1.0 + c_.drift_ppm * 1e-6))));
  }
  // Writes every packet that has arrived by now_ns.
  void RunUntil(int64_t now_ns) {
    while (enabled_ && next_arrival_ <= now_ns) {
      const uint64_t first = next_frame_;
      signal_->Fill(first, c_.packet_frames, buf_.data());
      int64_t qpc = FrameQpc(first);
      if (c_.qpc_noise_ns > 0) {
        qpc += rng_.UniformInt(-c_.qpc_noise_ns, c_.qpc_noise_ns);
      }
      ring_->Write(buf_.data(), c_.packet_frames, qpc, next_arrival_);
      next_frame_ += c_.packet_frames;
      ScheduleNext();
    }
  }
  // Packets stop arriving (a stalled capture); Resume continues at the device's current position, so the frames of
  // the pause are lost, as when Windows drops them.
  void Pause() { enabled_ = false; }
  void Resume(int64_t now_ns) {
    enabled_ = true;
    while (FrameQpc(next_frame_ + c_.packet_frames) + c_.latency_ns < now_ns) {
      next_frame_ += c_.packet_frames;
    }
    ScheduleNext();
  }
  uint64_t frames() const { return next_frame_; }
  // The next packet's arrival; never while paused.
  int64_t next_arrival() const { return enabled_ ? next_arrival_ : INT64_MAX; }
  bool paused() const { return !enabled_; }

private:
  void ScheduleNext() {
    int64_t a = FrameQpc(next_frame_ + c_.packet_frames) + c_.latency_ns;
    if (c_.jitter_ns > 0) {
      a += rng_.UniformInt(-c_.jitter_ns, c_.jitter_ns);
    }
    next_arrival_ = a > last_arrival_ ? a : last_arrival_;
    last_arrival_ = next_arrival_;
  }

  CaptureRing *ring_;
  Signal *signal_;
  ProducerConfig c_;
  Rng rng_;
  std::vector<float> buf_;
  uint64_t next_frame_ = 0;
  int64_t next_arrival_ = 0;
  int64_t last_arrival_ = INT64_MIN; // a stream playing before time 0 had its packets arrive at their own times
  bool enabled_ = true;
};

// Records what the mixer hands over: every chunk's time, the gaps (as the index of the chunk they precede), and,
// when keep_audio, the samples.
class RecordingSink final : public ChunkSink {
public:
  explicit RecordingSink(bool keep_audio = false, std::size_t reserve_chunks = 0) : keep_(keep_audio) {
    times.reserve(reserve_chunks);
    if (keep_audio) {
      audio.reserve(reserve_chunks * kChunkSamples);
    }
  }
  void OnChunk(const float *frames, int64_t capture_ns) noexcept override {
    if (!fixed || times.size() < times.capacity()) {
      times.push_back(capture_ns);
    }
    if (keep_ && (!fixed || audio.size() + kChunkSamples <= audio.capacity())) {
      audio.insert(audio.end(), frames, frames + kChunkSamples);
    }
    ++chunks;
    bool silent = true;
    for (uint32_t i = 0; i < kChunkSamples && silent; ++i) {
      silent = std::fabs(frames[i]) < kSilenceThreshold;
    }
    if (!silent) {
      ++loud_chunks;
    }
  }
  void OnGap(int64_t capture_ns) noexcept override {
    if (gap_count < 64) {
      gap_at[gap_count] = chunks;
      gap_ns[gap_count] = capture_ns;
    }
    ++gap_count;
  }

  bool keep_ = false;
  bool fixed = false; // never grow (the allocation test reserves first)
  std::vector<int64_t> times;
  std::vector<float> audio;
  uint64_t chunks = 0;
  uint64_t loud_chunks = 0;
  uint64_t gap_count = 0;
  uint64_t gap_at[64] = {};
  int64_t gap_ns[64] = {};
};

// A MixEngine on a simulated clock, fed by simulated capture streams: RunTo advances time from event to event (packet
// arrivals and chunk deadlines), so hours run in seconds and every run is the same.
class Rig {
public:
  struct Stream {
    std::unique_ptr<CaptureRing> ring;
    std::unique_ptr<Producer> producer;
    std::unique_ptr<Channel> channel;
  };

  explicit Rig(bool keep_audio = true, const MixParams &params = {}) : sink(keep_audio), engine(&sink, params) {}

  // A stream of signal (which must outlive the rig) with its own ring and producer, its channel handed to the
  // engine. speex: the production resampler; else the model resampler (speexdsp's consumption, no filter).
  Channel *Add(Signal *signal, const ProducerConfig &pc, bool speex = false, uint8_t group = kGroupInclude,
               bool crossfade = false, uint64_t tag = 0) {
    Stream s;
    s.ring = std::make_unique<CaptureRing>();
    s.producer = std::make_unique<Producer>(s.ring.get(), signal, pc);
    ChannelOptions o;
    o.controller = controller;
    o.ring = s.ring.get();
    o.tag = tag != 0 ? tag : streams.size() + 1;
    o.group = group;
    o.crossfade = crossfade;
    if (!speex) {
      o.resampler = NewModelResampler();
    }
    if (Channel::Create(std::move(o), &s.channel) != IM_OK || !engine.Add(s.channel.get())) {
      return nullptr;
    }
    streams.push_back(std::move(s));
    return streams.back().channel.get();
  }
  Stream &stream(Channel *ch) {
    for (auto &s : streams) {
      if (s.channel.get() == ch) {
        return s;
      }
    }
    return streams.front();
  }

  void Begin(int64_t t) {
    now = t;
    engine.Begin(t);
  }
  // Advances simulated time to t: every packet arrives at its time and every chunk is made when due (packets due at
  // the same time come first). before_tick runs right before each chunk.
  void RunTo(int64_t t) {
    for (;;) {
      int64_t next = engine.next_due_ns();
      for (const auto &s : streams) {
        next = std::min(next, s.producer->next_arrival());
      }
      if (next > t) {
        break;
      }
      now = std::max(now, next);
      for (auto &s : streams) {
        s.producer->RunUntil(now);
      }
      if (engine.next_due_ns() <= now) {
        if (before_tick) {
          before_tick();
        }
        engine.Pump(now);
      }
    }
    now = t;
  }
  void RunFor(int64_t d) { RunTo(now + d); }
  // Pumps the engine at t without letting packets arrive first (a stalled mixer catching up).
  void PumpAt(int64_t t) {
    now = t;
    engine.Pump(t);
  }

  RecordingSink sink;
  MixEngine engine;
  ControllerParams controller; // for the channels Add creates from now on
  std::vector<Stream> streams;
  std::function<void()> before_tick;
  int64_t now = 0;
};

} // namespace im::audiomix::test
