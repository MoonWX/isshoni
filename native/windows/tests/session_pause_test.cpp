// session_pause_test (docs/m2/04-windows-engine.md §8.5, §10.3, E14; README WA52): im_set_paused through the C ABI.
//
// Video (IM_TEST_HOOKS builds, WARP: CI's windows-2025 runner has no GPU): the luma tap. The test lists an encoder of
// its own first in the session's catalog (session::SetCatalogPrefixForTest), one that takes pixels on the session's
// WARP device, reads every picture it is given back and decodes the GPU test pattern's 16-bit frame counter from its
// luma plane (gpu/pattern_layout.h), then lets the test hooks' IM_ENC_FAKE write the access unit. Every packet is one
// such picture, so the counters are what the packets show. From one frame interval after im_set_paused(s, 1) returned
// on, every picture is a refresh or a keyframe of one frozen frame captured no later than that instant; after the
// resume the pictures show new frames again.
//
// Audio (the test tone, 04's own source: no audio device, so the engine runs with IM_INIT_NO_AUDIO; the PCM tap shows
// what the Opus stage was given, sample for sample, against TestTone's model of the signal):
// - pause: the tone ramps down linearly within the first 10 ms chunk filled after the call (the chunk's last sample
//   is digital zero), the tap is exact zeros until the resume, whose chunk ramps up within 10 ms; the Opus packets
//   keep coming at 50 a second on the unbroken grid (exactly 20 ms apart, one REINIT, no DISCONTINUITY) and decode to
//   silence while paused;
// - the user's mute (session::SetToneMuted, im_audio_set_muted's half for the tone, session.h) set before a pause
//   stays set through the resume: the tap stays digital zero until the mute is cleared by its own call. The same rule
//   for 05's mixer (Mixer::SetPaused only, never SetMuted) is audio_bridge_test's
//   UserMuteKeepsSilentFramesFlowingThroughAPause.
#include <gtest/gtest.h>

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstring>
#include <functional>
#include <iterator>
#include <optional>
#include <string>
#include <vector>

#include "im/audio/pcm_sink.h"
#include "im/audio/test_tone.h"
#include "isshoni_media.h"
#include "session/session.h"

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
#include <memory>
#include <mutex>

#include "gpu/device.h"
#include "gpu/nv12.h"
#include "gpu/pattern_layout.h"
#include "im/abi/status.h"
#include "session/encoder.h"
#include "session/pattern_grid.h"
#include "testhooks/fake_encoder.h"
#include "testhooks/testhooks.h"
#endif

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr int64_t kChunkNs = im::audio::kChunkNs;
constexpr int64_t kFrameNs = 2 * kChunkNs;            // one Opus packet
constexpr uint32_t kFrames = im::audio::kChunkFrames; // 480 frames: one PCM packet, the 10 ms fade

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

int64_t EngineNow() {
  im_clock_sample c{};
  c.struct_size = sizeof c;
  return im_clock(&c) == IM_OK ? c.mono_ns : 0;
}

struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0;
  int64_t ts = 0;
  std::vector<uint8_t> data;
};

// Pulls packets for d, or until stop returns true for one.
std::vector<Pkt> Pull(im_session s, milliseconds d, const std::function<bool(const Pkt &)> &stop = nullptr) {
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
    if (p.kind != IM_PACKET_VIDEO && p.data != nullptr) {
      k.data.assign(p.data, p.data + p.size);
    }
    out.push_back(std::move(k));
    if (stop && stop(out.back())) {
      break;
    }
  }
  return out;
}

void Append(std::vector<Pkt> &to, std::vector<Pkt> more) {
  for (Pkt &p : more) {
    to.push_back(std::move(p));
  }
}

std::vector<Pkt> OfKind(const std::vector<Pkt> &pkts, uint32_t kind) {
  std::vector<Pkt> out;
  std::copy_if(pkts.begin(), pkts.end(), std::back_inserter(out), [&](const Pkt &p) { return p.kind == kind; });
  return out;
}

class Engine : public ::testing::Test {
protected:
  void SetUp() override {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::testhooks::Reset();
#endif
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO; // CI runners have no audio device; the test tone needs none
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  }
  void TearDown() override {
    im_shutdown();
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
    im::session::SetCatalogPrefixForTest({});
    im::testhooks::Reset();
#endif
  }

  im_session Start(const im_start_config &cfg) {
    im_session s = 0;
    im_session_info info{};
    info.struct_size = sizeof info;
    const im_status st = im_start(&cfg, &s, &info);
    EXPECT_EQ(st, IM_OK) << LastError();
    epoch_ = info.epoch_ns;
    return s;
  }

  int64_t epoch_ = 0;
};

// ================================================================================================ audio: the tone

// An audio-only test-tone session with the PCM tap.
im_start_config ToneConfig() {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  c.audio.flags = IM_AUDIOF_TAP_PCM;
  return c;
}

// One PCM packet against the tone's model: how its left channel relates to the unfaded signal.
enum class Shape : uint8_t {
  kFull,     // the signal at full level, sample for sample
  kZero,     // digital zero
  kRampDown, // the 10 ms fade out from the chunk's first sample: gain 479/480 … 0, the last sample exactly zero
  kRampUp,   // the 10 ms fade in: gain 1/480 … 480/480
  kOther,
};

const char *ShapeName(Shape s) {
  switch (s) {
  case Shape::kFull:
    return "full";
  case Shape::kZero:
    return "zero";
  case Shape::kRampDown:
    return "ramp down";
  case Shape::kRampUp:
    return "ramp up";
  default:
    return "other";
  }
}

struct PcmChunk {
  int64_t ts = 0;
  Shape shape = Shape::kOther;
  bool stereo_equal = true;
};

Shape Classify(const float *x, int64_t ts, const im::audio::TestTone &model) {
  const uint32_t n0 = model.PeriodIndex(ts);
  bool full = true, zero = true, down = true, up = true;
  for (uint32_t j = 0; j < kFrames; ++j) {
    const float m = im::audio::TestTone::PeriodSample((n0 + j) % im::audio::kRate);
    const float v = x[2 * j];
    // The tone's own arithmetic (TestTone::Fill): the sample times the gain step over 480.
    const float d = m * static_cast<float>(kFrames - 1 - j) / static_cast<float>(kFrames);
    const float u = j + 1 == kFrames ? m : m * static_cast<float>(j + 1) / static_cast<float>(kFrames);
    full = full && std::fabs(v - m) <= 1e-6f;
    zero = zero && v == 0.0f;
    down = down && std::fabs(v - d) <= 1e-6f;
    up = up && std::fabs(v - u) <= 1e-6f;
  }
  down = down && x[2 * (kFrames - 1)] == 0.0f; // digital zero within the 10 ms
  if (full) {
    return Shape::kFull;
  }
  if (zero) {
    return Shape::kZero;
  }
  if (down) {
    return Shape::kRampDown;
  }
  return up ? Shape::kRampUp : Shape::kOther;
}

std::vector<PcmChunk> Chunks(const std::vector<Pkt> &pcm, int64_t epoch) {
  const im::audio::TestTone model(epoch);
  std::vector<PcmChunk> out;
  std::vector<float> x(im::audio::kChunkSamples);
  for (const Pkt &p : pcm) {
    PcmChunk c;
    c.ts = p.ts;
    if (p.data.size() != im::audio::kChunkBytes) {
      ADD_FAILURE() << "a PCM packet of " << p.data.size() << " bytes";
      out.push_back(c);
      continue;
    }
    std::memcpy(x.data(), p.data.data(), p.data.size());
    for (uint32_t j = 0; j < kFrames; ++j) {
      c.stereo_equal = c.stereo_equal && x[2 * j] == x[2 * j + 1];
    }
    c.shape = Classify(x.data(), p.ts, model);
    out.push_back(c);
  }
  return out;
}

// The PCM chunks are one unbroken 10 ms grid and the Opus packets one 20 ms grid: 50 packets a second, the first one
// REINIT, none after it, no DISCONTINUITY.
void ExpectUnbrokenGrids(const std::vector<Pkt> &opus, const std::vector<PcmChunk> &pcm) {
  ASSERT_FALSE(opus.empty());
  ASSERT_FALSE(pcm.empty());
  EXPECT_NE(opus.front().flags & IM_PKT_REINIT, 0u) << "the first Opus packet starts the grid";
  for (std::size_t i = 1; i < opus.size(); ++i) {
    EXPECT_EQ(opus[i].ts - opus[i - 1].ts, kFrameNs) << "Opus packet " << i;
    EXPECT_EQ(opus[i].flags & (IM_PKT_REINIT | IM_PKT_DISCONTINUITY), 0u) << "Opus packet " << i;
  }
  for (std::size_t i = 1; i < pcm.size(); ++i) {
    EXPECT_EQ(pcm[i].ts - pcm[i - 1].ts, kChunkNs) << "PCM packet " << i;
  }
  for (const PcmChunk &c : pcm) {
    EXPECT_TRUE(c.stereo_equal) << "PCM at " << c.ts;
  }
}

// The index of the first chunk at or after `from` whose shape is not `s`, or pcm.size().
std::size_t FirstNot(const std::vector<PcmChunk> &pcm, std::size_t from, Shape s) {
  while (from < pcm.size() && pcm[from].shape == s) {
    ++from;
  }
  return from;
}

// The peak of each Opus packet decoded with the DLL's libopus, in order (one decoder, as a receiver's).
std::vector<float> DecodedPeaks(const std::vector<Pkt> &opus) {
  std::vector<float> peaks;
  std::vector<float> pcm(2 * 5760);
  for (const Pkt &p : opus) {
    const int32_t n = im_test_opus_decode(p.data.data(), static_cast<int32_t>(p.data.size()), pcm.data(), 5760);
    EXPECT_EQ(n, 960) << LastError();
    float peak = 0;
    for (int32_t i = 0; i < 2 * std::max<int32_t>(n, 0); ++i) {
      peak = std::max(peak, std::fabs(pcm[static_cast<std::size_t>(i)]));
    }
    peaks.push_back(peak);
  }
  return peaks;
}

TEST_F(Engine, PausedToneRampsDownIn10msAndSilentFramesKeepFlowing) {
  const im_session s = Start(ToneConfig());
  ASSERT_NE(s, 0u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(400));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  const int64_t paused_at = EngineNow(); // after the call returned
  Append(pkts, Pull(s, milliseconds(600)));
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  const int64_t resumed_at = EngineNow();
  Append(pkts, Pull(s, milliseconds(400)));
  ASSERT_EQ(im_stop(s), IM_OK);

  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<PcmChunk> pcm = Chunks(OfKind(pkts, IM_PACKET_PCM), epoch_);
  ExpectUnbrokenGrids(opus, pcm);

  // Full level, one 10 ms ramp down, digital zero, one 10 ms ramp up, full level again.
  const std::size_t down = FirstNot(pcm, 0, Shape::kFull);
  ASSERT_LT(down, pcm.size()) << "no fade after the pause";
  ASSERT_EQ(pcm[down].shape, Shape::kRampDown) << ShapeName(pcm[down].shape) << " at " << pcm[down].ts - epoch_;
  EXPECT_LE(pcm[down].ts, paused_at) << "the chunk being mixed when the pause took effect fades out";
  const std::size_t up = FirstNot(pcm, down + 1, Shape::kZero);
  ASSERT_LT(up, pcm.size()) << "no fade in after the resume";
  ASSERT_EQ(pcm[up].shape, Shape::kRampUp) << ShapeName(pcm[up].shape) << " at " << pcm[up].ts - epoch_;
  EXPECT_GT(pcm[up].ts, paused_at + 400'000'000) << "sound before the resume";
  EXPECT_LE(pcm[up].ts, resumed_at);
  EXPECT_GE(up - down - 1, 40u) << "the tap shows digital zero while paused";
  EXPECT_EQ(FirstNot(pcm, up + 1, Shape::kFull), pcm.size()) << "full level after the resume";

  // The Opus stream keeps encoded silence meanwhile: the packets that encode only zero chunks (after two packets for
  // the decoder's overlap to settle) decode to silence; the ones before the pause carry the tone.
  const std::vector<float> peaks = DecodedPeaks(opus);
  const int64_t zero_from = pcm[down].ts + kChunkNs;
  const int64_t zero_to = pcm[up].ts;
  uint32_t quiet = 0, loud = 0;
  for (std::size_t i = 0; i < opus.size() && i < peaks.size(); ++i) {
    // An Opus packet stamped t encodes the chunks from t + the lookahead on: [t, t + 20 ms) of the input, give or take
    // the lookahead (6.5 ms), which the margins below cover.
    if (opus[i].ts >= zero_from + 2 * kFrameNs && opus[i].ts + 2 * kFrameNs <= zero_to) {
      EXPECT_LT(peaks[i], 0.01f) << "Opus packet at " << opus[i].ts - epoch_ << " ns is not silent";
      ++quiet;
    } else if (opus[i].ts + 2 * kFrameNs <= pcm[down].ts && i >= 2) {
      loud += peaks[i] > 0.05f ? 1u : 0u;
    }
  }
  EXPECT_GE(quiet, 20u);
  EXPECT_GE(loud, 10u) << "the tone before the pause";
}

TEST_F(Engine, ResumeKeepsTheUserMuteOfTheTone) {
  EXPECT_EQ(im::session::SetToneMuted(true), IM_ERR_BAD_STATE) << "no test-tone session runs";
  const im_session s = Start(ToneConfig());
  ASSERT_NE(s, 0u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(300));
  ASSERT_EQ(im::session::SetToneMuted(true), IM_OK) << LastError();
  const int64_t muted_at = EngineNow();
  Append(pkts, Pull(s, milliseconds(200)));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(200)));
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  const int64_t resumed_at = EngineNow();
  Append(pkts, Pull(s, milliseconds(300)));
  ASSERT_EQ(im::session::SetToneMuted(false), IM_OK) << LastError();
  const int64_t unmuted_at = EngineNow();
  Append(pkts, Pull(s, milliseconds(300)));
  ASSERT_EQ(im_stop(s), IM_OK);

  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<PcmChunk> pcm = Chunks(OfKind(pkts, IM_PACKET_PCM), epoch_);
  ExpectUnbrokenGrids(opus, pcm);

  // One fade out at the mute, digital zero through the pause and the resume, one fade in at the unmute.
  const std::size_t down = FirstNot(pcm, 0, Shape::kFull);
  ASSERT_LT(down, pcm.size()) << "no fade after the mute";
  ASSERT_EQ(pcm[down].shape, Shape::kRampDown) << ShapeName(pcm[down].shape);
  EXPECT_LE(pcm[down].ts, muted_at);
  const std::size_t up = FirstNot(pcm, down + 1, Shape::kZero);
  ASSERT_LT(up, pcm.size()) << "no fade in after the unmute";
  ASSERT_EQ(pcm[up].shape, Shape::kRampUp) << ShapeName(pcm[up].shape);
  // The resume left the mute alone: still digital zero well after it (the unmute came 300 ms later).
  EXPECT_GT(pcm[up].ts, resumed_at + 100'000'000) << "the resume cleared the user's mute";
  EXPECT_LE(pcm[up].ts, unmuted_at);
  EXPECT_EQ(FirstNot(pcm, up + 1, Shape::kFull), pcm.size()) << "full level after the unmute";
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

// ================================================================================================ video: the luma tap

constexpr uint32_t kSourceW = 640; // small, so WARP keeps up at 60 fps
constexpr uint32_t kSourceH = 360;
constexpr uint32_t kHz = 60;
constexpr int64_t kIntervalNs = 1'000'000'000 / kHz + 1; // one frame interval of the 60 Hz pattern and layer f
constexpr int64_t kWakeUpNs = 100'000'000; // more than one wake-up of the video thread takes, WARP conversions included

// One picture an encoder of the test was given, as its luma shows it.
struct Seen {
  uint32_t layer = 0;
  int64_t at = 0; // when Encode ran (engine clock)
  bool idr = false;
  bool refresh = false;
  bool black = false;   // EncodeInput::black
  uint32_t counter = 0; // the pattern's 16-bit frame counter in the picture
};

struct Recorder {
  std::mutex mu;
  std::vector<Seen> seen;
  std::vector<std::string> errors;
};

Recorder &TheRecorder() {
  static Recorder r;
  return r;
}

// The counter from an NV12 luma plane of w × h showing the whole kSourceW × kSourceH pattern (BT.709 limited range:
// white Y 235, black Y 16).
uint32_t DecodeCounter(const std::vector<uint8_t> &y, uint32_t w, uint32_t h) {
  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(kSourceW, kSourceH);
  const uint32_t row = (l.mover_end + l.counter_end) / 2 * h / kSourceH;
  uint32_t counter = 0;
  for (uint32_t i = 0; i < 16; ++i) {
    const uint32_t x = (2 * i + 1) * kSourceW / 32 * w / kSourceW; // the block's middle
    counter = (counter << 1) | (y[std::size_t{row} * w + x] > 128 ? 1u : 0u);
  }
  return counter;
}

// Takes pixels on the session's device, reads each picture back and records its counter, and lets IM_ENC_FAKE write
// the access unit.
class LumaTapEncoder final : public im::session::VideoEncoder {
public:
  static im_status Open(const im::session::EncoderParams &p, std::unique_ptr<im::session::VideoEncoder> *out) {
    if (p.device == nullptr) {
      return im::abi::Fail(IM_ERR_NO_ENCODER, im::abi::token::kOpenFailed, "luma tap: opened without a device");
    }
    auto e = std::unique_ptr<LumaTapEncoder>(new LumaTapEncoder(p));
    const im::video::Size size{p.width, p.height};
    if (const im_status st = im::gpu::Nv12Surface::Create(*p.device, size, &e->surface_); st != IM_OK) {
      return st;
    }
    if (const im_status st = im::gpu::Nv12Readback::Create(*p.device, size, e->surface_->planar(), &e->readback_);
        st != IM_OK) {
      return st;
    }
    if (const im_status st = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE).open(p, &e->inner_); st != IM_OK) {
      return st;
    }
    *out = std::move(e);
    return IM_OK;
  }

  im_status NextTarget(im::gpu::Nv12Target *out) noexcept override {
    *out = surface_->target();
    return IM_OK;
  }

  im_status Encode(const im::session::EncodeInput &in, im::session::EncodedAu *out) noexcept override {
    try {
      Seen s;
      s.layer = p_.layer;
      s.at = EngineNow();
      s.idr = in.idr;
      s.refresh = in.refresh;
      s.black = in.black;
      const im::gpu::Nv12Target t = surface_->target();
      Recorder &r = TheRecorder();
      if (in.picture == nullptr || in.picture->texture != t.texture || in.picture->luma != t.luma) {
        std::lock_guard<std::mutex> lock(r.mu);
        r.errors.push_back("layer " + std::to_string(p_.layer) + ": the picture is not the target handed out");
      } else {
        const im::gpu::Nv12Planes planes{y_.data(), p_.width, uv_.data(), p_.width};
        if (const im_status st = readback_->Read(*in.picture, planes); st != IM_OK) {
          std::lock_guard<std::mutex> lock(r.mu);
          r.errors.push_back("layer " + std::to_string(p_.layer) + ": readback " +
                             std::string(im::abi::LastErrorText()));
          return st;
        }
        s.counter = DecodeCounter(y_, p_.width, p_.height);
        std::lock_guard<std::mutex> lock(r.mu);
        r.seen.push_back(s);
      }
    } catch (...) {
      return im::abi::Fail(IM_ERR_INTERNAL, {}, "luma tap: out of memory");
    }
    return inner_->Encode(in, out);
  }

  im_status SetBitrate(uint32_t bps) noexcept override { return inner_->SetBitrate(bps); }

private:
  explicit LumaTapEncoder(const im::session::EncoderParams &p)
      : p_(p), y_(std::size_t{p.width} * p.height), uv_(std::size_t{p.width} * p.height / 2) {}

  im::session::EncoderParams p_;
  std::unique_ptr<im::gpu::Nv12Surface> surface_;
  std::unique_ptr<im::gpu::Nv12Readback> readback_;
  std::unique_ptr<im::session::VideoEncoder> inner_;
  std::vector<uint8_t> y_;
  std::vector<uint8_t> uv_;
};

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps, uint32_t bps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  l.bitrate_bps = bps;
  return l;
}

// test:pattern 640×360 at 60 Hz: f 1:1 at 60 fps, q 320×180 at 15 fps; no audio.
im_start_config VideoConfig() {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.flags = IM_START_ALLOW_LISTED_SOURCE;
  c.source_id = "test:pattern";
  c.profile = IM_PROFILE_HIGH;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_AUTO;
  c.layer_count = 2;
  c.layers[0] = Layer("f", kSourceW * kSourceH, kHz, 2'000'000);
  c.layers[1] = Layer("q", kSourceW * kSourceH / 4, 15, 300'000);
  c.audio.mode = IM_AUDIO_MODE_OFF;
  return c;
}

class LumaTap : public Engine {
protected:
  void SetUp() override {
    {
      std::lock_guard<std::mutex> lock(TheRecorder().mu);
      TheRecorder().seen.clear();
      TheRecorder().errors.clear();
    }
    im::session::EncoderEntry e = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE);
    e.open = &LumaTapEncoder::Open;
    e.input = im::session::FrameInput::kNv12;
    e.device.adapter = im::gpu::DeviceRequest::Adapter::kWarp;
    im::session::SetCatalogPrefixForTest({e});
    Engine::SetUp();
    im::testhooks::Options o;
    o.pattern = im::session::Pattern::kMoving;
    o.pattern_width = kSourceW;
    o.pattern_height = kSourceH;
    o.pattern_hz = kHz;
    im::testhooks::Configure(o);
  }

  // The newest pattern frame captured at or before t (frame k at epoch_ns + k / 60 s).
  uint64_t FrameAt(int64_t t) const {
    const im::session::PatternGrid g{epoch_, kHz, 0};
    if (t <= epoch_) {
      return 0;
    }
    uint64_t k = static_cast<uint64_t>((t - epoch_) * int64_t{kHz} / 1'000'000'000);
    while (g.FrameNs(k + 1) <= t) {
      ++k;
    }
    return k;
  }
};

TEST_F(LumaTap, NoPacketAfterThePausePlusOneIntervalShowsNewContent) {
  const im_session s = Start(VideoConfig());
  ASSERT_NE(s, 0u);
  ASSERT_TRUE(im::session::GpuPathOf(s)) << "the luma tap's encoder takes pixels: the session runs the GPU path";
  Pull(s, milliseconds(500));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  const int64_t paused_at = EngineNow(); // after the call returned
  Pull(s, milliseconds(400));
  ASSERT_EQ(im_request_keyframe(s, IM_ALL_LAYERS), IM_OK) << LastError();
  Pull(s, milliseconds(1300)); // the requested IDRs, and a refresh of each layer (every 1 s while static)
  const int64_t resumed_at = EngineNow();
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  Pull(s, milliseconds(500));
  EXPECT_EQ(im_stop(s), IM_OK);

  std::vector<Seen> seen;
  {
    std::lock_guard<std::mutex> lock(TheRecorder().mu);
    seen = TheRecorder().seen;
    for (const std::string &e : TheRecorder().errors) {
      ADD_FAILURE() << e;
    }
  }
  // From the pause plus one frame interval on, until the resume, every picture shows the same frozen frame, captured no
  // later than that instant (the counter is 16 bits; the run stays below 2^16 frames). An encode the video thread had
  // already planned before the pause took effect (layer q right after f, in the same wake-up) may still start then: it
  // shows the frozen frame too, but needn't be a refresh. Once that wake-up is over every encode is a refresh or a
  // keyframe (the pacer takes no frame while paused).
  const int64_t settled = paused_at + kIntervalNs;
  const uint64_t newest_allowed = FrameAt(settled);
  std::optional<uint32_t> frozen;
  uint32_t paused_pictures[2] = {0, 0};
  uint32_t idrs = 0;
  for (const Seen &v : seen) {
    if (v.at < settled || v.at >= resumed_at) {
      continue;
    }
    ++paused_pictures[v.layer < 2 ? v.layer : 0];
    idrs += v.idr ? 1u : 0u;
    if (v.at >= paused_at + kWakeUpNs) {
      EXPECT_TRUE(v.refresh || v.idr) << "layer " << v.layer << " at " << v.at - paused_at
                                      << " ns after the pause: a new picture while paused";
    }
    EXPECT_FALSE(v.black) << "the frozen frame, not black";
    EXPECT_LE(v.counter, newest_allowed) << "layer " << v.layer << " at " << v.at - paused_at
                                         << " ns after the pause shows a frame captured after it";
    if (!frozen) {
      frozen = v.counter;
    }
    EXPECT_EQ(v.counter, *frozen) << "layer " << v.layer << ": every paused picture shows the frozen frame";
  }
  ASSERT_TRUE(frozen.has_value()) << "no picture while paused";
  EXPECT_GE(paused_pictures[0], 2u) << "f: the refreshes and the requested keyframe";
  EXPECT_GE(paused_pictures[1], 2u) << "q: the refreshes and the requested keyframe";
  EXPECT_GE(idrs, 2u) << "a keyframe request while paused is an IDR of the frozen frame, on each layer";

  // Resume: new frames again, newer than the frozen one.
  uint32_t newer = 0;
  for (const Seen &v : seen) {
    if (v.at >= resumed_at + 100'000'000 && v.layer == 0 && !v.refresh) {
      newer += v.counter > *frozen ? 1u : 0u;
    }
  }
  EXPECT_GE(newer, 10u) << "resume encodes new frames again";
}

#else

TEST(SessionPause, VideoNeedsTestHooks) {
  GTEST_SKIP() << "the luma tap case needs an IM_TEST_HOOKS build (win-x64-ci)";
}

#endif // IM_TEST_HOOKS

} // namespace
