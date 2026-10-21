// The SPS parser and the emulation-prevention transforms (docs/m2/04-windows-engine.md §9.4, §18.1): the size after
// cropping (1920×1088 coded → 1080), profile, constraint flags, level, emulation prevention, the error cases, the
// size limit on the RBSP, the normalizer's constraint-byte rewrite, and the sps_fuzz properties over the seeds and a
// few thousand mutations of them.
#include <gtest/gtest.h>

#include <filesystem>
#include <fstream>
#include <iterator>
#include <vector>

#include "fuzz/mutator.h"
#include "fuzz/sps_props.h"
#include "h264_test_streams.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"

namespace {

using im::h264::BitReader;
using im::h264::kMaxSpsBytes;
using im::h264::kMaxSpsRbspBytes;
using im::h264::ParseSps;
using im::h264::SetConstraintByte;
using im::h264::Sps;
using im::h264::SpsError;
using im::h264::fuzzing::CheckSps;
using im::h264::fuzzing::SameSps;
using im::h264::testing::BitWriter;
using im::h264::testing::Bytes;
using im::h264::testing::MakeSps;
using im::h264::testing::SpsParams;

Sps MustParse(const Bytes &nal) {
  Sps s;
  EXPECT_EQ(ParseSps(nal, &s), SpsError::kOk);
  return s;
}

// The RBSP of an SPS NAL unit, header byte excluded.
Bytes RbspOf(const Bytes &unit) {
  Bytes rbsp(unit.size());
  rbsp.resize(im::h264::UnescapeRbsp(std::span<const uint8_t>(unit).subspan(1), rbsp));
  return rbsp;
}

// An SPS NAL unit from its RBSP, canonically escaped.
Bytes SpsUnit(const Bytes &rbsp) {
  Bytes nal(1 + im::h264::MaxEscapedSize(rbsp.size()));
  nal[0] = 0x67;
  nal.resize(1 + im::h264::EscapeRbsp(rbsp, std::span<uint8_t>(nal).subspan(1)));
  return nal;
}

// A Constrained Baseline SPS whose constraint byte the CB rewrite changes (40 → e0); level 40, 640×360.
Bytes BaselineSps() {
  SpsParams p;
  p.profile_idc = 66;
  p.constraint = 0x40;
  p.width = 640;
  p.height = 360;
  return MakeSps(p);
}

TEST(Sps, HighWithCropping) {
  SpsParams p;
  p.profile_idc = 100;
  p.constraint = 0x00;
  p.level_idc = 42;
  p.width = 1920;
  p.height = 1080;
  const Sps s = MustParse(MakeSps(p));
  EXPECT_EQ(s.profile_idc, 100u);
  EXPECT_EQ(s.constraint_flags, 0x00u);
  EXPECT_EQ(s.level_idc, 42u);
  EXPECT_EQ(s.coded_width, 1920u);
  EXPECT_EQ(s.coded_height, 1088u) << "1080 rounds up to 68 macroblock rows";
  EXPECT_EQ(s.crop_bottom, 4u) << "4:2:0 frame: one crop unit is 2 rows";
  EXPECT_EQ(s.width, 1920u);
  EXPECT_EQ(s.height, 1080u);
  EXPECT_EQ(s.profile_key(), 0x6400u);
  EXPECT_EQ(s.chroma_format_idc, 1u);
  EXPECT_EQ(s.bit_depth_luma, 8u);
  EXPECT_TRUE(s.frame_mbs_only);
}

TEST(Sps, ConstrainedBaseline) {
  SpsParams p;
  p.profile_idc = 66;
  p.constraint = 0xE0;
  p.level_idc = 31;
  p.width = 1280;
  p.height = 720;
  const Sps s = MustParse(MakeSps(p));
  EXPECT_EQ(s.profile_key(), 0x42E0u);
  EXPECT_EQ(s.level_idc, 31u);
  EXPECT_EQ(s.width, 1280u);
  EXPECT_EQ(s.height, 720u);
  EXPECT_EQ(s.crop_bottom, 0u);
}

TEST(Sps, ConstraintFlags) {
  const uint8_t constraints[] = {0x00, 0x40, 0xC0, 0xE0, 0x0C, 0xFC};
  for (uint8_t c : constraints) {
    SpsParams p;
    p.profile_idc = static_cast<uint8_t>(c == 0x0C ? 100 : 66);
    p.constraint = c;
    EXPECT_EQ(MustParse(MakeSps(p)).constraint_flags, c);
  }
}

TEST(Sps, OddAndSmallSizes) {
  struct Case {
    uint32_t w, h;
  };
  for (const Case c : {Case{640, 360}, Case{1000, 750}, Case{2224, 930}, Case{120, 64}, Case{16, 16}, Case{4096, 2160},
                       Case{3840, 2160}, Case{1366, 768}}) {
    for (uint8_t profile : {uint8_t{66}, uint8_t{100}}) {
      SpsParams p;
      p.profile_idc = profile;
      p.width = c.w;
      p.height = c.h;
      const Sps s = MustParse(MakeSps(p));
      EXPECT_EQ(s.width, c.w) << c.w << "x" << c.h;
      EXPECT_EQ(s.height, c.h) << c.w << "x" << c.h;
      EXPECT_EQ(s.coded_width % 16, 0u);
    }
  }
}

TEST(Sps, ScalingMatrixAndPocTypes) {
  SpsParams p;
  p.scaling_matrix = true;
  p.width = 1920;
  p.height = 1080;
  EXPECT_EQ(MustParse(MakeSps(p)).height, 1080u);
  p.scaling_matrix = false;
  p.poc_type = 1;
  EXPECT_EQ(MustParse(MakeSps(p)).height, 1080u);
  p.poc_type = 2;
  EXPECT_EQ(MustParse(MakeSps(p)).height, 1080u);
  p.chroma_format_idc = 3;
  p.scaling_matrix = true;
  const Sps s = MustParse(MakeSps(p));
  EXPECT_EQ(s.chroma_format_idc, 3u);
  EXPECT_EQ(s.height, 1080u);
}

TEST(Sps, FieldCoding) {
  SpsParams p;
  p.frame_mbs_only = false;
  p.width = 1920;
  p.height = 1080;
  const Sps s = MustParse(MakeSps(p));
  EXPECT_FALSE(s.frame_mbs_only);
  EXPECT_EQ(s.coded_height, 1088u) << "34 map units of 32 rows";
  EXPECT_EQ(s.crop_bottom, 2u) << "field 4:2:0: one crop unit is 4 rows";
  EXPECT_EQ(s.height, 1080u);
}

TEST(Sps, VuiFlag) {
  SpsParams p;
  p.vui = true;
  EXPECT_TRUE(MustParse(MakeSps(p)).vui_present);
}

TEST(Sps, EmulationPrevention) {
  // poc type 1 with offset_for_ref_frame = 2^30: its Exp-Golomb code is 31 zero bits, a one and 31 zero bits, so the
  // RBSP holds 00 00 0x sequences that the escaped unit must carry as 00 00 03 0x.
  BitWriter w;
  w.Bits(66, 8);
  w.Bits(0xE0, 8);
  w.Bits(31, 8);
  w.Ue(0);       // sps_id
  w.Ue(0);       // log2_max_frame_num_minus4
  w.Ue(1);       // poc type 1
  w.Bit(0);      // delta_pic_order_always_zero_flag
  w.Se(0);       // offset_for_non_ref_pic
  w.Se(0);       // offset_for_top_to_bottom_field
  w.Ue(1);       // one offset_for_ref_frame
  w.Se(1 << 30); // the long zero runs
  w.Ue(1);       // max_num_ref_frames
  w.Bit(0);      // gaps
  w.Ue(79);      // 1280 wide
  w.Ue(44);      // 720 high
  w.Bit(1);      // frame_mbs_only
  w.Bit(1);      // direct_8x8
  w.Bit(0);      // no cropping
  w.Bit(0);      // no VUI
  const Bytes rbsp = w.Finish();
  Bytes nal(1 + im::h264::MaxEscapedSize(rbsp.size()));
  nal[0] = 0x67;
  nal.resize(1 + im::h264::EscapeRbsp(rbsp, std::span<uint8_t>(nal).subspan(1)));
  ASSERT_GT(nal.size(), rbsp.size() + 1) << "the escaped unit must hold emulation-prevention bytes";
  bool has_epb = false;
  for (std::size_t i = 0; i + 2 < nal.size(); ++i) {
    EXPECT_FALSE(nal[i] == 0 && nal[i + 1] == 0 && nal[i + 2] <= 2) << "at " << i;
    has_epb = has_epb || (nal[i] == 0 && nal[i + 1] == 0 && nal[i + 2] == 3);
  }
  EXPECT_TRUE(has_epb);
  const Sps s = MustParse(nal);
  EXPECT_EQ(s.profile_key(), 0x42E0u);
  EXPECT_EQ(s.level_idc, 31u);
  EXPECT_EQ(s.width, 1280u);
  EXPECT_EQ(s.height, 720u);
}

TEST(Sps, Errors) {
  Sps s;
  EXPECT_EQ(ParseSps({}, &s), SpsError::kNotSps);
  EXPECT_EQ(ParseSps(Bytes{0x68, 0x42}, &s), SpsError::kNotSps) << "a PPS";
  EXPECT_EQ(ParseSps(Bytes{0xE7, 0x42, 0xE0, 0x1F}, &s), SpsError::kNotSps) << "forbidden bit";
  EXPECT_EQ(ParseSps(Bytes{0x67, 0x42, 0xE0}, &s), SpsError::kTruncated);
  Bytes full = MakeSps(SpsParams{});
  // Cut by two bytes or more (the last byte may hold nothing but the stop bit).
  for (std::size_t n = 1; n + 2 <= full.size(); ++n) {
    const Bytes cut(full.begin(), full.begin() + static_cast<std::ptrdiff_t>(n));
    EXPECT_NE(ParseSps(cut, &s), SpsError::kOk) << "cut to " << n << " bytes";
  }
  Bytes huge(im::h264::kMaxSpsBytes + 1, 0x55);
  huge[0] = 0x67;
  EXPECT_EQ(ParseSps(huge, &s), SpsError::kTooLarge);

  // Values out of range.
  const auto with = [](auto &&fill) {
    BitWriter w;
    fill(w);
    Bytes rbsp = w.Finish();
    Bytes nal(1 + im::h264::MaxEscapedSize(rbsp.size()));
    nal[0] = 0x67;
    nal.resize(1 + im::h264::EscapeRbsp(rbsp, std::span<uint8_t>(nal).subspan(1)));
    Sps out;
    return ParseSps(nal, &out);
  };
  EXPECT_EQ(with([](BitWriter &w) {
              w.Bits(66, 8);
              w.Bits(0, 8);
              w.Bits(30, 8);
              w.Ue(32); // sps_id > 31
              w.Bits(0xFFFF, 16);
            }),
            SpsError::kBadValue);
  EXPECT_EQ(with([](BitWriter &w) {
              w.Bits(100, 8);
              w.Bits(0, 8);
              w.Bits(40, 8);
              w.Ue(0);
              w.Ue(4); // chroma_format_idc > 3
              w.Bits(0xFFFF, 16);
            }),
            SpsError::kBadValue);
  EXPECT_EQ(with([](BitWriter &w) {
              w.Bits(66, 8);
              w.Bits(0, 8);
              w.Bits(30, 8);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Bit(0);
              w.Ue(0); // 16 wide
              w.Ue(0); // 16 high
              w.Bit(1);
              w.Bit(1);
              w.Bit(1); // cropping more than the picture
              w.Ue(4);
              w.Ue(4);
              w.Ue(0);
              w.Ue(0);
              w.Bit(0);
            }),
            SpsError::kBadValue);
  EXPECT_EQ(with([](BitWriter &w) {
              w.Bits(66, 8);
              w.Bits(0, 8);
              w.Bits(30, 8);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Ue(0);
              w.Bit(0);
              w.Ue(2048); // 32784 wide: above the parser's 32768
              w.Ue(0);
              w.Bit(1);
              w.Bit(1);
              w.Bit(0);
              w.Bit(0);
            }),
            SpsError::kBadValue);
}

TEST(Sps, BitReaderExpGolomb) {
  BitWriter w;
  const uint32_t ues[] = {0, 1, 2, 3, 7, 8, 254, 255, 65535, 0x7FFFFFFFu, 0xFFFFFFFEu};
  const int32_t ses[] = {0, 1, -1, 2, -2, 127, -128, 0x7FFFFFFF, -0x7FFFFFFF};
  for (uint32_t v : ues) {
    w.Ue(v);
  }
  for (int32_t v : ses) {
    w.Se(v);
  }
  const Bytes data = w.Finish();
  BitReader br(data);
  for (uint32_t v : ues) {
    uint32_t got = 0;
    ASSERT_TRUE(br.Ue(&got));
    EXPECT_EQ(got, v);
  }
  for (int32_t v : ses) {
    int32_t got = 0;
    ASSERT_TRUE(br.Se(&got));
    EXPECT_EQ(got, v);
  }
  EXPECT_FALSE(br.overrun());

  // 33 leading zeros: longer than any 32-bit value.
  const Bytes too_long = {0x00, 0x00, 0x00, 0x00, 0x40};
  BitReader br2(too_long);
  uint32_t v = 0;
  EXPECT_FALSE(br2.Ue(&v));
  EXPECT_FALSE(br2.overrun());
  const Bytes cut_suffix = {0x00, 0x01};
  BitReader br3(cut_suffix);
  EXPECT_FALSE(br3.Ue(&v)) << "the suffix runs past the end";
  EXPECT_TRUE(br3.overrun());
}

TEST(Sps, RbspTransforms) {
  const struct {
    Bytes rbsp, escaped;
    bool round_trip;
  } cases[] = {
      {{0x00, 0x00, 0x01}, {0x00, 0x00, 0x03, 0x01}, true},
      {{0x00, 0x00, 0x02}, {0x00, 0x00, 0x03, 0x02}, true},
      {{0x00, 0x00, 0x03}, {0x00, 0x00, 0x03, 0x03}, true},
      {{0x00, 0x00, 0x04}, {0x00, 0x00, 0x04}, true},
      {{0x65, 0x00, 0x00, 0x00, 0x00}, {0x65, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03}, true}, // cabac_zero_words: final 03
      {{0x67, 0x42, 0x00, 0x00, 0x00, 0x00, 0x01}, {0x67, 0x42, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03, 0x01}, true},
      {{0x01, 0x02}, {0x01, 0x02}, true},
      // An RBSP that ends in an odd run of zeros isn't valid H.264 (only whole cabac_zero_words may end one) and has
      // no Annex B form: it still escapes without a start-code-like sequence and without a trailing zero.
      {{0x00, 0x00, 0x00}, {0x00, 0x00, 0x03, 0x00, 0x03}, false},
  };
  for (const auto &c : cases) {
    Bytes esc(im::h264::MaxEscapedSize(c.rbsp.size()));
    esc.resize(im::h264::EscapeRbsp(c.rbsp, esc));
    EXPECT_EQ(esc, c.escaped);
    if (c.round_trip) {
      Bytes back(esc.size());
      back.resize(im::h264::UnescapeRbsp(esc, back));
      EXPECT_EQ(back, c.rbsp);
    }
  }
  // A non-canonical escape (03 before a byte > 3) unescapes like a canonical one and re-escapes canonically.
  const Bytes odd = {0x11, 0x00, 0x00, 0x03, 0x04};
  Bytes raw(odd.size());
  raw.resize(im::h264::UnescapeRbsp(odd, raw));
  EXPECT_EQ(raw, (Bytes{0x11, 0x00, 0x00, 0x04}));
}

TEST(Sps, SizeLimitIsOnTheRbsp) {
  // kMaxSpsRbspBytes bounds the RBSP, so an SPS whose tail carries 00 00 02 without emulation prevention parses like
  // its canonical form, which is a third longer; one RBSP byte more and both are too large.
  const Bytes head = RbspOf(BaselineSps());
  const Sps want = MustParse(BaselineSps());
  for (const std::size_t size : {kMaxSpsRbspBytes, kMaxSpsRbspBytes + 1}) {
    Bytes rbsp = head;
    while (rbsp.size() + 3 <= size) {
      rbsp.insert(rbsp.end(), {0x00, 0x00, 0x02});
    }
    rbsp.resize(size, 0x80);
    Bytes raw = {0x67};
    raw.insert(raw.end(), rbsp.begin(), rbsp.end());
    const Bytes canonical = SpsUnit(rbsp);
    ASSERT_GT(canonical.size(), raw.size() + 1000);
    ASSERT_LE(canonical.size(), kMaxSpsBytes);
    Sps a;
    Sps b;
    const SpsError ea = ParseSps(raw, &a);
    const SpsError eb = ParseSps(canonical, &b);
    if (size <= kMaxSpsRbspBytes) {
      ASSERT_EQ(ea, SpsError::kOk);
      ASSERT_EQ(eb, SpsError::kOk);
      EXPECT_TRUE(SameSps(a, want, false));
      EXPECT_TRUE(SameSps(b, want, false));
    } else {
      EXPECT_EQ(ea, SpsError::kTooLarge);
      EXPECT_EQ(eb, SpsError::kTooLarge);
    }
  }
}

TEST(Sps, NearTheLimitWithoutEmulationPrevention) {
  // A 4096-byte SPS whose tail, which the parser never reads, holds 00 00 02 without emulation prevention (the sps_fuzz
  // seed near_limit_unescaped). Re-escaping it gives 5458 bytes: with a limit on the escaped size the canonical form
  // and the normalizer's rewrite stopped parsing.
  Bytes unit = {0x67, 0x42, 0x40, 0x28, 0xED, 0x00, 0xA0, 0x0B, 0x72};
  for (int i = 0; i < 1362; ++i) {
    unit.insert(unit.end(), {0x00, 0x00, 0x02});
  }
  unit.push_back(0x05);
  ASSERT_EQ(unit.size(), 4096u);
  const Sps s = MustParse(unit);
  const Bytes canonical = SpsUnit(RbspOf(unit));
  EXPECT_EQ(canonical.size(), 5458u);
  Sps again;
  ASSERT_EQ(ParseSps(canonical, &again), SpsError::kOk);
  EXPECT_TRUE(SameSps(s, again, false));
  Bytes rewritten(kMaxSpsBytes);
  rewritten.resize(SetConstraintByte(unit, 0xE0, rewritten));
  Sps after;
  ASSERT_EQ(ParseSps(rewritten, &after), SpsError::kOk);
  EXPECT_EQ(after.profile_key(), 0x42E0u);
  EXPECT_TRUE(SameSps(s, after, true));
  const char *err = CheckSps(unit);
  EXPECT_EQ(err, nullptr) << err;
}

TEST(Sps, SetConstraintByte) {
  // A canonical unit changes in that one byte.
  const Bytes unit = BaselineSps();
  Bytes out(kMaxSpsBytes);
  out.resize(SetConstraintByte(unit, 0xE0, out));
  Bytes expected = unit;
  expected[2] = 0xE0;
  EXPECT_EQ(out, expected);

  // An RBSP of exactly kMaxSpsRbspBytes that ends in an odd run of zero bytes (the unit ends in 00 00 00 03: three
  // zeros of data and an emulation-prevention byte). No escaped form unescapes to it, so escaping the whole RBSP again
  // brings it back a byte longer, past the limit; SetConstraintByte keeps that tail as it is.
  Bytes odd = unit;
  odd.resize(odd.size() + (kMaxSpsRbspBytes - 3 - RbspOf(unit).size()), 0x80);
  odd.insert(odd.end(), {0x00, 0x00, 0x00, 0x03});
  const Bytes odd_rbsp = RbspOf(odd);
  ASSERT_EQ(odd_rbsp.size(), kMaxSpsRbspBytes);
  ASSERT_EQ(odd_rbsp.back(), 0x00u);
  const Sps before = MustParse(odd);
  Bytes changed = odd_rbsp;
  changed[1] = 0xE0;
  EXPECT_EQ(RbspOf(SpsUnit(changed)).size(), kMaxSpsRbspBytes + 1) << "the whole RBSP escaped again comes back longer";
  Bytes rewritten(kMaxSpsBytes);
  rewritten.resize(SetConstraintByte(odd, 0xE0, rewritten));
  EXPECT_EQ(RbspOf(rewritten), changed);
  Sps after;
  ASSERT_EQ(ParseSps(rewritten, &after), SpsError::kOk);
  EXPECT_EQ(after.constraint_flags, 0xE0u);
  EXPECT_TRUE(SameSps(before, after, true));
  const char *err = CheckSps(odd);
  EXPECT_EQ(err, nullptr) << err;

  // An emulation-prevention byte that is one only because the constraint byte is 00 (67 42 00 00 03 ..., level_idc 0):
  // after e0 it would be data, so replacing the byte alone isn't enough; the head is escaped again.
  BitWriter w;
  w.Bits(66, 8);
  w.Bits(0x00, 8);
  w.Bits(0, 8); // level_idc 0
  w.Ue(0);      // sps_id
  w.Ue(0);      // log2_max_frame_num_minus4
  w.Ue(2);      // poc type 2
  w.Ue(1);      // max_num_ref_frames
  w.Bit(0);     // gaps
  w.Ue(39);     // 640 wide
  w.Ue(21);     // 352 high
  w.Bit(1);     // frame_mbs_only
  w.Bit(1);     // direct_8x8
  w.Bit(0);     // no cropping
  w.Bit(0);     // no VUI
  const Bytes rbsp = w.Finish();
  ASSERT_GE(rbsp[3], 0x80u) << "sps_id 0 starts with a one bit";
  Bytes with_epb = {0x67, 0x42, 0x00, 0x00, 0x03}; // an emulation-prevention byte before rbsp[3] (not needed, legal)
  with_epb.insert(with_epb.end(), rbsp.begin() + 3, rbsp.end());
  EXPECT_EQ(MustParse(with_epb).width, 640u);
  Bytes got(kMaxSpsBytes);
  got.resize(SetConstraintByte(with_epb, 0xE0, got));
  Bytes want = {0x67, 0x42, 0xE0, 0x00};
  want.insert(want.end(), rbsp.begin() + 3, rbsp.end());
  EXPECT_EQ(got, want);
  EXPECT_EQ(MustParse(got).profile_key(), 0x42E0u);

  // Refusals: no room, too short, no non-zero byte after the constraint byte.
  Bytes small(4);
  EXPECT_EQ(SetConstraintByte(unit, 0xE0, small), 0u);
  Bytes room(kMaxSpsBytes);
  EXPECT_EQ(SetConstraintByte(Bytes{0x67, 0x42, 0x40}, 0xE0, room), 0u);
  Bytes zeros = {0x67, 0x42, 0x40};
  zeros.resize(40, 0x00);
  EXPECT_EQ(SetConstraintByte(zeros, 0xE0, room), 0u);
}

std::vector<Bytes> Seeds() {
  std::vector<Bytes> seeds;
  for (uint8_t profile : {uint8_t{66}, uint8_t{77}, uint8_t{100}}) {
    for (bool scaling : {false, true}) {
      SpsParams p;
      p.profile_idc = profile;
      p.constraint = profile == 66 ? 0xC0 : 0x00;
      p.scaling_matrix = scaling;
      p.poc_type = scaling ? 1 : 0;
      p.frame_mbs_only = !scaling;
      seeds.push_back(MakeSps(p));
    }
  }
  // The committed corpus, when the tree has it.
  const std::filesystem::path dir = std::filesystem::path(IM_TESTDATA_DIR) / ".." / "fuzz" / "corpus" / "sps_fuzz";
  std::error_code ec;
  if (std::filesystem::is_directory(dir, ec)) {
    for (const auto &entry : std::filesystem::directory_iterator(dir, ec)) {
      std::ifstream in(entry.path(), std::ios::binary);
      seeds.emplace_back(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
    }
  }
  return seeds;
}

TEST(Sps, FuzzPropertiesOnSeedsAndMutations) {
  const std::vector<Bytes> seeds = Seeds();
  ASSERT_FALSE(seeds.empty());
  for (const Bytes &s : seeds) {
    const char *err = im::h264::fuzzing::CheckSps(s);
    EXPECT_EQ(err, nullptr) << err;
  }
  im::fuzzing::Mutator m(20261022);
  for (int i = 0; i < 20000; ++i) {
    const Bytes input = m.Mutate(seeds[static_cast<std::size_t>(i) % seeds.size()], seeds);
    const char *err = im::h264::fuzzing::CheckSps(input);
    ASSERT_EQ(err, nullptr) << err << " (mutation " << i << ")";
  }
}

} // namespace
