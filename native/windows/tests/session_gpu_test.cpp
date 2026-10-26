// session_gpu_test (docs/m2/04-windows-engine.md §8, §6.3, §8.5, §18.2; README WF2): a session's GPU path through the
// C ABI on WARP (CI's windows-2025 runner has no GPU), with the test hooks (IM_TEST_HOOKS builds: win-x64-ci,
// win-x64-asan).
//
// The test lists an encoder of its own first in the session's catalog (session::SetCatalogPrefixForTest): one that
// takes pixels (FrameInput::kNv12) on a WARP device, as README WA44's encoders will. For every encode it hands out
// its own NV12 surface, reads the converted picture back (gpu::Nv12Readback), decodes the GPU test pattern's frame
// counter and flash box from it (gpu/pattern_layout.h), and leaves the access unit to the test hooks' IM_ENC_FAKE.
// So "test:pattern" runs as in a release build: im_start creates the device, the frame slots, the pattern and a
// converter per layer; the supervisor draws each frame into a slot; the video thread takes it and converts it into
// the encoder's target.
//
// It asserts: such a session runs the GPU path (IM_ENC_FAKE's doesn't: session_fake_test); each layer's first packet
// is KEYFRAME|REINIT within 500 ms; every picture of a new frame shows that frame's counter (the frame k whose grid
// instant epoch_ns + k / 60 s is its capture_ts_ns) and flash box, on f (1:1) and q (downscaled 2:1); on a static
// screen refreshes and keyframes show the one frame again; with no frame at all the first keyframe is black (Y 16,
// Cb/Cr 128); while paused every picture shows one frame captured before the pause took effect, and resuming shows new
// frames again.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <thread>
#include <vector>

#include "gpu/device.h"
#include "gpu/nv12.h"
#include "gpu/pattern_layout.h"
#include "im/abi/status.h"
#include "isshoni_media.h"
#include "session/encoder.h"
#include "session/pattern_grid.h"
#include "session/session.h"
#include "session/sources.h"
#include "testhooks/fake_encoder.h"
#include "testhooks/testhooks.h"

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr uint32_t kSourceW = 640; // small, so WARP keeps up at 60 fps
constexpr uint32_t kSourceH = 360;
constexpr uint32_t kHz = 60;
constexpr int64_t kOnGridNs = 20'000; // a new frame's capture time: its grid instant (plus the session's RTP bump)

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

// ---------------------------------------------------------------------------------------------- what the encoder saw

// One picture an encoder of the test got, decoded.
struct Seen {
  uint32_t layer = 0;
  int64_t ts = 0;   // EncodeInput::ts_ns
  int64_t at = 0;   // when Encode ran (engine clock)
  bool idr = false; // EncodeInput's flags
  bool refresh = false;
  bool black_asked = false; // EncodeInput::black
  bool black = false;       // every luma byte 16 and every chroma byte 128 (±1)
  uint32_t counter = 0;     // the pattern's 16-bit frame counter as the picture shows it
  bool flash = false;       // the flash box is white
};

struct Recorder {
  std::mutex mu;
  std::vector<Seen> seen;
  std::vector<std::string> errors;

  void Clear() {
    std::lock_guard<std::mutex> lock(mu);
    seen.clear();
    errors.clear();
  }
  void Add(const Seen &s) {
    std::lock_guard<std::mutex> lock(mu);
    seen.push_back(s);
  }
  void Error(std::string e) {
    std::lock_guard<std::mutex> lock(mu);
    errors.push_back(std::move(e));
  }
  std::vector<Seen> Snapshot() {
    std::lock_guard<std::mutex> lock(mu);
    return seen;
  }
  std::vector<std::string> Errors() {
    std::lock_guard<std::mutex> lock(mu);
    return errors;
  }
};

Recorder &TheRecorder() {
  static Recorder r;
  return r;
}

// Where pattern_layout.h's bands and blocks are in a picture of out_w × out_h that shows the whole kSourceW × kSourceH
// pattern (same aspect: no letterbox): source coordinates scaled.
struct Probe {
  uint32_t out_w = 0;
  uint32_t out_h = 0;
  uint32_t X(uint32_t source_x) const { return source_x * out_w / kSourceW; }
  uint32_t Y(uint32_t source_y) const { return source_y * out_h / kSourceH; }
};

// Decodes the counter and the flash box from an NV12 picture (BT.709 limited: white Y 235, black Y 16).
void Decode(const std::vector<uint8_t> &y, const std::vector<uint8_t> &uv, uint32_t w, uint32_t h, Seen *s) {
  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(kSourceW, kSourceH);
  const Probe p{w, h};
  const uint32_t counter_row = p.Y((l.mover_end + l.counter_end) / 2);
  uint32_t counter = 0;
  for (uint32_t i = 0; i < 16; ++i) {
    const uint32_t x = p.X((2 * i + 1) * kSourceW / 32); // the block's middle
    counter = (counter << 1) | (y[std::size_t{counter_row} * w + x] > 128 ? 1u : 0u);
  }
  s->counter = counter;
  const uint32_t flash_x = p.X(kSourceW - kSourceW / 16);
  const uint32_t flash_y = p.Y((l.counter_end + kSourceH) / 2);
  s->flash = y[std::size_t{flash_y} * w + flash_x] > 128;
  s->black = std::all_of(y.begin(), y.end(), [](uint8_t v) { return v >= 15 && v <= 17; }) &&
             std::all_of(uv.begin(), uv.end(), [](uint8_t v) { return v >= 127 && v <= 129; });
}

// The test's encoder: takes pixels on the session's device, reads them back and decodes them, and lets IM_ENC_FAKE
// write the access unit (the session's packets stay IM_ENC_FAKE's, Marker included).
class PixelEncoder final : public im::session::VideoEncoder {
public:
  static im_status Open(const im::session::EncoderParams &p, std::unique_ptr<im::session::VideoEncoder> *out) {
    if (p.device == nullptr) {
      return im::abi::Fail(IM_ERR_NO_ENCODER, im::abi::token::kOpenFailed, "test encoder: opened without a device");
    }
    auto e = std::unique_ptr<PixelEncoder>(new PixelEncoder(p));
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
      s.ts = in.ts_ns;
      s.at = EngineNow();
      s.idr = in.idr;
      s.refresh = in.refresh;
      s.black_asked = in.black;
      const im::gpu::Nv12Target t = surface_->target();
      if (in.picture == nullptr || in.picture->texture != t.texture || in.picture->luma != t.luma) {
        TheRecorder().Error("layer " + std::to_string(p_.layer) + ": the picture is not the target handed out");
      } else {
        const im::gpu::Nv12Planes planes{y_.data(), p_.width, uv_.data(), p_.width};
        if (const im_status st = readback_->Read(*in.picture, planes); st != IM_OK) {
          TheRecorder().Error("layer " + std::to_string(p_.layer) + ": readback " +
                              std::string(im::abi::LastErrorText()));
          return st;
        }
        Decode(y_, uv_, p_.width, p_.height, &s);
        TheRecorder().Add(s);
      }
    } catch (...) {
      return im::abi::Fail(IM_ERR_INTERNAL, {}, "test encoder: out of memory");
    }
    return inner_->Encode(in, out);
  }

  im_status SetBitrate(uint32_t bps) noexcept override { return inner_->SetBitrate(bps); }

private:
  explicit PixelEncoder(const im::session::EncoderParams &p)
      : p_(p), y_(std::size_t{p.width} * p.height), uv_(std::size_t{p.width} * p.height / 2) {}

  im::session::EncoderParams p_;
  std::unique_ptr<im::gpu::Nv12Surface> surface_;
  std::unique_ptr<im::gpu::Nv12Readback> readback_;
  std::unique_ptr<im::session::VideoEncoder> inner_;
  std::vector<uint8_t> y_;
  std::vector<uint8_t> uv_;
};

// ---------------------------------------------------------------------------------------------- packets

struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0;
  int64_t ts = 0;
  Clock::time_point at;

  bool video() const { return kind == IM_PACKET_VIDEO; }
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
    k.at = Clock::now();
    out.push_back(k);
    if (stop && stop(out.back())) {
      break;
    }
  }
  return out;
}

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps, uint32_t bps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  l.bitrate_bps = bps;
  return l;
}

// test:pattern 640×360 at 60 Hz: f 1:1 at 60 fps, q 320×180 (2:1, the preview's box filter) at 15 fps; no audio.
im_start_config Config() {
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

class SessionGpu : public ::testing::Test {
protected:
  void SetUp() override {
    im::testhooks::Reset();
    TheRecorder().Clear();
    im::session::EncoderEntry e = im::testhooks::FakeEncoderEntry(IM_ENC_FAKE);
    e.open = &PixelEncoder::Open;
    e.input = im::session::FrameInput::kNv12;
    e.device.adapter = im::gpu::DeviceRequest::Adapter::kWarp;
    im::session::SetCatalogPrefixForTest({e});
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO;
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
  }
  void TearDown() override {
    im_shutdown();
    im::session::SetCatalogPrefixForTest({});
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

  im_session Start() {
    const im_start_config cfg = Config();
    im_session s = 0;
    im_session_info info{};
    info.struct_size = sizeof info;
    const im_status st = im_start(&cfg, &s, &info);
    started_ = Clock::now();
    EXPECT_EQ(st, IM_OK) << LastError();
    epoch_ = info.epoch_ns;
    info_ = info;
    return s;
  }

  // The pattern frame whose grid instant is ts, if ts is one (a new frame's capture time).
  std::optional<uint64_t> GridFrame(int64_t ts) const {
    const im::session::PatternGrid g{epoch_, kHz, 0};
    if (ts < epoch_) {
      return std::nullopt;
    }
    const auto k = static_cast<uint64_t>(((ts - epoch_) * int64_t{kHz} + 500'000'000) / 1'000'000'000);
    const int64_t d = ts - g.FrameNs(k);
    if (d < -kOnGridNs || d > kOnGridNs) {
      return std::nullopt;
    }
    return k;
  }

  // The newest pattern frame captured at or before t.
  uint64_t FrameAt(int64_t t) const {
    return t <= epoch_ ? 0 : static_cast<uint64_t>((t - epoch_) * int64_t{kHz} / 1'000'000'000);
  }

  Clock::time_point started_;
  int64_t epoch_ = 0;
  im_session_info info_{};
};

void ExpectNoErrors() {
  for (const std::string &e : TheRecorder().Errors()) {
    ADD_FAILURE() << e;
  }
}

// ---------------------------------------------------------------------------------------------- the cases

TEST_F(SessionGpu, PatternPixelsGoThroughSlotsAndConverters) {
  Configure(im::session::Pattern::kMoving);
  const im_session s = Start();
  ASSERT_NE(s, 0u);
  EXPECT_TRUE(im::session::GpuPathOf(s)) << "an encoder that takes pixels: the session runs the GPU path";
  EXPECT_EQ(info_.layers[0].width, kSourceW);
  EXPECT_EQ(info_.layers[0].height, kSourceH);
  EXPECT_EQ(info_.layers[1].width, kSourceW / 2);
  EXPECT_EQ(info_.layers[1].height, kSourceH / 2);

  // The first packet of each layer: KEYFRAME|REINIT within 500 ms after im_start returned (§6.3).
  std::vector<std::optional<Pkt>> first(2);
  Pull(s, milliseconds(2000), [&](const Pkt &p) {
    if (p.video() && p.layer < 2 && !first[p.layer]) {
      first[p.layer] = p;
    }
    return first[0] && first[1];
  });
  for (uint32_t l = 0; l < 2; ++l) {
    ASSERT_TRUE(first[l].has_value()) << "layer " << l << ": no packet within 2 s";
    EXPECT_EQ(first[l]->flags & (IM_PKT_KEYFRAME | IM_PKT_REINIT), IM_PKT_KEYFRAME | IM_PKT_REINIT) << "layer " << l;
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(first[l]->at - started_).count(), 500) << "layer " << l;
  }
  Pull(s, milliseconds(1500));
  EXPECT_EQ(im_stop(s), IM_OK);
  ExpectNoErrors();

  const std::vector<Seen> seen = TheRecorder().Snapshot();
  std::vector<uint32_t> checked(2, 0), wrong(2, 0);
  std::string first_wrong;
  for (const Seen &v : seen) {
    ASSERT_LT(v.layer, 2u);
    const auto k = GridFrame(v.ts);
    if (!k || v.black_asked || v.refresh) {
      continue;
    }
    ++checked[v.layer];
    const int64_t elapsed = im::session::PatternGrid{epoch_, kHz, 0}.FrameNs(*k) - epoch_;
    const bool counter_ok = v.counter == (*k & 0xFFFFu);
    const bool flash_ok = v.flash == im::gpu::PatternFlash(elapsed);
    if ((!counter_ok || !flash_ok || v.black) && wrong[v.layer]++ == 0 && first_wrong.empty()) {
      first_wrong = "layer " + std::to_string(v.layer) + ", frame " + std::to_string(*k) + ": counter " +
                    std::to_string(v.counter) + ", flash " + (v.flash ? "on" : "off") + (v.black ? ", black" : "");
    }
  }
  std::printf("[ info     ] %u pictures of f and %u of q checked against their frames\n", checked[0], checked[1]);
  EXPECT_GE(checked[0], 45u) << "f: about 90 frames in 1.5 s";
  EXPECT_GE(checked[1], 12u) << "q: about 22 frames in 1.5 s";
  EXPECT_EQ(wrong[0] + wrong[1], 0u) << first_wrong;
}

TEST_F(SessionGpu, StaticScreenRefreshesAndKeyframesShowTheOneFrame) {
  Configure(im::session::Pattern::kStatic);
  const im_session s = Start();
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(700)); // the first IDRs and the 200 ms refresh
  const int64_t asked = EngineNow();
  ASSERT_EQ(im_request_keyframe(s, IM_ALL_LAYERS), IM_OK) << LastError();
  Pull(s, milliseconds(1000));
  EXPECT_EQ(im_stop(s), IM_OK);
  ExpectNoErrors();

  // The only frame is frame 0 (captured at epoch_ns): every picture shows it, the refreshes and the static IDRs (both
  // stamped now) included. A static screen gets no GOP IDR (§9.4): the IDRs after the request are the requested ones.
  uint32_t refreshes = 0, static_idrs = 0, total = 0;
  for (const Seen &v : TheRecorder().Snapshot()) {
    ++total;
    EXPECT_FALSE(v.black) << "layer " << v.layer << " at " << v.ts - epoch_ << " ns";
    EXPECT_EQ(v.counter, 0u) << "layer " << v.layer << " at " << v.ts - epoch_ << " ns";
    refreshes += v.refresh ? 1 : 0;
    static_idrs += v.idr && v.at >= asked ? 1 : 0;
  }
  EXPECT_GE(total, 4u);
  EXPECT_GE(refreshes, 2u) << "the 200 ms refresh of each layer";
  EXPECT_GE(static_idrs, 2u) << "the requested keyframe of each layer, from the last frame";
}

TEST_F(SessionGpu, FirstKeyframeIsBlackWithoutAFrame) {
  Configure(im::session::Pattern::kNone);
  const im_session s = Start();
  ASSERT_NE(s, 0u);
  std::vector<std::optional<Pkt>> first(2);
  Pull(s, milliseconds(2000), [&](const Pkt &p) {
    if (p.video() && p.layer < 2 && !first[p.layer]) {
      first[p.layer] = p;
    }
    return first[0] && first[1];
  });
  EXPECT_EQ(im_stop(s), IM_OK);
  ExpectNoErrors();
  for (uint32_t l = 0; l < 2; ++l) {
    ASSERT_TRUE(first[l].has_value()) << "layer " << l;
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(first[l]->at - started_).count(), 500) << "layer " << l;
  }
  const std::vector<Seen> seen = TheRecorder().Snapshot();
  ASSERT_GE(seen.size(), 2u);
  for (const Seen &v : seen) {
    EXPECT_TRUE(v.black_asked) << "layer " << v.layer << ": nothing was captured";
    EXPECT_TRUE(v.black) << "layer " << v.layer << ": the picture is black (Y 16, Cb/Cr 128)";
  }
}

TEST_F(SessionGpu, PausedPicturesShowOneFrameFromBeforeThePause) {
  Configure(im::session::Pattern::kMoving);
  const im_session s = Start();
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(500));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  const int64_t paused_at = EngineNow();
  Pull(s, milliseconds(400));
  ASSERT_EQ(im_request_keyframe(s, IM_ALL_LAYERS), IM_OK) << LastError();
  Pull(s, milliseconds(800));
  const int64_t resumed_at = EngineNow();
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  Pull(s, milliseconds(500));
  EXPECT_EQ(im_stop(s), IM_OK);
  ExpectNoErrors();

  // The video thread applies the pause within a wake-up of im_set_paused; from then on the slots keep the frozen frame.
  const int64_t settled = paused_at + 50'000'000;
  const uint64_t newest_before = FrameAt(settled) & 0xFFFFu;
  std::optional<uint32_t> frozen;
  uint32_t paused_pictures = 0, after = 0, newer_after = 0;
  for (const Seen &v : TheRecorder().Snapshot()) {
    if (v.at >= settled && v.at < resumed_at) {
      ++paused_pictures;
      EXPECT_TRUE(v.refresh || v.idr) << "only refreshes and keyframes while paused";
      EXPECT_LE(v.counter, newest_before) << "a picture of a frame captured after the pause";
      if (!frozen) {
        frozen = v.counter;
      }
      EXPECT_EQ(v.counter, *frozen) << "layer " << v.layer << ": every paused picture shows the frozen frame";
    } else if (v.at >= resumed_at + 100'000'000 && GridFrame(v.ts) && !v.refresh) {
      ++after;
      newer_after += frozen && v.counter > *frozen ? 1 : 0;
    }
  }
  EXPECT_GE(paused_pictures, 2u) << "the refreshes and the requested keyframes while paused";
  ASSERT_TRUE(frozen.has_value());
  EXPECT_GE(after, 10u) << "resume encodes new frames again";
  EXPECT_EQ(newer_after, after) << "after the resume the pictures are newer than the frozen frame";
}

} // namespace

#else // IM_TEST_HOOKS

TEST(SessionGpu, NeedsTestHooks) { GTEST_SKIP() << "session_gpu_test needs an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS
