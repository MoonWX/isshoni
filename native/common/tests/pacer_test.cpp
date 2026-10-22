// The pacer on a virtual clock (docs/m2/04-windows-engine.md §8.4, §8.5, §9.4, §18.1): target 60 with sources at
// 30, 60, 144 Hz, irregular and static; no added latency at or below the target, the newest frame kept above it; the
// rate bound for 75..240 Hz sources, every vsync delivered and quantized by a 1/(2 × fps) MinUpdateInterval, with the
// old `last_encode + interval − 2 ms` rule as a regression guard; no catch-up burst after a static stretch; q at 15
// fps; a keyframe on a static screen within one interval; refresh at 200 ms then every 1 s; pause; inactive layers;
// 250 ms IDR spacing; the time-based GOP; the first keyframe; timestamps; overload → cpu/gpu after 3 s and back to
// none after 10 s.
//
// The simulation is event-driven like the video thread: it wakes at the next frame arrival, action or the pacer's
// NextWakeNs, tells the pacer about the newest frame (and how many it never saw while busy), polls, and stays busy
// for the encodes' simulated cost.
#include <gtest/gtest.h>

#include <algorithm>
#include <cmath>
#include <cstdint>
#include <functional>
#include <limits>
#include <random>
#include <vector>

#include "im/pace/pacer.h"
#include "isshoni_media.h"

namespace {

using im::pace::Encode;
using im::pace::Input;
using im::pace::kNever;
using im::pace::LayerConfig;
using im::pace::Limit;
using im::pace::LimitChange;
using im::pace::Pacer;

constexpr int64_t kMs = 1'000'000;
constexpr int64_t kSec = 1'000'000'000;
constexpr int64_t kEpoch = 1'000 * kSec; // engine clocks don't start at 0

struct Rec {
  int64_t at;
  Encode e;
};

struct Action {
  int64_t at;
  std::function<void(Pacer &, int64_t)> fn;
};

class Sim {
public:
  explicit Sim(std::vector<LayerConfig> layers, int64_t epoch = kEpoch) : pacer(epoch, layers), epoch_(epoch) {}

  // Per-encode busy time of the video thread.
  std::function<int64_t(const Encode &, int64_t now)> cost = [](const Encode &, int64_t) { return int64_t{0}; };

  // Runs until `until` (absolute). frames: arrival = capture times, sorted.
  void Run(const std::vector<int64_t> &frames, std::vector<Action> actions, int64_t until) {
    std::stable_sort(actions.begin(), actions.end(), [](const Action &a, const Action &b) { return a.at < b.at; });
    std::size_t fi = 0;
    std::size_t ai = 0;
    int64_t busy = epoch_;
    for (int guard = 0; guard < 10'000'000; ++guard) {
      int64_t next = pacer.NextWakeNs();
      if (fi < frames.size()) {
        next = std::min(next, frames[fi]);
      }
      if (ai < actions.size()) {
        next = std::min(next, actions[ai].at);
      }
      if (next == kNever || next > until) {
        return;
      }
      const int64_t t = std::max(next, busy);
      if (t > until) {
        return;
      }
      while (ai < actions.size() && actions[ai].at <= t) {
        actions[ai].fn(pacer, t);
        ++ai;
      }
      std::size_t k = 0;
      while (fi + k < frames.size() && frames[fi + k] <= t) {
        ++k;
      }
      if (k > 0) {
        pacer.OnFrame(frames[fi + k - 1], t, static_cast<uint32_t>(k - 1));
        fi += k;
      }
      Encode out[Pacer::kMaxLayers];
      const std::size_t n = pacer.Poll(t, out);
      int64_t c = 0;
      for (std::size_t i = 0; i < n; ++i) {
        recs.push_back(Rec{t, out[i]});
        c += cost(out[i], t);
      }
      busy = t + c;
      std::size_t got = 0;
      LimitChange changes[Pacer::kMaxLimitChanges];
      got = pacer.TakeLimitChanges(changes);
      for (std::size_t i = 0; i < got; ++i) {
        limit_changes.emplace_back(t, changes[i]);
      }
      // After Poll(now), NextWakeNs is later than now: the thread never spins.
      ASSERT_GT(pacer.NextWakeNs(), t) << "at " << (t - epoch_);
    }
    FAIL() << "the simulation didn't finish";
  }

  std::vector<Rec> Layer(uint32_t layer, int64_t from = std::numeric_limits<int64_t>::min(),
                         int64_t to = std::numeric_limits<int64_t>::max()) const {
    std::vector<Rec> r;
    for (const Rec &x : recs) {
      if (x.e.layer == layer && x.at >= from && x.at < to) {
        r.push_back(x);
      }
    }
    return r;
  }

  Pacer pacer;
  std::vector<Rec> recs;
  std::vector<std::pair<int64_t, LimitChange>> limit_changes;

private:
  int64_t epoch_;
};

LayerConfig Layer(uint32_t fps, bool active = true, bool hardware = true) {
  LayerConfig c;
  c.fps = fps;
  c.active = active;
  c.hardware = hardware;
  return c;
}

// Frames at hz from `from` for `seconds`, each displaced by up to ±jitter_ns (deterministic).
std::vector<int64_t> Source(double hz, int64_t from, double seconds, int64_t jitter_ns = 0, uint32_t seed = 1) {
  std::mt19937 rng(seed);
  std::uniform_int_distribution<int64_t> jitter(-jitter_ns, jitter_ns);
  std::vector<int64_t> v;
  const auto count = static_cast<int64_t>(std::floor(hz * seconds));
  for (int64_t k = 0; k < count; ++k) {
    const auto nominal = from + static_cast<int64_t>(std::llround(static_cast<double>(k) * 1e9 / hz));
    v.push_back(nominal + (jitter_ns > 0 ? jitter(rng) : 0));
  }
  std::sort(v.begin(), v.end());
  return v;
}

// WGC with MinUpdateInterval: DWM delivers on the first vsync at least `min_interval` after the previous delivery.
std::vector<int64_t> Quantized(const std::vector<int64_t> &vsyncs, int64_t min_interval) {
  std::vector<int64_t> v;
  for (int64_t t : vsyncs) {
    if (v.empty() || t - v.back() >= min_interval) {
      v.push_back(t);
    }
  }
  return v;
}

// The old rule: encode a pending frame once now >= last_encode + interval - 2 ms (timer there). Returns encodes in
// [from, to).
int OldRuleEncodes(const std::vector<int64_t> &frames, int64_t interval, int64_t from, int64_t to) {
  int64_t last = std::numeric_limits<int64_t>::min() / 4;
  std::size_t fi = 0;
  bool pending = false;
  int count = 0;
  int64_t t = frames.empty() ? to : frames[0];
  while (t < to) {
    while (fi < frames.size() && frames[fi] <= t) {
      pending = true;
      ++fi;
    }
    if (pending && t >= last + interval - 2 * kMs) {
      last = t;
      pending = false;
      if (t >= from) {
        ++count;
      }
    }
    int64_t next = fi < frames.size() ? frames[fi] : to;
    if (pending) {
      next = std::min(next, last + interval - 2 * kMs);
    }
    t = std::max(next, t + 1);
  }
  return count;
}

int Count(const std::vector<Rec> &recs, const std::function<bool(const Rec &)> &pred) {
  return static_cast<int>(std::count_if(recs.begin(), recs.end(), pred));
}

// ------------------------------------------------------------------------------------------------------ cadence

TEST(Pacer, NoAddedLatencyAtOrBelowTheTarget) {
  for (const double hz : {30.0, 50.0, 59.94, 60.0}) {
    Sim sim({Layer(60)});
    const auto frames = Source(hz, kEpoch + 10 * kMs, 10.0, kMs / 2, 7);
    sim.Run(frames, {}, kEpoch + 11 * kSec);
    const auto f = sim.Layer(0);
    std::vector<int64_t> encoded;
    for (const Rec &r : f) {
      if (r.e.input == Input::kLatest) {
        EXPECT_EQ(r.at, r.e.ts_ns) << hz << " Hz: encoded the moment it arrived";
        encoded.push_back(r.at);
      }
    }
    EXPECT_EQ(encoded, frames) << hz << " Hz: no frame lost";
    EXPECT_EQ(sim.pacer.Stats(0, kEpoch + 11 * kSec).pacer_drops, 0u);
  }
}

TEST(Pacer, NewestFrameKeptAboveTheTarget) {
  Sim sim({Layer(60)});
  const auto frames = Source(144.0, kEpoch + 1 * kMs, 5.0, kMs / 2, 3); // all after the epoch
  sim.Run(frames, {}, kEpoch + 5 * kSec);
  for (const Rec &r : sim.Layer(0)) {
    ASSERT_EQ(r.e.input, Input::kLatest);
    // The frame stamped is the newest that had arrived when the encode ran.
    const auto newest = std::upper_bound(frames.begin(), frames.end(), r.at) - 1;
    EXPECT_EQ(r.e.ts_ns, *newest);
  }
}

TEST(Pacer, RateBoundAboveTheTarget) {
  for (const double hz : {75.0, 100.0, 120.0, 144.0, 165.0, 240.0}) {
    const auto vsyncs = Source(hz, kEpoch, 11.0, kMs / 2, static_cast<uint32_t>(hz));
    const int64_t min_interval = kSec / (2 * 60); // MinUpdateInterval = 1 / (2 × fps), §7.3
    for (const bool quantized : {false, true}) {
      const auto frames = quantized ? Quantized(vsyncs, min_interval) : vsyncs;
      Sim sim({Layer(60)});
      sim.Run(frames, {}, kEpoch + 11 * kSec);
      const auto f = sim.Layer(0, kEpoch + 1 * kSec, kEpoch + 11 * kSec);
      const double fps = static_cast<double>(f.size()) / 10.0;
      EXPECT_GE(fps, 59.0) << hz << " Hz, quantized " << quantized;
      EXPECT_LE(fps, 60.5) << hz << " Hz, quantized " << quantized;
      // Regression guard: the old rule restarts the interval from each late encode, ≈ 68 fps.
      const double old_fps = OldRuleEncodes(frames, 1'000'000'000 / 60, kEpoch + 1 * kSec, kEpoch + 11 * kSec) / 10.0;
      EXPECT_GT(old_fps, 62.0) << hz << " Hz, quantized " << quantized << ": the old rule must fail this case";
      EXPECT_EQ(sim.pacer.Stats(0, kEpoch + 11 * kSec).pacer_drops, 0u) << "thinning is not a drop";
    }
  }
}

TEST(Pacer, IrregularSource) {
  // Bursts and gaps: never more than the target in any second, and every frame of a slow stretch is encoded.
  std::vector<int64_t> frames;
  std::mt19937 rng(42);
  std::uniform_int_distribution<int64_t> gap(1 * kMs, 60 * kMs);
  for (int64_t t = kEpoch; t < kEpoch + 10 * kSec; t += gap(rng)) {
    frames.push_back(t);
  }
  Sim sim({Layer(60)});
  sim.Run(frames, {}, kEpoch + 10 * kSec);
  const auto f = sim.Layer(0);
  for (int64_t s = 0; s < 10; ++s) {
    const int n = Count(f, [&](const Rec &r) {
      return r.e.input == Input::kLatest && r.at >= kEpoch + s * kSec && r.at < kEpoch + (s + 1) * kSec;
    });
    EXPECT_LE(n, 62) << "second " << s;
  }
  // A frame that arrives at least one interval after the previous encode is due: encoded on arrival. (One that
  // follows an encode more closely waits for the nominal schedule: the burst is above the target.)
  int checked = 0;
  for (std::size_t i = 1; i < frames.size(); ++i) {
    const auto before = std::find_if(f.rbegin(), f.rend(), [&](const Rec &r) { return r.at < frames[i]; });
    if (before != f.rend() && frames[i] - before->at >= 1'000'000'000 / 60) {
      EXPECT_TRUE(std::any_of(f.begin(), f.end(), [&](const Rec &r) { return r.at == frames[i]; })) << i;
      ++checked;
    }
  }
  EXPECT_GT(checked, 50);
}

TEST(Pacer, NoCatchUpBurstAfterAStaticStretch) {
  auto frames = Source(144.0, kEpoch, 2.0, kMs / 2, 5);
  const auto resumed = Source(144.0, kEpoch + 3 * kSec, 2.0, kMs / 2, 6);
  frames.insert(frames.end(), resumed.begin(), resumed.end());
  Sim sim({Layer(60)});
  sim.Run(frames, {}, kEpoch + 5 * kSec);
  const int first_second = static_cast<int>(sim.Layer(0, kEpoch + 3 * kSec, kEpoch + 4 * kSec).size());
  EXPECT_LE(first_second, 61);
  EXPECT_GE(first_second, 59);
}

TEST(Pacer, PreviewLayerAt15) {
  Sim sim({Layer(60), Layer(15)});
  sim.Run(Source(60.0, kEpoch, 11.0, kMs / 2, 9), {}, kEpoch + 11 * kSec);
  const auto q = sim.Layer(1, kEpoch + 1 * kSec, kEpoch + 11 * kSec);
  EXPECT_GE(q.size(), 149u);
  EXPECT_LE(q.size(), 151u);
  const auto f = sim.Layer(0, kEpoch + 1 * kSec, kEpoch + 11 * kSec);
  EXPECT_GE(f.size(), 595u);
  const auto stats_q = sim.pacer.Stats(1, kEpoch + 11 * kSec);
  EXPECT_NEAR(stats_q.fps_out, 15.0, 0.6);
  EXPECT_NEAR(stats_q.fps_in, 60.0, 0.6);
  EXPECT_EQ(stats_q.pacer_drops, 0u) << "q's thinning is not a drop";
  EXPECT_EQ(sim.pacer.Stats(0, kEpoch + 11 * kSec).pacer_drops, 0u);
}

// ----------------------------------------------------------------------------------------- keyframes, refresh

TEST(Pacer, FirstKeyframe) {
  {
    Sim sim({Layer(60), Layer(15)});
    sim.Run({kEpoch + 50 * kMs, kEpoch + 66 * kMs}, {}, kEpoch + 100 * kMs);
    for (uint32_t l = 0; l < 2; ++l) {
      const auto r = sim.Layer(l);
      ASSERT_FALSE(r.empty());
      EXPECT_EQ(r[0].at, kEpoch + 50 * kMs) << "the first captured frame is encoded at once";
      EXPECT_TRUE(r[0].e.idr);
      EXPECT_EQ(r[0].e.input, Input::kLatest);
    }
  }
  {
    // Nothing captured: black at 300 ms, then the first real frame follows at once (a P frame).
    Sim sim({Layer(60)});
    sim.Run({kEpoch + 400 * kMs}, {}, kEpoch + 450 * kMs);
    const auto r = sim.Layer(0);
    ASSERT_EQ(r.size(), 2u);
    EXPECT_EQ(r[0].at, kEpoch + 300 * kMs);
    EXPECT_TRUE(r[0].e.idr);
    EXPECT_EQ(r[0].e.input, Input::kLast);
    EXPECT_EQ(r[0].e.frame, 0u) << "frame 0: black";
    EXPECT_EQ(r[1].at, kEpoch + 400 * kMs);
    EXPECT_FALSE(r[1].e.idr);
    EXPECT_EQ(r[1].e.input, Input::kLatest);
  }
  {
    // A paused start shows nothing of the capture: black at once.
    Sim sim({Layer(60)});
    sim.pacer.SetPaused(true, kEpoch);
    sim.Run({kEpoch + 10 * kMs}, {}, kEpoch + 100 * kMs);
    const auto r = sim.Layer(0);
    ASSERT_FALSE(r.empty());
    EXPECT_EQ(r[0].at, kEpoch);
    EXPECT_EQ(r[0].e.frame, 0u);
    EXPECT_TRUE(r[0].e.idr);
  }
}

TEST(Pacer, KeyframeOnAStaticScreenWithinOneInterval) {
  Sim sim({Layer(60), Layer(15)});
  const int64_t ask = kEpoch + 2 * kSec + 3 * kMs;
  sim.Run({kEpoch}, {Action{ask, [](Pacer &p, int64_t) { p.RequestKeyframe(IM_ALL_LAYERS); }}}, kEpoch + 3 * kSec);
  for (uint32_t l = 0; l < 2; ++l) {
    const auto r = sim.Layer(l, ask);
    ASSERT_FALSE(r.empty()) << l;
    EXPECT_TRUE(r[0].e.idr) << l;
    EXPECT_EQ(r[0].e.input, Input::kLast) << "the last frame re-encoded";
    EXPECT_EQ(r[0].e.frame, 1u);
    EXPECT_LE(r[0].at - ask, 1'000'000'000 / 60) << "within one interval";
    EXPECT_EQ(r[0].e.ts_ns, r[0].at) << "a static IDR is stamped now";
  }
}

TEST(Pacer, RefreshAt200msThenEverySecond) {
  Sim sim({Layer(60)});
  sim.Run({kEpoch + 5 * kMs}, {}, kEpoch + 4 * kSec);
  const auto r = sim.Layer(0);
  ASSERT_GE(r.size(), 5u);
  EXPECT_TRUE(r[0].e.idr);
  const int64_t base = kEpoch + 5 * kMs;
  const int64_t want[] = {base + 200 * kMs, base + 1200 * kMs, base + 2200 * kMs, base + 3200 * kMs};
  for (std::size_t i = 0; i < 4; ++i) {
    EXPECT_EQ(r[i + 1].at, want[i]);
    EXPECT_TRUE(r[i + 1].e.refresh);
    EXPECT_FALSE(r[i + 1].e.idr) << "no periodic IDR while static";
    EXPECT_EQ(r[i + 1].e.input, Input::kLast);
    EXPECT_EQ(r[i + 1].e.ts_ns, r[i + 1].at);
  }
  EXPECT_EQ(sim.pacer.Stats(0, kEpoch + 4 * kSec).refreshes, 4u);
}

TEST(Pacer, NoRefreshWhileFramesFlow) {
  Sim sim({Layer(60)});
  sim.Run(Source(10.0, kEpoch, 3.0), {}, kEpoch + 3 * kSec);
  EXPECT_EQ(Count(sim.recs, [](const Rec &r) { return r.e.refresh; }), 0) << "a frame every 100 ms";
}

TEST(Pacer, IdrSpacing250ms) {
  std::vector<Action> asks;
  for (int64_t t = kEpoch + 1 * kSec; t < kEpoch + 3 * kSec; t += 10 * kMs) {
    asks.push_back(Action{t, [](Pacer &p, int64_t) { p.RequestKeyframe(0); }});
  }
  Sim sim({Layer(60)});
  sim.Run(Source(60.0, kEpoch, 3.0), asks, kEpoch + 3 * kSec);
  std::vector<int64_t> idrs;
  for (const Rec &r : sim.Layer(0, kEpoch + 1 * kSec)) {
    if (r.e.idr) {
      idrs.push_back(r.at);
    }
  }
  ASSERT_GE(idrs.size(), 7u);
  for (std::size_t i = 1; i < idrs.size(); ++i) {
    EXPECT_GE(idrs[i] - idrs[i - 1], 250 * kMs);
    EXPECT_LE(idrs[i] - idrs[i - 1], 250 * kMs + 17 * kMs) << "the next due frame after the mark";
  }
}

TEST(Pacer, RequiredKeyframeSkipsTheSpacing) {
  Sim sim({Layer(60)});
  const int64_t at = kEpoch + 50 * kMs;
  sim.Run(Source(60.0, kEpoch, 0.2), {Action{at, [](Pacer &p, int64_t) { p.RequireKeyframe(0); }}}, kEpoch + 200 * kMs);
  const auto r = sim.Layer(0, at);
  ASSERT_FALSE(r.empty());
  EXPECT_TRUE(r[0].e.idr) << "a ring overflow's IDR doesn't wait for the 250 ms mark";
  EXPECT_LE(r[0].at - at, 17 * kMs);
}

TEST(Pacer, TimeBasedGop) {
  Sim sim({Layer(60), Layer(15)});
  sim.Run(Source(60.0, kEpoch, 10.0), {}, kEpoch + 10 * kSec);
  std::vector<int64_t> f_idrs;
  for (const Rec &r : sim.Layer(0)) {
    if (r.e.idr) {
      f_idrs.push_back(r.at - kEpoch);
    }
  }
  ASSERT_EQ(f_idrs.size(), 3u) << "4 s GOP over 10 s of motion";
  EXPECT_GE(f_idrs[1] - f_idrs[0], 4 * kSec);
  EXPECT_LE(f_idrs[1] - f_idrs[0], 4 * kSec + 17 * kMs);
  EXPECT_EQ(Count(sim.Layer(1), [](const Rec &r) { return r.e.idr; }), 5) << "2 s GOP for q";
  // Static after the first IDR: no periodic IDR at all.
  Sim still({Layer(60)});
  still.Run({kEpoch}, {}, kEpoch + 10 * kSec);
  EXPECT_EQ(Count(still.recs, [](const Rec &r) { return r.e.idr; }), 1);
  // A custom GOP.
  LayerConfig c = Layer(60);
  c.gop_ms = 1000;
  Sim one({c});
  one.Run(Source(60.0, kEpoch, 5.0), {}, kEpoch + 5 * kSec);
  EXPECT_EQ(Count(one.recs, [](const Rec &r) { return r.e.idr; }), 5);
}

TEST(Pacer, ReopenStartsWithAnIdr) {
  Sim sim({Layer(60)});
  const int64_t at = kEpoch + 1 * kSec + 3 * kMs;
  sim.Run({kEpoch}, {Action{at, [](Pacer &p, int64_t now) { p.Reopen(0, now); }}}, kEpoch + 2 * kSec);
  const auto r = sim.Layer(0, at);
  ASSERT_FALSE(r.empty());
  EXPECT_EQ(r[0].at, at) << "a re-opened encoder's IDR comes at once, also on a static screen";
  EXPECT_TRUE(r[0].e.idr);
}

// --------------------------------------------------------------------------------------------- pause, layers

TEST(Pacer, PauseIgnoresNewFramesYetAnswersIdrs) {
  const int64_t pause = kEpoch + 1 * kSec;
  const int64_t ask = kEpoch + 2 * kSec;
  const int64_t resume = kEpoch + 3 * kSec + 5 * kMs;
  Sim sim({Layer(60)});
  sim.Run(Source(60.0, kEpoch, 4.0),
          {Action{pause, [](Pacer &p, int64_t now) { p.SetPaused(true, now); }},
           Action{ask, [](Pacer &p, int64_t) { p.RequestKeyframe(0); }},
           Action{resume, [](Pacer &p, int64_t now) { p.SetPaused(false, now); }}},
          kEpoch + 4 * kSec);
  const auto paused = sim.Layer(0, pause, resume);
  ASSERT_FALSE(paused.empty());
  uint64_t frozen = 0;
  for (const Rec &r : sim.Layer(0, 0, pause)) {
    frozen = r.e.frame;
  }
  int idrs = 0;
  for (const Rec &r : paused) {
    EXPECT_EQ(r.e.input, Input::kLast) << "no captured frame is used while paused";
    EXPECT_EQ(r.e.frame, frozen) << "the frozen last frame";
    EXPECT_TRUE(r.e.refresh || r.e.idr);
    idrs += r.e.idr ? 1 : 0;
  }
  EXPECT_EQ(idrs, 1) << "the keyframe request is answered with an IDR of the frozen frame";
  EXPECT_GE(Count(paused, [](const Rec &r) { return r.e.refresh; }), 2) << "refreshes continue";
  const auto after = sim.Layer(0, resume);
  ASSERT_FALSE(after.empty());
  EXPECT_EQ(after[0].at, resume) << "resume encodes the newest captured frame at once";
  EXPECT_EQ(after[0].e.input, Input::kLatest);
  EXPECT_TRUE(sim.pacer.paused() == false);
}

TEST(Pacer, InactiveLayerEncodesNothingAndRestartsWithAnIdr) {
  const int64_t on = kEpoch + 2 * kSec;
  const int64_t off = kEpoch + 4 * kSec;
  Sim sim({Layer(60), Layer(15, /*active=*/false)});
  sim.Run(Source(60.0, kEpoch, 6.0),
          {Action{on, [](Pacer &p, int64_t now) { p.SetLayerActive(1, true, now); }},
           Action{off, [](Pacer &p, int64_t now) { p.SetLayerActive(1, false, now); }}},
          kEpoch + 6 * kSec);
  EXPECT_TRUE(sim.Layer(1, 0, on).empty()) << "IM_LAYER_INACTIVE: nothing until activated";
  const auto active = sim.Layer(1, on, off);
  ASSERT_FALSE(active.empty());
  EXPECT_LE(active[0].at - on, 17 * kMs);
  EXPECT_TRUE(active[0].e.idr) << "the first frame after reactivation is an IDR";
  EXPECT_EQ(Count(active, [](const Rec &r) { return r.e.idr; }), 1);
  EXPECT_GE(active.size(), 29u);
  EXPECT_TRUE(sim.Layer(1, off).empty()) << "inactive again: no encodes";
  EXPECT_GE(sim.Layer(0).size(), 355u) << "layer 0 is unaffected";
  EXPECT_FALSE(sim.pacer.Stats(1, kEpoch + 6 * kSec).active);
}

// ------------------------------------------------------------------------------------------------ timestamps

TEST(Pacer, Timestamps) {
  // Two frames with one capture time, one before the epoch, an old one after a static stretch.
  const std::vector<int64_t> frames = {kEpoch + 10 * kMs, kEpoch + 30 * kMs, kEpoch + 60 * kMs};
  Sim sim({Layer(60)});
  sim.Run(frames,
          {Action{kEpoch + 2 * kSec, [](Pacer &p, int64_t now) { p.OnFrame(now - 900 * kMs, now); }},
           Action{kEpoch + 3 * kSec, [](Pacer &p, int64_t now) { p.OnFrame(kEpoch - 5 * kMs, now); }}},
          kEpoch + 4 * kSec);
  int64_t last = std::numeric_limits<int64_t>::min();
  for (const Rec &r : sim.Layer(0)) {
    EXPECT_GT(r.e.ts_ns, last) << "strictly increasing";
    EXPECT_GE(r.e.ts_ns, kEpoch) << "never before epoch_ns";
    last = r.e.ts_ns;
  }
  const auto at2 = sim.Layer(0, kEpoch + 2 * kSec, kEpoch + 2 * kSec + 1);
  ASSERT_EQ(at2.size(), 1u);
  EXPECT_EQ(at2[0].e.ts_ns, kEpoch + 2 * kSec) << "a frame captured 900 ms ago is a static picture: stamped now";

  // Equal capture times are bumped by 1 µs; a capture time before the epoch is raised to it.
  Pacer p(kEpoch, std::vector<LayerConfig>{Layer(60)});
  Encode e[1];
  p.OnFrame(kEpoch - 1 * kMs, kEpoch);
  ASSERT_EQ(p.Poll(kEpoch, e), 1u);
  EXPECT_EQ(e[0].ts_ns, kEpoch);
  p.OnFrame(kEpoch, kEpoch + 20 * kMs);
  ASSERT_EQ(p.Poll(kEpoch + 20 * kMs, e), 1u);
  EXPECT_EQ(e[0].ts_ns, kEpoch + 1000) << "bumped by 1 µs";
  p.OnFrame(kEpoch + 40 * kMs, kEpoch + 40 * kMs);
  ASSERT_EQ(p.Poll(kEpoch + 40 * kMs, e), 1u);
  EXPECT_EQ(e[0].ts_ns, kEpoch + 40 * kMs) << "capture times pass through unchanged";
}

// ------------------------------------------------------------------------------------------------- overload

void OverloadCase(bool hardware, Limit want) {
  const int64_t slow_from = kEpoch + 2 * kSec;
  const int64_t slow_to = kEpoch + 10 * kSec;
  Sim sim({Layer(60, true, hardware)});
  sim.cost = [&](const Encode &, int64_t now) { return (now >= slow_from && now < slow_to) ? 25 * kMs : 1 * kMs; };
  sim.Run(Source(60.0, kEpoch, 30.0, kMs / 2, 11), {}, kEpoch + 30 * kSec);
  ASSERT_EQ(sim.limit_changes.size(), 2u);
  const auto &[t1, c1] = sim.limit_changes[0];
  EXPECT_EQ(c1.limit, want);
  EXPECT_EQ(c1.layer, 0u);
  EXPECT_GE(t1 - slow_from, 3 * kSec) << "3 s below 90 % (its window takes a moment to fall)";
  EXPECT_LE(t1 - slow_from, 4 * kSec);
  EXPECT_GT(c1.fps, 30.0);
  EXPECT_LT(c1.fps, 50.0);
  const auto &[t2, c2] = sim.limit_changes[1];
  EXPECT_EQ(c2.limit, Limit::kNone);
  EXPECT_GE(t2 - slow_to, 10 * kSec) << "10 s above 95 %";
  EXPECT_LE(t2 - slow_to, 12 * kSec);
  EXPECT_GT(sim.pacer.Stats(0, kEpoch + 30 * kSec).pacer_drops, 100u) << "frames lost while the encoder lagged";
  EXPECT_EQ(sim.pacer.Stats(0, kEpoch + 30 * kSec).limit, Limit::kNone);
  // The frame rate dropped by itself meanwhile: encodes simply came later.
  const auto slow = sim.Layer(0, slow_from + 1 * kSec, slow_to);
  EXPECT_LT(slow.size(), 7u * 45u);
}

TEST(Pacer, OverloadSoftwareIsCpu) { OverloadCase(false, Limit::kCpu); }

TEST(Pacer, OverloadHardwareIsGpu) { OverloadCase(true, Limit::kGpu); }

TEST(Pacer, NoLimitOnAStaticOrSlowSource) {
  Sim sim({Layer(60, true, false)});
  sim.Run(Source(5.0, kEpoch, 20.0), {}, kEpoch + 20 * kSec);
  EXPECT_TRUE(sim.limit_changes.empty()) << "a slow source isn't an overload: min(target, fpsIn)";
  EXPECT_EQ(im::pace::LimitToken(Limit::kNone), "none");
  EXPECT_EQ(im::pace::LimitToken(Limit::kCpu), "cpu");
  EXPECT_EQ(im::pace::LimitToken(Limit::kGpu), "gpu");
}

TEST(Pacer, FpsChange) {
  // Text 30: an fps re-init (Reopen with SetFps) halves the cadence.
  const int64_t at = kEpoch + 2 * kSec;
  Sim sim({Layer(60)});
  sim.Run(Source(60.0, kEpoch, 6.0),
          {Action{at,
                  [](Pacer &p, int64_t now) {
                    p.SetFps(0, 30, now);
                    p.Reopen(0, now);
                  }}},
          kEpoch + 6 * kSec);
  const auto after = sim.Layer(0, at + 1 * kSec, kEpoch + 6 * kSec);
  EXPECT_GE(after.size(), 89u);
  EXPECT_LE(after.size(), 91u);
}

TEST(Pacer, DefaultGopAndConstruction) {
  EXPECT_EQ(im::pace::DefaultGopMs(0), 4000u);
  EXPECT_EQ(im::pace::DefaultGopMs(1), 2000u);
  std::vector<LayerConfig> four(4, Layer(60));
  Pacer p(kEpoch, four);
  EXPECT_EQ(p.layer_count(), 3u) << "at most IM_MAX_LAYERS";
  Pacer none(kEpoch, std::vector<LayerConfig>{});
  EXPECT_EQ(none.NextWakeNs(), kNever);
  Encode e[3];
  EXPECT_EQ(none.Poll(kEpoch, e), 0u);
}

} // namespace
