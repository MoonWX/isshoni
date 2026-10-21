// The access-unit normalizer (docs/m2/04-windows-engine.md §9.4, §18.1): synthetic cases (3-byte start codes,
// AUD/SEI/filler/EOS dropped, an IDR without SPS/PPS gets them from the cache, duplicates removed, CB byte → e0, a
// non-100 profile in a High session flagged, KEYFRAME from NAL type 5, idempotence), the annexb_fuzz properties over
// seeds and mutations, and the golden AUs per encoder.
//
// Golden AUs (WA44, captured with `isshoni-enginetest capture -dump-aus` in 04 E7): testdata/aus/<encoder token>/
// holds one <name>.h264 file per access unit exactly as the encoder produced it (Annex B, before normalization),
// named so that name order is stream order (the first is an IDR with its headers). An optional <name>.h264.norm next
// to it holds the expected normalized bytes; other files are ignored. The test runs each folder as one stream through
// one normalizer with the session profile its first SPS says, checks every invariant of §9.4 and, where a .norm file
// exists, the exact bytes. Without the folder the test is skipped.
#include <gtest/gtest.h>

#include <algorithm>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <string>
#include <vector>

#include "fuzz/annexb_props.h"
#include "fuzz/mutator.h"
#include "h264_test_streams.h"
#include "im/h264/annexb.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"

namespace {

namespace nal = im::h264::nal;
using im::h264::AuInfo;
using im::h264::NalType;
using im::h264::NormalizeOptions;
using im::h264::Normalizer;
using im::h264::testing::AnnexB;
using im::h264::testing::Bytes;
using im::h264::testing::MakePps;
using im::h264::testing::MakeSlice;
using im::h264::testing::MakeSps;
using im::h264::testing::SpsParams;
using im::h264::testing::Unit;
using im::h264::testing::Units;

Bytes Sps(uint8_t profile = 100, uint8_t constraint = 0x00, uint32_t w = 640, uint32_t h = 360) {
  SpsParams p;
  p.profile_idc = profile;
  p.constraint = constraint;
  p.width = w;
  p.height = h;
  return MakeSps(p);
}

const Bytes kAud = Unit({0x09, 0xF0});
const Bytes kSei = Unit({0x06, 0x05, 0x10, 0xAA, 0xBB, 0x80});
const Bytes kFiller = Unit({0x0C, 0xFF, 0xFF, 0xFF, 0x80});
const Bytes kEos = Unit({0x0A});
const Bytes kEob = Unit({0x0B});

NormalizeOptions Opts(uint32_t profile = 0) {
  NormalizeOptions o;
  o.session_profile = profile;
  return o;
}

Bytes Norm(Normalizer &n, const Bytes &au, const NormalizeOptions &o = Opts(), AuInfo *info = nullptr) {
  const auto out = n.Normalize(au, o, info);
  return Bytes(out.begin(), out.end());
}

std::vector<uint8_t> Types(const Bytes &stream) {
  std::vector<uint8_t> types;
  for (const Bytes &u : Units(stream)) {
    types.push_back(NalType(u));
  }
  return types;
}

// Normalizing the output again with a fresh normalizer gives the same bytes.
void ExpectIdempotent(const Bytes &out, const NormalizeOptions &o) {
  Normalizer fresh;
  EXPECT_EQ(Norm(fresh, out, o), out);
}

TEST(AnnexB, ThreeByteStartCodesBecomeFour) {
  Normalizer n;
  const Bytes sps = Sps();
  const Bytes pps = MakePps();
  const Bytes idr = MakeSlice(nal::kIdr);
  AuInfo info;
  const Bytes out = Norm(n, AnnexB({sps, pps, idr}, /*short_codes=*/true), Opts(), &info);
  EXPECT_EQ(out, AnnexB({sps, pps, idr}));
  EXPECT_TRUE(info.keyframe);
  EXPECT_EQ(info.nal_count, 3u);
  EXPECT_EQ(info.width, 640u);
  EXPECT_EQ(info.height, 360u);
  EXPECT_EQ(info.profile, 0x6400u);
  ExpectIdempotent(out, Opts());

  // Mixed 3- and 4-byte codes, trailing zero bytes after units, a leading zero_byte run.
  Bytes mixed = {0, 0, 0, 0, 1};
  mixed.insert(mixed.end(), sps.begin(), sps.end());
  mixed.insert(mixed.end(), {0, 0, 0, 0, 1});
  mixed.insert(mixed.end(), pps.begin(), pps.end());
  mixed.insert(mixed.end(), {0, 0, 1});
  mixed.insert(mixed.end(), idr.begin(), idr.end());
  mixed.insert(mixed.end(), {0, 0});
  EXPECT_EQ(Norm(n, mixed, Opts(), &info), AnnexB({sps, pps, idr}));
  EXPECT_EQ(info.leading_bytes, 0u);
}

TEST(AnnexB, DropsAudSeiFillerEndOfSequenceAndStream) {
  Normalizer n;
  const Bytes sps = Sps();
  const Bytes pps = MakePps();
  const Bytes idr = MakeSlice(nal::kIdr);
  AuInfo info;
  const Bytes out = Norm(n, AnnexB({kAud, kSei, sps, pps, kSei, idr, kFiller, kEos, kEob}), Opts(), &info);
  EXPECT_EQ(out, AnnexB({sps, pps, idr}));
  EXPECT_EQ(info.dropped_nals, 6u);
  // P frames too; also unit types no stream of ours carries (0, reserved, RTP's 24..31) and the forbidden bit.
  const Bytes p = MakeSlice(nal::kSlice);
  const Bytes out2 = Norm(n,
                          AnnexB({kAud, Unit({0x00, 0x11}), Unit({0x10, 0x11}), Unit({0x18, 0x11}), Unit({0x1F, 0x11}),
                                  Unit({0xE1, 0x11}), p, kFiller}),
                          Opts(), &info);
  EXPECT_EQ(out2, AnnexB({p}));
  EXPECT_FALSE(info.keyframe);
  ExpectIdempotent(out2, Opts());
}

TEST(AnnexB, IdrWithoutHeadersGetsThemFromTheCache) {
  Normalizer n;
  const Bytes sps = Sps();
  const Bytes pps = MakePps();
  const Bytes idr = MakeSlice(nal::kIdr);
  const Bytes p = MakeSlice(nal::kSlice);
  Norm(n, AnnexB({sps, pps, idr}));
  AuInfo info;
  EXPECT_EQ(Norm(n, AnnexB({p}), Opts(), &info), AnnexB({p}));
  EXPECT_FALSE(info.inserted_parameter_sets) << "a P frame gets no headers";
  EXPECT_EQ(info.width, 640u) << "but the cached SPS's size";
  const Bytes out = Norm(n, AnnexB({kAud, idr}), Opts(), &info);
  EXPECT_EQ(out, AnnexB({sps, pps, idr}));
  EXPECT_TRUE(info.inserted_parameter_sets);
  EXPECT_FALSE(info.missing_parameter_sets);
  EXPECT_TRUE(info.keyframe);
  ExpectIdempotent(out, Opts());

  // Only the PPS is missing: the AU's SPS and the cached PPS.
  const Bytes sps2 = Sps(100, 0x00, 1280, 720);
  EXPECT_EQ(Norm(n, AnnexB({sps2, idr}), Opts(), &info), AnnexB({sps2, pps, idr}));
  EXPECT_TRUE(info.inserted_parameter_sets);
  EXPECT_EQ(info.width, 1280u);

  // An encoder that sends the headers as their own packet: no output, but the next IDR gets them.
  Normalizer m;
  EXPECT_TRUE(Norm(m, AnnexB({sps, pps}), Opts(), &info).empty());
  EXPECT_FALSE(info.has_vcl);
  EXPECT_EQ(Norm(m, AnnexB({idr}), Opts(), &info), AnnexB({sps, pps, idr}));

  // Nothing cached: the IDR goes out as it is, flagged.
  Normalizer empty;
  EXPECT_EQ(Norm(empty, AnnexB({idr}), Opts(), &info), AnnexB({idr}));
  EXPECT_TRUE(info.missing_parameter_sets);
  EXPECT_EQ(info.profile, 0u);

  // Reset forgets the cache.
  n.Reset();
  EXPECT_FALSE(n.has_sps());
  EXPECT_EQ(Norm(n, AnnexB({idr}), Opts(), &info), AnnexB({idr}));
  EXPECT_TRUE(info.missing_parameter_sets);
}

TEST(AnnexB, DuplicatesRemovedAndOrderFixed) {
  Normalizer n;
  const Bytes sps_old = Sps(100, 0x00, 640, 360);
  const Bytes sps = Sps(100, 0x00, 1280, 720);
  const Bytes pps_old = MakePps(0xAA);
  const Bytes pps = MakePps(0xBB);
  const Bytes idr = MakeSlice(nal::kIdr);
  const Bytes idr2 = MakeSlice(nal::kIdr, {0xB8, 0x41});
  AuInfo info;
  // PPS before SPS, two of each, a two-slice IDR: one SPS, one PPS (the last of each), SPS first, both slices.
  const Bytes out = Norm(n, AnnexB({pps_old, sps_old, sps, pps, idr, idr2}), Opts(), &info);
  EXPECT_EQ(out, AnnexB({sps, pps, idr, idr2}));
  EXPECT_EQ(info.dropped_nals, 2u);
  EXPECT_EQ(info.width, 1280u);
  ExpectIdempotent(out, Opts());
  // Headers repeated in a P frame stay (once each, in front).
  const Bytes p = MakeSlice(nal::kSlice);
  EXPECT_EQ(Norm(n, AnnexB({p, sps, sps}), Opts(), &info), AnnexB({sps, p}));
}

TEST(AnnexB, ConstrainedBaselineByteBecomesE0) {
  const uint8_t constraints[] = {0x00, 0x40, 0xC0, 0xE0, 0xE3};
  for (uint8_t c : constraints) {
    Normalizer n;
    const Bytes sps = Sps(66, c);
    AuInfo info;
    const Bytes out = Norm(n, AnnexB({sps, MakePps(), MakeSlice(nal::kIdr)}), Opts(IM_PROFILE_CB), &info);
    const auto units = Units(out);
    ASSERT_EQ(units.size(), 3u);
    im::h264::Sps parsed;
    ASSERT_EQ(im::h264::ParseSps(units[0], &parsed), im::h264::SpsError::kOk);
    EXPECT_EQ(parsed.constraint_flags, 0xE0u) << int{c};
    EXPECT_EQ(info.profile, 0x42E0u);
    EXPECT_EQ(units[0][2], 0xE0u) << "the byte right after profile_idc";
    EXPECT_EQ(info.sps_rewritten, c != 0xE0);
    EXPECT_FALSE(info.profile_mismatch);
    EXPECT_EQ(parsed.width, 640u);
    ExpectIdempotent(out, Opts(IM_PROFILE_CB));
  }
  // Without a session profile nothing is rewritten.
  Normalizer n;
  AuInfo info;
  Norm(n, AnnexB({Sps(66, 0x40), MakePps(), MakeSlice(nal::kIdr)}), Opts(), &info);
  EXPECT_EQ(info.profile, 0x4240u);
  EXPECT_FALSE(info.sps_rewritten);
}

TEST(AnnexB, HighByteBecomes00) {
  Normalizer n;
  AuInfo info;
  const Bytes out = Norm(n, AnnexB({Sps(100, 0x0C), MakePps(), MakeSlice(nal::kIdr)}), Opts(IM_PROFILE_HIGH), &info);
  EXPECT_EQ(info.profile, 0x6400u) << "constraint_set4/5 cleared: the key is 6400 (04 §12.2)";
  EXPECT_TRUE(info.sps_rewritten);
  EXPECT_FALSE(info.profile_mismatch);
  ExpectIdempotent(out, Opts(IM_PROFILE_HIGH));
}

TEST(AnnexB, RewriteKeepsEmulationPreventionSound) {
  // An SPS whose escaped form holds 00 00 03 (poc type 1 with a huge offset, see sps_test): the rewrite unescapes,
  // changes byte 2 and escapes again, so the result parses to the same fields and holds no 00 00 0x (x <= 2).
  im::h264::testing::BitWriter w;
  w.Bits(66, 8);
  w.Bits(0x40, 8);
  w.Bits(31, 8);
  w.Ue(0);
  w.Ue(0);
  w.Ue(1);
  w.Bit(0);
  w.Se(0);
  w.Se(0);
  w.Ue(1);
  w.Se(1 << 30);
  w.Ue(1);
  w.Bit(0);
  w.Ue(39); // 640
  w.Ue(22); // 368, cropped to 360
  w.Bit(1);
  w.Bit(1);
  w.Bit(1);
  w.Ue(0);
  w.Ue(0);
  w.Ue(0);
  w.Ue(4);
  w.Bit(0);
  const Bytes rbsp = w.Finish();
  Bytes sps(1 + im::h264::MaxEscapedSize(rbsp.size()));
  sps[0] = 0x67;
  sps.resize(1 + im::h264::EscapeRbsp(rbsp, std::span<uint8_t>(sps).subspan(1)));
  Normalizer n;
  AuInfo info;
  const Bytes out = Norm(n, AnnexB({sps, MakePps(), MakeSlice(nal::kIdr)}), Opts(IM_PROFILE_CB), &info);
  EXPECT_TRUE(info.sps_rewritten);
  const auto units = Units(out);
  ASSERT_EQ(units.size(), 3u);
  const Bytes &got = units[0];
  for (std::size_t i = 0; i + 2 < got.size(); ++i) {
    EXPECT_FALSE(got[i] == 0 && got[i + 1] == 0 && got[i + 2] <= 2) << "at " << i;
  }
  im::h264::Sps before;
  im::h264::Sps after;
  ASSERT_EQ(im::h264::ParseSps(sps, &before), im::h264::SpsError::kOk);
  ASSERT_EQ(im::h264::ParseSps(got, &after), im::h264::SpsError::kOk);
  EXPECT_EQ(after.constraint_flags, 0xE0u);
  EXPECT_EQ(after.width, before.width);
  EXPECT_EQ(after.height, 360u);
  // Apart from byte 2, the unescaped units are identical.
  Bytes a(sps.size());
  a.resize(im::h264::UnescapeRbsp(sps, a));
  Bytes b(got.size());
  b.resize(im::h264::UnescapeRbsp(got, b));
  ASSERT_EQ(a.size(), b.size());
  a[2] = 0xE0;
  EXPECT_EQ(a, b);
  ExpectIdempotent(out, Opts(IM_PROFILE_CB));
}

TEST(AnnexB, ProfileMismatchFlagged) {
  struct Case {
    uint32_t session;
    uint8_t profile_idc;
    bool mismatch;
  };
  for (const Case c :
       {Case{IM_PROFILE_HIGH, 100, false}, Case{IM_PROFILE_HIGH, 77, true}, Case{IM_PROFILE_HIGH, 66, true},
        Case{IM_PROFILE_CB, 66, false}, Case{IM_PROFILE_CB, 100, true}, Case{0, 77, false}}) {
    Normalizer n;
    AuInfo info;
    const Bytes sps = Sps(c.profile_idc, 0x00);
    const Bytes out = Norm(n, AnnexB({sps, MakePps(), MakeSlice(nal::kIdr)}), Opts(c.session), &info);
    EXPECT_EQ(info.profile_mismatch, c.mismatch) << c.session << " " << int{c.profile_idc};
    if (c.mismatch) {
      EXPECT_FALSE(info.sps_rewritten) << "a mismatching SPS is left alone";
      EXPECT_EQ(Units(out)[0], sps);
    }
    // P frames of that stream carry the flag too (from the cached SPS).
    Norm(n, AnnexB({MakeSlice(nal::kSlice)}), Opts(c.session), &info);
    EXPECT_EQ(info.profile_mismatch, c.mismatch);
  }
}

TEST(AnnexB, KeyframeFromNalType5) {
  Normalizer n;
  AuInfo info;
  NormalizeOptions o;
  o.encoder_keyframe = 1;
  Norm(n, AnnexB({Sps(), MakePps(), MakeSlice(nal::kIdr)}), o, &info);
  EXPECT_TRUE(info.keyframe);
  EXPECT_FALSE(info.key_mismatch);
  // An I slice in a non-IDR unit (type 1) is not a keyframe, whatever the encoder says.
  Norm(n, AnnexB({Sps(), MakePps(), MakeSlice(nal::kSlice)}), o, &info);
  EXPECT_FALSE(info.keyframe);
  EXPECT_TRUE(info.key_mismatch);
  o.encoder_keyframe = 0;
  Norm(n, AnnexB({MakeSlice(nal::kIdr)}), o, &info);
  EXPECT_TRUE(info.keyframe);
  EXPECT_TRUE(info.key_mismatch);
  o.encoder_keyframe = -1;
  Norm(n, AnnexB({MakeSlice(nal::kIdr)}), o, &info);
  EXPECT_FALSE(info.key_mismatch);
}

TEST(AnnexB, GarbageAndEdges) {
  Normalizer n;
  AuInfo info;
  EXPECT_TRUE(Norm(n, {}, Opts(), &info).empty());
  EXPECT_FALSE(info.has_vcl);
  EXPECT_TRUE(Norm(n, Bytes{0x12, 0x34, 0x56}, Opts(), &info).empty()) << "no start code";
  EXPECT_EQ(info.leading_bytes, 3u);
  Bytes lead = {0xAB, 0xCD};
  const Bytes p = MakeSlice(nal::kSlice);
  const Bytes tail = AnnexB({p});
  lead.insert(lead.end(), tail.begin(), tail.end());
  EXPECT_EQ(Norm(n, lead, Opts(), &info), AnnexB({p}));
  EXPECT_EQ(info.leading_bytes, 2u);
  EXPECT_TRUE(Norm(n, Bytes{0, 0, 0, 1, 0, 0, 0, 1}, Opts(), &info).empty()) << "empty units";
  // A slice ending in cabac_zero_words keeps its 00 00 03.
  const Bytes cabac = Unit({0x65, 0x88, 0x00, 0x00, 0x03, 0x00, 0x00, 0x03});
  EXPECT_EQ(Types(Norm(n, AnnexB({Sps(), MakePps(), cabac}), Opts(), &info)),
            (std::vector<uint8_t>{nal::kSps, nal::kPps, nal::kIdr}));
  EXPECT_EQ(Units(Norm(n, AnnexB({cabac}), Opts(), &info)).back(), cabac);
}

TEST(AnnexB, OutputBoundAndReserve) {
  Normalizer n;
  n.Reserve(1 << 20);
  const Bytes sps = Sps();
  const Bytes pps = MakePps();
  Norm(n, AnnexB({sps, pps, MakeSlice(nal::kIdr)}));
  // Worst case: many one-byte units with 3-byte start codes, plus the inserted headers.
  Bytes au;
  au.insert(au.end(), {0, 0, 1, 0x65});
  for (int i = 0; i < 1000; ++i) {
    au.insert(au.end(), {0, 0, 1, 0x41});
  }
  const auto out = n.Normalize(au, Opts(), nullptr);
  EXPECT_LE(out.size(), Normalizer::MaxOutputSize(au.size()));
  EXPECT_EQ(out.size(), 4 + sps.size() + 4 + pps.size() + 1001u * 5);
}

TEST(AnnexB, Idempotence) {
  const Bytes sps = Sps(66, 0x40);
  const Bytes pps = MakePps();
  const std::vector<Bytes> aus = {
      AnnexB({kAud, sps, pps, kSei, MakeSlice(nal::kIdr)}, true),
      AnnexB({kAud, MakeSlice(nal::kSlice), kFiller}),
      AnnexB({MakeSlice(nal::kIdr), MakeSlice(nal::kIdr, {0x11, 0x22})}),
      AnnexB({pps, sps, pps, MakeSlice(nal::kSlice)}),
  };
  for (uint32_t profile : {0u, IM_PROFILE_CB, IM_PROFILE_HIGH}) {
    Normalizer n;
    for (const Bytes &au : aus) {
      const Bytes once = Norm(n, au, Opts(profile));
      Normalizer second;
      EXPECT_EQ(Norm(second, once, Opts(profile)), once);
      EXPECT_EQ(Norm(n, once, Opts(profile)), once) << "also with the same (warm) normalizer";
    }
  }
}

std::vector<Bytes> Seeds() {
  std::vector<Bytes> seeds;
  const uint8_t option_bytes[] = {0x00, 0x01, 0x02, 0x09, 0x0A, 0x05};
  const std::vector<Bytes> streams = {
      AnnexB({kAud, Sps(100), MakePps(), kSei, MakeSlice(nal::kIdr)}),
      AnnexB({Sps(66, 0x40), MakePps(), MakeSlice(nal::kIdr), MakeSlice(nal::kSlice)}, true),
      AnnexB({MakeSlice(nal::kSlice), kFiller, kEos}),
      AnnexB({Sps(66, 0xC0, 1280, 720), Sps(100, 0x0C, 1920, 1080), MakePps(), MakePps(0x11), MakeSlice(nal::kIdr)}),
  };
  for (std::size_t i = 0; i < streams.size(); ++i) {
    Bytes s = {option_bytes[i % sizeof(option_bytes)], static_cast<uint8_t>(i)};
    s.insert(s.end(), streams[i].begin(), streams[i].end());
    seeds.push_back(s);
  }
  const std::filesystem::path dir = std::filesystem::path(IM_TESTDATA_DIR) / ".." / "fuzz" / "corpus" / "annexb_fuzz";
  std::error_code ec;
  if (std::filesystem::is_directory(dir, ec)) {
    for (const auto &entry : std::filesystem::directory_iterator(dir, ec)) {
      std::ifstream in(entry.path(), std::ios::binary);
      seeds.emplace_back(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
    }
  }
  return seeds;
}

TEST(AnnexB, FuzzPropertiesOnSeedsAndMutations) {
  const std::vector<Bytes> seeds = Seeds();
  for (const Bytes &s : seeds) {
    const char *err = im::h264::fuzzing::CheckAnnexB(s);
    EXPECT_EQ(err, nullptr) << err;
  }
  im::fuzzing::Mutator m(20261021);
  for (int i = 0; i < 20000; ++i) {
    const Bytes input = m.Mutate(seeds[static_cast<std::size_t>(i) % seeds.size()], seeds);
    const char *err = im::h264::fuzzing::CheckAnnexB(input);
    ASSERT_EQ(err, nullptr) << err << " (mutation " << i << ")";
  }
}

Bytes ReadFile(const std::filesystem::path &p) {
  std::ifstream in(p, std::ios::binary);
  return Bytes(std::istreambuf_iterator<char>(in), std::istreambuf_iterator<char>());
}

TEST(AnnexB, GoldenAccessUnits) {
  const std::filesystem::path root = std::filesystem::path(IM_TESTDATA_DIR) / "aus";
  std::error_code ec;
  if (!std::filesystem::is_directory(root, ec)) {
    GTEST_SKIP() << "no testdata/aus/ yet: the golden AUs come with WA44 (04 E7)";
  }
  int checked = 0;
  for (const auto &enc : std::filesystem::directory_iterator(root, ec)) {
    if (!enc.is_directory()) {
      continue;
    }
    std::vector<std::filesystem::path> files;
    for (const auto &f : std::filesystem::directory_iterator(enc.path(), ec)) {
      if (f.is_regular_file() && f.path().extension() == ".h264") {
        files.push_back(f.path());
      }
    }
    std::sort(files.begin(), files.end());
    Normalizer n;
    NormalizeOptions o;
    bool first = true;
    for (const auto &file : files) {
      const Bytes au = ReadFile(file);
      if (first) {
        // The session profile is what the stream's first SPS says (High 100 or CB 66).
        for (const Bytes &u : Units(au)) {
          im::h264::Sps s;
          if (NalType(u) == nal::kSps && im::h264::ParseSps(u, &s) == im::h264::SpsError::kOk) {
            o.session_profile = s.profile_idc == im::h264::kProfileBaseline ? IM_PROFILE_CB : IM_PROFILE_HIGH;
          }
        }
        first = false;
      }
      AuInfo info;
      const auto out = n.Normalize(au, o, &info);
      const std::string name = enc.path().filename().string() + "/" + file.filename().string();
      const char *err = im::h264::fuzzing::CheckOutput(out, info, o);
      EXPECT_EQ(err, nullptr) << name << ": " << err;
      EXPECT_FALSE(info.profile_mismatch) << name;
      EXPECT_FALSE(info.missing_parameter_sets) << name;
      EXPECT_TRUE(info.profile == IM_PROFILE_HIGH || info.profile == IM_PROFILE_CB) << name << " " << info.profile;
      EXPECT_GT(info.width, 0u) << name;
      const Bytes norm(out.begin(), out.end());
      ExpectIdempotent(norm, o);
      std::filesystem::path expected = file;
      expected += ".norm";
      if (std::filesystem::exists(expected, ec)) {
        EXPECT_EQ(norm, ReadFile(expected)) << name;
      }
      ++checked;
    }
  }
  EXPECT_GT(checked, 0) << "testdata/aus/ holds no access units";
}

} // namespace
