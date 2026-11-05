// session_fake_test (docs/m2/04-windows-engine.md §18.2, README WA23): the session core through the C ABI with the
// test hooks (IM_TEST_HOOKS builds: win-x64-ci, win-x64-asan): test:pattern (and the fake picker's picks), IM_ENC_FAKE
// and the test tone; no GPU and no audio device (the engine runs with IM_INIT_NO_AUDIO, which the tone doesn't need).
// IM_ENC_FAKE takes no pixels, so these sessions run no GPU path (README WF2: frame times only, no WARP device) and
// the timing bounds below hold on a runner without a GPU; session_gpu_test runs the session's GPU path.
//
// It asserts: each layer's first packet is KEYFRAME|REINIT within 500 ms after im_start returns, also on a static
// pattern and with no captured frame at all; IM_ENC_FAKE's output (real SPS/PPS for the profile and size, CB byte e0,
// AU sizes, the Marker's CaptureNS = capture_ts_ns − epoch_ns); every capture_ts_ns ≥ epoch_ns; f ≈ 60 fps, q 15 fps; a
// keyframe within one interval, also on a static screen; two AUs of a layer at least one 90 kHz tick apart (a static
// IDR followed by a late frame); im_set_layer_active; pause (only REFRESH and IDR packets, audio keeps 50 packets/s,
// fpsIn keeps counting the captured frames);
// im_set_profile; im_set_content_hint coalesced with an im_update_layer; an injected encode error (the same encoder
// re-opened, or ENCODER_CHANGED before the next candidate's first REINIT|KEYFRAME); an injected device removal
// (ENDED{IM_ERR_ENCODER_LOST} device_removed within 1 s, then a new session on a new device); exhausted candidates; a
// consumer that stops for 2 s (RING_OVERFLOW, stale drops, an IDR when it resumes); im_stop with a blocked consumer;
// the session limits; the pick-token rules; the test tone's Marker and its beeps on the video flashes; stats and
// session info; the encoder catalog with the probe's real encoders after the fakes (README WF10: still no GPU path for
// a test: session); each AU's own capture time from an encoder one frame behind (04 §9.2); a layer's gop_ms.
//
// README WA61 extended the fault-injection cases with scripted encoders (the fake behind catalog entries of other
// kinds, adapters and session limits, SetCatalogPrefixForTest's only form, so no real encoder takes part): 04 §9.1's
// candidate order with and without a capture adapter; one profile per session (a CB-only encoder makes the whole
// session CB, and High asked of it stays CB); a preview layer past NVENC's session limit at im_start and the one retry
// after 500 ms; bitrate re-inits 10 s apart with the latest value winning, and a bitrate riding along with an
// im_update_layer re-init; im_update_layer re-inits 1 s apart and coalesced; mid-session fallbacks with their reasons
// (session_limit, open_failed), each ENCODER_CHANGED before the new encoder's first REINIT|KEYFRAME; an encoder that
// reports a removed device (ENDED device_removed within 1 s, no fallback, the encoders released before im_stop, then a
// new session on a new device), and the next start re-probing after a device removal.
//
// The test links the engine's objects (im_engine), so it calls the test hooks directly.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <array>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <functional>
#include <iterator>
#include <map>
#include <memory>
#include <mutex>
#include <optional>
#include <set>
#include <span>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include "im/abi/status.h"
#include "im/audio/opus_packet.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"
#include "session/encoder.h"
#include "session/session.h"
#include "session/sources.h"
#include "testhooks/fake_encoder.h"
#include "testhooks/testhooks.h"

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr int64_t kSecond = 1'000'000'000;

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

bool StartsWith(const std::string &s, const std::string &prefix) { return s.rfind(prefix, 0) == 0; }

// im_get_stats' document ("" when it fails).
std::string StatsOf(im_session s) {
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  return n > 0 ? std::string(buf.data(), static_cast<std::size_t>(std::min<int32_t>(n, 16383))) : std::string();
}

// The number right after the first occurrence of key in doc (-1 when there is none).
double NumberAfter(const std::string &doc, const std::string &key) {
  const std::size_t at = doc.find(key);
  return at == std::string::npos ? -1.0 : std::strtod(doc.c_str() + at + key.size(), nullptr);
}

// One packet, copied out of the engine's memory (valid until the next call).
struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0;
  int64_t ts = 0;
  std::vector<uint8_t> data;
  uint32_t duration_us = 0;
  uint16_t width = 0;
  uint16_t height = 0;
  uint32_t profile = 0;
  int32_t event = 0;
  im_status status = IM_OK;
  Clock::time_point at; // when im_next_packet returned it

  bool video() const { return kind == IM_PACKET_VIDEO; }
  bool key() const { return (flags & IM_PKT_KEYFRAME) != 0; }
  bool reinit() const { return (flags & IM_PKT_REINIT) != 0; }
  std::string json() const { return std::string(data.begin(), data.end()); }
};

// The status of im_next_packet and, on IM_OK, the packet.
im_status NextPkt(im_session s, uint32_t timeout_ms, Pkt *out) {
  im_packet p{};
  p.struct_size = sizeof p;
  const im_status st = im_next_packet(s, timeout_ms, &p);
  if (st != IM_OK) {
    return st;
  }
  out->kind = p.kind;
  out->layer = p.layer;
  out->flags = p.flags;
  out->ts = p.capture_ts_ns;
  out->data.assign(p.data, p.data + p.size);
  out->duration_us = p.duration_us;
  out->width = p.width;
  out->height = p.height;
  out->profile = p.profile;
  out->event = p.event;
  out->status = p.status;
  out->at = Clock::now();
  return IM_OK;
}

// Pulls packets for d, or until stop returns true for one (then it is the last one returned).
std::vector<Pkt> Pull(im_session s, milliseconds d, const std::function<bool(const Pkt &)> &stop = nullptr) {
  std::vector<Pkt> out;
  const auto end = Clock::now() + d;
  while (Clock::now() < end) {
    Pkt p;
    const im_status st = NextPkt(s, 50, &p);
    if (st == IM_TIMEOUT) {
      continue;
    }
    if (st != IM_OK) {
      break;
    }
    out.push_back(std::move(p));
    if (stop && stop(out.back())) {
      break;
    }
  }
  return out;
}

// The NAL units of an Annex B access unit.
std::vector<std::span<const uint8_t>> Nals(const std::vector<uint8_t> &au) {
  std::vector<std::span<const uint8_t>> nals;
  im::h264::ForEachNal(std::span<const uint8_t>(au), [&](std::span<const uint8_t> nal) {
    nals.push_back(nal);
    return true;
  });
  return nals;
}

// The fake slice's Marker (the RBSP after the slice header byte starts with it).
std::optional<im::audio::Marker> VideoMarker(const std::vector<uint8_t> &au) {
  for (const auto nal : Nals(au)) {
    const uint8_t type = im::h264::NalType(nal);
    if (type != 1 && type != 5) {
      continue;
    }
    std::vector<uint8_t> rbsp(nal.size());
    const std::size_t n = im::h264::UnescapeRbsp(nal.subspan(1), rbsp);
    im::audio::Marker m;
    if (n >= im::audio::kMarkerSize && im::audio::DecodeMarker(std::span<const uint8_t>(rbsp.data(), n), &m)) {
      return m;
    }
  }
  return std::nullopt;
}

std::optional<im::h264::Sps> FirstSps(const std::vector<uint8_t> &au) {
  for (const auto nal : Nals(au)) {
    if (im::h264::NalType(nal) == im::h264::nal::kSps) {
      im::h264::Sps sps;
      if (im::h264::ParseSps(nal, &sps) == im::h264::SpsError::kOk) {
        return sps;
      }
    }
  }
  return std::nullopt;
}

std::vector<uint8_t> NalTypes(const std::vector<uint8_t> &au) {
  std::vector<uint8_t> types;
  for (const auto nal : Nals(au)) {
    types.push_back(im::h264::NalType(nal));
  }
  return types;
}

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps, uint32_t bps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  l.bitrate_bps = bps;
  return l;
}

// test:pattern, f 1080p60 at 4 Mbit/s and q 360p15 at 300 kbit/s (Conformance's layers), the test tone.
im_start_config Config(const char *source = "test:pattern", bool tone = true, uint32_t profile = IM_PROFILE_HIGH) {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.flags = IM_START_ALLOW_LISTED_SOURCE;
  c.source_id = source;
  c.profile = profile;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_AUTO;
  c.layer_count = 2;
  c.layers[0] = Layer("f", 1920 * 1080, 60, 4'000'000);
  c.layers[1] = Layer("q", 640 * 360, 15, 300'000);
  c.audio.mode = tone ? IM_AUDIO_MODE_TEST_TONE : IM_AUDIO_MODE_OFF;
  return c;
}

class Session : public ::testing::Test {
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

  // Starts cfg and records when im_start returned.
  im_session Start(const im_start_config &cfg, im_session_info *info = nullptr) {
    im_session s = 0;
    im_session_info local{};
    local.struct_size = sizeof local;
    const im_status st = im_start(&cfg, &s, &local);
    started_ = Clock::now();
    EXPECT_EQ(st, IM_OK) << LastError();
    if (info != nullptr) {
      *info = local;
    }
    epoch_ = local.epoch_ns;
    return s;
  }

  Clock::time_point started_;
  int64_t epoch_ = 0;
};

// ---------------------------------------------------------------------------------------------- first packets

void ExpectFirstKeyframes(im_session s, Clock::time_point started, int64_t epoch, uint32_t layers, const char *what) {
  std::vector<std::optional<Pkt>> first(layers);
  Pull(s, milliseconds(2000), [&](const Pkt &p) {
    if (p.video() && p.layer < layers && !first[p.layer]) {
      first[p.layer] = p;
    }
    return std::all_of(first.begin(), first.end(), [](const auto &f) { return f.has_value(); });
  });
  for (uint32_t l = 0; l < layers; ++l) {
    SCOPED_TRACE(std::string(what) + ", layer " + std::to_string(l));
    ASSERT_TRUE(first[l].has_value()) << "no packet within 2 s";
    const Pkt &p = *first[l];
    EXPECT_TRUE(p.key() && p.reinit()) << "flags " << p.flags;
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(p.at - started).count(), 500);
    EXPECT_GE(p.ts, epoch);
    const std::vector<uint8_t> types = NalTypes(p.data);
    ASSERT_GE(types.size(), 3u);
    EXPECT_EQ(types[0], 7) << "SPS first";
    EXPECT_EQ(types[1], 8) << "then PPS";
    EXPECT_EQ(types[2], 5) << "then the IDR slice";
  }
}

TEST_F(Session, FirstPacketOfEachLayerIsAReinitKeyframeWithin500ms) {
  im_session_info info{};
  const im_session s = Start(Config(), &info);
  ASSERT_NE(s, 0u);
  EXPECT_GT(info.epoch_ns, 0);
  EXPECT_EQ(info.layer_count, 2u);
  EXPECT_EQ(info.capture_path, IM_CAPTURE_TEST);
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_NE(info.flags & IM_SESS_NO_OS_INDICATOR, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_OK);
  EXPECT_EQ(info.layers[0].encoder, IM_ENC_FAKE);
  EXPECT_EQ(std::string(info.layers[0].rid), "f");
  EXPECT_EQ(info.layers[0].width, 1920u);
  EXPECT_EQ(info.layers[0].height, 1080u);
  EXPECT_EQ(info.layers[0].fps, 60u);
  EXPECT_EQ(info.layers[0].profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
  EXPECT_EQ(info.layers[1].width, 640u);
  EXPECT_EQ(info.layers[1].height, 360u);
  EXPECT_EQ(info.layers[1].fps, 15u);
  EXPECT_FALSE(im::session::GpuPathOf(s)) << "IM_ENC_FAKE sessions run no GPU path: they stay off WARP";
  ExpectFirstKeyframes(s, started_, epoch_, 2, "moving pattern");
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, FirstKeyframeAlsoOnAStaticPattern) {
  im::testhooks::Options o;
  o.pattern = im::session::Pattern::kStatic;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  ExpectFirstKeyframes(s, started_, epoch_, 2, "static pattern");
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, FirstKeyframeWithoutAnyCapturedFrame) {
  // No frame lands: the slot's black is encoded as the IDR at 300 ms (§6.3).
  im::testhooks::Options o;
  o.pattern = im::session::Pattern::kNone;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  ExpectFirstKeyframes(s, started_, epoch_, 2, "no frame");
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- the fake's output

TEST_F(Session, FakeOutputCarriesRealParameterSetsAndTheMarker) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(1500));
  uint32_t video = 0;
  uint32_t audio = 0;
  std::vector<uint32_t> next_index(2, 0);
  for (const Pkt &p : pkts) {
    EXPECT_GE(p.ts, epoch_) << "kind " << p.kind; // every kind, events included (§5.2)
    if (p.kind == IM_PACKET_AUDIO) {
      ++audio;
      EXPECT_EQ(p.layer, IM_NO_LAYER);
      EXPECT_EQ(p.duration_us, 20000u);
      continue;
    }
    if (!p.video()) {
      continue;
    }
    ++video;
    ASSERT_LT(p.layer, 2u);
    const auto m = VideoMarker(p.data);
    ASSERT_TRUE(m.has_value()) << "a fake AU without its Marker";
    EXPECT_EQ(m->rid, p.layer == 0 ? 'f' : 'q');
    EXPECT_EQ(m->keyframe, p.key());
    EXPECT_EQ(m->capture_ns, p.ts - epoch_) << "Marker CaptureNS = capture_ts_ns - epoch_ns";
    EXPECT_EQ(m->frame, next_index[p.layer]) << "consecutive frame indices";
    next_index[p.layer] = m->frame + 1;
    EXPECT_EQ(p.profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
    EXPECT_EQ(p.width, p.layer == 0 ? 1920 : 640);
    EXPECT_EQ(p.height, p.layer == 0 ? 1080 : 360);
    if (p.key()) {
      const auto sps = FirstSps(p.data);
      ASSERT_TRUE(sps.has_value());
      EXPECT_EQ(sps->profile_idc, 100);
      EXPECT_EQ(sps->constraint_flags, 0x00);
      EXPECT_EQ(sps->width, p.layer == 0 ? 1920u : 640u);
      EXPECT_EQ(sps->height, p.layer == 0 ? 1080u : 360u) << "cropped from whole macroblocks";
      EXPECT_GE(sps->level_idc, p.layer == 0 ? 42 : 22) << "the level fits size and rate";
    } else {
      EXPECT_EQ(NalTypes(p.data), std::vector<uint8_t>{1}) << "a P AU is one NAL 1 slice";
    }
  }
  EXPECT_GT(video, 60u);
  EXPECT_GT(audio, 50u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, ConstrainedBaselineSessionsCarryE0) {
  const im_session s = Start(Config("test:pattern", false, IM_PROFILE_CB));
  ASSERT_NE(s, 0u);
  std::optional<Pkt> key;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    if (p.video() && p.key()) {
      key = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(key.has_value());
  EXPECT_EQ(key->profile, static_cast<uint32_t>(IM_PROFILE_CB));
  const auto sps = FirstSps(key->data);
  ASSERT_TRUE(sps.has_value());
  EXPECT_EQ(sps->profile_idc, 66);
  EXPECT_EQ(sps->constraint_flags, 0xE0);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, FrameRatesAndAccessUnitSizesFollowTheTargets) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300)); // past the first IDRs
  const std::vector<Pkt> pkts = Pull(s, milliseconds(3000));
  std::vector<uint64_t> frames(2, 0), bytes(2, 0);
  uint64_t p_bytes = 0, p_count = 0, k_bytes = 0, k_count = 0;
  int64_t first = 0, last = 0;
  for (const Pkt &p : pkts) {
    if (!p.video()) {
      continue;
    }
    ++frames[p.layer];
    bytes[p.layer] += p.data.size();
    if (p.layer == 0) {
      first = first == 0 ? p.ts : first;
      last = p.ts;
      (p.key() ? k_bytes : p_bytes) += p.data.size();
      ++(p.key() ? k_count : p_count);
    }
  }
  const double seconds = static_cast<double>(last - first) / 1e9;
  ASSERT_GT(seconds, 2.0);
  const double f_fps = static_cast<double>(frames[0] - 1) / seconds;
  const double q_fps = static_cast<double>(frames[1]) / seconds;
  EXPECT_NEAR(f_fps, 60.0, 3.0);
  EXPECT_NEAR(q_fps, 15.0, 1.5);
  // f over its 3 s carries no GOP IDR (4 s): the P size is lowered for the GOP's IDR, so 3 s of P frames are a bit
  // below the target, and within ±10 % of it.
  const double f_bps = static_cast<double>(bytes[0]) * 8.0 / seconds;
  EXPECT_GT(f_bps, 4'000'000 * 0.90) << f_bps;
  EXPECT_LT(f_bps, 4'000'000 * 1.10) << f_bps;
  const auto sizes = im::testhooks::FakeFrameSizes(4'000'000, 60, 0, 0);
  EXPECT_EQ(sizes.idr, sizes.p * 10);
  ASSERT_GT(p_count, 100u);
  const double p_avg = static_cast<double>(p_bytes) / static_cast<double>(p_count);
  EXPECT_NEAR(p_avg, sizes.p, sizes.p * 0.05);
  // Over a whole GOP (240 P-equivalents + 9 for the IDR) the average is the target within 1 %.
  const double gop_avg = (239.0 * sizes.p + sizes.idr) / 240.0 * 60.0 * 8.0;
  EXPECT_NEAR(gop_avg, 4'000'000, 40'000);
  (void)k_bytes;
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- keyframes

TEST_F(Session, KeyframeRequestIsAnsweredWithinOneInterval) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(600)); // ≥ 250 ms since the first IDR (the forced-IDR spacing, §6.4)
  const auto asked = Clock::now();
  ASSERT_EQ(im_request_keyframe(s, 0), IM_OK) << LastError();
  int f_frames = 0;
  std::optional<Pkt> key;
  Pull(s, milliseconds(1000), [&](const Pkt &p) {
    if (p.video() && p.layer == 0) {
      ++f_frames;
      if (p.key()) {
        key = p;
        return true;
      }
    }
    return false;
  });
  ASSERT_TRUE(key.has_value());
  EXPECT_LE(f_frames, 2) << "the next or second-next frame of f";
  EXPECT_FALSE(key->reinit()) << "a requested IDR is no re-init";
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(key->at - asked).count(), 50);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, KeyframeOnAStaticScreenWithinOneInterval) {
  im::testhooks::Options o;
  o.pattern = im::session::Pattern::kStatic;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(700)); // the first IDRs and the 200 ms refresh
  const auto asked = Clock::now();
  ASSERT_EQ(im_request_keyframe(s, IM_ALL_LAYERS), IM_OK) << LastError();
  std::vector<std::optional<Pkt>> key(2);
  Pull(s, milliseconds(1000), [&](const Pkt &p) {
    if (p.video() && p.key()) {
      key[p.layer] = p;
    }
    return key[0] && key[1];
  });
  for (uint32_t l = 0; l < 2; ++l) {
    ASSERT_TRUE(key[l].has_value()) << "layer " << l;
    // Within one interval of layer 0 (16.7 ms) plus scheduling: the last frame re-encoded as an IDR (§8.5).
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(key[l]->at - asked).count(), l == 0 ? 50 : 100);
    EXPECT_GE(key[l]->ts, epoch_ + kSecond / 2) << "a static IDR is stamped now";
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, AccessUnitsOfALayerAreAtLeastOneRtpTickApart) {
  // A pending keyframe request and a frame that arrives late: the pacer re-encodes the last frame as an IDR stamped
  // now (§8.5), and the late frame, captured before that instant, follows with the pacer's 1 µs bump (§8.4). The
  // session keeps two AUs of a layer one 90 kHz tick apart, or 01's publisher would send both under one RTP timestamp
  // and the viewer would lose the IDR. Two of every three pattern frames arrive 8 ms late, so most requests hit this.
  constexpr int64_t kRtpTickNs = 11'112; // ceil(1e9 / 90000)
  im::testhooks::Options o;
  o.pattern_late_ns = 8'000'000;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(600));
  for (int i = 0; i < 20; ++i) {
    ASSERT_EQ(im_request_keyframe(s, 0), IM_OK) << LastError();
    std::vector<Pkt> more = Pull(s, milliseconds(250 + (i * 13) % 61)); // 250–310 ms: past the IDR spacing (§6.4)
    pkts.insert(pkts.end(), std::make_move_iterator(more.begin()), std::make_move_iterator(more.end()));
  }
  std::vector<int64_t> last(2, 0);
  int keys = 0, one_tick = 0;
  for (const Pkt &p : pkts) {
    if (!p.video()) {
      continue;
    }
    ASSERT_LT(p.layer, 2u);
    if (last[p.layer] != 0) {
      const int64_t step = p.ts - last[p.layer];
      EXPECT_GE(step, kRtpTickNs) << "layer " << p.layer << " at " << p.ts - epoch_ << " ns, flags " << p.flags;
      one_tick += step == kRtpTickNs ? 1 : 0;
    }
    last[p.layer] = p.ts;
    keys += p.layer == 0 && p.key() ? 1 : 0;
  }
  EXPECT_GE(keys, 18) << "the first IDR and the 20 requests' (some may coalesce)";
  EXPECT_GT(one_tick, 0) << "no static IDR was followed by a late frame: the case went untested";
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, StaticScreenRefreshesAfter200msThenEverySecond) {
  im::testhooks::Options o;
  o.pattern = im::session::Pattern::kStatic;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  std::vector<int64_t> refreshes;
  for (const Pkt &p : Pull(s, milliseconds(2600))) {
    if (p.video() && p.layer == 0 && (p.flags & IM_PKT_REFRESH) != 0) {
      refreshes.push_back(p.ts - epoch_);
    }
  }
  ASSERT_GE(refreshes.size(), 2u);
  EXPECT_NEAR(static_cast<double>(refreshes[0]) / 1e6, 200.0, 40.0);
  EXPECT_NEAR(static_cast<double>(refreshes[1] - refreshes[0]) / 1e6, 1000.0, 50.0);
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- layer, pause

TEST_F(Session, LayerActiveStopsAndRestartsWithAnIdr) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(500));
  ASSERT_EQ(im_set_layer_active(s, 0, 0), IM_OK) << LastError();
  Pull(s, milliseconds(100)); // what was already encoded
  uint32_t f = 0, q = 0, audio = 0;
  for (const Pkt &p : Pull(s, milliseconds(1000))) {
    if (p.video()) {
      ++(p.layer == 0 ? f : q);
    } else if (p.kind == IM_PACKET_AUDIO) {
      ++audio;
    }
  }
  EXPECT_EQ(f, 0u) << "an inactive layer is not encoded";
  EXPECT_GE(q, 12u);
  EXPECT_GE(audio, 45u);
  const auto resumed = Clock::now();
  ASSERT_EQ(im_set_layer_active(s, 0, 1), IM_OK) << LastError();
  std::optional<Pkt> first;
  Pull(s, milliseconds(1000), [&](const Pkt &p) {
    if (p.video() && p.layer == 0) {
      first = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(first.has_value());
  EXPECT_TRUE(first->key());
  EXPECT_FALSE(first->reinit()) << "reactivation is an IDR, not a re-init (§8.5)";
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(first->at - resumed).count(), 500);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, PauseSendsOnlyRefreshesAndIdrsWhileAudioFlows) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(500));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Pull(s, milliseconds(100)); // one frame interval, and what was in the rings
  std::this_thread::sleep_for(milliseconds(300));
  ASSERT_EQ(im_request_keyframe(s, 0), IM_OK);
  uint32_t audio = 0, new_frames = 0, idrs = 0, refreshes = 0;
  const std::vector<Pkt> pkts = Pull(s, milliseconds(2000));
  // fpsIn counts captured frames (§5.6) and capture runs on while paused (§8.5): the 60 Hz pattern over the last 2 s,
  // all of them paused, although the video thread takes none of them.
  const std::string stats = StatsOf(s);
  EXPECT_NE(stats.find("\"paused\":true"), std::string::npos) << stats;
  EXPECT_GT(NumberAfter(stats, "\"fpsIn\":"), 40.0) << stats;
  int64_t first_audio = 0, last_audio = 0;
  for (const Pkt &p : pkts) {
    if (p.kind == IM_PACKET_AUDIO) {
      ++audio;
      first_audio = first_audio == 0 ? p.ts : first_audio;
      last_audio = p.ts;
    } else if (p.video()) {
      if ((p.flags & IM_PKT_REFRESH) != 0) {
        ++refreshes;
      } else if (p.key()) {
        ++idrs;
      } else {
        ++new_frames;
      }
    }
  }
  EXPECT_EQ(new_frames, 0u) << "no new captured content while paused";
  EXPECT_GE(idrs, 1u) << "a keyframe request while paused is an IDR of the frozen frame";
  EXPECT_GE(refreshes, 1u);
  // 50 silent packets a second on the unbroken grid (the tone fades out in 10 ms, §10.3).
  ASSERT_GT(audio, 80u);
  EXPECT_NEAR(static_cast<double>(last_audio - first_audio) / static_cast<double>(audio - 1), 20e6, 1e3);
  ASSERT_EQ(im_set_paused(s, 0), IM_OK);
  std::optional<Pkt> fresh;
  Pull(s, milliseconds(500), [&](const Pkt &p) {
    if (p.video() && p.layer == 0 && (p.flags & IM_PKT_REFRESH) == 0 && !p.key()) {
      fresh = p;
      return true;
    }
    return false;
  });
  EXPECT_TRUE(fresh.has_value()) << "resume encodes new frames again";
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- re-inits

// Waits for the next REINIT|KEYFRAME of both layers; returns them (index 0 f, 1 q).
std::vector<std::optional<Pkt>> NextReinits(im_session s, milliseconds d) {
  std::vector<std::optional<Pkt>> r(2);
  Pull(s, d, [&](const Pkt &p) {
    if (p.video() && p.reinit() && p.layer < 2 && !r[p.layer]) {
      EXPECT_TRUE(p.key());
      r[p.layer] = p;
    }
    return r[0] && r[1];
  });
  return r;
}

TEST_F(Session, SetProfileReinitsEveryLayerInTheNewProfile) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  ASSERT_EQ(im_set_profile(s, IM_PROFILE_CB), IM_OK) << LastError();
  const auto r = NextReinits(s, milliseconds(2000));
  for (uint32_t l = 0; l < 2; ++l) {
    ASSERT_TRUE(r[l].has_value()) << "layer " << l;
    EXPECT_EQ(r[l]->profile, static_cast<uint32_t>(IM_PROFILE_CB));
    const auto sps = FirstSps(r[l]->data);
    ASSERT_TRUE(sps.has_value());
    EXPECT_EQ(sps->profile_idc, 66);
    EXPECT_EQ(sps->constraint_flags, 0xE0);
  }
  im_session_info info{};
  info.struct_size = sizeof info;
  ASSERT_EQ(im_get_session_info(s, &info), IM_OK);
  EXPECT_EQ(info.layers[0].profile, static_cast<uint32_t>(IM_PROFILE_CB));
  EXPECT_EQ(info.layers[1].profile, static_cast<uint32_t>(IM_PROFILE_CB));
  // Every packet after the switch is CB.
  for (const Pkt &p : Pull(s, milliseconds(300))) {
    if (p.video()) {
      EXPECT_EQ(p.profile, static_cast<uint32_t>(IM_PROFILE_CB));
    }
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, ContentHintAndUpdateLayerInOneSecondAreOneReinit) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  // A re-init now (the profile), so the next requests fall into its 1 s spacing window (§6.4) and coalesce.
  ASSERT_EQ(im_set_profile(s, IM_PROFILE_CB), IM_OK);
  ASSERT_TRUE(NextReinits(s, milliseconds(2000))[0].has_value());
  const auto t0 = Clock::now();
  ASSERT_EQ(im_set_content_hint(s, IM_CONTENT_DETAIL), IM_OK) << LastError();
  im_layer_config q = Layer("q", 640 * 360, 10, 300'000);
  ASSERT_EQ(im_update_layer(s, 1, &q), IM_OK) << LastError();
  std::vector<uint32_t> reinits(2, 0);
  std::vector<Clock::time_point> when(2);
  for (const Pkt &p : Pull(s, milliseconds(2500))) {
    if (p.video() && p.reinit()) {
      ++reinits[p.layer];
      when[p.layer] = p.at;
    }
  }
  EXPECT_EQ(reinits[0], 1u) << "f: the content hint";
  EXPECT_EQ(reinits[1], 1u) << "q: the content hint and the new fps together";
  EXPECT_GE(std::chrono::duration_cast<milliseconds>(when[1] - t0).count(), 400) << "spaced from the profile re-init";
  im_session_info info{};
  info.struct_size = sizeof info;
  ASSERT_EQ(im_get_session_info(s, &info), IM_OK);
  EXPECT_EQ(info.layers[1].fps, 10u);
  // The same hint again changes nothing.
  ASSERT_EQ(im_set_content_hint(s, IM_CONTENT_DETAIL), IM_OK);
  for (const Pkt &p : Pull(s, milliseconds(1500))) {
    EXPECT_FALSE(p.video() && p.reinit()) << "layer " << p.layer;
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, LiveBitrateChangeNeedsNoReinit) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  ASSERT_EQ(im_set_bitrate(s, 0, 1'000'000), IM_OK) << LastError();
  Pull(s, milliseconds(200));
  uint64_t bytes = 0;
  int64_t first = 0, last = 0;
  for (const Pkt &p : Pull(s, milliseconds(2000))) {
    if (p.video() && p.layer == 0) {
      EXPECT_FALSE(p.reinit()) << "IM_ENC_FAKE changes its bitrate live";
      bytes += p.data.size();
      first = first == 0 ? p.ts : first;
      last = p.ts;
    }
  }
  const double bps = static_cast<double>(bytes) * 8.0 / (static_cast<double>(last - first) / 1e9);
  EXPECT_NEAR(bps, 1'000'000, 120'000);
  im_session_info info{};
  info.struct_size = sizeof info;
  ASSERT_EQ(im_get_session_info(s, &info), IM_OK);
  EXPECT_EQ(info.layers[0].bitrate_bps, 1'000'000u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- injected failures

TEST_F(Session, EncodeErrorReopensTheSameEncoder) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  ASSERT_EQ(im::testhooks::Inject(s, 0, im::testhooks::Fault::kEncodeError), IM_OK);
  bool changed = false;
  std::optional<Pkt> reinit;
  Pull(s, milliseconds(1000), [&](const Pkt &p) {
    changed = changed || (p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_ENCODER_CHANGED);
    if (p.video() && p.layer == 0 && p.reinit()) {
      reinit = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(reinit.has_value());
  EXPECT_TRUE(reinit->key());
  EXPECT_FALSE(changed) << "the same encoder again: no ENCODER_CHANGED";
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, EncoderChangedComesBeforeTheNextCandidatesFirstPacket) {
  im::testhooks::Options o;
  o.fallback_kinds = {IM_ENC_MF_SW};
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  im::testhooks::FailOpens(IM_ENC_FAKE, true); // the re-open of the failed encoder fails too
  ASSERT_EQ(im::testhooks::Inject(s, 0, im::testhooks::Fault::kEncodeError), IM_OK);
  std::optional<Pkt> event;
  std::optional<Pkt> reinit;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    if (p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_ENCODER_CHANGED) {
      EXPECT_FALSE(reinit.has_value()) << "ENCODER_CHANGED after the new encoder's packet";
      event = p;
    }
    if (p.video() && p.layer == 0 && p.reinit()) {
      reinit = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(event.has_value());
  ASSERT_TRUE(reinit.has_value());
  EXPECT_TRUE(reinit->key());
  EXPECT_EQ(event->status, IM_ERR_ENCODER_LOST);
  EXPECT_EQ(event->layer, 0u);
  const std::string j = event->json();
  EXPECT_NE(j.find("\"layer\":\"f\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"from\":\"fake\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"to\":\"mf-sw\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"reason\":\"encode_error\""), std::string::npos) << j;
  im_session_info info{};
  info.struct_size = sizeof info;
  ASSERT_EQ(im_get_session_info(s, &info), IM_OK);
  EXPECT_EQ(info.layers[0].encoder, IM_ENC_MF_SW);
  EXPECT_EQ(info.layers[1].encoder, IM_ENC_FAKE) << "only the failed layer falls back";
  EXPECT_EQ(info.layers[0].flags & IM_ENCF_LIVE_BITRATE, 0u) << "mf-sw re-inits for a bitrate";
  EXPECT_EQ(im_stop(s), IM_OK);
}

// Waits for ENDED; returns it.
std::optional<Pkt> WaitEnded(im_session s, milliseconds d) {
  std::optional<Pkt> ended;
  const auto end = Clock::now() + d;
  while (Clock::now() < end) {
    Pkt p;
    const im_status st = NextPkt(s, 50, &p);
    if (st == IM_OK && p.kind == IM_PACKET_EVENT) {
      EXPECT_NE(p.event, IM_EVENT_DEVICE_RESET) << "M2 has no device-loss recovery";
      if (p.event == IM_EVENT_ENDED) {
        ended = p;
        break;
      }
    }
    if (st == IM_ENDED) {
      break;
    }
  }
  return ended;
}

TEST_F(Session, DeviceRemovalEndsTheSessionAndTheNextStartGetsANewDevice) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  const uint64_t old_device = im::testhooks::SessionDevice(s);
  EXPECT_NE(old_device, 0u);
  const auto injected = Clock::now();
  ASSERT_EQ(im::testhooks::Inject(s, 0, im::testhooks::Fault::kDeviceRemoved), IM_OK);
  const auto ended = WaitEnded(s, milliseconds(1500));
  ASSERT_TRUE(ended.has_value());
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(ended->at - injected).count(), 1000);
  EXPECT_EQ(ended->status, IM_ERR_ENCODER_LOST);
  const std::string j = ended->json();
  EXPECT_NE(j.find("\"reason\":\"encoder_lost\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"detail\":\"device_removed: "), std::string::npos) << j;
  Pkt p;
  EXPECT_EQ(NextPkt(s, 0, &p), IM_ENDED) << "ENDED is the last packet";
  EXPECT_TRUE(im::testhooks::ProbeCacheStale());
  EXPECT_EQ(im_stop(s), IM_OK) << "the host still stops an ended session";

  const im_session again = Start(Config());
  ASSERT_NE(again, 0u);
  EXPECT_NE(im::testhooks::SessionDevice(again), old_device);
  EXPECT_FALSE(im::testhooks::ProbeCacheStale()) << "the next start probed the encoders again (04 §9.8)";
  ExpectFirstKeyframes(again, started_, epoch_, 2, "after the device removal");
  EXPECT_EQ(im_stop(again), IM_OK);
}

TEST_F(Session, ExhaustedCandidatesEndTheSession) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  ASSERT_EQ(im::testhooks::Inject(s, 1, im::testhooks::Fault::kEncodeError), IM_OK);
  const auto ended = WaitEnded(s, milliseconds(1500));
  ASSERT_TRUE(ended.has_value());
  EXPECT_EQ(ended->status, IM_ERR_ENCODER_LOST);
  EXPECT_NE(ended->json().find("\"reason\":\"encoder_lost\""), std::string::npos) << ended->json();
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- the consumer

TEST_F(Session, AConsumerThatStopsGetsOverflowStaleDropsAndAnIdr) {
  im_start_config cfg = Config();
  cfg.layers[0].bitrate_bps = 20'000'000;
  const im_session s = Start(cfg);
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300));
  std::this_thread::sleep_for(milliseconds(2000)); // the consumer stalls (a pub PC rebuild, a GC pause)
  const auto resumed = Clock::now();
  bool overflow = false;
  bool discontinuity = false;
  std::optional<Pkt> idr;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    if (p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_RING_OVERFLOW) {
      overflow = true;
      EXPECT_NE(p.json().find("\"dropped\":"), std::string::npos) << p.json();
    }
    if (p.video() && p.layer == 0) {
      discontinuity = discontinuity || (p.flags & IM_PKT_DISCONTINUITY) != 0;
      if (p.key()) {
        idr = p;
        return true;
      }
    }
    return false;
  });
  EXPECT_TRUE(overflow) << "RING_OVERFLOW";
  EXPECT_TRUE(discontinuity) << "DISCONTINUITY on the first packet after the drops";
  ASSERT_TRUE(idr.has_value()) << "an IDR when the consumer resumes";
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(idr->at - resumed).count(), 500);
  char buf[4096];
  const int32_t n = im_get_stats(s, buf, static_cast<int32_t>(sizeof buf));
  ASSERT_GT(n, 0);
  const std::string stats(buf, static_cast<std::size_t>(std::min<int32_t>(n, static_cast<int32_t>(sizeof buf) - 1)));
  EXPECT_EQ(stats.find("\"stale\":0}"), std::string::npos) << "stale drops are counted: " << stats;
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, StopWithABlockedConsumer) {
  im::testhooks::Options o;
  o.pattern = im::session::Pattern::kStatic;
  im::testhooks::Configure(o);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(1500)); // the first IDRs and refreshes: from here the consumer mostly waits
  std::atomic<bool> stopping{false};
  std::atomic<int64_t> returned_after_ms{-1};
  std::atomic<im_status> last{IM_OK};
  Clock::time_point stop_called;
  std::thread consumer([&] {
    for (;;) {
      Pkt p;
      const im_status st = NextPkt(s, 1000, &p);
      if (st == IM_ENDED || st == IM_ERR_INVALID_ARG) {
        last = st;
        if (stopping.load()) {
          returned_after_ms = std::chrono::duration_cast<milliseconds>(Clock::now() - stop_called).count();
        }
        return;
      }
    }
  });
  std::this_thread::sleep_for(milliseconds(300));
  stop_called = Clock::now();
  stopping = true;
  EXPECT_EQ(im_stop(s), IM_OK);
  const auto stop_ms = std::chrono::duration_cast<milliseconds>(Clock::now() - stop_called).count();
  consumer.join();
  EXPECT_LE(stop_ms, 2000);
  EXPECT_EQ(last.load(), IM_ENDED);
  EXPECT_GE(returned_after_ms.load(), 0);
  EXPECT_LE(returned_after_ms.load(), 50);
  im_packet pkt{};
  pkt.struct_size = sizeof pkt;
  EXPECT_EQ(im_next_packet(s, 0, &pkt), IM_ERR_INVALID_ARG) << "the handle is invalid after im_stop";
  EXPECT_EQ(im_stop(s), IM_ERR_INVALID_ARG);
}

// ---------------------------------------------------------------------------------------------- limits

TEST_F(Session, TheFifthSessionIsLimit) {
  std::vector<im_session> s;
  for (int i = 0; i < 4; ++i) {
    s.push_back(Start(Config("test:pattern", false)));
    ASSERT_NE(s.back(), 0u) << i;
  }
  im_start_config cfg = Config("test:pattern", false);
  im_session fifth = 7;
  EXPECT_EQ(im_start(&cfg, &fifth, nullptr), IM_ERR_LIMIT);
  EXPECT_TRUE(StartsWith(LastError(), "session_limit: ")) << LastError();
  EXPECT_EQ(fifth, 0u);
  for (const im_session h : s) {
    EXPECT_EQ(im_stop(h), IM_OK);
  }
}

TEST_F(Session, OneSessionWithAudio) {
  const im_session a = Start(Config());
  ASSERT_NE(a, 0u);
  im_start_config cfg = Config();
  im_session b = 0;
  EXPECT_EQ(im_start(&cfg, &b, nullptr), IM_ERR_LIMIT);
  EXPECT_TRUE(StartsWith(LastError(), "session_limit: ")) << LastError();
  const im_session video_only = Start(Config("test:pattern", false));
  EXPECT_NE(video_only, 0u) << "a second session without audio runs";
  EXPECT_EQ(im_stop(video_only), IM_OK);
  EXPECT_EQ(im_stop(a), IM_OK);
}

// ---------------------------------------------------------------------------------------------- pick tokens

TEST_F(Session, PickTokens) {
  const std::string token = im::testhooks::FakePick(IM_SOURCE_WINDOW, 1280, 720);
  EXPECT_TRUE(StartsWith(token, "pick:")) << token;
  im_start_config cfg = Config(token.c_str(), false);
  cfg.flags = 0; // a pick needs no IM_START_ALLOW_LISTED_SOURCE

  // A failed start keeps the token.
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  im_session s = 0;
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_NO_ENCODER);
  EXPECT_TRUE(StartsWith(LastError(), "layer_failed: ")) << LastError();
  im::testhooks::FailOpens(IM_ENC_FAKE, false);
  im_session_info info{};
  s = Start(cfg, &info);
  ASSERT_NE(s, 0u) << "the token survived the failed start";
  EXPECT_EQ(info.layers[0].width, 1280u);
  EXPECT_EQ(info.layers[0].height, 720u);
  EXPECT_EQ(im_stop(s), IM_OK);

  // Consumed.
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_SOURCE_GONE);
  EXPECT_TRUE(StartsWith(LastError(), "pick_expired: ")) << LastError();

  // Expired: 5 min after the pick (the clock hook).
  const std::string expiring = im::testhooks::FakePick();
  im::testhooks::AdvancePickClock(im::session::kPickLifetimeNs);
  cfg.source_id = expiring.c_str();
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_SOURCE_GONE);
  EXPECT_TRUE(StartsWith(LastError(), "pick_expired: ")) << LastError();

  // Evicted: a fifth pick evicts the oldest unused one.
  std::vector<std::string> picks;
  for (int i = 0; i < 5; ++i) {
    picks.push_back(im::testhooks::FakePick());
  }
  EXPECT_EQ(im::session::Sources().PickCount(), 4u);
  cfg.source_id = picks[0].c_str();
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_SOURCE_GONE);
  EXPECT_TRUE(StartsWith(LastError(), "pick_expired: ")) << LastError();
  cfg.source_id = picks[1].c_str();
  s = Start(cfg);
  EXPECT_NE(s, 0u) << "the other four are still there";
  EXPECT_EQ(im_stop(s), IM_OK);

  // A listed source needs the flag; a token never named is pick_expired too.
  im_start_config listed = Config("test:pattern", false);
  listed.flags = 0;
  EXPECT_EQ(im_start(&listed, &s, nullptr), IM_ERR_INVALID_ARG);
  listed.source_id = "pick:999999";
  EXPECT_EQ(im_start(&listed, &s, nullptr), IM_ERR_SOURCE_GONE);
  // im_shutdown forgets every pick (TearDown), so nothing leaks into the next test.
}

// ---------------------------------------------------------------------------------------------- audio

TEST_F(Session, AudioOnlyTestToneAndPcmTap) {
  im_start_config cfg{};
  cfg.struct_size = sizeof cfg;
  cfg.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  cfg.audio.flags = IM_AUDIOF_TAP_PCM;
  im_session_info info{};
  const im_session s = Start(cfg, &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.layer_count, 0u);
  uint32_t opus = 0, pcm = 0;
  int64_t first_opus = -1;
  for (const Pkt &p : Pull(s, milliseconds(1200))) {
    EXPECT_GE(p.ts, epoch_);
    if (p.kind == IM_PACKET_AUDIO) {
      if (opus == 0) {
        EXPECT_TRUE(p.reinit()) << "the first audio packet starts a grid";
        first_opus = p.ts;
      }
      ++opus;
    } else if (p.kind == IM_PACKET_PCM) {
      ++pcm;
      EXPECT_EQ(p.data.size(), 3840u);
      EXPECT_EQ(p.duration_us, 10000u);
    } else {
      EXPECT_NE(p.kind, IM_PACKET_VIDEO);
    }
  }
  EXPECT_EQ(first_opus, epoch_) << "the tone's packets are stamped epoch_ns + k * 20 ms";
  EXPECT_GE(opus, 50u);
  EXPECT_GE(pcm, 2 * opus - 4);
  EXPECT_EQ(im_set_audio_bitrate(s, 256000), IM_OK) << LastError();
  EXPECT_EQ(im_request_keyframe(s, 0), IM_ERR_INVALID_ARG) << "no video layer";
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, ToneBeepsLandOnTheVideoFlashes) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  std::vector<int64_t> flashes_f, flashes_q, beeps;
  uint32_t marked = 0;
  for (const Pkt &p : Pull(s, milliseconds(3300))) {
    if (p.kind == IM_PACKET_AUDIO) {
      im::audio::Marker m;
      if (im::audio::ParseAudioMarker(p.data, &m)) {
        ++marked;
        EXPECT_EQ(m.capture_ns, p.ts - epoch_);
        if (m.beep) {
          beeps.push_back(m.capture_ns);
        }
      }
    } else if (p.video()) {
      const auto m = VideoMarker(p.data);
      if (m && m->flash) {
        (p.layer == 0 ? flashes_f : flashes_q).push_back(m->capture_ns);
      }
    }
  }
  EXPECT_GT(marked, 100u) << "the tone's packets carry M1's Marker under test hooks with IM_ENC_FAKE";
  ASSERT_GE(beeps.size(), 3u);
  for (std::size_t k = 0; k < beeps.size(); ++k) {
    EXPECT_EQ(beeps[k], static_cast<int64_t>(k) * kSecond) << "a beep starts each second at epoch_ns + k s";
  }
  ASSERT_GE(flashes_f.size(), 3u);
  ASSERT_GE(flashes_q.size(), 3u);
  for (std::size_t k = 0; k < 3; ++k) {
    // The pattern's frames are on a grid anchored at epoch_ns, so f (every frame) shows the flash frame exactly at
    // k s. q thins the 60 Hz pattern to 15 fps by the pacer's schedule (§8.4), so its flash is its first frame at or
    // after k s: within one q interval.
    EXPECT_EQ(flashes_f[k], static_cast<int64_t>(k) * kSecond);
    EXPECT_GE(flashes_q[k], static_cast<int64_t>(k) * kSecond);
    EXPECT_LT(flashes_q[k], static_cast<int64_t>(k) * kSecond + kSecond / 15 + 2'000'000);
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ---------------------------------------------------------------------------------------------- stats, handles

TEST_F(Session, StatsDocument) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(2500));
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  ASSERT_GT(n, 0) << LastError();
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  for (const char *field : {"\"layers\":[",
                            "\"rid\":\"f\"",
                            "\"rid\":\"q\"",
                            "\"encoder\":\"fake\"",
                            "\"codec\":\"fake\"",
                            "\"hw\":false",
                            "\"width\":1920",
                            "\"height\":1080",
                            "\"active\":true",
                            "\"fpsIn\":",
                            "\"fpsOut\":",
                            "\"bitrateBps\":",
                            "\"targetBps\":4000000",
                            "\"keyframes\":",
                            "\"refreshes\":",
                            "\"reinits\":0",
                            "\"drops\":{\"pacer\":",
                            "\"encodeMs\":{\"p50\":",
                            "\"latencyMs\":{\"p50\":",
                            "\"limit\":\"none\"",
                            "\"capture\":{\"path\":\"test\"",
                            "\"sourceWidth\":1920",
                            "\"sourceHeight\":1080",
                            "\"frames\":",
                            "\"interrupted\":false",
                            "\"audio\":{\"state\":\"ok\"",
                            "\"packets\":",
                            "\"bitrateBps\":128000",
                            "\"paused\":false"}) {
    EXPECT_NE(doc.find(field), std::string::npos) << field << " in " << doc;
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(Session, ControlCallsCheckTheLayerAndTheHandle) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  EXPECT_EQ(im_request_keyframe(s, 2), IM_ERR_INVALID_ARG) << "the session has 2 layers";
  EXPECT_EQ(im_set_bitrate(s, 2, 100000), IM_ERR_INVALID_ARG);
  im_layer_config wrong = Layer("x", 0, 30, 0);
  EXPECT_EQ(im_update_layer(s, 1, &wrong), IM_ERR_INVALID_ARG) << "the rid must be the layer's";
  EXPECT_EQ(im_set_audio_bitrate(s, 128000), IM_ERR_INVALID_ARG) << "no audio";
  EXPECT_EQ(im_stop(s), IM_OK);
  for (const auto fn :
       {+[](im_session h) { return im_request_keyframe(h, 0); }, +[](im_session h) { return im_set_paused(h, 1); },
        +[](im_session h) { return im_set_profile(h, IM_PROFILE_CB); }}) {
    EXPECT_EQ(fn(s), IM_ERR_INVALID_ARG) << "a stopped session's handle is stale";
  }
}

TEST_F(Session, NothingToCaptureIsInvalid) {
  im_start_config cfg{};
  cfg.struct_size = sizeof cfg;
  cfg.audio.mode = IM_AUDIO_MODE_OFF;
  im_session s = 1;
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_INVALID_ARG);
  EXPECT_EQ(s, 0u);
}

// ------------------------------------------------------------------------ the catalog, AU times, GOP (README WF10)

// The session's catalog lists the probe's encoders (encode::CatalogEntries) after the test hooks' fakes: real
// entries that take pixels, the probe's own IM_ENC_FAKE left out. A test: session still runs IM_ENC_FAKE without a
// GPU path, and a fallback kind a test asked for comes before the real encoder of that kind.
TEST_F(Session, CatalogListsTheProbesEncodersAfterTheFakes) {
  im::testhooks::Options o;
  o.fallback_kinds = {IM_ENC_MF_SW};
  im::testhooks::Configure(o);
  im_session_info info{};
  const im_session s = Start(Config("test:pattern", false), &info); // im_start runs the encoder probe on the worker
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.layers[0].encoder, IM_ENC_FAKE);
  EXPECT_EQ(info.layers[1].encoder, IM_ENC_FAKE);
  EXPECT_FALSE(im::session::GpuPathOf(s)) << "the first candidate takes nothing: no device, whatever follows it";
  EXPECT_EQ(im_stop(s), IM_OK);

  const std::vector<im::session::EncoderEntry> catalog = im::session::EncoderCatalog(); // the stored probe
  ASSERT_GE(catalog.size(), 4u) << "fake, fake-backed mf-sw, then at least the probe's MF software and OpenH264";
  EXPECT_EQ(catalog[0].info.kind, IM_ENC_FAKE);
  EXPECT_EQ(catalog[0].input, im::session::FrameInput::kNone);
  EXPECT_EQ(catalog[1].info.kind, IM_ENC_MF_SW);
  EXPECT_EQ(catalog[1].input, im::session::FrameInput::kNone) << "the fallback kind is the fake's";
  bool mf_sw = false;
  bool openh264 = false;
  for (std::size_t i = 2; i < catalog.size(); ++i) {
    const im::session::EncoderEntry &e = catalog[i];
    SCOPED_TRACE("entry " + std::to_string(i) + ", kind " + std::to_string(e.info.kind));
    EXPECT_NE(e.info.kind, IM_ENC_FAKE) << "the probe's IM_ENC_FAKE is left out: the catalog adds the fake itself";
    EXPECT_EQ(e.input, im::session::FrameInput::kNv12);
    EXPECT_NE(e.open, nullptr);
    mf_sw = mf_sw || e.info.kind == IM_ENC_MF_SW;
    openh264 = openh264 || e.info.kind == IM_ENC_OPENH264;
  }
  EXPECT_TRUE(mf_sw) << "the probe always lists MF software";
  EXPECT_TRUE(openh264) << "the probe always lists OpenH264";
}

// An encoder that holds one frame back (some MF and QSV configurations, 04 §9.2): each Encode hands back the access
// unit of the frame before, which the test hooks' fake encodes from that frame's EncodeInput (so its Marker carries
// that frame's capture time), with EncodedAu::capture_ts_ns set to it; the first Encode returns nothing.
class FrameBehindEncoder final : public im::session::VideoEncoder {
public:
  explicit FrameBehindEncoder(std::unique_ptr<im::session::VideoEncoder> inner, uint32_t layer) noexcept
      : inner_(std::move(inner)), layer_(layer) {}

  static im_status Open(const im::session::EncoderParams &p, std::unique_ptr<im::session::VideoEncoder> *out) noexcept {
    try {
      std::unique_ptr<im::session::VideoEncoder> inner;
      const im_status st = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE).open(p, &inner);
      if (st != IM_OK) {
        return st;
      }
      *out = std::make_unique<FrameBehindEncoder>(std::move(inner), p.layer);
      return IM_OK;
    } catch (...) {
      return IM_ERR_INTERNAL;
    }
  }

  im_status Encode(const im::session::EncodeInput &in, im::session::EncodedAu *out) noexcept override {
    *out = im::session::EncodedAu{};
    if (layer_ == 0 && FirstF().load() == 0) {
      FirstF().store(in.ts_ns); // the first frame f's encoder was given (f's one video thread writes it)
    }
    if (!held_) {
      held_ = in;
      return IM_OK; // held back
    }
    const im::session::EncodeInput prev = *held_;
    held_ = in;
    const im_status st = inner_->Encode(prev, out);
    if (st == IM_OK && !out->data.empty()) {
      out->capture_ts_ns = prev.ts_ns;
    }
    return st;
  }

  im_status SetBitrate(uint32_t bps) noexcept override { return inner_->SetBitrate(bps); }

  // The capture time of the first frame given to a layer-0 FrameBehindEncoder (0 before one); the test resets it.
  static std::atomic<int64_t> &FirstF() noexcept {
    static std::atomic<int64_t> first{0};
    return first;
  }

private:
  std::unique_ptr<im::session::VideoEncoder> inner_;
  uint32_t layer_ = 0;
  std::optional<im::session::EncodeInput> held_;
};

// Clears the catalog prefix a test set, also when an assertion ends it early.
struct PrefixGuard {
  explicit PrefixGuard(std::vector<im::session::EncoderEntry> entries) {
    im::session::SetCatalogPrefixForTest(std::move(entries));
  }
  ~PrefixGuard() { im::session::SetCatalogPrefixForTest({}); }
  PrefixGuard(const PrefixGuard &) = delete;
  PrefixGuard &operator=(const PrefixGuard &) = delete;
};

// 04 §9.2's pts → capture-time table, at the session: with an encoder one frame behind, every packet carries its own
// frame's capture time (the Marker's CaptureNS, which the fake wrote from that frame), not the next frame's, and the
// first IDR carries the first frame's time.
TEST_F(Session, AnEncoderOneFrameBehindKeepsEachAccessUnitsOwnCaptureTime) {
  im::session::EncoderEntry e = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE);
  e.open = &FrameBehindEncoder::Open;
  e.input = im::session::FrameInput::kNone;
  FrameBehindEncoder::FirstF().store(0);
  const PrefixGuard guard({e}); // listed before the test hooks' own fake: a test: source picks it first
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(1500));
  EXPECT_EQ(im_stop(s), IM_OK);

  std::vector<uint32_t> video(2, 0);
  std::vector<int64_t> last(2, 0);
  std::optional<Pkt> first_f;
  for (const Pkt &p : pkts) {
    if (!p.video() || p.layer > 1) {
      continue;
    }
    ++video[p.layer];
    if (p.layer == 0 && !first_f) {
      first_f = p;
    }
    const std::optional<im::audio::Marker> m = VideoMarker(p.data);
    ASSERT_TRUE(m.has_value()) << "layer " << p.layer << ": no Marker";
    EXPECT_EQ(p.ts - epoch_, m->capture_ns)
        << "layer " << p.layer << ", AU of frame " << m->frame << ": the packet's capture time is its own frame's";
    EXPECT_GT(p.ts, last[p.layer]) << "capture times increase per layer";
    last[p.layer] = p.ts;
  }
  EXPECT_GE(video[0], 60u) << "f runs at about 60 fps";
  EXPECT_GE(video[1], 15u) << "q runs at about 15 fps";
  ASSERT_TRUE(first_f.has_value());
  EXPECT_TRUE(first_f->key() && first_f->reinit()) << "flags " << first_f->flags;
  EXPECT_NE(FrameBehindEncoder::FirstF().load(), 0);
  EXPECT_EQ(first_f->ts, FrameBehindEncoder::FirstF().load()) << "the first IDR carries the first frame's time";
}

// im_layer_config.gop_ms reaches the layer (04 §9.4): f with 1000 ms gets a periodic IDR about every second on the
// moving pattern, q with 10000 ms none after its first within 3.5 s (its default would be 2 s).
TEST_F(Session, LayerGopFromTheStartConfig) {
  im_start_config cfg = Config("test:pattern", false);
  cfg.layers[0].gop_ms = 1000;
  cfg.layers[1].gop_ms = 10000;
  const im_session s = Start(cfg);
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(3500));
  EXPECT_EQ(im_stop(s), IM_OK);
  std::vector<int64_t> f_keys;
  uint32_t q_keys = 0;
  uint32_t q_packets = 0;
  for (const Pkt &p : pkts) {
    if (!p.video()) {
      continue;
    }
    if (p.layer == 0 && p.key()) {
      f_keys.push_back(p.ts);
    }
    if (p.layer == 1) {
      ++q_packets;
      q_keys += p.key() ? 1 : 0;
    }
  }
  ASSERT_GE(f_keys.size(), 3u) << "the first IDR and periodic ones about 1 s apart";
  for (std::size_t i = 1; i < f_keys.size(); ++i) {
    const int64_t gap_ms = (f_keys[i] - f_keys[i - 1]) / 1'000'000;
    // The pacer forces the IDR on the first new frame once 1 s passed since the last one (04 §9.4): about a frame
    // later in capture time, with room for a loaded runner.
    EXPECT_GE(gap_ms, 900) << "keyframe " << i;
    EXPECT_LE(gap_ms, 1250) << "keyframe " << i;
  }
  EXPECT_GE(q_packets, 30u);
  EXPECT_EQ(q_keys, 1u) << "q's GOP is 10 s: only its first packet is a keyframe";
}

// ------------------------------------------- selection, fallback, device loss, profiles (04 §9.1, §9.5–§9.8; WA61)

// The scripted encoders' world: which catalog entry was tried for which layer, in order (an entry is told apart by
// the id of its open function), how many encoders of a kind are open, and what the script makes fail. A scripted
// encoder is the test hooks' fake behind it (FrameInput::kNone: no GPU path), reported as its entry's kind.
struct Script {
  struct Attempt {
    uint32_t layer = 0;
    int id = 0;
    Clock::time_point at;
    bool opened = false;
  };
  std::mutex mu;
  std::vector<Attempt> attempts;
  std::map<uint32_t, int> live;  // open encoders by kind
  std::map<uint32_t, int> limit; // a session limit by kind (NVENC's per system, 04 §9.7); absent: none
  std::set<int> failing;         // entry ids whose opens fail (open_failed)
  int layer1_failures = 0;       // the next opens on layer 1 that fail, whatever the entry
  std::atomic<bool> lost{false}; // Encode reports a removed device (IM_ERR_ENCODER_LOST)

  // Changes the script under its lock.
  template <class F> void Edit(F &&change) {
    std::lock_guard<std::mutex> lock(mu);
    change(*this);
  }
  void Clear() {
    std::lock_guard<std::mutex> lock(mu);
    attempts.clear();
    live.clear();
    limit.clear();
    failing.clear();
    layer1_failures = 0;
    lost.store(false);
  }
  // The ids tried for layer, in order.
  std::vector<int> Tried(uint32_t layer) {
    std::lock_guard<std::mutex> lock(mu);
    std::vector<int> ids;
    for (const Attempt &a : attempts) {
      if (a.layer == layer) {
        ids.push_back(a.id);
      }
    }
    return ids;
  }
  std::vector<Attempt> AttemptsOf(uint32_t layer) {
    std::lock_guard<std::mutex> lock(mu);
    std::vector<Attempt> out;
    std::copy_if(attempts.begin(), attempts.end(), std::back_inserter(out),
                 [&](const Attempt &a) { return a.layer == layer; });
    return out;
  }
  int Live(uint32_t kind) {
    std::lock_guard<std::mutex> lock(mu);
    const auto it = live.find(kind);
    return it == live.end() ? 0 : it->second;
  }
  int LiveTotal() {
    std::lock_guard<std::mutex> lock(mu);
    int n = 0;
    for (const auto &by_kind : live) {
      n += by_kind.second;
    }
    return n;
  }
};

Script &TheScript() {
  static Script *const s = new Script(); // outlives every session thread
  return *s;
}

class ScriptedEncoder final : public im::session::VideoEncoder {
public:
  ScriptedEncoder(std::unique_ptr<im::session::VideoEncoder> inner, uint32_t kind) noexcept
      : inner_(std::move(inner)), kind_(kind) {}
  ~ScriptedEncoder() override {
    Script &s = TheScript();
    std::lock_guard<std::mutex> lock(s.mu);
    --s.live[kind_];
  }
  ScriptedEncoder(const ScriptedEncoder &) = delete;
  ScriptedEncoder &operator=(const ScriptedEncoder &) = delete;

  im_status Encode(const im::session::EncodeInput &in, im::session::EncodedAu *out) noexcept override {
    if (TheScript().lost.load()) {
      *out = im::session::EncodedAu{};
      return im::abi::Fail(IM_ERR_ENCODER_LOST, im::abi::token::kDeviceRemoved, "scripted: the device was removed");
    }
    return inner_->Encode(in, out);
  }
  im_status SetBitrate(uint32_t bps) noexcept override { return inner_->SetBitrate(bps); }

private:
  std::unique_ptr<im::session::VideoEncoder> inner_;
  uint32_t kind_ = 0;
};

im_status OpenScriptedAs(int id, const im::session::EncoderParams &p,
                         std::unique_ptr<im::session::VideoEncoder> *out) noexcept {
  try {
    Script &s = TheScript();
    {
      std::lock_guard<std::mutex> lock(s.mu);
      im_status st = IM_OK;
      const auto lim = s.limit.find(p.kind);
      if (s.failing.count(id) != 0) {
        st = im::abi::Fail(IM_ERR_NO_ENCODER, im::abi::token::kOpenFailed, "scripted: this entry fails to open");
      } else if (p.layer == 1 && s.layer1_failures > 0) {
        --s.layer1_failures;
        st = im::abi::Fail(IM_ERR_NO_ENCODER, im::abi::token::kOpenFailed, "scripted: layer 1's open fails");
      } else if (lim != s.limit.end() && s.live[p.kind] >= lim->second) {
        st = im::abi::Fail(IM_ERR_LIMIT, im::abi::token::kSessionLimit, "scripted: the kind's session limit");
      }
      s.attempts.push_back({p.layer, id, Clock::now(), st == IM_OK});
      if (st != IM_OK) {
        return st;
      }
      ++s.live[p.kind]; // counted before the lock goes, so a concurrent open sees it
    }
    std::unique_ptr<im::session::VideoEncoder> inner;
    const im_status st = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE).open(p, &inner);
    if (st != IM_OK) {
      std::lock_guard<std::mutex> lock(s.mu);
      --s.live[p.kind];
      return st;
    }
    *out = std::make_unique<ScriptedEncoder>(std::move(inner), p.kind);
    return IM_OK;
  } catch (...) {
    return IM_ERR_INTERNAL;
  }
}

template <int Id>
im_status OpenScripted(const im::session::EncoderParams &p, std::unique_ptr<im::session::VideoEncoder> *out) noexcept {
  return OpenScriptedAs(Id, p, out);
}

constexpr std::array<im::session::OpenEncoderFn, 7> kScriptedOpen = {
    &OpenScripted<0>, &OpenScripted<1>, &OpenScripted<2>, &OpenScripted<3>,
    &OpenScripted<4>, &OpenScripted<5>, &OpenScripted<6>};

// Entry id (kScriptedOpen's index) of kind on adapter (encpolicy::kNoAdapter for a software one), with the kind's
// traits (hardware, live bitrate, profiles: OpenH264 is CB only).
im::session::EncoderEntry Scripted(int id, uint32_t kind, uint32_t adapter) {
  im::session::EncoderEntry e = im::testhooks::FakeEncoderEntry(kind);
  e.info.adapter_index = adapter;
  e.open = kScriptedOpen[static_cast<std::size_t>(id)];
  e.input = im::session::FrameInput::kNone;
  return e;
}

constexpr uint32_t kNoAdapter = 0xFFFFFFFFu;

// The catalog of a test: its scripted entries and the test hooks' fakes only (no probe), the capture adapter, and a
// clean script; all undone at the end, also when an assertion ends the test early.
struct ScriptGuard {
  ScriptGuard(std::vector<im::session::EncoderEntry> entries, uint32_t capture_adapter) {
    TheScript().Clear();
    im::session::SetCatalogPrefixForTest(std::move(entries), true);
    im::session::SetCaptureAdapterForTest(capture_adapter);
  }
  ~ScriptGuard() {
    im::session::SetCatalogPrefixForTest({});
    im::session::SetCaptureAdapterForTest(std::nullopt);
    TheScript().Clear();
  }
  ScriptGuard(const ScriptGuard &) = delete;
  ScriptGuard &operator=(const ScriptGuard &) = delete;
};

im_session_info InfoOf(im_session s) {
  im_session_info info{};
  info.struct_size = sizeof info;
  EXPECT_EQ(im_get_session_info(s, &info), IM_OK) << LastError();
  return info;
}

// The ENCODER_CHANGED events and each layer's first REINIT|KEYFRAME, in delivery order, pulled for d or until every
// layer in want has its REINIT packet.
struct Fallbacks {
  std::vector<Pkt> events;                    // ENCODER_CHANGED
  std::array<std::optional<Pkt>, 2> reinit;   // per layer
  std::array<std::size_t, 2> events_before{}; // ENCODER_CHANGED events delivered before the layer's REINIT packet
};
Fallbacks PullFallbacks(im_session s, milliseconds d, std::vector<uint32_t> want) {
  Fallbacks f;
  Pull(s, d, [&](const Pkt &p) {
    if (p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_ENCODER_CHANGED) {
      f.events.push_back(p);
    }
    if (p.video() && p.reinit() && p.layer < 2 && !f.reinit[p.layer]) {
      f.reinit[p.layer] = p;
      std::size_t n = 0;
      for (const Pkt &e : f.events) {
        n += e.layer == p.layer ? 1 : 0;
      }
      f.events_before[p.layer] = n;
    }
    return std::all_of(want.begin(), want.end(), [&](uint32_t l) { return f.reinit[l].has_value(); });
  });
  return f;
}

// 04 §9.1's order and §9.6's one profile per session. Scrambled entries (ids 0..6 in catalog order: OpenH264, MF
// software, MF hardware on adapter 1, NVENC on 1, AMF on 0, MF hardware on 0, QSV on 1) where only OpenH264 opens:
// with adapter 0 as the capture adapter each layer tries AMF@0, MF-HW@0, then the vendors on the other adapters
// (NVENC before QSV), MF-HW@1, MF software; none opens at High, so the whole session tries again at CB and gets
// OpenH264 on both layers (never a mixed session). Without a capture adapter every adapter counts as "other": the
// vendor encoders by adapter first, then MF hardware. High asked of the CB session leaves it at CB (IM_OK, no
// re-init).
TEST_F(Session, CandidatesFollowTheOrderAndTheSessionKeepsOneProfile) {
  const std::vector<im::session::EncoderEntry> entries = {Scripted(0, IM_ENC_OPENH264, kNoAdapter),
                                                          Scripted(1, IM_ENC_MF_SW, kNoAdapter),
                                                          Scripted(2, IM_ENC_MF_HW, 1),
                                                          Scripted(3, IM_ENC_NVENC, 1),
                                                          Scripted(4, IM_ENC_AMF, 0),
                                                          Scripted(5, IM_ENC_MF_HW, 0),
                                                          Scripted(6, IM_ENC_QSV, 1)};
  {
    const ScriptGuard guard(entries, 0);
    TheScript().Edit([](Script &sc) { sc.failing = {1, 2, 3, 4, 5, 6}; });
    im::testhooks::FailOpens(IM_ENC_FAKE, true); // the test: source's fake comes first; it steps aside
    im_session_info info{};
    const im_session s = Start(Config("test:pattern", false), &info);
    ASSERT_NE(s, 0u);
    const std::vector<int> high_then_cb = {4, 5, 3, 6, 2, 1, 4, 5, 3, 6, 2, 1, 0};
    EXPECT_EQ(TheScript().Tried(0), high_then_cb) << "f: the High round (OpenH264 can't do High), then the CB round";
    EXPECT_EQ(TheScript().Tried(1), (std::vector<int>{4, 5, 3, 6, 2, 1, 0})) << "q: the CB round";
    for (uint32_t l = 0; l < 2; ++l) {
      EXPECT_EQ(info.layers[l].encoder, IM_ENC_OPENH264) << "layer " << l;
      EXPECT_EQ(info.layers[l].profile, static_cast<uint32_t>(IM_PROFILE_CB)) << "layer " << l;
    }
    ExpectFirstKeyframes(s, started_, epoch_, 2, "a CB session");

    ASSERT_EQ(im_set_profile(s, IM_PROFILE_HIGH), IM_OK) << "High asked of encoders that can't do it is IM_OK";
    for (const Pkt &p : Pull(s, milliseconds(1500))) {
      if (p.video()) {
        EXPECT_FALSE(p.reinit()) << "layer " << p.layer << ": no re-init, the session stays CB";
        EXPECT_EQ(p.profile, static_cast<uint32_t>(IM_PROFILE_CB)) << "layer " << p.layer;
      }
    }
    const im_session_info after = InfoOf(s);
    EXPECT_EQ(after.layers[0].profile, static_cast<uint32_t>(IM_PROFILE_CB));
    EXPECT_EQ(after.layers[1].profile, static_cast<uint32_t>(IM_PROFILE_CB));
    EXPECT_EQ(im_stop(s), IM_OK);
  }
  {
    const ScriptGuard guard(entries, kNoAdapter);
    TheScript().Edit([](Script &sc) { sc.failing = {1, 2, 3, 4, 5, 6}; });
    im::testhooks::FailOpens(IM_ENC_FAKE, true);
    const im_session s = Start(Config("test:pattern", false));
    ASSERT_NE(s, 0u);
    const std::vector<int> tried = TheScript().Tried(0);
    ASSERT_GE(tried.size(), 6u);
    EXPECT_EQ(std::vector<int>(tried.begin(), tried.begin() + 6), (std::vector<int>{4, 3, 6, 5, 2, 1}))
        << "no capture adapter: AMF@0, NVENC@1, QSV@1, then MF-HW@0, MF-HW@1, then MF software";
    EXPECT_EQ(im_stop(s), IM_OK);
  }
}

// 04 §9.7: layer 0's NVENC opens, layer 1's hits the session limit (one NVENC session left), so q takes the next
// encoder that does the session profile (MF software) at im_start: no event, both layers High, both first keyframes in
// time.
TEST_F(Session, APreviewLayerTakesTheNextEncoderWhenNvencHitsItsLimit) {
  const ScriptGuard guard({Scripted(0, IM_ENC_NVENC, 0), Scripted(1, IM_ENC_MF_SW, kNoAdapter)}, 0);
  TheScript().Edit([](Script &sc) { sc.limit[IM_ENC_NVENC] = 1; });
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  im_session_info info{};
  const im_session s = Start(Config("test:pattern", false), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.layers[0].encoder, IM_ENC_NVENC);
  EXPECT_EQ(info.layers[1].encoder, IM_ENC_MF_SW) << "q falls back past NVENC's limit";
  EXPECT_EQ(info.layers[0].profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
  EXPECT_EQ(info.layers[1].profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
  const std::vector<Script::Attempt> q = TheScript().AttemptsOf(1);
  ASSERT_EQ(q.size(), 2u);
  EXPECT_EQ(q[0].id, 0);
  EXPECT_FALSE(q[0].opened) << "NVENC: the session limit";
  EXPECT_EQ(q[1].id, 1);
  EXPECT_TRUE(q[1].opened);
  bool changed = false;
  std::vector<std::optional<Pkt>> first(2);
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    changed = changed || (p.kind == IM_PACKET_EVENT && p.event == IM_EVENT_ENCODER_CHANGED);
    if (p.video() && p.layer < 2 && !first[p.layer]) {
      first[p.layer] = p;
    }
    return first[0] && first[1];
  });
  EXPECT_FALSE(changed) << "a fallback at im_start is no ENCODER_CHANGED";
  for (uint32_t l = 0; l < 2; ++l) {
    ASSERT_TRUE(first[l].has_value()) << "layer " << l;
    EXPECT_TRUE(first[l]->key() && first[l]->reinit()) << "layer " << l;
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(first[l]->at - started_).count(), 500) << "layer " << l;
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

// 04 §9.7: a further layer whose every option failed is retried once after 500 ms; when the retry fails too, im_start
// is IM_ERR_NO_ENCODER (layer_failed, the detail names the layer) and nothing stays open.
TEST_F(Session, AFurtherLayerIsRetriedOnceAfter500ms) {
  const ScriptGuard guard({Scripted(0, IM_ENC_NVENC, 0)}, 0);
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  TheScript().Edit([](Script &sc) { sc.layer1_failures = 1; });
  im_session_info info{};
  const im_session s = Start(Config("test:pattern", false), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.layers[1].encoder, IM_ENC_NVENC);
  std::vector<Script::Attempt> q = TheScript().AttemptsOf(1);
  ASSERT_EQ(q.size(), 2u);
  EXPECT_FALSE(q[0].opened);
  EXPECT_TRUE(q[1].opened);
  EXPECT_GE(std::chrono::duration_cast<milliseconds>(q[1].at - q[0].at).count(), 480) << "the retry waits 500 ms";
  EXPECT_EQ(im_stop(s), IM_OK);

  TheScript().Clear();
  TheScript().Edit([](Script &sc) { sc.layer1_failures = 2; });
  im_start_config cfg = Config("test:pattern", false);
  im_session none = 0;
  im_session_info out{};
  out.struct_size = sizeof out;
  EXPECT_EQ(im_start(&cfg, &none, &out), IM_ERR_NO_ENCODER);
  const std::string err = LastError();
  EXPECT_TRUE(StartsWith(err, "layer_failed: ")) << err;
  EXPECT_NE(err.find("layer q"), std::string::npos) << err;
  q = TheScript().AttemptsOf(1);
  EXPECT_EQ(q.size(), 2u) << "one retry, no more";
  EXPECT_EQ(TheScript().LiveTotal(), 0) << "f's encoder was closed again";
}

// 04 §9.5: a re-init encoder (MF software) changes its bitrate by a re-init at most every 10 s per layer: the first
// change at once, the next ones stored and applied when the 10 s are over, the latest value winning; a bitrate that
// comes with an im_update_layer re-init rides along with it (1 s spacing, not 10 s).
TEST_F(Session, BitrateReinitsAreTenSecondsApartAndTheLatestValueWins) {
  const ScriptGuard guard({Scripted(0, IM_ENC_MF_SW, kNoAdapter)}, 0);
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  im_session_info info{};
  const im_session s = Start(Config("test:pattern", false), &info);
  ASSERT_NE(s, 0u);
  ASSERT_EQ(info.layers[0].encoder, IM_ENC_MF_SW);
  EXPECT_EQ(info.layers[0].flags & IM_ENCF_LIVE_BITRATE, 0u);
  Pull(s, milliseconds(400));

  const auto t0 = Clock::now();
  ASSERT_EQ(im_set_bitrate(s, 0, 2'000'000), IM_OK) << LastError();
  std::vector<Clock::time_point> f_reinits;
  uint32_t q_reinits = 0;
  bool set_second = false;
  bool set_third = false;
  for (;;) {
    const auto elapsed = Clock::now() - t0;
    if (elapsed > milliseconds(11'500)) {
      break;
    }
    if (!set_second && elapsed > milliseconds(1000)) {
      ASSERT_EQ(im_set_bitrate(s, 0, 3'000'000), IM_OK);
      set_second = true;
    }
    if (!set_third && elapsed > milliseconds(2000)) {
      ASSERT_EQ(im_set_bitrate(s, 0, 2'500'000), IM_OK);
      set_third = true;
      // The stored value replaced: still 2 Mbit/s until the window ends.
      EXPECT_EQ(InfoOf(s).layers[0].bitrate_bps, 2'000'000u);
    }
    Pkt p;
    if (NextPkt(s, 50, &p) == IM_OK && p.video() && p.reinit()) {
      if (p.layer == 0) {
        EXPECT_TRUE(p.key());
        f_reinits.push_back(p.at);
      } else {
        ++q_reinits;
      }
    }
  }
  ASSERT_EQ(f_reinits.size(), 2u) << "one re-init at once, one when the 10 s are over";
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(f_reinits[0] - t0).count(), 500);
  const auto gap = std::chrono::duration_cast<milliseconds>(f_reinits[1] - f_reinits[0]).count();
  EXPECT_GE(gap, 9800) << "10 s apart (packet arrival times)";
  EXPECT_LE(gap, 11'000);
  EXPECT_EQ(q_reinits, 0u) << "q's bitrate didn't change";
  EXPECT_EQ(InfoOf(s).layers[0].bitrate_bps, 2'500'000u) << "the latest value won";

  // A new fps with a bitrate in the same call: one re-init within the 1 s spacing carries both.
  const auto t1 = Clock::now();
  im_layer_config f = Layer("f", 1920 * 1080, 30, 3'000'000);
  ASSERT_EQ(im_update_layer(s, 0, &f), IM_OK) << LastError();
  std::vector<Clock::time_point> more;
  for (const Pkt &p : Pull(s, milliseconds(2000))) {
    if (p.video() && p.layer == 0 && p.reinit()) {
      more.push_back(p.at);
    }
  }
  ASSERT_EQ(more.size(), 1u);
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(more[0] - t1).count(), 1500);
  const im_session_info after = InfoOf(s);
  EXPECT_EQ(after.layers[0].fps, 30u);
  EXPECT_EQ(after.layers[0].bitrate_bps, 3'000'000u) << "the bitrate rode along with the fps re-init";
  // The current value again is a no-op.
  ASSERT_EQ(im_set_bitrate(s, 0, 3'000'000), IM_OK);
  for (const Pkt &p : Pull(s, milliseconds(1200))) {
    EXPECT_FALSE(p.video() && p.reinit()) << "layer " << p.layer;
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

// 04 §9.5: im_update_layer re-inits at most once per second per layer, coalesced: a first change at once, then the
// changes inside the next second become one re-init with the latest values (fps and size together).
TEST_F(Session, UpdateLayerReinitsAreOneSecondApartAndCoalesced) {
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  const auto t0 = Clock::now();
  im_layer_config f = Layer("f", 1920 * 1080, 30, 4'000'000);
  ASSERT_EQ(im_update_layer(s, 0, &f), IM_OK) << LastError();
  std::vector<Pkt> reinits;
  bool second = false;
  bool third = false;
  while (Clock::now() - t0 < milliseconds(2500)) {
    const auto elapsed = Clock::now() - t0;
    if (!second && elapsed > milliseconds(200)) {
      f = Layer("f", 1920 * 1080, 20, 4'000'000);
      ASSERT_EQ(im_update_layer(s, 0, &f), IM_OK);
      second = true;
    }
    if (!third && elapsed > milliseconds(300)) {
      f = Layer("f", 1280 * 720, 20, 4'000'000);
      ASSERT_EQ(im_update_layer(s, 0, &f), IM_OK);
      third = true;
    }
    Pkt p;
    if (NextPkt(s, 20, &p) == IM_OK && p.video() && p.reinit()) {
      EXPECT_EQ(p.layer, 0u) << "only f changed";
      if (p.layer == 0) {
        reinits.push_back(p);
      }
    }
  }
  ASSERT_EQ(reinits.size(), 2u) << "the first change at once, the next two as one";
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(reinits[0].at - t0).count(), 500);
  EXPECT_EQ(reinits[0].width, 1920u);
  EXPECT_GE(std::chrono::duration_cast<milliseconds>(reinits[1].at - reinits[0].at).count(), 800)
      << "1 s apart (packet arrival times)";
  EXPECT_EQ(reinits[1].width, 1280u) << "the coalesced re-init has the latest size";
  EXPECT_EQ(reinits[1].height, 720u);
  const im_session_info info = InfoOf(s);
  EXPECT_EQ(info.layers[0].fps, 20u) << "and the latest fps";
  EXPECT_EQ(info.layers[0].width, 1280u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

// 04 §9.8 with a session limit mid-session: q's encode error, NVENC's re-open hits the limit (another process took the
// session), so q falls back to MF software: ENCODER_CHANGED (reason session_limit) before its first REINIT|KEYFRAME;
// f keeps its NVENC and doesn't re-init.
TEST_F(Session, ASessionLimitMidSessionFallsBackWithItsReason) {
  const ScriptGuard guard({Scripted(0, IM_ENC_NVENC, 0), Scripted(1, IM_ENC_MF_SW, kNoAdapter)}, 0);
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  im_session_info info{};
  const im_session s = Start(Config("test:pattern", false), &info);
  ASSERT_NE(s, 0u);
  ASSERT_EQ(info.layers[1].encoder, IM_ENC_NVENC);
  Pull(s, milliseconds(400));
  TheScript().Edit([](Script &sc) { sc.limit[IM_ENC_NVENC] = 1; }); // f's stays; q's re-open finds none left
  ASSERT_EQ(im::testhooks::Inject(s, 1, im::testhooks::Fault::kEncodeError), IM_OK);
  const Fallbacks fb = PullFallbacks(s, milliseconds(1500), {1});
  ASSERT_TRUE(fb.reinit[1].has_value());
  EXPECT_TRUE(fb.reinit[1]->key());
  ASSERT_EQ(fb.events.size(), 1u);
  EXPECT_EQ(fb.events_before[1], 1u) << "ENCODER_CHANGED comes before the new encoder's first packet";
  EXPECT_FALSE(fb.reinit[0].has_value()) << "f didn't re-init";
  const std::string j = fb.events[0].json();
  EXPECT_EQ(fb.events[0].layer, 1u);
  EXPECT_NE(j.find("\"layer\":\"q\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"from\":\"nvenc\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"to\":\"mf-sw\""), std::string::npos) << j;
  EXPECT_NE(j.find("\"reason\":\"session_limit\""), std::string::npos) << j;
  const im_session_info after = InfoOf(s);
  EXPECT_EQ(after.layers[0].encoder, IM_ENC_NVENC);
  EXPECT_EQ(after.layers[1].encoder, IM_ENC_MF_SW);
  EXPECT_EQ(after.layers[1].profile, static_cast<uint32_t>(IM_PROFILE_HIGH)) << "the session profile is kept";
  EXPECT_EQ(im_stop(s), IM_OK);
}

// 04 §9.5 and §9.8: a re-init (the content hint, every layer) whose re-open fails goes to the next candidate:
// ENCODER_CHANGED (open_failed) for each layer, each before that layer's first REINIT|KEYFRAME.
TEST_F(Session, AFailedReinitFallsBackToTheNextCandidate) {
  const ScriptGuard guard({Scripted(0, IM_ENC_NVENC, 0), Scripted(1, IM_ENC_MF_SW, kNoAdapter)}, 0);
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  TheScript().Edit([](Script &sc) { sc.failing = {0}; }); // NVENC doesn't re-open
  ASSERT_EQ(im_set_content_hint(s, IM_CONTENT_DETAIL), IM_OK) << LastError();
  const Fallbacks fb = PullFallbacks(s, milliseconds(2000), {0, 1});
  for (uint32_t l = 0; l < 2; ++l) {
    SCOPED_TRACE("layer " + std::to_string(l));
    ASSERT_TRUE(fb.reinit[l].has_value());
    EXPECT_TRUE(fb.reinit[l]->key());
    EXPECT_EQ(fb.events_before[l], 1u) << "ENCODER_CHANGED first";
  }
  ASSERT_EQ(fb.events.size(), 2u);
  for (const Pkt &e : fb.events) {
    EXPECT_NE(e.json().find("\"reason\":\"open_failed\""), std::string::npos) << e.json();
    EXPECT_NE(e.json().find("\"to\":\"mf-sw\""), std::string::npos) << e.json();
  }
  const im_session_info info = InfoOf(s);
  EXPECT_EQ(info.layers[0].encoder, IM_ENC_MF_SW);
  EXPECT_EQ(info.layers[1].encoder, IM_ENC_MF_SW);
  EXPECT_EQ(im_stop(s), IM_OK);
}

// 04 §9.8: an encoder that reports a removed device (IM_ERR_ENCODER_LOST from Encode) ends the session within 1 s with
// ENDED{IM_ERR_ENCODER_LOST} device_removed: no fallback is tried, no DEVICE_RESET, the probe's cache is stale, and the
// ended session holds no encoder while the host has yet to stop it. The next start runs on a new device.
TEST_F(Session, AnEncoderReportingADeviceRemovalEndsTheSession) {
  const ScriptGuard guard({Scripted(0, IM_ENC_NVENC, 0), Scripted(1, IM_ENC_MF_SW, kNoAdapter)}, 0);
  im::testhooks::FailOpens(IM_ENC_FAKE, true);
  const im_session s = Start(Config("test:pattern", false));
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(400));
  const uint64_t old_device = im::testhooks::SessionDevice(s);
  const std::size_t tried = TheScript().AttemptsOf(0).size() + TheScript().AttemptsOf(1).size();
  const auto lost_at = Clock::now();
  TheScript().lost.store(true);
  const auto ended = WaitEnded(s, milliseconds(1500));
  ASSERT_TRUE(ended.has_value());
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(ended->at - lost_at).count(), 1000);
  EXPECT_EQ(ended->status, IM_ERR_ENCODER_LOST);
  EXPECT_NE(ended->json().find("\"detail\":\"device_removed: "), std::string::npos) << ended->json();
  EXPECT_TRUE(im::testhooks::ProbeCacheStale());
  EXPECT_EQ(TheScript().AttemptsOf(0).size() + TheScript().AttemptsOf(1).size(), tried) << "no fallback was tried";
  const auto released_by = Clock::now() + milliseconds(500);
  while (TheScript().LiveTotal() != 0 && Clock::now() < released_by) {
    std::this_thread::sleep_for(milliseconds(10));
  }
  EXPECT_EQ(TheScript().LiveTotal(), 0) << "the ended session released its encoders before im_stop";
  EXPECT_EQ(im_stop(s), IM_OK);

  TheScript().lost.store(false);
  const im_session again = Start(Config("test:pattern", false));
  ASSERT_NE(again, 0u);
  EXPECT_NE(im::testhooks::SessionDevice(again), old_device);
  ExpectFirstKeyframes(again, started_, epoch_, 2, "after the device removal");
  EXPECT_EQ(im_stop(again), IM_OK);
}

} // namespace

#else // IM_TEST_HOOKS

TEST(Session, NeedsTestHooks) { GTEST_SKIP() << "session_fake_test needs an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS
