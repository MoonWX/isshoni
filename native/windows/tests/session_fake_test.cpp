// session_fake_test (docs/m2/04-windows-engine.md §18.2, README WA23): the session core through the C ABI with the
// test hooks (IM_TEST_HOOKS builds: win-x64-ci, win-x64-asan): test:pattern (and the fake picker's picks), IM_ENC_FAKE
// and the test tone; no GPU and no audio device (the engine runs with IM_INIT_NO_AUDIO, which the tone doesn't need).
//
// It asserts: each layer's first packet is KEYFRAME|REINIT within 500 ms after im_start returns, also on a static
// pattern and with no captured frame at all; IM_ENC_FAKE's output (real SPS/PPS for the profile and size, CB byte
// e0, AU sizes, the Marker's CaptureNS = capture_ts_ns − epoch_ns); every capture_ts_ns ≥ epoch_ns; f ≈ 60 fps, q
// 15 fps; a keyframe within one interval, also on a static screen; im_set_layer_active; pause (only REFRESH and IDR
// packets, audio keeps 50 packets/s); im_set_profile; im_set_content_hint coalesced with an im_update_layer; an
// injected encode error (the same encoder re-opened, or ENCODER_CHANGED before the next candidate's first
// REINIT|KEYFRAME); an injected device removal (ENDED{IM_ERR_ENCODER_LOST} device_removed within 1 s, then a new
// session on a new device); exhausted candidates; a consumer that stops for 2 s (RING_OVERFLOW, stale drops, an IDR
// when it resumes); im_stop with a blocked consumer; the session limits; the pick-token rules; the test tone's
// Marker and its beeps on the video flashes; stats and session info.
//
// The test links the engine's objects (im_engine), so it calls the test hooks directly.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <functional>
#include <optional>
#include <span>
#include <string>
#include <thread>
#include <vector>

#include "im/audio/opus_packet.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"
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
  EXPECT_GT(f_bps, 4'000'000 * 0.88) << f_bps;
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

} // namespace

#else // IM_TEST_HOOKS

TEST(Session, NeedsTestHooks) { GTEST_SKIP() << "session_fake_test needs an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS
