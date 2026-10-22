#include "im/audiomix/mix_engine.h"

#include <algorithm>
#include <cmath>
#include <cstring>

#include "im/audiomix/dsp.h"

namespace im::audiomix {

namespace {

constexpr uint32_t kPrerollFrames = 40 * kFramesPerMs;      // >= 40 ms buffered: Preroll -> Running (§9.2)
constexpr uint32_t kLowWaterFrames = 20 * kFramesPerMs;     // < 20 ms while running: fade out, Preroll (underrun)
constexpr uint32_t kHighWaterFrames = 100 * kFramesPerMs;   // > 100 ms while running: fade out, trim, Preroll
constexpr uint32_t kTrimToFrames = 40 * kFramesPerMs;       // a trim keeps the newest 40 ms
constexpr uint32_t kBypassAfterFrames = 100 * kFramesPerMs; // 100 ms of silent input: the silent bypass (§9.3)
constexpr int64_t kBypassMaxCorrection = 48;                // frames dropped or padded per tick in the bypass
// The input a chunk can need: at most 480 * (1 + 992e-6) frames plus speexdsp's carry of a frame or two at the
// highest ratio; 496 leaves room (MixEngine::in_ holds 600, enough for the bypass's 528 too).
constexpr uint32_t kPeekFrames = kChunkFrames + 16;
constexpr uint32_t kLevelTicks = 10; // levels over 100 ms (§9.6)
constexpr double kLevelSamples = static_cast<double>(kLevelTicks) * kChunkSamples;

uint32_t FloatBits(float f) noexcept {
  uint32_t u = 0;
  std::memcpy(&u, &f, sizeof u);
  return u;
}

float BitsFloat(uint32_t u) noexcept {
  float f = 0;
  std::memcpy(&f, &u, sizeof f);
  return f;
}

template <typename T> void AddRelaxed(std::atomic<T> &a, T v) noexcept {
  a.store(a.load(std::memory_order_relaxed) + v, std::memory_order_relaxed);
}

} // namespace

const char *MixEventName(MixEventCode code) noexcept {
  switch (code) {
  case MixEventCode::kStreamRunning:
    return "stream_running";
  case MixEventCode::kStreamRemoved:
    return "stream_removed";
  case MixEventCode::kUnderrun:
    return "underrun";
  case MixEventCode::kTrim:
    return "trim";
  case MixEventCode::kMixerResync:
    return "mixer_resync";
  case MixEventCode::kReanchor:
    return "reanchor";
  case MixEventCode::kFastpathLateCut:
    return "fastpath_late_cut";
  }
  return "";
}

void LevelSum::Add(float peak_dbfs, float rms_dbfs) noexcept {
  if (peak_dbfs > kLevelFloorDb) {
    peak += std::pow(10.0, peak_dbfs / 20.0);
  }
  if (rms_dbfs > kLevelFloorDb) {
    const double r = std::pow(10.0, rms_dbfs / 20.0);
    power += r * r;
  }
}

float LevelSum::peak_db() const noexcept { return LinearToDb(peak); }
float LevelSum::rms_db() const noexcept { return LinearToDb(std::sqrt(power)); }

MixEngine::MixEngine(ChunkSink *sink, const MixParams &params)
    : sink_(sink), params_(params), delay_(params.initial_delay_ns) {
  delay_pub_.store(delay_, std::memory_order_relaxed);
}

bool MixEngine::Add(Channel *ch) noexcept {
  if (ch == nullptr) {
    return false;
  }
  for (auto &slot : slots_) {
    Channel *expected = nullptr;
    if (slot.compare_exchange_strong(expected, ch, std::memory_order_acq_rel, std::memory_order_acquire)) {
      return true;
    }
  }
  return false;
}

MixCounters MixEngine::counters() const noexcept {
  MixCounters c;
  c.chunks = chunks_.load(std::memory_order_relaxed);
  c.gaps = gaps_.load(std::memory_order_relaxed);
  c.underruns = underruns_.load(std::memory_order_relaxed);
  c.trims = trims_.load(std::memory_order_relaxed);
  c.overflows = overflows_.load(std::memory_order_relaxed);
  c.mixer_resyncs = resyncs_.load(std::memory_order_relaxed);
  c.reanchors = reanchors_.load(std::memory_order_relaxed);
  c.fastpath_late_cuts = late_cuts_.load(std::memory_order_relaxed);
  c.max_resample_ppm = max_ppm_.load(std::memory_order_relaxed);
  c.delay_ns = delay_pub_.load(std::memory_order_relaxed);
  return c;
}

uint32_t MixEngine::DrainEvents(MixEvent *out, uint32_t cap) noexcept {
  uint32_t head = ev_head_.load(std::memory_order_relaxed);
  const uint32_t tail = ev_tail_.load(std::memory_order_acquire);
  uint32_t n = 0;
  while (head != tail && n < cap) {
    out[n++] = events_[head % kEventCapacity];
    ++head;
  }
  ev_head_.store(head, std::memory_order_release);
  return n;
}

void MixEngine::PushEvent(int64_t t_ns, MixEventCode code, uint64_t tag) noexcept {
  const uint32_t tail = ev_tail_.load(std::memory_order_relaxed);
  const uint32_t head = ev_head_.load(std::memory_order_acquire);
  if (tail - head >= kEventCapacity) {
    AddRelaxed<uint64_t>(events_dropped_, 1);
    return;
  }
  events_[tail % kEventCapacity] = MixEvent{t_ns, code, tag};
  ev_tail_.store(tail + 1, std::memory_order_release);
}

void MixEngine::Begin(int64_t now_ns) noexcept {
  tick0_ = now_ns;
  k_ = 0;
  next_due_ = now_ns;
  delay_ = params_.initial_delay_ns;
  delay_pub_.store(delay_, std::memory_order_relaxed);
  have_prev_ = false;
  need_gap_ = true;
  resync_ = false;
  silent_since_anchor_ = true;
}

int64_t MixEngine::Pump(int64_t now_ns) noexcept {
  while (next_due_ <= now_ns) {
    if (now_ns - next_due_ > params_.resync_after_ns) {
      // Fell more than 100 ms behind (a suspend, a debugger): no burst of catch-up chunks; a new grid at now (§9.1).
      tick0_ = now_ns;
      k_ = 0;
      next_due_ = now_ns;
      resync_ = true;
      Count(resyncs_);
      PushEvent(now_ns, MixEventCode::kMixerResync, 0);
    }
    Tick(next_due_);
    ++k_;
    next_due_ = tick0_ + k_ * kChunkNs;
  }
  return next_due_;
}

void MixEngine::StartFade(Channel &ch, uint32_t frames) noexcept {
  ch.gain_target_ = 0.0f;
  ch.gain_step_ = 1.0f / static_cast<float>(frames);
}

void MixEngine::Release(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept {
  cur_[slot] = nullptr;
  slots_[slot].store(nullptr, std::memory_order_release);
  ch.st_ = StreamState::kReleased;
  ch.state_.store(StreamState::kReleased, std::memory_order_release);
  PushEvent(tick_ns, MixEventCode::kStreamRemoved, ch.tag_);
  ch.released_.store(true, std::memory_order_release); // the watcher may destroy it from here on: don't touch it
}

void MixEngine::ReleaseAll() noexcept {
  for (auto &slot : slots_) {
    if (Channel *ch = slot.exchange(nullptr, std::memory_order_acq_rel); ch != nullptr) {
      ch->st_ = StreamState::kReleased;
      ch->state_.store(StreamState::kReleased, std::memory_order_release);
      ch->released_.store(true, std::memory_order_release);
    }
  }
}

void MixEngine::Prepare(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept {
  ch.entered_ = false;
  ch.skip_ = false;
  ch.stop_after_ = false;
  ch.trim_after_ = false;
  CaptureRing &ring = *ch.ring_;
  if (!ch.joined_) {
    // Joining: nothing captured before the join is mixed, and the 40 ms preroll counts from here (§6.2 step 5).
    ch.joined_ = true;
    ring.DiscardAll();
    ch.seen_overflows_ = ring.overflows();
    ch.st_ = StreamState::kPreroll;
    ch.state_.store(StreamState::kPreroll, std::memory_order_release);
    ch.gain_ = 0.0f;
    ch.gain_target_ = 0.0f;
  }
  if (const uint64_t ov = ring.overflows(); ov != ch.seen_overflows_) {
    AddRelaxed<uint64_t>(overflows_, ov - ch.seen_overflows_);
    ch.seen_overflows_ = ov;
  }

  // Audible: a gain above 0, or a backstop tail still sounding.
  const bool audible = (ch.st_ == StreamState::kRunning && ch.gain_ > 0.0f) || ch.tail_pos_ > 0;
  if (ch.st_ != StreamState::kFading &&
      (ch.remove_.load(std::memory_order_acquire) || ch.fail_.load(std::memory_order_acquire))) {
    if (!audible) {
      Release(slot, ch, tick_ns); // nothing to fade
      return;
    }
    ch.st_ = StreamState::kFading;
    ch.state_.store(StreamState::kFading, std::memory_order_release);
    StartFade(ch, kFadeFrames);
  }
  if (!ch.cut_active_ && ch.cut_.load(std::memory_order_acquire)) {
    if (!audible) {
      Release(slot, ch, tick_ns);
      return;
    }
    const uint32_t before = ring.FramesBefore(ch.cut_qpc_.load(std::memory_order_relaxed));
    ch.st_ = StreamState::kFading;
    ch.state_.store(StreamState::kFading, std::memory_order_release);
    ch.cut_active_ = true;
    if (before == 0) { // the cut point was already mixed: 5 ms out at once (§6.4)
      Count(late_cuts_);
      PushEvent(tick_ns, MixEventCode::kFastpathLateCut, ch.tag_);
      ch.cut_left_ = 0;
      StartFade(ch, kCutFadeFrames);
    } else { // the cut envelope fades the last 5 ms before the cut point (all that is left, when less)
      ch.cut_left_ = before;
      ch.cut_fade_ = std::min(kCutFadeFrames, before);
    }
  }
  if (ch.st_ == StreamState::kFading) {
    return;
  }

  const uint32_t buffered = ring.Buffered();
  if (ch.st_ == StreamState::kPreroll) {
    if (buffered < kPrerollFrames) {
      ch.skip_ = true;
      return;
    }
    ch.st_ = StreamState::kRunning;
    ch.state_.store(StreamState::kRunning, std::memory_order_release);
    ch.entered_ = true;
    any_entered_ = true;
    ch.gain_ = 0.0f;
    ch.gain_target_ = 1.0f;
    // The 20 ms crossfade ramp only when this entry takes over (Tick); a re-entry fades in over 10 ms (§9.2).
    ch.gain_step_ = 1.0f / static_cast<float>(ch.crossfade_ && !ch.took_over_ ? kCrossfadeFrames : kFadeFrames);
    // A channel (re)entering Running ramps its first non-silent sample in (§9.5: the counter starts saturated).
    ch.silent_frames_ = kOnsetSilenceFrames;
    ch.onset_pos_ = kOnsetRampFrames;
    ch.bypass_ = false;
    ch.bypass_flag_.store(false, std::memory_order_relaxed);
    ch.ctl_.SetHold(false);
    ch.silent_input_ = 0;
    ch.resampler_->Reset();
    if (int64_t head = 0; ring.HeadQpc(&head)) {
      const int64_t age = tick_ns - head;
      ch.ctl_.Seed(age > 0 ? age : 0);
      ch.age_ns_.store(static_cast<int64_t>(ch.ctl_.age_ns()), std::memory_order_relaxed);
    }
    PushEvent(tick_ns, MixEventCode::kStreamRunning, ch.tag_);
  }
  if (buffered < kLowWaterFrames) {
    ch.stop_after_ = true; // about to run dry: fade out over this chunk, then preroll again
    Count(underruns_);
    AddRelaxed<uint64_t>(ch.underruns_, 1);
    PushEvent(tick_ns, MixEventCode::kUnderrun, ch.tag_);
  } else if (buffered > kHighWaterFrames) {
    ch.stop_after_ = true; // too far behind: fade out, drop the excess, preroll again
    ch.trim_after_ = true;
    Count(trims_);
    AddRelaxed<uint64_t>(ch.trims_, 1);
    PushEvent(tick_ns, MixEventCode::kTrim, ch.tag_);
  }
  if (ch.stop_after_) {
    StartFade(ch, kFadeFrames);
  }
}

void MixEngine::EmitTail(Channel &ch, uint32_t from) noexcept {
  for (uint32_t j = from; ch.tail_pos_ > 0 && j < kChunkFrames; ++j) {
    bus_[2 * j] += TailFrame(ch.tail_v_[0], ch.tail_s_[0], ch.tail_pos_);
    bus_[2 * j + 1] += TailFrame(ch.tail_v_[1], ch.tail_s_[1], ch.tail_pos_);
    if (++ch.tail_pos_ >= kTailRampFrames) {
      ch.tail_pos_ = 0;
    }
  }
}

int64_t MixEngine::TargetFor(Channel &ch) noexcept {
  // The fill floor: the head age at which the fill averages 40 ms, i.e. 40 ms plus the newest frame's mean age (the
  // packets' lag behind the mix plus half a packet, capture_ring.h). The target holds L unless that leaves less fill.
  const CaptureRing &ring = *ch.ring_;
  const int64_t packet = ring.packet_ns();
  ch.floor_ns_ = packet > 0 ? params_.fill_target_ns + ring.arrival_lag_ns() + packet / 2 : 0;
  const int64_t target =
      std::clamp(std::max(delay_, ch.floor_ns_), params_.min_target_age_ns, params_.max_target_age_ns);
  ch.target_pub_.store(target, std::memory_order_relaxed);
  return target;
}

void MixEngine::Render(uint32_t slot, Channel &ch, int64_t tick_ns) noexcept {
  EmitTail(ch, 0); // the rest of a tail that began near the end of the previous chunk
  if (ch.skip_ || (ch.st_ != StreamState::kRunning && ch.st_ != StreamState::kFading)) {
    ch.peak_[ch.level_pos_] = 0.0f;
    ch.sumsq_[ch.level_pos_] = 0.0;
    ch.level_pos_ = (ch.level_pos_ + 1) % kLevelTicks;
    return;
  }
  CaptureRing &ring = *ch.ring_;

  // The head age d (§10.3) drives the controller (§9.3) and the bypass's correction, towards the channel's target.
  const int64_t target_age = TargetFor(ch);
  int64_t head = 0;
  const bool have_head = ring.HeadQpc(&head);
  int64_t age = have_head ? tick_ns - head : target_age;
  if (age < 0) {
    age = 0; // a bad timestamp; causality says d > 0
  }
  if (ch.st_ == StreamState::kRunning) {
    ch.ctl_.SetTarget(target_age);
    if (ch.ctl_.Tick(age)) {
      ch.resampler_->SetSteps(ch.ctl_.steps());
      const int32_t ppm = ch.ctl_.ppm();
      ch.ppm_.store(ppm, std::memory_order_relaxed);
      const int32_t mag = ppm < 0 ? -ppm : ppm;
      if (mag > max_ppm_.load(std::memory_order_relaxed)) {
        max_ppm_.store(mag, std::memory_order_relaxed);
      }
    }
    ch.age_ns_.store(static_cast<int64_t>(ch.ctl_.age_ns()), std::memory_order_relaxed);
  }

  uint32_t made = 0;
  uint32_t consumed = 0;
  bool done = false;
  if (ch.bypass_) {
    // Silent: no resampler; the age is corrected directly by dropping or padding up to 48 frames of zeros (§9.3).
    int64_t corr = (age - target_age) * kFramesPerMs / kNsPerMs;
    corr = corr > kBypassMaxCorrection ? kBypassMaxCorrection
                                       : (corr < -kBypassMaxCorrection ? -kBypassMaxCorrection : corr);
    const auto want = static_cast<uint32_t>(static_cast<int64_t>(kChunkFrames) + corr);
    const uint32_t n = ring.Peek(in_, want);
    bool silent = true;
    for (uint32_t i = 0; i < n; ++i) {
      if (!IsSilentFrame(in_[2 * i], in_[2 * i + 1])) {
        silent = false;
        break;
      }
    }
    if (silent) {
      (void)ring.Consume(n);
      consumed = n;
      std::memset(out_, 0, sizeof out_);
      made = kChunkFrames;
      done = true;
      ch.silent_input_ = std::min(ch.silent_input_ + n, kBypassAfterFrames);
      ch.silent_frames_ = std::min(ch.silent_frames_ + n, kOnsetSilenceFrames);
    } else {
      // Sound again: leave the bypass with a clean filter, so nothing from before the silence comes out at the onset.
      ch.bypass_ = false;
      ch.bypass_flag_.store(false, std::memory_order_relaxed);
      ch.ctl_.SetHold(false);
      ch.silent_input_ = 0;
      ch.resampler_->Reset();
    }
  }
  if (!done) {
    const uint32_t n = ring.Peek(in_, kPeekFrames);
    // The onset fade (§9.5) on the stream's input, before the resampler: sound after at least 50 ms of digital
    // silence is ramped in over 5 ms. (After the resampler it would come too late: speexdsp's 80-tap filter turns a
    // waveform that starts mid-cycle into a band-limited step that rings for 40 frames on either side, and a ramp
    // that starts at the first ringing sample has barely risen when the step arrives.) The peek is ramped with a copy
    // of the state; the state then moves on by the frames the resampler consumed.
    uint32_t sf = ch.silent_frames_, op = ch.onset_pos_;
    for (uint32_t i = 0; i < n; ++i) {
      float *f = in_ + 2 * static_cast<std::size_t>(i);
      const bool quiet = IsSilentFrame(f[0], f[1]);
      in_silent_[i] = quiet;
      if (quiet) {
        sf = sf < kOnsetSilenceFrames ? sf + 1 : sf;
      } else {
        if (sf >= kOnsetSilenceFrames) {
          op = 0;
        }
        sf = 0;
      }
      if (op < kOnsetRampFrames) {
        const float g = static_cast<float>(++op) / static_cast<float>(kOnsetRampFrames);
        f[0] *= g;
        f[1] *= g;
      }
      onset_sf_[i] = sf;
      onset_op_[i] = op;
    }
    uint32_t used = 0;
    ch.resampler_->Process(in_, n, out_, kChunkFrames, &used, &made);
    (void)ring.Consume(used);
    consumed = used;
    if (used > 0) {
      ch.silent_frames_ = onset_sf_[used - 1];
      ch.onset_pos_ = onset_op_[used - 1];
    }
    // The bypass rule counts the consumed input's digital silence in a row.
    uint32_t run = 0;
    while (run < used && in_silent_[used - 1 - run]) {
      ++run;
    }
    ch.silent_input_ = run == used ? std::min(ch.silent_input_ + used, kBypassAfterFrames) : run;
    if (ch.st_ == StreamState::kRunning && !ch.stop_after_ && ch.silent_input_ >= kBypassAfterFrames) {
      ch.bypass_ = true;
      ch.bypass_flag_.store(true, std::memory_order_relaxed);
      ch.ctl_.SetHold(true);
    }
  }

  // The gain ramp and the cut envelope, frame by frame.
  const float target = ch.gain_target_;
  const float step = ch.gain_step_;
  float gain = ch.gain_;
  const bool cutting = ch.cut_active_ && ch.cut_left_ > 0;
  const int64_t cut_end = ch.cut_left_;
  const auto cut_fade = static_cast<int64_t>(ch.cut_fade_);
  float peak = 0.0f;
  double sumsq = 0.0;
  float last_l = ch.last_l_, last_r = ch.last_r_;
  float prev_l = ch.prev_l_, prev_r = ch.prev_r_;
  for (uint32_t j = 0; j < made; ++j) {
    const float l = out_[2 * j], r = out_[2 * j + 1];
    if (gain != target) {
      const float d = target - gain;
      gain = d > step ? gain + step : (d < -step ? gain - step : target);
    }
    float g = gain;
    if (cutting) {
      const int64_t left = cut_end - j;
      g *= left <= 0 ? 0.0f : (left >= cut_fade ? 1.0f : static_cast<float>(left) / static_cast<float>(cut_fade));
    }
    const float yl = l * g, yr = r * g;
    bus_[2 * j] += yl;
    bus_[2 * j + 1] += yr;
    const float al = yl < 0 ? -yl : yl, ar = yr < 0 ? -yr : yr;
    peak = al > peak ? al : peak;
    peak = ar > peak ? ar : peak;
    sumsq += static_cast<double>(yl) * yl + static_cast<double>(yr) * yr;
    prev_l = last_l;
    prev_r = last_r;
    last_l = yl;
    last_r = yr;
  }
  ch.gain_ = gain;

  bool cut_done = false;
  if (ch.cut_active_) {
    ch.cut_left_ -= consumed;
    cut_done = ch.cut_left_ <= 0 && cutting;
    if (cut_done) {
      ch.gain_ = 0.0f;
      last_l = last_r = prev_l = prev_r = 0.0f;
    }
  }
  if (made < kChunkFrames && ch.gain_ > 0.0f) {
    // Backstop: the chunk ended early mid-wave; a 64-frame tail from the last value and slope to zero (§9.4,
    // TailFrame), continued in the next chunk when this one ends first.
    ch.tail_v_[0] = last_l;
    ch.tail_v_[1] = last_r;
    ch.tail_s_[0] = last_l - prev_l;
    ch.tail_s_[1] = last_r - prev_r;
    ch.tail_pos_ = 1;
    EmitTail(ch, made);
    ch.gain_ = 0.0f;
    last_l = last_r = prev_l = prev_r = 0.0f;
    if (ch.st_ == StreamState::kRunning && !ch.stop_after_) {
      ch.st_ = StreamState::kPreroll;
      ch.state_.store(StreamState::kPreroll, std::memory_order_release);
      Count(underruns_);
      AddRelaxed<uint64_t>(ch.underruns_, 1);
      PushEvent(tick_ns, MixEventCode::kUnderrun, ch.tag_);
    }
  }
  if (ch.stop_after_) {
    ch.gain_ = 0.0f;
    ch.st_ = StreamState::kPreroll;
    ch.state_.store(StreamState::kPreroll, std::memory_order_release);
    if (ch.trim_after_) {
      if (const uint32_t b = ring.Buffered(); b > kTrimToFrames) {
        (void)ring.Consume(b - kTrimToFrames);
      }
    }
  }
  ch.last_l_ = last_l;
  ch.last_r_ = last_r;
  ch.prev_l_ = prev_l;
  ch.prev_r_ = prev_r;
  ch.peak_[ch.level_pos_] = peak;
  ch.sumsq_[ch.level_pos_] = sumsq;
  ch.level_pos_ = (ch.level_pos_ + 1) % kLevelTicks;

  if (ch.st_ == StreamState::kFading && (ch.gain_ <= 0.0f || cut_done) && ch.tail_pos_ == 0) {
    Release(slot, ch, tick_ns);
  }
}

void MixEngine::Tick(int64_t tick_ns) noexcept {
  std::memset(bus_, 0, sizeof bus_);
  any_entered_ = false;

  // The channels of this tick: one that the watcher adds meanwhile joins at the next one.
  for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
    cur_[i] = slots_[i].load(std::memory_order_acquire);
  }
  for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
    if (Channel *ch = cur_[i]; ch != nullptr) {
      Prepare(i, *ch, tick_ns);
    }
  }
  // The first crossfading channel of a group to start running takes over from the other engines: they fade out over
  // the same 20 ms (§6.5), or leave at once when nothing of them is audible. Its group takes over once: every channel
  // of it here now is marked, so a re-entry (after an underrun or a trim) or a later first entry of the same switch
  // never releases the streams another switch has opened meanwhile.
  int keep = -1;
  for (const Channel *ch : cur_) {
    if (ch != nullptr && ch->entered_ && ch->crossfade_ && !ch->took_over_) {
      keep = ch->group_;
      break;
    }
  }
  if (keep >= 0) {
    for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
      Channel *ch = cur_[i];
      if (ch == nullptr) {
        continue;
      }
      if (ch->group_ == keep) {
        ch->took_over_ = true;
        continue;
      }
      if (ch->st_ == StreamState::kFading) {
        continue;
      }
      if ((ch->st_ != StreamState::kRunning || ch->gain_ <= 0.0f) && ch->tail_pos_ == 0) {
        Release(i, *ch, tick_ns);
        continue;
      }
      ch->st_ = StreamState::kFading;
      ch->state_.store(StreamState::kFading, std::memory_order_release);
      ch->stop_after_ = false;
      ch->trim_after_ = false;
      StartFade(*ch, kCrossfadeFrames);
    }
  }
  for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
    if (Channel *ch = cur_[i]; ch != nullptr) {
      Render(i, *ch, tick_ns);
    }
  }

  // The bus: soft clip, then the silence flags' 10 ms fade (§9.6).
  const bool silenced = paused() || muted() || sound_off();
  const float target = silenced ? 0.0f : 1.0f;
  constexpr float kStep = 1.0f / static_cast<float>(kFadeFrames);
  float gain = bus_gain_;
  bool silent = true;
  float peak = 0.0f;
  double sumsq = 0.0;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    if (gain != target) {
      const float d = target - gain;
      gain = d > kStep ? gain + kStep : (d < -kStep ? gain - kStep : target);
    }
    for (uint32_t c = 0; c < 2; ++c) {
      const float in = bus_[2 * j + c];
      const float x = (in <= kSoftClipKnee && in >= -kSoftClipKnee ? in : SoftClip(in)) * gain;
      bus_[2 * j + c] = x;
      const float a = x < 0 ? -x : x;
      if (a >= kSilenceThreshold) {
        silent = false;
      }
      peak = a > peak ? a : peak;
      sumsq += static_cast<double>(x) * x;
    }
  }
  bus_gain_ = gain;
  bus_peak_[bus_level_pos_] = peak;
  bus_sumsq_[bus_level_pos_] = sumsq;
  bus_level_pos_ = (bus_level_pos_ + 1) % kLevelTicks;

  // The chunk's time on the grid (§10.3, §10.4).
  int64_t chunk = tick_ns - delay_;
  bool gap = need_gap_;
  if (resync_) {
    resync_ = false;
    gap = true;
    if (have_prev_ && chunk < prev_chunk_ + kChunkNs) {
      chunk = prev_chunk_ + kChunkNs;
      delay_ = tick_ns - chunk;
    }
  }
  if (gap) {
    silent_since_anchor_ = true; // a new grid is a new anchor
  }
  if (!silent) {
    silent_since_anchor_ = false;
  }
  if (!gap && any_entered_ && silent_since_anchor_) {
    // Re-anchor in silence: L' = the median of the head ages the running channels settle at (their smoothed head
    // age, or their fill floor when higher: the target never goes below it).
    double ages[kMaxMixChannels];
    uint32_t n = 0;
    for (const Channel *ch : cur_) {
      if (ch != nullptr && ch->st_ == StreamState::kRunning) {
        ages[n++] = std::max(ch->ctl_.age_ns(), static_cast<double>(ch->floor_ns_));
      }
    }
    if (n > 0) {
      std::nth_element(ages, ages + n / 2, ages + n);
      double median = ages[n / 2];
      if (n % 2 == 0) {
        median = (median + *std::max_element(ages, ages + n / 2)) / 2;
      }
      const auto measured = static_cast<int64_t>(std::llround(median < 0 ? 0 : median));
      const int64_t change = measured - delay_;
      if (change > params_.reanchor_change_ns || change < -params_.reanchor_change_ns) {
        int64_t cand = tick_ns - measured;
        if (have_prev_ && cand < prev_chunk_ + kChunkNs) {
          cand = prev_chunk_ + kChunkNs; // never earlier than the previous chunk + 10 ms: L moves part of the way
        }
        if (cand != chunk) {
          chunk = cand;
          delay_ = tick_ns - cand;
          gap = true;
          Count(reanchors_);
          PushEvent(tick_ns, MixEventCode::kReanchor, 0);
        }
      }
    }
  }
  if (gap) {
    need_gap_ = false;
    Count(gaps_);
    sink_->OnGap(chunk);
  }
  sink_->OnChunk(bus_, chunk);
  prev_chunk_ = chunk;
  have_prev_ = true;
  Count(chunks_);
  delay_pub_.store(delay_, std::memory_order_relaxed);
  PublishLevels(tick_ns, bus_gain_);
}

void MixEngine::PublishLevels(int64_t tick_ns, float bus_gain) noexcept {
  const uint64_t s = lv_seq_.load(std::memory_order_relaxed);
  lv_seq_.store(s + 1, std::memory_order_relaxed);
  std::atomic_thread_fence(std::memory_order_release);
  lv_t_.store(tick_ns, std::memory_order_relaxed);
  float peak = 0.0f;
  double sumsq = 0.0;
  for (uint32_t i = 0; i < kLevelTicks; ++i) {
    peak = std::max(peak, bus_peak_[i]);
    sumsq += bus_sumsq_[i];
  }
  lv_mix_peak_.store(FloatBits(LinearToDb(peak)), std::memory_order_relaxed);
  lv_mix_rms_.store(FloatBits(LinearToDb(std::sqrt(sumsq / kLevelSamples))), std::memory_order_relaxed);
  for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
    const Channel *ch = cur_[i];
    if (ch == nullptr) {
      lv_[i].flags.store(0, std::memory_order_relaxed);
      continue;
    }
    float cp = 0.0f;
    double cs = 0.0;
    for (uint32_t t = 0; t < kLevelTicks; ++t) {
      cp = std::max(cp, ch->peak_[t]);
      cs += ch->sumsq_[t];
    }
    const bool running = ch->st_ == StreamState::kRunning || ch->st_ == StreamState::kFading;
    lv_[i].tag.store(ch->tag_, std::memory_order_relaxed);
    lv_[i].peak.store(FloatBits(LinearToDb(static_cast<double>(cp) * bus_gain)), std::memory_order_relaxed);
    lv_[i].rms.store(FloatBits(LinearToDb(std::sqrt(cs / kLevelSamples) * bus_gain)), std::memory_order_relaxed);
    lv_[i].flags.store(1u | (running ? 2u : 0u), std::memory_order_relaxed);
  }
  lv_seq_.store(s + 2, std::memory_order_release);
}

bool MixEngine::ReadLevels(LevelsSnapshot *out) const noexcept {
  for (int attempt = 0; attempt < 64; ++attempt) {
    const uint64_t s1 = lv_seq_.load(std::memory_order_acquire);
    if (s1 == 0) {
      return false;
    }
    if ((s1 & 1u) != 0) {
      continue;
    }
    out->t_ns = lv_t_.load(std::memory_order_relaxed);
    out->mix_peak_db = BitsFloat(lv_mix_peak_.load(std::memory_order_relaxed));
    out->mix_rms_db = BitsFloat(lv_mix_rms_.load(std::memory_order_relaxed));
    uint32_t n = 0;
    for (const auto &slot : lv_) {
      const uint32_t flags = slot.flags.load(std::memory_order_relaxed);
      if ((flags & 1u) == 0) {
        continue;
      }
      ChannelLevel &c = out->channels[n++];
      c.tag = slot.tag.load(std::memory_order_relaxed);
      c.peak_db = BitsFloat(slot.peak.load(std::memory_order_relaxed));
      c.rms_db = BitsFloat(slot.rms.load(std::memory_order_relaxed));
      c.running = (flags & 2u) != 0;
    }
    out->count = n;
    std::atomic_thread_fence(std::memory_order_acquire);
    if (lv_seq_.load(std::memory_order_relaxed) == s1) {
      out->seq = s1 / 2;
      return true;
    }
  }
  return false;
}

} // namespace im::audiomix
