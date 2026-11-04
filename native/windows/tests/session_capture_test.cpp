// session_capture_test (docs/m2/04-windows-engine.md §7.1, §7.3, §7.6–§7.8, §6.1; README WF12): a session whose
// source a capture area captures, through the C ABI with the test hooks (IM_TEST_HOOKS builds: win-x64-ci,
// win-x64-asan). A fake capture (capture::SetCaptureFactoryForTest) stands in for WGC: it captures the test hooks'
// fake picks, delivers a frame every 16 ms on a thread of its own and reports what the test asks for (a resize, a
// close, a failure, a minimized window), one call at a time like WGC's FrameArrived. The sessions run IM_ENC_FAKE
// without a device (frame times only), except the one whose capture needs slots, which runs the GPU path on the
// default adapter (Basic Render on CI).
//
// It asserts: the capture starts with the start flags (IM_START_HIDE_CURSOR, IM_START_KEEP_BORDER) and layer 0's fps;
// IM_SESS_NO_OS_INDICATOR is set only when the capture runs borderless; its frames reach the encoders (the first
// KEYFRAME|REINIT within 500 ms, packets carry the capture's times, the stats count its frames and no test pattern
// runs besides); a closed source ends the session with ENDED{IM_ERR_SOURCE_GONE} source_gone and the capture stops; a
// failed capture ends it (source_gone, internal, or encoder_lost device_removed); a resize sends one SOURCE_RESIZED
// {"width","height"} and, once the size is stable for 1 s, re-inits only the layers whose output size changes
// (REINIT|KEYFRAME at the new size); a source whose size changed since the pick is a resize at once; a minimized
// window sends SOURCE_MINIMIZED and SOURCE_RESTORED; a layer-0 fps re-init calls SetFps (a layer-1 one doesn't);
// im_stop stops the capture first, before the session releases its frames and device (the capture's last callback
// still finds them; the logs show the order); a capture that needs slots gets them (the GPU path) even for
// IM_ENC_FAKE.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cstdint>
#include <cstring>
#include <functional>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <thread>
#include <utility>
#include <vector>

#include "capture/capture_source.h"
#include "capture/source_watch.h"
#include "engine/os.h"
#include "gpu/slots.h"
#include "im/log/ring.h"
#include "isshoni_media.h"
#include "testhooks/testhooks.h"

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr milliseconds kFrameEvery{16};

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

std::string StatsOf(im_session s) {
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  return n > 0 ? std::string(buf.data(), static_cast<std::size_t>(std::min<int32_t>(n, 16383))) : std::string();
}

double NumberAfter(const std::string &doc, const std::string &key) {
  const std::size_t at = doc.find(key);
  return at == std::string::npos ? -1.0 : std::strtod(doc.c_str() + at + key.size(), nullptr);
}

// Everything in the engine's log ring.
std::string DrainLogs() {
  std::vector<char> buf(64 * 1024);
  std::string all;
  for (;;) {
    const int32_t n = im_read_logs(buf.data(), static_cast<int32_t>(buf.size()));
    if (n <= 0) {
      return all;
    }
    all.append(buf.data(), static_cast<std::size_t>(n));
  }
}

struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = IM_NO_LAYER;
  uint32_t flags = 0;
  int64_t ts = 0;
  std::string data;
  uint16_t width = 0;
  uint16_t height = 0;
  int32_t event = 0;
  im_status status = IM_OK;
  Clock::time_point at;

  bool video() const { return kind == IM_PACKET_VIDEO; }
  bool key() const { return (flags & IM_PKT_KEYFRAME) != 0; }
  bool reinit() const { return (flags & IM_PKT_REINIT) != 0; }
  bool is_event(int32_t e) const { return kind == IM_PACKET_EVENT && event == e; }
};

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
  out->data.assign(reinterpret_cast<const char *>(p.data), p.size);
  out->width = p.width;
  out->height = p.height;
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

// What the test tells the fake capture and what it saw. One per test.
struct FakeState {
  bool needs_slots = false;
  bool borderless = false;
  im::capture::WindowHandle window = 0x1230;
  im::video::Size start_size{1280, 720}; // CaptureSource::size()
  std::atomic<bool> minimized{false};
  std::atomic<uint64_t> resize{0}; // capture::PackSize of a size to report next
  std::atomic<bool> close{false};
  std::atomic<im_status> fail{IM_OK};

  // Seen by the fake.
  std::atomic<int> starts{0};
  std::atomic<int> stops{0};
  std::atomic<bool> got_slots{false};
  im::capture::CaptureOptions options; // written by Start on the engine worker, read after im_start returned
  std::atomic<uint32_t> fps_set{0};
  std::atomic<int> set_fps_calls{0};
  std::atomic<uint64_t> frames{0};
  std::atomic<int64_t> first_ts{0};
};

class FakeCapture final : public im::capture::CaptureSource {
public:
  FakeCapture(FakeState *state, im::gpu::FrameSlots *slots, im::capture::CaptureSink *sink)
      : state_(state), slots_(slots), sink_(sink) {
    thread_ = std::thread([this] {
      while (!quit_.load(std::memory_order_acquire)) {
        Step();
        std::this_thread::sleep_for(kFrameEvery);
      }
    });
  }
  ~FakeCapture() override { Stop(); }

  bool borderless() const noexcept override { return state_->borderless; }
  im::capture::WindowHandle window() const noexcept override { return state_->window; }
  bool minimized() const noexcept override { return state_->minimized.load(std::memory_order_acquire); }
  im::video::Size size() const noexcept override { return state_->start_size; }
  void SetFps(uint32_t fps) noexcept override {
    state_->fps_set.store(fps, std::memory_order_release);
    state_->set_fps_calls.fetch_add(1, std::memory_order_acq_rel);
  }
  void Stop() noexcept override {
    std::lock_guard<std::mutex> stop_lock(stop_mu_); // a second caller returns only once the first one is done
    if (stopped_) {
      return;
    }
    quit_.store(true, std::memory_order_release);
    if (thread_.joinable()) {
      thread_.join();
    }
    // One last callback before Stop returns, as a WGC frame in flight may arrive: the session's frames (and, with
    // slots, its slots and device) must still be there (ASan builds catch a use after free).
    Step();
    {
      std::lock_guard<std::mutex> lock(mu_);
      stopped_ = true;
    }
    im::log::Info("test", "fake capture stopped");
    state_->stops.fetch_add(1, std::memory_order_acq_rel);
  }

private:
  void Step() {
    std::lock_guard<std::mutex> lock(mu_);
    if (stopped_) {
      return;
    }
    if (const uint64_t r = state_->resize.exchange(0, std::memory_order_acq_rel); r != 0) {
      content_ = im::capture::UnpackSize(r);
      sink_->OnResized(content_);
    }
    if (state_->close.exchange(false, std::memory_order_acq_rel)) {
      sink_->OnClosed();
    }
    if (const im_status f = state_->fail.exchange(IM_OK, std::memory_order_acq_rel); f != IM_OK) {
      sink_->OnFailed(f, "fake: the capture failed");
    }
    const int64_t ts = im::engine::os::QpcNs();
    if (slots_ != nullptr) {
      im::gpu::Slot *slot = nullptr;
      if (slots_->BeginWrite(content_, &slot) == IM_OK) {
        im::gpu::FrameMeta meta;
        meta.capture_ts_ns = ts;
        meta.content = content_;
        slots_->Publish(meta);
      }
    }
    int64_t none = 0;
    state_->first_ts.compare_exchange_strong(none, ts, std::memory_order_acq_rel);
    state_->frames.fetch_add(1, std::memory_order_acq_rel);
    sink_->OnFrame(ts);
  }

  FakeState *state_;
  im::gpu::FrameSlots *slots_;
  im::capture::CaptureSink *sink_;
  im::video::Size content_{1280, 720};
  std::mutex mu_;      // serializes the callbacks (Step) and the stopped flag
  std::mutex stop_mu_; // serializes Stop
  bool stopped_ = false;
  std::atomic<bool> quit_{false};
  std::thread thread_;
};

// Captures the test hooks' fake picks (a test source of a window or monitor kind).
class FakeFactory final : public im::capture::CaptureFactory {
public:
  explicit FakeFactory(FakeState *state) : state_(state) {}
  bool Handles(const im::session::SourceSpec &source) const noexcept override {
    return source.test && source.capture_path == IM_CAPTURE_TEST && source.kind != IM_SOURCE_TEST_PATTERN;
  }
  bool NeedsSlots(const im::session::SourceSpec &) const noexcept override { return state_->needs_slots; }
  im_status Start(const im::session::SourceSpec &, im::gpu::FrameSlots *slots,
                  const im::capture::CaptureOptions &options, im::capture::CaptureSink *sink,
                  std::unique_ptr<im::capture::CaptureSource> *out) noexcept override {
    state_->options = options;
    state_->got_slots.store(slots != nullptr, std::memory_order_release);
    state_->starts.fetch_add(1, std::memory_order_acq_rel);
    *out = std::make_unique<FakeCapture>(state_, slots, sink);
    return IM_OK;
  }

private:
  FakeState *state_;
};

im_layer_config Layer(const char *rid, uint32_t max_pixels, uint32_t fps, uint32_t bps) {
  im_layer_config l{};
  std::memcpy(l.rid, rid, std::strlen(rid) + 1);
  l.max_pixels = max_pixels;
  l.fps = fps;
  l.bitrate_bps = bps;
  return l;
}

class SessionCapture : public ::testing::Test {
protected:
  void SetUp() override {
    im::testhooks::Reset();
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO;
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
    im::capture::SetCaptureFactoryForTest(&factory_);
  }
  void TearDown() override {
    im_shutdown();
    im::capture::SetCaptureFactoryForTest(nullptr);
    im::testhooks::Reset();
  }

  // A fake pick of 1280x720 with f (up to 1080p, 60 fps) and q (360p, 15 fps), no audio.
  im_start_config Config(uint32_t flags = 0) {
    token_ = im::testhooks::FakePick(IM_SOURCE_WINDOW, 1280, 720);
    im_start_config c{};
    c.struct_size = sizeof c;
    c.flags = flags;
    c.source_id = token_.c_str();
    c.profile = IM_PROFILE_HIGH;
    c.content_hint = IM_CONTENT_AUTO;
    c.encoder = IM_ENC_AUTO;
    c.layer_count = 2;
    c.layers[0] = Layer("f", 1920 * 1080, 60, 4'000'000);
    c.layers[1] = Layer("q", 640 * 360, 15, 300'000);
    c.audio.mode = IM_AUDIO_MODE_OFF;
    return c;
  }

  im_session Start(const im_start_config &cfg, im_session_info *out_info = nullptr) {
    im_session s = 0;
    im_session_info local{};
    local.struct_size = sizeof local;
    const im_status st = im_start(&cfg, &s, &local);
    started_ = Clock::now();
    EXPECT_EQ(st, IM_OK) << LastError();
    if (out_info != nullptr) {
      *out_info = local;
    }
    epoch_ = local.epoch_ns;
    return s;
  }

  // Waits for ENDED.
  std::optional<Pkt> WaitEnded(im_session s, milliseconds d) {
    std::optional<Pkt> ended;
    Pull(s, d, [&](const Pkt &p) {
      if (p.is_event(IM_EVENT_ENDED)) {
        ended = p;
        return true;
      }
      return false;
    });
    return ended;
  }

  FakeState state_;
  FakeFactory factory_{&state_};
  std::string token_;
  Clock::time_point started_;
  int64_t epoch_ = 0;
};

// ------------------------------------------------------------------------------------------------------ start

TEST_F(SessionCapture, StartsWithTheStartFlagsAndLayer0Fps) {
  const im_session s = Start(Config(IM_START_HIDE_CURSOR | IM_START_KEEP_BORDER));
  ASSERT_NE(s, 0u);
  EXPECT_EQ(state_.starts.load(), 1);
  EXPECT_TRUE(state_.options.hide_cursor);
  EXPECT_TRUE(state_.options.keep_border);
  EXPECT_EQ(state_.options.fps, 60u) << "layer 0's fps";
  EXPECT_FALSE(state_.got_slots.load()) << "an IM_ENC_FAKE session without slots (the fake needs none)";
  EXPECT_EQ(im_stop(s), IM_OK);

  const im_session plain = Start(Config());
  ASSERT_NE(plain, 0u);
  EXPECT_EQ(state_.starts.load(), 2);
  EXPECT_FALSE(state_.options.hide_cursor) << "the pointer is drawn unless IM_START_HIDE_CURSOR (§7.8)";
  EXPECT_FALSE(state_.options.keep_border);
  EXPECT_EQ(im_stop(plain), IM_OK);
}

TEST_F(SessionCapture, NoOsIndicatorOnlyWhenTheCaptureIsBorderless) {
  im_session_info with_border{};
  const im_session s = Start(Config(), &with_border);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(with_border.flags & IM_SESS_NO_OS_INDICATOR, 0u) << "Windows draws its border";
  EXPECT_EQ(im_stop(s), IM_OK);

  state_.borderless = true;
  im_session_info borderless{};
  const im_session b = Start(Config(), &borderless);
  ASSERT_NE(b, 0u);
  EXPECT_NE(borderless.flags & IM_SESS_NO_OS_INDICATOR, 0u);
  EXPECT_EQ(im_stop(b), IM_OK);
}

// ----------------------------------------------------------------------------------------------------- frames

TEST_F(SessionCapture, FramesReachTheEncoders) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  std::vector<std::optional<Pkt>> first(2);
  std::vector<Pkt> video;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    if (p.video()) {
      video.push_back(p);
      if (p.layer < 2 && !first[p.layer]) {
        first[p.layer] = p;
      }
    }
    return false;
  });
  for (uint32_t l = 0; l < 2; ++l) {
    SCOPED_TRACE("layer " + std::to_string(l));
    ASSERT_TRUE(first[l].has_value());
    EXPECT_TRUE(first[l]->key() && first[l]->reinit()) << "flags " << first[l]->flags;
    EXPECT_LE(std::chrono::duration_cast<milliseconds>(first[l]->at - started_).count(), 500);
  }
  // The packets carry the capture's frame times: none before its first frame (there is no test pattern besides).
  const int64_t first_ts = state_.first_ts.load();
  ASSERT_NE(first_ts, 0);
  std::size_t f_packets = 0;
  for (const Pkt &p : video) {
    EXPECT_GE(p.ts, first_ts) << "a packet older than the capture's first frame";
    f_packets += p.layer == 0 ? 1u : 0u;
  }
  // The fake's 16 ms sleeps may take a timer tick longer on Windows (15.6 ms): at least 30 fps.
  EXPECT_GE(f_packets, 30u) << "the capture's frames keep coming for 1.5 s";
  // fpsIn and frames count the capture's frames only.
  const std::string stats = StatsOf(s);
  const double frames = NumberAfter(stats, "\"frames\":");
  const auto captured = static_cast<double>(state_.frames.load());
  EXPECT_GT(frames, 0.0) << stats;
  EXPECT_LE(frames, captured + 1.0) << "more frames than the capture delivered: the test pattern ran too\n" << stats;
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ------------------------------------------------------------------------------------------------------- ends

TEST_F(SessionCapture, ClosedSourceEndsWithSourceGone) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300));
  const auto closed = Clock::now();
  state_.close.store(true);
  const std::optional<Pkt> ended = WaitEnded(s, milliseconds(2000));
  ASSERT_TRUE(ended.has_value());
  EXPECT_LE(std::chrono::duration_cast<milliseconds>(ended->at - closed).count(), 500);
  EXPECT_EQ(ended->status, IM_ERR_SOURCE_GONE);
  EXPECT_NE(ended->data.find("\"reason\":\"source_gone\""), std::string::npos) << ended->data;
  Pkt p;
  EXPECT_EQ(NextPkt(s, 0, &p), IM_ENDED) << "ENDED is the last packet";
  // The supervisor stopped the capture once the session ended, before im_stop.
  const auto until = Clock::now() + milliseconds(1000);
  while (state_.stops.load() == 0 && Clock::now() < until) {
    std::this_thread::sleep_for(milliseconds(10));
  }
  EXPECT_EQ(state_.stops.load(), 1);
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(state_.stops.load(), 1) << "Stop is idempotent";
}

TEST_F(SessionCapture, FailedCaptureEndsTheSession) {
  struct Case {
    im_status fail;
    im_status ended;
    const char *reason;
    const char *detail;
  };
  const Case cases[] = {
      {IM_ERR_SOURCE_GONE, IM_ERR_SOURCE_GONE, "source_gone", "fake: the capture failed"},
      {IM_ERR_INTERNAL, IM_ERR_INTERNAL, "internal", "fake: the capture failed"},
      {IM_ERR_ENCODER_LOST, IM_ERR_ENCODER_LOST, "encoder_lost", "device_removed: "},
  };
  for (const Case &c : cases) {
    SCOPED_TRACE(c.reason);
    const im_session s = Start(Config());
    ASSERT_NE(s, 0u);
    Pull(s, milliseconds(200));
    state_.fail.store(c.fail);
    const std::optional<Pkt> ended = WaitEnded(s, milliseconds(2000));
    ASSERT_TRUE(ended.has_value());
    EXPECT_EQ(ended->status, c.ended);
    EXPECT_NE(ended->data.find(std::string("\"reason\":\"") + c.reason + "\""), std::string::npos) << ended->data;
    EXPECT_NE(ended->data.find(c.detail), std::string::npos) << ended->data;
    EXPECT_EQ(im_stop(s), IM_OK);
  }
}

// ---------------------------------------------------------------------------------------------- source changes

TEST_F(SessionCapture, ResizeSendsSourceResizedThenReinitsTheLayersWhoseSizeChanges) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(1200)); // past the first keyframes and the re-init spacing
  const auto resized_at = Clock::now();
  state_.resize.store(im::capture::PackSize(im::video::Size{1920, 1080}));
  int resized_events = 0;
  std::optional<Pkt> f_reinit;
  std::vector<Pkt> q_reinits;
  Pull(s, milliseconds(4000), [&](const Pkt &p) {
    if (p.is_event(IM_EVENT_SOURCE_RESIZED)) {
      ++resized_events;
      EXPECT_NE(p.data.find("\"width\":1920"), std::string::npos) << p.data;
      EXPECT_NE(p.data.find("\"height\":1080"), std::string::npos) << p.data;
      EXPECT_LE(std::chrono::duration_cast<milliseconds>(p.at - resized_at).count(), 300) << "at once";
    }
    if (p.video() && p.reinit()) {
      if (p.layer == 0 && !f_reinit) {
        f_reinit = p;
      } else if (p.layer == 1) {
        q_reinits.push_back(p);
      }
    }
    return false;
  });
  EXPECT_EQ(resized_events, 1) << "one SOURCE_RESIZED per resize";
  ASSERT_TRUE(f_reinit.has_value()) << "f's output size changes (1280x720 -> 1920x1080)";
  EXPECT_TRUE(f_reinit->key());
  EXPECT_EQ(f_reinit->width, 1920);
  EXPECT_EQ(f_reinit->height, 1080);
  EXPECT_GE(std::chrono::duration_cast<milliseconds>(f_reinit->at - resized_at).count(), 900)
      << "only once the size was stable for 1 s";
  EXPECT_TRUE(q_reinits.empty()) << "q stays 640x360: no re-init";
  im_session_info now{};
  now.struct_size = sizeof now;
  ASSERT_EQ(im_get_session_info(s, &now), IM_OK);
  EXPECT_EQ(now.layers[0].width, 1920u);
  EXPECT_EQ(now.layers[1].width, 640u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(SessionCapture, ASizeChangedSinceThePickIsAResizeAtOnce) {
  state_.start_size = im::video::Size{1600, 900};
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  std::optional<Pkt> resized;
  Pull(s, milliseconds(1000), [&](const Pkt &p) {
    if (p.is_event(IM_EVENT_SOURCE_RESIZED)) {
      resized = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(resized.has_value());
  EXPECT_NE(resized->data.find("\"width\":1600"), std::string::npos) << resized->data;
  EXPECT_NE(resized->data.find("\"height\":900"), std::string::npos) << resized->data;
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(SessionCapture, MinimizedWindowSendsMinimizedAndRestored) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300));
  state_.minimized.store(true);
  std::optional<Pkt> minimized;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    EXPECT_FALSE(p.is_event(IM_EVENT_SOURCE_RESTORED)) << "restored before minimized";
    if (p.is_event(IM_EVENT_SOURCE_MINIMIZED)) {
      minimized = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(minimized.has_value()) << "IsIconic is polled every 500 ms";
  EXPECT_EQ(minimized->data, "{}");
  state_.minimized.store(false);
  std::optional<Pkt> restored;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    EXPECT_FALSE(p.is_event(IM_EVENT_SOURCE_MINIMIZED)) << "one event per edge";
    if (p.is_event(IM_EVENT_SOURCE_RESTORED)) {
      restored = p;
      return true;
    }
    return false;
  });
  ASSERT_TRUE(restored.has_value());
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(SessionCapture, MonitorsAndUnidentifiedWindowsAreNotPolled) {
  state_.window = 0;
  state_.minimized.store(true);
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> got = Pull(s, milliseconds(1200));
  for (const Pkt &p : got) {
    EXPECT_FALSE(p.is_event(IM_EVENT_SOURCE_MINIMIZED)) << "no window to poll";
  }
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(SessionCapture, Layer0FpsReinitSetsTheCapturesInterval) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300));
  im_layer_config q = Layer("q", 640 * 360, 10, 0);
  ASSERT_EQ(im_update_layer(s, 1, &q), IM_OK) << LastError();
  Pull(s, milliseconds(1500));
  EXPECT_EQ(state_.set_fps_calls.load(), 0) << "layer 1's fps doesn't drive the capture";

  im_layer_config f = Layer("f", 1920 * 1080, 30, 0);
  ASSERT_EQ(im_update_layer(s, 0, &f), IM_OK) << LastError();
  const auto until = Clock::now() + milliseconds(3000);
  while (state_.set_fps_calls.load() == 0 && Clock::now() < until) {
    Pull(s, milliseconds(100));
  }
  EXPECT_EQ(state_.set_fps_calls.load(), 1);
  EXPECT_EQ(state_.fps_set.load(), 30u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

// ------------------------------------------------------------------------------------------------------- stop

TEST_F(SessionCapture, StopStopsTheCaptureBeforeTheFramesAndTheDeviceGo) {
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(300));
  DrainLogs();
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(state_.stops.load(), 1);
  const std::string logs = DrainLogs();
  const std::size_t stopped = logs.find("fake capture stopped");
  const std::size_t released = logs.find("video released");
  ASSERT_NE(stopped, std::string::npos) << logs;
  ASSERT_NE(released, std::string::npos) << logs;
  EXPECT_LT(stopped, released) << "the capture stops before the session releases its frames and device";
}

TEST_F(SessionCapture, ACaptureThatNeedsSlotsKeepsTheGpuPath) {
  state_.needs_slots = true;
  const im_session s = Start(Config());
  ASSERT_NE(s, 0u) << "a device on the default adapter (Basic Render on CI)";
  EXPECT_TRUE(state_.got_slots.load()) << "IM_ENC_FAKE takes nothing, the capture still gets slots";
  bool keyframe = false;
  Pull(s, milliseconds(1500), [&](const Pkt &p) {
    keyframe = keyframe || (p.video() && p.layer == 0 && p.key());
    return keyframe;
  });
  EXPECT_TRUE(keyframe) << "frames from the slots reach the encoder";
  DrainLogs();
  EXPECT_EQ(im_stop(s), IM_OK);
  const std::string logs = DrainLogs();
  const std::size_t stopped = logs.find("fake capture stopped");
  const std::size_t released = logs.find("video released");
  ASSERT_NE(stopped, std::string::npos) << logs;
  ASSERT_NE(released, std::string::npos) << logs;
  EXPECT_LT(stopped, released) << "the capture's last write into the slots comes before they go";
}

} // namespace

#else // IM_TEST_HOOKS

TEST(SessionCapture, NeedsTestHooks) {
  GTEST_SKIP() << "session_capture_test needs an IM_TEST_HOOKS build (win-x64-ci)";
}

#endif // IM_TEST_HOOKS
