// session_luma_tap_test (docs/m2/04-windows-engine.md §5.8, §14 avsync; README WF16): the luma tap's producer through
// the C ABI, with the test hooks (IM_TEST_HOOKS builds: win-x64-ci, win-x64-asan), on the fake/WARP path: test:pattern
// with IM_ENC_FAKE, whose sessions the tap's start flag puts on the GPU path (CI's windows-2025 runner has no GPU, so
// the default adapter is WARP's Basic Render Driver), so the supervisor draws the GPU test pattern into the frame slots
// and the tap reads it back.
//
// It asserts: a session started with IM_START_TEST_LUMA_TAP (luma_sample.h's kStartFlag) emits IM_PACKET_DEBUG packets
// (layer IM_NO_LAYER, 48 bytes, §5.8's payload) whose capture time is the packet's and a pattern frame's grid instant,
// in capture order, whose region is the centered 256-pixel box and whose mean luma is the CPU model's
// (gpu/pattern_layout.h's PatternPixel) for that frame, and whose `missed` count never exceeds the frames between two
// readings; a static screen's one frame still gets its reading (the video thread polls the copy in flight); without
// the flag there is no debug packet and an IM_ENC_FAKE session stays off the GPU path; the flag is IM_ERR_INVALID_ARG
// for an audio-only session and for a session whose streams leave no room for the tap's ring.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <functional>
#include <set>
#include <span>
#include <string>
#include <vector>

#include "gpu/pattern_layout.h"
#include "im/abi/status.h"
#include "isshoni_media.h"
#include "session/luma_sample.h"
#include "session/pattern_grid.h"
#include "session/session.h"
#include "session/sources.h"
#include "testhooks/testhooks.h"

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;
namespace luma = im::session::luma;

constexpr uint32_t kSourceW = 640; // small, so WARP draws 60 fps
constexpr uint32_t kSourceH = 360;
constexpr uint32_t kHz = 60;

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0;
  int64_t ts = 0;
  std::vector<uint8_t> data;
  int32_t event = 0;
  im_status status = IM_OK;
};

// Pulls packets for d (or until ENDED).
std::vector<Pkt> Pull(im_session s, milliseconds d) {
  std::vector<Pkt> out;
  const auto end = Clock::now() + d;
  while (Clock::now() < end) {
    im_packet p{};
    p.struct_size = sizeof p;
    const im_status st = im_next_packet(s, 50, &p);
    if (st == IM_TIMEOUT) {
      continue;
    }
    if (st != IM_OK) {
      break;
    }
    Pkt k;
    k.kind = p.kind;
    k.layer = p.layer;
    k.flags = p.flags;
    k.ts = p.capture_ts_ns;
    k.data.assign(p.data, p.data + p.size);
    k.event = p.event;
    k.status = p.status;
    out.push_back(std::move(k));
    if (out.back().kind == IM_PACKET_EVENT && out.back().event == IM_EVENT_ENDED) {
      break;
    }
  }
  return out;
}

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  return l;
}

// test:pattern, one layer at the source's size and 60 fps on IM_ENC_FAKE, no audio; the tap when asked.
im_start_config Config(bool tap) {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.flags = IM_START_ALLOW_LISTED_SOURCE | (tap ? luma::kStartFlag : 0u);
  c.source_id = "test:pattern";
  c.profile = IM_PROFILE_HIGH;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_FAKE;
  c.layer_count = 1;
  c.layers[0] = Layer("f", kSourceW * kSourceH, kHz);
  c.audio.mode = IM_AUDIO_MODE_OFF;
  return c;
}

// The mean luma of the tap's region of pattern frame k (captured at elapsed_ns after epoch_ns), from the pattern's CPU
// model: the same pixels the GPU draws (gpu/pattern_layout.h), through the same accumulator.
uint32_t ModelMean(uint64_t k, int64_t elapsed_ns) {
  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(kSourceW, kSourceH);
  const im::gpu::PatternFrame f{k, elapsed_ns};
  const luma::Region r = luma::CenterBox(kSourceW, kSourceH);
  luma::Accumulator acc;
  std::vector<uint8_t> row(static_cast<std::size_t>(r.width) * 4);
  for (uint32_t y = r.y; y < r.y + r.height; ++y) {
    for (uint32_t x = 0; x < r.width; ++x) {
      // 0xAARRGGBB into the slot's byte order: B, G, R, A.
      const uint32_t argb = im::gpu::PatternPixel(l, f, r.x + x, y);
      row[x * 4 + 0] = static_cast<uint8_t>(argb);
      row[x * 4 + 1] = static_cast<uint8_t>(argb >> 8);
      row[x * 4 + 2] = static_cast<uint8_t>(argb >> 16);
      row[x * 4 + 3] = static_cast<uint8_t>(argb >> 24);
    }
    acc.AddBgraRow(row.data(), r.width);
  }
  return acc.Mean();
}

class SessionLumaTap : public ::testing::Test {
protected:
  void SetUp() override {
    im::testhooks::Reset();
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO; // CI runners have no audio device; the test tone needs none
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  }
  void TearDown() override {
    im_shutdown();
    im::testhooks::Reset();
  }

  void Configure(im::session::Pattern pattern) {
    im::testhooks::Options o;
    o.pattern = pattern;
    o.pattern_width = kSourceW;
    o.pattern_height = kSourceH;
    o.pattern_hz = kHz;
    im::testhooks::Configure(o);
  }

  im_session Start(const im_start_config &cfg) {
    im_session s = 0;
    im_session_info info{};
    info.struct_size = sizeof info;
    EXPECT_EQ(im_start(&cfg, &s, &info), IM_OK) << LastError();
    epoch_ = info.epoch_ns;
    return s;
  }

  // The pattern frame whose grid instant is exactly ts (the slot's capture time), if ts is one.
  bool GridFrame(int64_t ts, uint64_t *k) const {
    if (ts < epoch_) {
      return false;
    }
    const im::session::PatternGrid g{epoch_, kHz, 0};
    // FrameNs rounds down, so ts · hz / 1 s lies in (k − 1, k]: k is its ceiling.
    const auto n = static_cast<uint64_t>(((ts - epoch_) * int64_t{kHz} + 999'999'999) / 1'000'000'000);
    *k = n;
    return g.FrameNs(n) == ts;
  }

  int64_t epoch_ = 0;
};

struct Reading {
  int64_t ts = 0;
  luma::Sample s;
};

// The debug packets' readings, checked against their packets.
std::vector<Reading> Readings(const std::vector<Pkt> &pkts) {
  std::vector<Reading> out;
  for (const Pkt &p : pkts) {
    if (p.kind != IM_PACKET_DEBUG) {
      continue;
    }
    EXPECT_EQ(p.layer, IM_NO_LAYER);
    EXPECT_EQ(p.data.size(), luma::kPayloadBytes);
    Reading r;
    r.ts = p.ts;
    if (!luma::Decode(std::span<const uint8_t>(p.data.data(), p.data.size()), &r.s)) {
      ADD_FAILURE() << "a debug packet that is not §5.8's payload (" << p.data.size() << " bytes)";
      continue;
    }
    EXPECT_EQ(r.s.capture_ts_ns, p.ts) << "the payload's capture time is the packet's";
    out.push_back(r);
  }
  return out;
}

// ---------------------------------------------------------------------------------------------- the cases

TEST_F(SessionLumaTap, ReadingsFollowThePatternFrameByFrame) {
  Configure(im::session::Pattern::kMoving);
  const im_session s = Start(Config(true));
  ASSERT_NE(s, 0u);
  EXPECT_TRUE(im::session::GpuPathOf(s)) << "the tap puts an IM_ENC_FAKE pattern session on the GPU path";
  const std::vector<Pkt> pkts = Pull(s, milliseconds(2000));
  EXPECT_EQ(im_stop(s), IM_OK);
  for (const Pkt &p : pkts) {
    ASSERT_FALSE(p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_ENDED) << "the session ended: " << p.status;
  }
  const std::vector<Reading> rs = Readings(pkts);
  // 2 s at 60 fps is 120 frames; WARP on a loaded runner may skip some, never most.
  ASSERT_GE(rs.size(), 60u);
  const luma::Region box = luma::CenterBox(kSourceW, kSourceH);
  std::set<uint32_t> means;
  uint64_t prev_k = 0;
  for (std::size_t i = 0; i < rs.size(); ++i) {
    const luma::Sample &x = rs[i].s;
    SCOPED_TRACE("reading " + std::to_string(i) + " at +" + std::to_string(x.capture_ts_ns - epoch_) + " ns");
    uint64_t k = 0;
    ASSERT_TRUE(GridFrame(x.capture_ts_ns, &k)) << "a reading's time is its frame's grid instant";
    if (i > 0) {
      ASSERT_GT(k, prev_k) << "in capture order, one reading per frame";
      EXPECT_LE(x.missed, k - prev_k - 1) << "missed counts frames between two readings at most";
    }
    prev_k = k;
    EXPECT_EQ(x.source_width, kSourceW);
    EXPECT_EQ(x.source_height, kSourceH);
    EXPECT_EQ(x.region.x, box.x);
    EXPECT_EQ(x.region.y, box.y);
    EXPECT_EQ(x.region.width, box.width);
    EXPECT_EQ(x.region.height, box.height);
    const uint32_t want = ModelMean(k, x.capture_ts_ns - epoch_);
    EXPECT_LE(x.mean > want ? x.mean - want : want - x.mean, 1u) << "mean " << x.mean << ", the model's " << want;
    means.insert(x.mean);
  }
  // The frame counter and the moving bar are inside the box, so the readings change with the content.
  EXPECT_GE(means.size(), 10u);
}

TEST_F(SessionLumaTap, AStaticScreensOneFrameGetsItsReading) {
  Configure(im::session::Pattern::kStatic);
  const im_session s = Start(Config(true));
  ASSERT_NE(s, 0u);
  const std::vector<Reading> rs = Readings(Pull(s, milliseconds(1500)));
  EXPECT_EQ(im_stop(s), IM_OK);
  // The one frame is taken once; its copy finishes after the video thread's wake-up for it, and the thread polls again
  // while it is in flight (no new frame wakes it).
  ASSERT_EQ(rs.size(), 1u);
  EXPECT_EQ(rs[0].s.capture_ts_ns, epoch_);
  EXPECT_EQ(rs[0].s.missed, 0u);
  const uint32_t want = ModelMean(0, 0);
  EXPECT_LE(rs[0].s.mean > want ? rs[0].s.mean - want : want - rs[0].s.mean, 1u);
}

TEST_F(SessionLumaTap, NoDebugPacketsWithoutTheFlag) {
  Configure(im::session::Pattern::kMoving);
  const im_session s = Start(Config(false));
  ASSERT_NE(s, 0u);
  EXPECT_FALSE(im::session::GpuPathOf(s)) << "IM_ENC_FAKE without the tap stays off WARP (session_fake_test)";
  uint32_t video = 0;
  for (const Pkt &p : Pull(s, milliseconds(700))) {
    EXPECT_NE(p.kind, IM_PACKET_DEBUG);
    video += p.kind == IM_PACKET_VIDEO ? 1u : 0u;
  }
  EXPECT_GT(video, 10u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(SessionLumaTap, TheFlagNeedsVideoAndAFreeStream) {
  Configure(im::session::Pattern::kMoving);
  im_session s = 0;
  im_session_info info{};
  info.struct_size = sizeof info;

  im_start_config audio_only{};
  audio_only.struct_size = sizeof audio_only;
  audio_only.flags = luma::kStartFlag;
  audio_only.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  EXPECT_EQ(im_start(&audio_only, &s, &info), IM_ERR_INVALID_ARG);
  EXPECT_NE(LastError().find("luma tap"), std::string::npos) << LastError();

  // Three layers, Opus and the PCM tap use every stream of the pump (ring::kMaxStreams).
  im_start_config full = Config(true);
  full.encoder = IM_ENC_AUTO;
  full.layer_count = 3;
  full.layers[0] = Layer("f", kSourceW * kSourceH, kHz);
  full.layers[1] = Layer("h", 320 * 180, 30);
  full.layers[2] = Layer("q", 160 * 90, 15);
  full.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  full.audio.flags = IM_AUDIOF_TAP_PCM;
  EXPECT_EQ(im_start(&full, &s, &info), IM_ERR_INVALID_ARG);
  EXPECT_NE(LastError().find("luma tap"), std::string::npos) << LastError();

  // Without the PCM tap there is room.
  full.audio.flags = 0;
  s = Start(full);
  ASSERT_NE(s, 0u);
  bool debug = false;
  for (const Pkt &p : Pull(s, milliseconds(1000))) {
    debug = debug || p.kind == IM_PACKET_DEBUG;
  }
  EXPECT_TRUE(debug);
  EXPECT_EQ(im_stop(s), IM_OK);
}

} // namespace

#else // IM_TEST_HOOKS

TEST(SessionLumaTap, NeedsTestHooks) {
  GTEST_SKIP() << "session_luma_tap_test needs an IM_TEST_HOOKS build (win-x64-ci)";
}

#endif // IM_TEST_HOOKS
