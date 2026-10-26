// speexdsp as the mixer uses it (docs/m2/05-windows-audio.md §9.3): why the ratio's denominator is 62 500, that the
// model of its consumption is exact, and that ratio changes around 1:1 make no click.
#include <gtest/gtest.h>

#include <speex/speex_resampler.h>

#include <vector>

#include "im/audiomix/controller.h"
#include "im/audiomix/resampler.h"
#include "mix_test_support.h"

namespace im::audiomix {
namespace {

// §9.3's (1 000 000 + ppm) / 1 000 000: after some audio the filter phase is large, and the next rate change
// overflows speexdsp 1.2.1's 32-bit phase rescale (multiply_frac) and fails half done. This is the reason for
// kRatioDen (controller.h). The state is destroyed without processing: a resampler left like this crashed in a probe.
TEST(Speexdsp, AMillionDenominatorOverflowsTheRateChange) {
  int err = 0;
  SpeexResamplerState *st = speex_resampler_init_frac(2, 1'000'001, 1'000'000, kRate, kRate, 5, &err);
  ASSERT_NE(st, nullptr);
  ASSERT_EQ(speex_resampler_set_rate_frac(st, 1'000'000 - 3, 1'000'000, kRate, kRate), RESAMPLER_ERR_SUCCESS);
  std::vector<float> in(2 * 600, 0.1f), out(2 * kChunkFrames);
  spx_uint32_t il = 600, ol = kChunkFrames;
  speex_resampler_process_interleaved_float(st, in.data(), &il, out.data(), &ol);
  EXPECT_EQ(speex_resampler_set_rate_frac(st, 1'000'000 + 7, 1'000'000, kRate, kRate), RESAMPLER_ERR_OVERFLOW);
  speex_resampler_destroy(st);
}

// With kRatioDen every rate change of the controller's range succeeds, whatever the phase.
TEST(Speexdsp, TheRatioDenominatorNeverOverflows) {
  int err = 0;
  SpeexResamplerState *st = speex_resampler_init_frac(2, kRatioDen + 1, kRatioDen, kRate, kRate, 5, &err);
  ASSERT_NE(st, nullptr);
  test::Rng rng(3);
  std::vector<float> in(2 * 600, 0.1f), out(2 * kChunkFrames);
  for (int i = 0; i < 400; ++i) {
    auto steps = static_cast<int32_t>(rng.UniformInt(-kMaxSteps, kMaxSteps));
    if (steps == 0) {
      steps = 1;
    }
    ASSERT_EQ(speex_resampler_set_rate_frac(st, static_cast<spx_uint32_t>(kRatioDen + steps), kRatioDen, kRate, kRate),
              RESAMPLER_ERR_SUCCESS)
        << "change " << i;
    spx_uint32_t il = 600, ol = kChunkFrames;
    speex_resampler_process_interleaved_float(st, in.data(), &il, out.data(), &ol);
    ASSERT_EQ(ol, kChunkFrames);
  }
  speex_resampler_destroy(st);
}

// RateModel consumes exactly what speexdsp consumes: random ratios (±1 step included), random amounts of input
// (enough, and too little), resets.
TEST(RateModel, ConsumesExactlyLikeSpeexdsp) {
  std::unique_ptr<Resampler> speex;
  ASSERT_EQ(NewSpeexResampler(&speex), IM_OK);
  RateModel model;
  test::Rng rng(11);
  std::vector<float> in(2 * 600, 0.25f), out(2 * kChunkFrames);
  int32_t steps = 1;
  for (int call = 0; call < 3000; ++call) {
    if (call % 10 == 0) {
      const int64_t pick = rng.UniformInt(0, 5);
      steps = pick == 0 ? 1 : (pick == 1 ? -1 : static_cast<int32_t>(rng.UniformInt(-kMaxSteps, kMaxSteps)));
      if (steps == 0) {
        steps = -1;
      }
      speex->SetSteps(steps);
      model.SetSteps(steps);
    }
    if (call % 997 == 0) {
      speex->Reset();
      model.Reset();
    }
    const auto avail =
        static_cast<uint32_t>(rng.UniformInt(0, 9) == 0 ? rng.UniformInt(0, 479) : rng.UniformInt(480, 600));
    const uint32_t need = model.InputFor(kChunkFrames);
    uint32_t su = 0, sm = 0, mu = 0, mm = 0;
    speex->Process(in.data(), avail, out.data(), kChunkFrames, &su, &sm);
    model.Run(nullptr, avail, nullptr, kChunkFrames, &mu, &mm);
    ASSERT_EQ(mu, su) << "call " << call << " steps " << steps << " avail " << avail;
    ASSERT_EQ(mm, sm) << "call " << call;
    if (avail >= need) {
      ASSERT_EQ(sm, kChunkFrames) << "InputFor said " << need << " is enough";
    }
    if (avail + 1 == need) {
      ASSERT_LT(sm, kChunkFrames) << "InputFor said " << need << " is needed";
    }
  }
}

// The model resampler is the model with sample-and-hold output: at a ratio of exactly +1 step it copies its input.
TEST(RateModel, ModelResamplerHoldsSamples) {
  auto r = NewModelResampler();
  std::vector<float> in(2 * 600), out(2 * kChunkFrames);
  for (uint32_t i = 0; i < 600; ++i) {
    in[2 * i] = static_cast<float>(i);
    in[2 * i + 1] = -static_cast<float>(i);
  }
  uint32_t used = 0, made = 0;
  r->Process(in.data(), 600, out.data(), kChunkFrames, &used, &made);
  ASSERT_EQ(made, kChunkFrames);
  EXPECT_EQ(used, kChunkFrames); // +16 ppm: the extra frame is still a fraction
  for (uint32_t j = 0; j < kChunkFrames; ++j) {
    ASSERT_EQ(out[2 * j], static_cast<float>(j));
    ASSERT_EQ(out[2 * j + 1], -static_cast<float>(j));
  }
}

// Ratio changes through -1 and +1 step around 1:1 (and a sweep) on a -6 dBFS 1 kHz tone: no click (§19.1).
TEST(SpeexResampler, RatioSweepAroundOneHasNoClick) {
  std::unique_ptr<Resampler> r;
  ASSERT_EQ(NewSpeexResampler(&r), IM_OK);
  test::Tone tone(1000, 0.5f);
  std::vector<float> in(2 * 600), chunk(2 * kChunkFrames), all;
  uint64_t pos = 0;
  constexpr int kChunks = 500; // 5 s
  for (int k = 0; k < kChunks; ++k) {
    if (k % 10 == 0) {
      const int phase = (k / 10) % 12;
      // +1/-1 alternation, then a sweep -3 .. +3 and back
      static constexpr int32_t kSeq[12] = {1, -1, 1, -1, -2, -3, -2, -1, 1, 2, 3, 2};
      r->SetSteps(kSeq[phase]);
    }
    tone.Fill(pos, 600, in.data());
    uint32_t used = 0, made = 0;
    r->Process(in.data(), 600, chunk.data(), kChunkFrames, &used, &made);
    ASSERT_EQ(made, kChunkFrames);
    pos += used;
    all.insert(all.end(), chunk.begin(), chunk.end());
  }
  // From 20 ms on (the filter's start from silence is the stream's onset, the mixer's fade-in covers it).
  const std::vector<float> steady(all.begin() + 2 * 960, all.end());
  EXPECT_TRUE(test::ClickFrames(steady).empty());
  // The detector does see a click: a 2-frame jump of the same tone.
  std::vector<float> broken = steady;
  for (std::size_t i = 2 * 24000; i < broken.size() - 4; ++i) {
    broken[i] = broken[i + 4];
  }
  EXPECT_FALSE(test::ClickFrames(broken).empty());
}

} // namespace
} // namespace im::audiomix
