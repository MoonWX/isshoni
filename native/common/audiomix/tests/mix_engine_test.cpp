// The mixer's tick on a simulated clock (docs/m2/05-windows-audio.md §9, §10.3, §10.4, §19.1 "audiomix"): channel
// states and their fades, the onset fade, the silent bypass, crossfades and cuts, the silence flags, levels, events,
// and the chunk times with their OnGap rules. Exact sample checks use the model resampler (speexdsp's consumption,
// sample-and-hold output) with the ratio held at +1 step; the click checks run real speexdsp on a -6 dBFS tone.
#include <gtest/gtest.h>

#include <cmath>
#include <vector>

#include "im/audiomix/dsp.h"
#include "im/audiomix/mix_engine.h"
#include "mix_test_support.h"

namespace im::audiomix {
namespace {

using test::ClickFrames;
using test::ProducerConfig;
using test::Rig;

constexpr int64_t kMs = kNsPerMs;

// Packet k holds frames [480k, 480k + 480), mixed at 10k ms and arriving at 10(k + 1) ms: no latency, no jitter.
ProducerConfig Plain() {
  ProducerConfig p;
  p.latency_ns = 0;
  return p;
}

// A controller that never updates: the ratio stays at +1 step, so sample-and-hold output is the input itself.
ControllerParams Frozen() {
  ControllerParams c;
  c.update_ticks = 1u << 30;
  return c;
}

float Out(const Rig &r, uint64_t chunk, uint32_t frame, uint32_t c = 0) {
  return r.sink.audio[(chunk * kChunkFrames + frame) * 2 + c];
}

// The recorded audio from chunk `from` on.
std::vector<float> AudioFrom(const Rig &r, uint64_t from) {
  return std::vector<float>(r.sink.audio.begin() + static_cast<std::ptrdiff_t>(from * kChunkSamples),
                            r.sink.audio.end());
}

std::vector<MixEvent> Events(MixEngine &e) {
  std::vector<MixEvent> v(MixEngine::kEventCapacity);
  v.resize(e.DrainEvents(v.data(), static_cast<uint32_t>(v.size())));
  return v;
}

int CountEvents(const std::vector<MixEvent> &v, MixEventCode code) {
  int n = 0;
  for (const auto &e : v) {
    n += e.code == code;
  }
  return n;
}

// The level of a sine at hz in mono samples (S1's Goertzel with a Hann window), in dBFS of its peak.
double ToneDb(const std::vector<float> &interleaved, std::size_t from_frame, std::size_t frames, double hz) {
  const double coeff = 2 * std::cos(2 * test::kPi * hz / kRate);
  double s1 = 0, s2 = 0, sum_w = 0;
  for (std::size_t i = 0; i < frames; ++i) {
    const double w = 0.5 - 0.5 * std::cos(2 * test::kPi * static_cast<double>(i) / static_cast<double>(frames - 1));
    sum_w += w;
    const double x = (interleaved[2 * (from_frame + i)] + interleaved[2 * (from_frame + i) + 1]) / 2.0;
    const double s0 = x * w + coeff * s1 - s2;
    s2 = s1;
    s1 = s0;
  }
  const double power = s1 * s1 + s2 * s2 - coeff * s1 * s2;
  return 20 * std::log10(std::max(2 * std::sqrt(std::max(power, 0.0)) / sum_w, 1e-10));
}

// ---------------------------------------------------------------------------------------------- states and fades

TEST(MixEngine, PrerollsFortyMsThenFadesInWithTheOnsetRamp) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  ASSERT_NE(ch, nullptr);
  EXPECT_EQ(ch->state(), StreamState::kIdle);
  r.Begin(0);
  r.RunTo(30 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kPreroll);
  EXPECT_EQ(r.sink.loud_chunks, 0u);
  r.RunTo(40 * kMs); // the fourth packet: 40 ms buffered
  EXPECT_EQ(ch->state(), StreamState::kRunning);
  ASSERT_EQ(r.sink.chunks, 5u);
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    // The 10 ms gain ramp and, because a new channel's silence counter starts saturated, the 5 ms onset ramp.
    const float onset = j < 240 ? static_cast<float>(j + 1) / 240.0f : 1.0f;
    ASSERT_NEAR(Out(r, 4, j), 0.5f * onset * static_cast<float>(j + 1) / 480.0f, 1e-5f) << j;
    ASSERT_EQ(Out(r, 4, j, 1), Out(r, 4, j));
  }
  r.RunTo(60 * kMs);
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_EQ(Out(r, 5, j), 0.5f);
  }
  auto ev = Events(r.engine);
  ASSERT_EQ(CountEvents(ev, MixEventCode::kStreamRunning), 1);
  EXPECT_EQ(ev[0].tag, ch->tag());
  EXPECT_EQ(ev[0].t_ns, 40 * kMs);
}

TEST(MixEngine, NothingCapturedBeforeTheJoinIsMixed) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  ProducerConfig p = Plain();
  p.start_ns = -500 * kMs; // half a second of audio is in the ring before the first chunk
  Channel *ch = r.Add(&dc, p);
  r.Begin(0);
  r.RunTo(0);
  EXPECT_EQ(ch->ring()->Buffered(), 0u); // dropped at the join
  r.RunTo(30 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kPreroll);
  r.RunTo(40 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kRunning); // 40 ms after the join, not at once
}

TEST(MixEngine, UnderrunFadesOutOverTheChunkThenPrerollsAgain) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(200 * kMs);
  auto &s = r.stream(ch);
  s.producer->Pause();
  uint64_t fade_chunk = 0;
  for (int i = 0; i < 10 && ch->state() == StreamState::kRunning; ++i) {
    r.RunFor(10 * kMs);
    fade_chunk = r.sink.chunks - 1;
  }
  ASSERT_EQ(ch->state(), StreamState::kPreroll);
  EXPECT_EQ(ch->underruns(), 1u);
  EXPECT_EQ(r.engine.counters().underruns, 1u);
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_NEAR(Out(r, fade_chunk, j), 0.5f * (1.0f - static_cast<float>(j + 1) / 480.0f), 1e-5f) << j;
  }
  r.RunFor(50 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), 0.0f);
  // Packets again: preroll, then running with a fresh fade-in and onset ramp.
  s.producer->Resume(r.now);
  r.RunFor(60 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, kChunkFrames - 1), 0.5f);
  EXPECT_EQ(CountEvents(Events(r.engine), MixEventCode::kUnderrun), 1);
}

TEST(MixEngine, FarBehindFadesOutTrimsToFortyMsAndComesBack) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(200 * kMs);
  // A burst: 80 ms more than the stream's own packets (a capture that caught up after a stall).
  std::vector<float> burst(2 * 3840, 0.5f);
  ch->ring()->Write(burst.data(), 3840, 0);
  r.RunFor(10 * kMs);
  EXPECT_EQ(ch->trims(), 1u);
  EXPECT_EQ(r.engine.counters().trims, 1u);
  EXPECT_EQ(ch->state(), StreamState::kPreroll);
  const uint64_t fade = r.sink.chunks - 1;
  EXPECT_NEAR(Out(r, fade, 0), 0.5f * (1.0f - 1.0f / 480.0f), 1e-5f);
  EXPECT_EQ(Out(r, fade, kChunkFrames - 1), 0.0f);
  EXPECT_LE(ch->ring()->Buffered(), 40u * kFramesPerMs + kChunkFrames); // 40 ms, plus a packet that arrived since
  r.RunFor(10 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_EQ(CountEvents(Events(r.engine), MixEventCode::kTrim), 1);
}

TEST(MixEngine, RemovalFadesOverTenMsThenTheChannelIsReleased) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(100 * kMs);
  ch->Remove();
  EXPECT_FALSE(ch->released());
  r.RunFor(10 * kMs);
  const uint64_t fade = r.sink.chunks - 1;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_NEAR(Out(r, fade, j), 0.5f * (1.0f - static_cast<float>(j + 1) / 480.0f), 1e-5f) << j;
  }
  EXPECT_TRUE(ch->released());
  EXPECT_EQ(ch->state(), StreamState::kReleased);
  r.RunFor(10 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), 0.0f);
  auto ev = Events(r.engine);
  EXPECT_EQ(CountEvents(ev, MixEventCode::kStreamRemoved), 1);
  EXPECT_EQ(ev.back().tag, ch->tag());
}

TEST(MixEngine, AChannelNotYetAudibleLeavesAtOnce) {
  Rig r;
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(20 * kMs); // prerolling
  ch->Fail();
  r.RunFor(10 * kMs);
  EXPECT_TRUE(ch->released());
  EXPECT_EQ(r.sink.loud_chunks, 0u);
}

TEST(MixEngine, AudioEndingMidChunkGetsTheTail) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(100 * kMs);
  auto &s = r.stream(ch);
  s.producer->Pause();
  // The stream fails with only 100 frames left: the fade-out chunk ends early.
  ch->ring()->DiscardAll();
  std::vector<float> rest(2 * 100, 0.5f);
  ch->ring()->Write(rest.data(), 100, 0);
  ch->Fail();
  r.RunFor(10 * kMs);
  const uint64_t c = r.sink.chunks - 1;
  float prev = 0, last = 0;
  uint32_t made = 0;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    const float want = 0.5f * (1.0f - static_cast<float>(j + 1) / 480.0f);
    if (std::fabs(Out(r, c, j) - want) > 1e-5f) {
      break;
    }
    prev = last;
    last = Out(r, c, j);
    made = j + 1;
  }
  ASSERT_GE(made, 99u); // the model resampler makes 100 frames of 100 (99 when the carry ate one)
  ASSERT_LE(made, 100u);
  for (uint32_t t = 1; t <= kTailRampFrames; ++t) {
    ASSERT_NEAR(Out(r, c, made + t - 1), TailFrame(last, last - prev, t), 1e-6f) << t;
  }
  for (uint32_t j = made + kTailRampFrames; j < kChunkFrames; ++j) {
    ASSERT_EQ(Out(r, c, j), 0.0f);
  }
  EXPECT_TRUE(ch->released());
}

// A -6 dBFS 1 kHz tone ending at every phase: the tail's second differences stay below S1's click threshold.
TEST(MixEngine, TheTailIsClickFreeAtEveryPhase) {
  for (uint32_t phase = 0; phase < 48; ++phase) {
    std::vector<float> wave;
    float prev = 0, last = 0;
    for (uint32_t i = 0; i < 480; ++i) {
      const float x = 0.5f * static_cast<float>(std::sin(2 * test::kPi * (phase + i) / 48.0));
      wave.push_back(x);
      wave.push_back(x);
      prev = last;
      last = x;
    }
    for (uint32_t t = 1; t <= kTailRampFrames + 10; ++t) {
      const float y = TailFrame(last, last - prev, t);
      wave.push_back(y);
      wave.push_back(y);
      EXPECT_LE(std::fabs(y), 0.75f);
    }
    EXPECT_TRUE(ClickFrames(wave).empty()) << "phase " << phase;
  }
  // S1's linear ramp from the same point does click at some phases (why the tail is shaped).
  int linear_clicks = 0;
  for (uint32_t phase = 0; phase < 48; ++phase) {
    std::vector<float> wave;
    float last = 0;
    for (uint32_t i = 0; i < 480; ++i) {
      last = 0.5f * static_cast<float>(std::sin(2 * test::kPi * (phase + i) / 48.0));
      wave.push_back(last);
      wave.push_back(last);
    }
    for (uint32_t t = 0; t < kTailRampFrames; ++t) {
      const float y = last * (1.0f - static_cast<float>(t + 1) / kTailRampFrames);
      wave.push_back(y);
      wave.push_back(y);
    }
    linear_clicks += !ClickFrames(wave).empty();
  }
  EXPECT_GT(linear_clicks, 0);
}

TEST(MixEngine, ATailCarriesIntoTheNextChunk) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(100 * kMs);
  r.stream(ch).producer->Pause();
  // 450 frames left: the chunk ends 30 frames early, so the rest of the tail goes into the next chunk.
  ch->ring()->DiscardAll();
  std::vector<float> rest(2 * 450, 0.5f);
  ch->ring()->Write(rest.data(), 450, 0);
  ch->Fail();
  r.RunFor(10 * kMs);
  const uint64_t c = r.sink.chunks - 1;
  uint32_t made = 0;
  while (made < kChunkFrames &&
         std::fabs(Out(r, c, made) - 0.5f * (1.0f - static_cast<float>(made + 1) / 480.0f)) <= 1e-5f) {
    ++made;
  }
  ASSERT_GE(made, 449u);
  ASSERT_LE(made, 450u);
  const float last = Out(r, c, made - 1), prev = Out(r, c, made - 2);
  EXPECT_FALSE(ch->released()); // its tail isn't over
  r.RunFor(10 * kMs);
  EXPECT_TRUE(ch->released());
  for (uint32_t t = 1; t <= kTailRampFrames; ++t) {
    const uint64_t f = c * kChunkFrames + made + t - 1;
    ASSERT_NEAR(r.sink.audio[2 * f], TailFrame(last, last - prev, t), 1e-6f) << t;
  }
  EXPECT_EQ(Out(r, c + 1, kTailRampFrames), 0.0f);
}

// ------------------------------------------------------------------------------------------------ onset fade

// Exactly 50 ms of digital silence, then sound: ramped over 5 ms. 49 ms: not ramped (§9.5, §19.1).
TEST(MixEngine, OnsetRampAfterExactlyFiftyMsOfSilenceNotFortyNine) {
  for (const uint32_t silence_ms : {50u, 49u}) {
    Rig r;
    r.controller = Frozen();
    test::Segments sig;
    const uint64_t from = 48000, to = from + silence_ms * kFramesPerMs;
    sig.Add(0, from, 0, 0.5f);
    sig.Add(from, to, 0, 0.0f);
    sig.Add(to, 1'000'000, 0, 0.5f);
    Channel *ch = r.Add(&sig, Plain());
    r.Begin(0);
    r.RunTo(2000 * kMs);
    ASSERT_EQ(ch->state(), StreamState::kRunning);
    // The output frame where the sound comes back: the first non-silent frame after a silent one, past chunk 10.
    const auto &a = r.sink.audio;
    std::size_t onset = 0;
    for (std::size_t f = 10 * kChunkFrames; f + 1 < a.size() / 2; ++f) {
      if (a[2 * f] == 0.0f && a[2 * (f + 1)] != 0.0f) {
        onset = f + 1;
        break;
      }
    }
    ASSERT_GT(onset, 0u);
    for (uint32_t k = 0; k < 300; ++k) {
      const float want = silence_ms == 50 && k < 240 ? 0.5f * static_cast<float>(k + 1) / 240.0f : 0.5f;
      ASSERT_NEAR(a[2 * (onset + k)], want, 1e-6f) << silence_ms << " ms, frame " << k;
    }
  }
}

// S1's diagnosed click: a new stream silent for 30 ms after it starts running, then a tone at full level mid-cycle.
// The new channel's silence counter starts saturated, so the tone is ramped and S1's detector finds nothing.
TEST(MixEngine, NewStreamWithThirtyMsOfSilenceThenAToneIsRamped) {
  Rig r(true);
  test::Segments sig;
  const uint64_t start = 30 * kFramesPerMs + 12; // a quarter cycle in: the tone starts at its peak
  sig.Add(0, start, 0, 0.0f);
  sig.Add(start, 1'000'000, 1000, 0.5f);
  // The raw signal clicks there.
  std::vector<float> raw(2 * 4800);
  sig.Fill(0, 4800, raw.data());
  ASSERT_FALSE(ClickFrames(raw).empty());

  Channel *ch = r.Add(&sig, Plain(), /*speex=*/true);
  r.Begin(0);
  r.RunTo(1000 * kMs);
  ASSERT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_TRUE(ClickFrames(r.sink.audio).empty());
  // And it was the onset ramp: the first millisecond of the tone is far below its level.
  const auto &a = r.sink.audio;
  std::size_t first = 0;
  while (first < a.size() / 2 && std::fabs(a[2 * first]) < 1e-3f) {
    ++first;
  }
  float peak = 0;
  for (std::size_t f = first; f < first + 48; ++f) {
    peak = std::max(peak, std::fabs(a[2 * f]));
  }
  EXPECT_LT(peak, 0.15f);
}

// The same after a Preroll -> Running re-entry: an underrun, then 30 ms of silence and the tone at its peak.
TEST(MixEngine, ReentryFromPrerollRampsTheOnsetToo) {
  Rig r(true);
  test::Segments sig;
  // Tone until 200 ms; the stream stalls 200..300 ms; when it resumes (frame ~14400) 30 ms of silence, then the tone.
  sig.Add(0, 9600, 1000, 0.5f);
  sig.Add(9600, 14400 + 30 * kFramesPerMs + 12, 0, 0.0f);
  sig.Add(14400 + 30 * kFramesPerMs + 12, 10'000'000, 1000, 0.5f);
  Channel *ch = r.Add(&sig, Plain(), /*speex=*/true);
  r.Begin(0);
  r.RunTo(200 * kMs);
  r.stream(ch).producer->Pause();
  r.RunTo(300 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kPreroll);
  EXPECT_EQ(ch->underruns(), 1u);
  r.stream(ch).producer->Resume(r.now);
  r.RunTo(800 * kMs);
  EXPECT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_TRUE(ClickFrames(r.sink.audio).empty());
}

// ----------------------------------------------------------------------------------------------- silent bypass

// 100 ms of digital silence: the stream skips the resampler; when sound comes back the filter starts clean, so
// nothing from before the silence comes out at the onset (§9.3, §19.1: a tone, 200 ms of zeros, the tone again).
TEST(MixEngine, SilentBypassAndACleanFilterAtTheOnset) {
  Rig r(true);
  test::Segments sig;
  sig.Add(0, 48000, 440, 0.5f);               // 1 s of 440 Hz
  sig.Add(48000, 48000 + 9600, 0, 0.0f);      // 200 ms of zeros
  sig.Add(48000 + 9600, 200'000, 1000, 0.5f); // then 1 kHz
  Channel *ch = r.Add(&sig, Plain(), /*speex=*/true);
  r.Begin(0);
  r.RunTo(1200 * kMs); // 100 ms of the silence consumed: bypassed
  EXPECT_TRUE(ch->bypassed());
  r.RunTo(2000 * kMs);
  EXPECT_FALSE(ch->bypassed());
  EXPECT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_EQ(ch->underruns() + ch->trims(), 0u);
  // The onset: the first non-silent output frame after the silence (output frame 57600 is inside it).
  const auto &a = r.sink.audio;
  ASSERT_LT(std::fabs(a[2 * 57600]), kSilenceThreshold);
  std::size_t onset = 0;
  for (std::size_t f = 57600; f < a.size() / 2; ++f) {
    if (std::fabs(a[2 * f]) >= kSilenceThreshold) {
      onset = f;
      break;
    }
  }
  ASSERT_GT(onset, 57600u);
  // From the onset on there is 1 kHz and nothing of the 440 Hz from before the silence.
  const double hz1000 = ToneDb(a, onset, 960, 1000), hz440 = ToneDb(a, onset, 960, 440);
  EXPECT_GT(hz1000, -20.0);
  EXPECT_LT(hz440, hz1000 - 40.0);
  EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty());
}

// In the bypass the head age is corrected directly, up to 48 frames per tick, so a silent stream still sits at its
// target when its sound starts. (Its target is L: this stream started running in silence, so L was re-measured.)
TEST(MixEngine, SilentBypassHoldsTheAgeByDroppingAndPaddingZeros) {
  Rig r(false);
  test::Zeros zeros;
  ProducerConfig p = Plain();
  p.drift_ppm = 400;
  Channel *ch = r.Add(&zeros, p);
  r.Begin(0);
  r.RunTo(120'000 * kMs); // 2 minutes of silence with a fast device
  EXPECT_TRUE(ch->bypassed());
  EXPECT_EQ(r.engine.delay_ns(), 40 * kMs);
  EXPECT_NEAR(static_cast<double>(ch->age_ns()), 40.0 * kMs, 0.1 * kMs);
  EXPECT_EQ(ch->underruns() + ch->trims(), 0u);
}

// -------------------------------------------------------------------------------------- crossfades and cuts

// An engine switch: the new stream fades in over 20 ms while the old one fades out over the same 20 ms (equal gain),
// then the old one leaves (§6.5, §9.4).
TEST(MixEngine, CrossfadeIsEqualGainOverTwentyMs) {
  Rig r;
  r.controller = Frozen();
  test::Level a(0.25f), b(0.25f);
  Channel *old = r.Add(&a, Plain(), false, kGroupInclude);
  r.Begin(0);
  r.RunTo(200 * kMs);
  ProducerConfig pb = Plain();
  pb.start_ns = 200 * kMs;
  Channel *neu = r.Add(&b, pb, false, kGroupSingle, /*crossfade=*/true);
  uint64_t xf = 0;
  r.before_tick = [&] {
    if (xf == 0 && neu->state() == StreamState::kPreroll && neu->ring()->Buffered() >= 40 * kFramesPerMs) {
      xf = r.sink.chunks;
    }
  };
  r.RunTo(300 * kMs);
  ASSERT_GT(xf, 0u);
  EXPECT_TRUE(old->released());
  EXPECT_EQ(neu->state(), StreamState::kRunning);
  // Over the 20 ms the gains sum to 1; the new channel's own 5 ms onset ramp comes on top at its start.
  for (uint32_t j = 0; j < 2 * kChunkFrames; ++j) {
    const float t = static_cast<float>(j + 1) / 960.0f;
    const float onset = j < 240 ? static_cast<float>(j + 1) / 240.0f : 1.0f;
    ASSERT_NEAR(r.sink.audio[2 * (xf * kChunkFrames + j)], 0.25f * (1 - t) + 0.25f * t * onset, 1e-5f) << j;
  }
  EXPECT_EQ(Out(r, xf + 2, 0), 0.25f);
  EXPECT_EQ(r.sink.gap_count, 1u); // an engine switch keeps the grid
}

TEST(MixEngine, CrossfadeOfTwoTonesIsClickFree) {
  Rig r(true);
  test::Tone a(440, 0.5f), b(1000, 0.5f);
  r.Add(&a, Plain(), true, kGroupInclude);
  r.Begin(0);
  r.RunTo(300 * kMs);
  ProducerConfig pb = Plain();
  pb.start_ns = 300 * kMs;
  r.Add(&b, pb, true, kGroupSingle, true);
  r.RunTo(700 * kMs);
  EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty());
  // And back to the include set.
  ProducerConfig pc = Plain();
  pc.start_ns = 700 * kMs;
  r.Add(&a, pc, true, kGroupInclude, true);
  r.RunTo(1100 * kMs);
  EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty());
  EXPECT_TRUE(r.streams[1].channel->released());
}

// The fast path's exit (§6.4): the ring is cut at a QPC time with a 5 ms fade that ends there.
TEST(MixEngine, CutEndsAtTheCaptureTimeWithAFiveMsFade) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain(), false, kGroupSingle);
  r.Begin(0);
  r.RunTo(100 * kMs);
  int64_t head = 0;
  ASSERT_TRUE(ch->ring()->HeadQpc(&head));
  ch->CutAt(head + 20 * kMs); // 960 frames ahead of the head
  const uint64_t c = r.sink.chunks;
  r.RunFor(20 * kMs);
  std::vector<float> got;
  for (uint32_t j = 0; j < 2 * kChunkFrames; ++j) {
    got.push_back(r.sink.audio[2 * (c * kChunkFrames + j)]);
  }
  for (uint32_t j = 0; j < 960; ++j) {
    const int64_t left = 960 - static_cast<int64_t>(j);
    const float want = left >= 240 ? 0.5f : 0.5f * static_cast<float>(left) / 240.0f;
    ASSERT_NEAR(got[j], want, 1e-6f) << j;
  }
  EXPECT_TRUE(ch->released());
  EXPECT_EQ(r.engine.counters().fastpath_late_cuts, 0u);
  r.RunFor(10 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), 0.0f);
}

TEST(MixEngine, ALateCutFadesOutAtOnceAndCounts) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  Channel *ch = r.Add(&dc, Plain(), false, kGroupSingle, false, 77);
  r.Begin(0);
  r.RunTo(100 * kMs);
  int64_t head = 0;
  ASSERT_TRUE(ch->ring()->HeadQpc(&head));
  ch->CutAt(head - 5 * kMs); // already mixed
  r.RunFor(10 * kMs);
  const uint64_t c = r.sink.chunks - 1;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    const float want = j < 240 ? 0.5f * (1.0f - static_cast<float>(j + 1) / 240.0f) : 0.0f;
    ASSERT_NEAR(Out(r, c, j), want, 1e-5f) << j;
  }
  EXPECT_TRUE(ch->released());
  EXPECT_EQ(r.engine.counters().fastpath_late_cuts, 1u);
  auto ev = Events(r.engine);
  ASSERT_EQ(CountEvents(ev, MixEventCode::kFastpathLateCut), 1);
  for (const auto &e : ev) {
    if (e.code == MixEventCode::kFastpathLateCut) {
      EXPECT_EQ(e.tag, 77u);
    }
  }
}

TEST(MixEngine, CutsAreClickFree) {
  for (const int64_t ahead_ms : {20, 3, -5}) {
    Rig r(true);
    test::Tone tone(1000, 0.5f);
    Channel *ch = r.Add(&tone, Plain(), true, kGroupSingle);
    r.Begin(0);
    r.RunTo(300 * kMs);
    int64_t head = 0;
    ASSERT_TRUE(ch->ring()->HeadQpc(&head));
    ch->CutAt(head + ahead_ms * kMs);
    r.RunFor(50 * kMs);
    EXPECT_TRUE(ch->released());
    EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty()) << ahead_ms;
  }
}

// --------------------------------------------------------------------------------------- the bus

// Pause, sound off, resume: the bus stays at 0 until sound on (§9.6, §19.1). Each flag fades in 10 ms.
TEST(MixEngine, SilenceFlagsAreIndependent) {
  Rig r;
  r.controller = Frozen();
  test::Level dc(0.5f);
  r.Add(&dc, Plain());
  r.Begin(0);
  r.RunTo(100 * kMs);
  r.engine.SetPaused(true);
  r.RunFor(10 * kMs);
  uint64_t c = r.sink.chunks - 1;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_NEAR(Out(r, c, j), 0.5f * (1.0f - static_cast<float>(j + 1) / 480.0f), 1e-5f);
  }
  r.engine.SetSoundOff(true);
  r.RunFor(30 * kMs);
  r.engine.SetPaused(false); // resume: still off
  r.RunFor(50 * kMs);
  for (uint64_t k = c + 1; k < r.sink.chunks; ++k) {
    for (uint32_t j = 0; j < kChunkFrames; ++j) {
      ASSERT_EQ(Out(r, k, j), 0.0f);
    }
  }
  r.engine.SetMuted(true);
  r.engine.SetSoundOff(false); // muted: still silent
  r.RunFor(30 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), 0.0f);
  r.engine.SetMuted(false);
  r.RunFor(10 * kMs);
  c = r.sink.chunks - 1;
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_NEAR(Out(r, c, j), 0.5f * static_cast<float>(j + 1) / 480.0f, 1e-5f);
  }
  r.RunFor(10 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), 0.5f);
  EXPECT_EQ(r.sink.gap_count, 1u); // sound off and on keep the grid
}

TEST(MixEngine, SilenceFlagFadesAreClickFree) {
  Rig r(true);
  test::Tone tone(1000, 0.5f);
  r.Add(&tone, Plain(), true);
  r.Begin(0);
  r.RunTo(200 * kMs);
  r.engine.SetPaused(true);
  r.RunFor(50 * kMs);
  r.engine.SetPaused(false);
  r.RunFor(50 * kMs);
  r.engine.SetMuted(true);
  r.RunFor(35 * kMs);
  r.engine.SetMuted(false);
  r.RunFor(35 * kMs);
  r.engine.SetSoundOff(true);
  r.RunFor(15 * kMs);
  r.engine.SetSoundOff(false);
  r.RunFor(100 * kMs);
  EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty());
}

TEST(MixEngine, StartsAndStopsAreClickFree) {
  Rig r(true);
  test::Tone tone(1000, 0.5f);
  Channel *ch = r.Add(&tone, Plain(), true);
  r.Begin(0);
  r.RunTo(300 * kMs);
  r.stream(ch).producer->Pause(); // an underrun's fade
  r.RunFor(100 * kMs);
  r.stream(ch).producer->Resume(r.now); // a fade-in after the preroll
  r.RunFor(300 * kMs);
  EXPECT_EQ(ch->underruns(), 1u);
  // The mixer stalls 90 ms (no resync) and catches up: a trim's fade, then whatever the catch-up burst does.
  r.stream(ch).producer->RunUntil(r.now + 90 * kMs);
  r.PumpAt(r.now + 90 * kMs);
  r.RunFor(300 * kMs);
  EXPECT_EQ(ch->trims(), 1u);
  ch->Remove(); // the removal's fade
  r.RunFor(100 * kMs);
  EXPECT_TRUE(ch->released());
  EXPECT_TRUE(ClickFrames(AudioFrom(r, 4)).empty());
}

TEST(MixEngine, TheBusSoftClips) {
  Rig r;
  r.controller = Frozen();
  test::Level a(0.6f), b(0.6f);
  r.Add(&a, Plain());
  r.Add(&b, Plain());
  r.Begin(0);
  r.RunTo(100 * kMs);
  EXPECT_EQ(Out(r, r.sink.chunks - 1, 0), SoftClip(1.2f));
  EXPECT_LT(Out(r, r.sink.chunks - 1, 0), 1.0f);
}

// ---------------------------------------------------------------------------------------------------- levels

TEST(MixEngine, LevelsOverTheLast100Ms) {
  Rig r;
  r.controller = Frozen();
  test::Tone tone(1000, 0.5f);
  Channel *ch = r.Add(&tone, Plain(), false, kGroupInclude, false, 42);
  LevelsSnapshot snap;
  EXPECT_FALSE(r.engine.ReadLevels(&snap)); // nothing published yet
  r.Begin(0);
  r.RunTo(500 * kMs);
  ASSERT_TRUE(r.engine.ReadLevels(&snap));
  EXPECT_EQ(snap.seq, 51u);
  EXPECT_EQ(snap.t_ns, 500 * kMs);
  EXPECT_NEAR(snap.mix_peak_db, -6.02, 0.05);
  EXPECT_NEAR(snap.mix_rms_db, -9.03, 0.05);
  ASSERT_EQ(snap.count, 1u);
  EXPECT_EQ(snap.channels[0].tag, 42u);
  EXPECT_TRUE(snap.channels[0].running);
  EXPECT_NEAR(snap.channels[0].peak_db, -6.02, 0.05);
  EXPECT_NEAR(snap.channels[0].rms_db, -9.03, 0.05);
  // Paused: what friends hear is silence, after the 100 ms window has passed.
  r.engine.SetPaused(true);
  r.RunFor(120 * kMs);
  ASSERT_TRUE(r.engine.ReadLevels(&snap));
  EXPECT_EQ(snap.mix_peak_db, kLevelFloorDb);
  EXPECT_EQ(snap.channels[0].peak_db, kLevelFloorDb);
  EXPECT_EQ(snap.channels[0].rms_db, kLevelFloorDb);
  ch->Remove();
  r.RunFor(20 * kMs);
  ASSERT_TRUE(r.engine.ReadLevels(&snap));
  EXPECT_EQ(snap.count, 0u);
}

// -------------------------------------------------------------------------------------------------- timestamps

void ExpectGrid(const Rig &r, std::size_t from, std::size_t to) {
  for (std::size_t k = from + 1; k < to; ++k) {
    ASSERT_EQ(r.sink.times[k] - r.sink.times[k - 1], kChunkNs) << "chunk " << k;
  }
}

TEST(MixEngine, ChunksOnAnExactTenMsGridFromTheFirstWithL0) {
  Rig r(false);
  r.Begin(1'000'000'007); // any start
  r.RunTo(1'000'000'007 + 2000 * kMs);
  ASSERT_EQ(r.sink.chunks, 201u);
  for (std::size_t k = 0; k < r.sink.times.size(); ++k) {
    ASSERT_EQ(r.sink.times[k], 1'000'000'007 + static_cast<int64_t>(k) * kChunkNs - 50 * kMs);
  }
  ASSERT_EQ(r.sink.gap_count, 1u);
  EXPECT_EQ(r.sink.gap_at[0], 0u);
  EXPECT_EQ(r.sink.gap_ns[0], r.sink.times[0]);
}

// Sound from t = 0 (a stream already playing before the first chunk, with realistic jitter): chunk spacing exactly
// 10 ms through the first 2 s, stamps with L0 = 50 ms, and no OnGap after the first chunk (§19.1).
TEST(MixEngine, SoundFromTheStartKeepsL0AndTheGrid) {
  for (const int64_t offset_us : {0, 1700, 4300, 7900}) {
    Rig r(true);
    test::Tone tone(1000, 0.5f);
    ProducerConfig p;
    p.start_ns = -1000 * kMs + offset_us * 1000;
    p.latency_ns = 3 * kMs;
    p.jitter_ns = 3 * kMs;
    p.qpc_noise_ns = 200'000;
    p.seed = static_cast<uint64_t>(offset_us) + 1;
    Channel *ch = r.Add(&tone, p, true);
    r.Begin(0);
    r.RunTo(2000 * kMs);
    ASSERT_EQ(ch->state(), StreamState::kRunning);
    EXPECT_GT(r.sink.loud_chunks, 190u);
    ASSERT_EQ(r.sink.gap_count, 1u) << offset_us;
    for (std::size_t k = 0; k < r.sink.times.size(); ++k) {
      ASSERT_EQ(r.sink.times[k], static_cast<int64_t>(k) * kChunkNs - 50 * kMs);
    }
    EXPECT_EQ(r.engine.counters().reanchors, 0u);
    EXPECT_EQ(r.engine.delay_ns(), 50 * kMs);
  }
}

// A stream that starts running in silence re-measures L: the grid restarts once, L0 is replaced by the measured
// delay, and from then on a chunk's time is the QPC time at which its first frame was mixed (no device-latency
// term), within 1 ms, while the controller holds the stream at that delay.
TEST(MixEngine, SilenceReanchorReplacesL0WithTheMeasuredDelay) {
  Rig r(false);
  test::Segments sig;
  sig.Add(0, 24000, 0, 0.0f);             // 500 ms of silence
  sig.Add(24000, 50'000'000, 1000, 0.5f); // then a tone
  ProducerConfig p = Plain();
  p.start_ns = -3 * kMs; // packets end 3 ms before each tick: the head is 43 ms old when 40 ms are buffered
  Channel *ch = r.Add(&sig, p, false);
  int64_t worst = 0;
  std::size_t checked = 0;
  int64_t head_before = 0;
  bool have_head = false;
  r.before_tick = [&] {
    if (have_head && r.sink.gap_count == 2 && r.sink.chunks > r.sink.gap_at[1]) {
      const int64_t err = r.sink.times.back() - head_before;
      worst = std::max(worst, err < 0 ? -err : err);
      ++checked;
    }
    have_head = ch->state() == StreamState::kRunning && ch->ring()->HeadQpc(&head_before);
  };
  r.Begin(0);
  r.RunTo(180'000 * kMs); // 3 minutes
  ASSERT_EQ(r.sink.gap_count, 2u);
  EXPECT_EQ(r.sink.gap_at[1], 4u); // the chunk at which the stream started running
  EXPECT_EQ(r.sink.gap_ns[1], -3 * kMs);
  EXPECT_EQ(r.engine.counters().reanchors, 1u);
  EXPECT_EQ(r.engine.delay_ns(), 43 * kMs);
  EXPECT_EQ(r.sink.times[4] - r.sink.times[3], 17 * kMs); // the grid moved 7 ms forward, in silence
  ExpectGrid(r, 4, r.sink.times.size());
  EXPECT_GT(checked, 17000u);
  EXPECT_LE(worst, 1 * kMs);
  auto ev = Events(r.engine);
  EXPECT_EQ(CountEvents(ev, MixEventCode::kReanchor), 1);
}

// A re-measured L' more than 20 ms above L never gives a chunk time at or before the previous one: the new grid may
// not start earlier than the previous chunk + 10 ms, which here leaves L where it was (§10.3's clamp).
TEST(MixEngine, AReanchorNeverMovesTheGridBack) {
  Rig r(false);
  test::Zeros zeros;
  ProducerConfig p = Plain();
  p.latency_ns = 25 * kMs; // the head is about 70 ms old when it starts running
  Channel *ch = r.Add(&zeros, p, false);
  r.Begin(0);
  r.RunTo(1000 * kMs);
  ASSERT_EQ(ch->state(), StreamState::kRunning);
  EXPECT_EQ(r.sink.gap_count, 1u);
  EXPECT_EQ(r.engine.delay_ns(), 50 * kMs);
  for (std::size_t k = 1; k < r.sink.times.size(); ++k) {
    ASSERT_GT(r.sink.times[k], r.sink.times[k - 1]);
  }
  ExpectGrid(r, 0, r.sink.times.size());
}

// More than 100 ms behind: a resync and a new grid at now; up to 100 ms: a catch-up burst on the same grid (§9.1).
TEST(MixEngine, ResyncAfterAStallOfMoreThan100Ms) {
  Rig r(false);
  r.Begin(0);
  r.RunTo(100 * kMs);
  r.PumpAt(190 * kMs); // 90 ms late: catch up
  EXPECT_EQ(r.sink.chunks, 20u);
  EXPECT_EQ(r.sink.gap_count, 1u);
  ExpectGrid(r, 0, r.sink.times.size());
  r.PumpAt(450 * kMs); // the next chunk was due at 200 ms: 250 ms behind
  EXPECT_EQ(r.engine.counters().mixer_resyncs, 1u);
  ASSERT_EQ(r.sink.gap_count, 2u);
  EXPECT_EQ(r.sink.gap_ns[1], 450 * kMs - 50 * kMs);
  EXPECT_EQ(r.sink.times.back(), 400 * kMs);
  r.RunTo(1000 * kMs);
  ExpectGrid(r, r.sink.gap_at[1], r.sink.times.size());
  EXPECT_EQ(r.sink.times.back(), 950 * kMs); // the new grid: 450 ms + k * 10 ms - L
  auto ev = Events(r.engine);
  EXPECT_EQ(CountEvents(ev, MixEventCode::kMixerResync), 1);
}

// OnGap only at the first chunk, a resync and a silence re-anchor; never at a device change (a stream reopened), an
// engine switch or sound off and on (§10.4).
TEST(MixEngine, NoGapForDeviceChangesEngineSwitchesOrSoundOffAndOn) {
  Rig r(false);
  test::Tone tone(1000, 0.5f);
  Channel *a = r.Add(&tone, Plain(), false);
  r.Begin(0);
  r.RunTo(500 * kMs);
  // A device change: the stream is reopened (a new channel for the new endpoint).
  a->Remove();
  ProducerConfig p = Plain();
  p.start_ns = 500 * kMs;
  p.latency_ns = 1 * kMs; // the new device's packets come at another phase
  r.Add(&tone, p, false);
  r.RunTo(1000 * kMs);
  // An engine switch: the include set gives way to an EXCLUDE stream.
  p.start_ns = 1000 * kMs;
  p.latency_ns = 6 * kMs;
  Channel *x = r.Add(&tone, p, false, kGroupSingle, true);
  r.RunTo(1500 * kMs);
  // Sound off (the streams close) and on (new streams).
  r.engine.SetSoundOff(true);
  r.RunFor(10 * kMs);
  x->Remove();
  r.RunTo(2000 * kMs);
  r.engine.SetSoundOff(false);
  p.start_ns = 2000 * kMs;
  p.latency_ns = 2 * kMs;
  r.Add(&tone, p, false, kGroupSingle);
  r.RunTo(3000 * kMs);
  EXPECT_EQ(r.sink.gap_count, 1u);
  EXPECT_EQ(r.engine.counters().reanchors, 0u);
  ExpectGrid(r, 0, r.sink.times.size());
  EXPECT_GT(r.sink.loud_chunks, 200u);
}

// --------------------------------------------------------------------------------------------------- the engine

TEST(MixEngine, SixtyFourChannelsAtMostAndReleaseAll) {
  Rig r(false);
  test::Zeros zeros;
  for (uint32_t i = 0; i < kMaxMixChannels; ++i) {
    ASSERT_NE(r.Add(&zeros, Plain()), nullptr) << i;
  }
  EXPECT_EQ(r.Add(&zeros, Plain()), nullptr);
  r.Begin(0);
  r.RunTo(100 * kMs);
  r.engine.ReleaseAll();
  for (const auto &s : r.streams) {
    EXPECT_TRUE(s.channel->released());
    EXPECT_EQ(s.channel->state(), StreamState::kReleased);
  }
}

TEST(MixEngine, EventNamesAreTheReportsCodes) {
  EXPECT_STREQ(MixEventName(MixEventCode::kStreamRunning), "stream_running");
  EXPECT_STREQ(MixEventName(MixEventCode::kStreamRemoved), "stream_removed");
  EXPECT_STREQ(MixEventName(MixEventCode::kUnderrun), "underrun");
  EXPECT_STREQ(MixEventName(MixEventCode::kTrim), "trim");
  EXPECT_STREQ(MixEventName(MixEventCode::kMixerResync), "mixer_resync");
  EXPECT_STREQ(MixEventName(MixEventCode::kReanchor), "reanchor");
  EXPECT_STREQ(MixEventName(MixEventCode::kFastpathLateCut), "fastpath_late_cut");
}

TEST(MixEngine, AFullEventQueueDropsAndCounts) {
  Rig r(false);
  r.Begin(0);
  for (int i = 0; i < 300; ++i) {
    r.PumpAt(r.now + 200 * kMs); // a resync each time
  }
  EXPECT_EQ(r.engine.counters().mixer_resyncs, 300u);
  EXPECT_EQ(r.engine.dropped_events(), 300u - MixEngine::kEventCapacity);
  EXPECT_EQ(Events(r.engine).size(), MixEngine::kEventCapacity);
}

} // namespace
} // namespace im::audiomix
