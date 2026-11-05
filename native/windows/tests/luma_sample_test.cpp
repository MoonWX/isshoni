// luma_sample_test (docs/m2/04-windows-engine.md §5.8; README WF16): the luma tap's portable parts, session/
// luma_sample.h. The fixed region (the centered box of up to 256 pixels a side), BT.709 luma of BGRA pixels and its
// rounded mean, and the IM_PACKET_DEBUG payload's bytes, which internal/media/native's luma.go decodes (its
// TestLumaPayloadGolden reads the same 48 bytes as kGoldenHex below). Portable: no Windows header, any build.
#include <gtest/gtest.h>

#include <array>
#include <cstdint>
#include <span>
#include <vector>

#include "session/luma_sample.h"

namespace {

namespace luma = im::session::luma;

TEST(LumaSample, CenterBoxIsTheCenteredSquareOfUpTo256Pixels) {
  const luma::Region r = luma::CenterBox(1920, 1080);
  EXPECT_EQ(r.x, 832u);
  EXPECT_EQ(r.y, 412u);
  EXPECT_EQ(r.width, 256u);
  EXPECT_EQ(r.height, 256u);
  // Inside avsync's 1280×720 flash window centered on a 1080p monitor ((320, 180) to (1600, 900)).
  EXPECT_GE(r.x, 320u);
  EXPECT_LE(r.x + r.width, 1600u);
  EXPECT_GE(r.y, 180u);
  EXPECT_LE(r.y + r.height, 900u);

  const luma::Region small = luma::CenterBox(200, 100);
  EXPECT_EQ(small.x, 0u);
  EXPECT_EQ(small.y, 0u);
  EXPECT_EQ(small.width, 200u);
  EXPECT_EQ(small.height, 100u);

  const luma::Region odd = luma::CenterBox(641, 361);
  EXPECT_EQ(odd.x, 192u);
  EXPECT_EQ(odd.y, 52u);
  EXPECT_FALSE(odd.empty());
  EXPECT_TRUE(luma::CenterBox(0, 720).empty());

  // Rotated content (90°: the sides swap) gives the same square.
  const luma::Region a = luma::CenterBox(3840, 2160);
  const luma::Region b = luma::CenterBox(2160, 3840);
  EXPECT_EQ(a.width, b.height);
  EXPECT_EQ(a.height, b.width);
}

std::vector<uint8_t> Bgra(uint32_t pixels, uint8_t r, uint8_t g, uint8_t b) {
  std::vector<uint8_t> row(static_cast<std::size_t>(pixels) * 4);
  for (uint32_t i = 0; i < pixels; ++i) {
    row[i * 4 + 0] = b;
    row[i * 4 + 1] = g;
    row[i * 4 + 2] = r;
    row[i * 4 + 3] = 0xFF;
  }
  return row;
}

TEST(LumaSample, MeanIsBt709LumaOfTheBgraPixels) {
  {
    luma::Accumulator acc;
    EXPECT_EQ(acc.Mean(), 0u) << "no pixels";
    const auto white = Bgra(256, 255, 255, 255);
    for (int y = 0; y < 256; ++y) {
      acc.AddBgraRow(white.data(), 256);
    }
    EXPECT_EQ(acc.count(), 65536u);
    EXPECT_EQ(acc.Mean(), luma::kMeanScale);
  }
  {
    luma::Accumulator acc;
    const auto black = Bgra(16, 0, 0, 0);
    acc.AddBgraRow(black.data(), 16);
    EXPECT_EQ(acc.Mean(), 0u);
  }
  // Pure red, green and blue: the weights 0.2126, 0.7152 and 0.0722 (rounded to 1/65535).
  struct Primary {
    uint8_t r, g, b;
    uint32_t mean;
  };
  const Primary primaries[] = {
      {255, 0, 0, 13933},
      {0, 255, 0, 46871},
      {0, 0, 255, 4732},
  };
  for (const Primary &c : primaries) {
    luma::Accumulator acc;
    const auto px = Bgra(8, c.r, c.g, c.b);
    acc.AddBgraRow(px.data(), 8);
    EXPECT_EQ(acc.Mean(), c.mean) << int{c.r} << "," << int{c.g} << "," << int{c.b};
  }
  // Half white, half black: 0.5 (32767.5 rounds up).
  luma::Accumulator half;
  const auto white = Bgra(4, 255, 255, 255);
  const auto black = Bgra(4, 0, 0, 0);
  half.AddBgraRow(white.data(), 4);
  half.AddBgraRow(black.data(), 4);
  EXPECT_EQ(half.Mean(), 32768u);
  // The alpha byte is ignored.
  auto grey = Bgra(2, 128, 128, 128);
  grey[3] = 0;
  luma::Accumulator g;
  g.AddBgraRow(grey.data(), 2);
  EXPECT_EQ(g.Mean(), (128u * 65535u + 127u) / 255u);
}

// The payload of luma_sample.h's table for GoldenSample, field by field (magic "LUMA", version 1, capture_ts_ns
// 0x0102030405060708, mean 32768, source 1920 × 1080, region (832, 412) 256 × 256, missed 3); internal/media/native's
// luma_test.go decodes the same hex.
constexpr char kGoldenHex[] = "4c554d41"
                              "01000000"
                              "0807060504030201"
                              "00800000"
                              "80070000"
                              "38040000"
                              "40030000"
                              "9c010000"
                              "00010000"
                              "00010000"
                              "03000000";

std::array<uint8_t, luma::kPayloadBytes> Golden() {
  static_assert(sizeof kGoldenHex == 2 * luma::kPayloadBytes + 1, "48 bytes of hex");
  const auto nibble = [](char c) { return static_cast<uint8_t>(c <= '9' ? c - '0' : c - 'a' + 10); };
  std::array<uint8_t, luma::kPayloadBytes> out{};
  for (std::size_t i = 0; i < out.size(); ++i) {
    out[i] = static_cast<uint8_t>(nibble(kGoldenHex[2 * i]) << 4 | nibble(kGoldenHex[2 * i + 1]));
  }
  return out;
}

luma::Sample GoldenSample() {
  luma::Sample s;
  s.capture_ts_ns = 0x0102030405060708;
  s.mean = 32768;
  s.source_width = 1920;
  s.source_height = 1080;
  s.region = luma::CenterBox(1920, 1080);
  s.missed = 3;
  return s;
}

TEST(LumaSample, PayloadBytes) {
  const std::array<uint8_t, luma::kPayloadBytes> kGolden = Golden();
  std::array<uint8_t, luma::kPayloadBytes> out{};
  luma::Encode(GoldenSample(), out);
  EXPECT_EQ(out, kGolden);

  luma::Sample back;
  ASSERT_TRUE(luma::Decode(kGolden, &back));
  const luma::Sample want = GoldenSample();
  EXPECT_EQ(back.capture_ts_ns, want.capture_ts_ns);
  EXPECT_EQ(back.mean, want.mean);
  EXPECT_EQ(back.source_width, want.source_width);
  EXPECT_EQ(back.source_height, want.source_height);
  EXPECT_EQ(back.region.x, want.region.x);
  EXPECT_EQ(back.region.y, want.region.y);
  EXPECT_EQ(back.region.width, want.region.width);
  EXPECT_EQ(back.region.height, want.region.height);
  EXPECT_EQ(back.missed, want.missed);

  // A capture time with the top bit set survives the round trip (int64 as two's complement).
  luma::Sample neg = want;
  neg.capture_ts_ns = -2;
  luma::Encode(neg, out);
  ASSERT_TRUE(luma::Decode(out, &back));
  EXPECT_EQ(back.capture_ts_ns, -2);
}

TEST(LumaSample, DecodeRefusesOtherPayloads) {
  const std::array<uint8_t, luma::kPayloadBytes> kGolden = Golden();
  luma::Sample s;
  EXPECT_FALSE(luma::Decode(std::span<const uint8_t>(kGolden.data(), kGolden.size() - 1), &s)) << "short";
  std::vector<uint8_t> longer(kGolden.begin(), kGolden.end());
  longer.push_back(0);
  EXPECT_FALSE(luma::Decode(longer, &s)) << "long";
  auto bad = kGolden;
  bad[0] = 'X';
  EXPECT_FALSE(luma::Decode(bad, &s)) << "magic";
  bad = kGolden;
  bad[4] = 2;
  EXPECT_FALSE(luma::Decode(bad, &s)) << "version";
  bad = kGolden;
  bad[18] = 1; // mean 65536 + 32768
  EXPECT_FALSE(luma::Decode(bad, &s)) << "mean above 65535";
}

} // namespace
