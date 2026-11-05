#include "session/session_object.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <exception>
#include <utility>

#include "api/guard.h"
#include "audio_bridge/audio_bridge.h"
#include "encode/selector.h"
#include "im/abi/status.h"
#include "im/encpolicy/encoders.h"
#include "im/json/writer.h"
#include "im/log/ring.h"

namespace im::session {
namespace {

using engine::os::QpcNs;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
constexpr uint8_t kFaultNone = 0;
constexpr uint8_t kFaultEncodeError = 1;
std::atomic<bool> g_device_removed_for_test{false};  // SetDeviceRemovedForTest
std::atomic<int64_t> g_capture_adapter_for_test{-1}; // SetCaptureAdapterForTest: -1 none, else the adapter index
#endif

// The capture adapter of 04 §9.1's order for a session's candidates (README WA61): a Desktop Duplication source's
// output adapter (capture::OutputAdapterLuid, README WA60), else the primary monitor's adapter, as WGC doesn't report
// the source's own monitor yet (encode::PrimaryAdapterIndex). Called after EncoderCatalog, which stored the probe.
uint32_t CaptureAdapterIndex(const SourceSpec &source) noexcept {
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (const int64_t v = g_capture_adapter_for_test.load(std::memory_order_acquire); v >= 0) {
    return static_cast<uint32_t>(v);
  }
#endif
  if (const uint64_t luid = capture::OutputAdapterLuid(source); luid != 0) {
    if (const uint32_t i = encode::AdapterIndexOfLuid(luid); i != encpolicy::kNoAdapter) {
      return i;
    }
  }
  return encode::PrimaryAdapterIndex();
}

std::string_view RidView(const char (&rid)[4]) noexcept {
  return std::string_view(rid, static_cast<std::size_t>(std::find(rid, rid + 4, '\0') - rid));
}

// The JSON of an event; "{}" when the writer can't allocate (an event never fails).
template <class F> std::string EventJson(F &&fill) noexcept {
  try {
    json::Writer w;
    w.BeginObject();
    fill(w);
    w.EndObject();
    return w.Take();
  } catch (...) {
    try {
      return "{}";
    } catch (...) {
      return {};
    }
  }
}

bool ValidLayerBitrate(uint32_t layer, uint32_t bps) noexcept { return encpolicy::ValidVideoBitrate(layer, bps); }

} // namespace

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
void SetDeviceRemovedForTest(bool removed) noexcept {
  g_device_removed_for_test.store(removed, std::memory_order_release);
}

void SetCaptureAdapterForTest(std::optional<uint32_t> adapter_index) noexcept {
  g_capture_adapter_for_test.store(adapter_index ? static_cast<int64_t>(*adapter_index) : -1,
                                   std::memory_order_release);
}
#endif

// ------------------------------------------------------------------------------------------------- small parts

bool Session::RingWriter::Write(const audio::AudioPacket &p) noexcept {
  ring::PacketMeta m;
  m.flags = p.flags;
  m.capture_ts_ns = p.capture_ts_ns;
  m.duration_us = p.duration_us;
  return ring_->Push(m, std::span<const uint8_t>(p.data, p.size)) == ring::PushResult::kOk;
}

im_status Session::Opener::Open(uint32_t layer, const encpolicy::Candidate &c, uint32_t profile) {
  return s_->OpenLayer(layer, c, profile);
}

void Session::Opener::Close(uint32_t layer) {
  if (layer < IM_MAX_LAYERS) {
    s_->layers_[layer].enc.reset();
  }
}

void Session::Opener::Wait(uint32_t ms) { std::this_thread::sleep_for(std::chrono::milliseconds(ms)); }

Session::Session() : events_(&signal_), pump_(&events_, &signal_, &clock_) {}

// ------------------------------------------------------------------------------------------- the capture's sink

void Session::Sink::OnFrame(int64_t capture_ts_ns) noexcept {
  if (s_->capture_stopped_.load(std::memory_order_acquire)) {
    return; // the session ended: nothing more is counted or encoded
  }
  s_->frames_->PublishTime(capture_ts_ns); // a capture without slots (test hooks); with slots it published the frame
  s_->frames_captured_.fetch_add(1, std::memory_order_relaxed);
  s_->video_waiter_.Wake();
}

void Session::Sink::OnResized(video::Size content) noexcept {
  s_->reported_size_.store(capture::PackSize(content), std::memory_order_relaxed);
  s_->resize_reported_.store(true, std::memory_order_release);
  s_->supervisor_waiter_.Wake();
}

void Session::Sink::OnInterrupted(std::string_view cause) noexcept {
  if (s_->capture_stopped_.load(std::memory_order_acquire) || s_->ended_.load(std::memory_order_acquire)) {
    return;
  }
  // 04 §5.6, §7.4: the last frame repeats until OnResumed (no frames come meanwhile).
  const std::string doc = EventJson([&](json::Writer &w) { w.Key("cause").String(cause); });
  s_->events_.Push(IM_EVENT_CAPTURE_INTERRUPTED, IM_OK, IM_NO_LAYER, QpcNs(), doc);
  log::Info("session", "capture interrupted", {{"cause", cause}});
}

void Session::Sink::OnResumed(std::string_view cause) noexcept {
  if (s_->capture_stopped_.load(std::memory_order_acquire) || s_->ended_.load(std::memory_order_acquire)) {
    return;
  }
  const std::string doc = EventJson([&](json::Writer &w) { w.Key("cause").String(cause); });
  s_->events_.Push(IM_EVENT_CAPTURE_RESUMED, IM_OK, IM_NO_LAYER, QpcNs(), doc);
  log::Info("session", "capture resumed", {{"cause", cause}});
}

void Session::Sink::OnClosed() noexcept {
  s_->source_closed_.store(true, std::memory_order_release);
  s_->supervisor_waiter_.Wake();
}

void Session::Sink::OnFailed(im_status status, std::string_view detail) noexcept {
  try {
    std::lock_guard<std::mutex> lock(s_->failure_mu_);
    if (s_->failure_status_ == IM_OK) {
      s_->failure_status_ = status;
      s_->failure_detail_.assign(detail);
    }
  } catch (...) {
    // No detail: the supervisor still ends the session (IM_ERR_INTERNAL when no status was kept).
  }
  s_->capture_failed_.store(true, std::memory_order_release);
  s_->supervisor_waiter_.Wake();
}

Session::~Session() {
  // Stop ran (the area calls it before it lets go), or Start failed before any thread ran.
  for (Thread *t : {&video_thread_, &supervisor_thread_, &tone_thread_}) {
    if (t->th.joinable()) {
      t->th.detach(); // only an abandoned thread: it holds no reference any more once it exits
    }
  }
}

im_status Session::CheckLayer(const char *fn, uint32_t layer) const noexcept {
  if (layer < layer_count_) {
    return IM_OK;
  }
  return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: the session has no layer %u (%u video layers)", fn,
                    static_cast<unsigned>(layer), static_cast<unsigned>(layer_count_));
}

std::string_view Session::StreamName(uint32_t stream) const noexcept {
  if (stream < layer_count_) {
    return RidView(layers_[stream].rid);
  }
  return stream == layer_count_ ? std::string_view("audio") : std::string_view("pcm");
}

// --------------------------------------------------------------------------------------------------------- start

im_status Session::Start(const api::StartRequest &req, const SourceSpec *source, uint32_t init_flags) {
  const im_start_config &cfg = req.config();
  epoch_ = QpcNs();
  has_video_ = source != nullptr && cfg.layer_count > 0;
  layer_count_ = has_video_ ? cfg.layer_count : 0;
  audio_mode_ = cfg.audio.mode;
  profile_ = cfg.profile;
  content_hint_ = cfg.content_hint;
  if (source != nullptr) {
    source_ = *source;
  }
  src_size_ = video::Size{source_.width, source_.height};
  info_.struct_size = sizeof(im_session_info);
  info_.epoch_ns = epoch_;
  info_.layer_count = layer_count_;
  info_.capture_path = has_video_ ? source_.capture_path : 0;

  im_status st = IM_OK;
  try {
    if (has_video_) {
      st = StartVideo(cfg);
    }
    if (st == IM_OK) {
      st = StartAudio(cfg, init_flags);
    }
    if (st == IM_OK) {
      if ((cfg.flags & IM_START_PAUSED) != 0) {
        paused_.store(true, std::memory_order_release);
        if (pacer_) {
          pacer_->SetPaused(true, epoch_);
        }
        if (tone_) {
          tone_->tone().SetPaused(true);
        }
        if (mixer_) {
          mixer_->SetPaused(true);
        }
      }
      StartThreads();
    }
  } catch (const std::exception &e) {
    Stop();
    return api::OnException("im_start", e.what());
  } catch (...) {
    Stop();
    return api::OnException("im_start", nullptr);
  }
  if (st != IM_OK) {
    Stop(); // nothing runs yet but the encoders and the mixer: release them
    return st;
  }
  log::Info("session", "started",
            {{"layers", layer_count_},
             {"capture", CapturePathName(info_.capture_path)},
             {"encoder", layer_count_ > 0 ? encpolicy::EncoderToken(layers_[0].kind) : std::string_view()},
             {"profile", profile_},
             {"audio_mode", audio_mode_},
             {"device", device_}});
  return IM_OK;
}

im_status Session::StartVideo(const im_start_config &cfg) {
  for (uint32_t l = 0; l < layer_count_; ++l) {
    const im_layer_config &lc = cfg.layers[l];
    if (lc.bitrate_bps != 0 && !ValidLayerBitrate(l, lc.bitrate_bps)) {
      return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_start: cfg->layers[%u].bitrate_bps %u is outside the layer's range",
                        static_cast<unsigned>(l), static_cast<unsigned>(lc.bitrate_bps));
    }
    Layer &L = layers_[l];
    std::memcpy(L.rid, lc.rid, sizeof L.rid);
    L.max_pixels = lc.max_pixels;
    L.box_width = lc.width;
    L.box_height = lc.height;
    L.fps = lc.fps;
    L.gop_ms = lc.gop_ms;
    L.next_flash = epoch_;
    L.out = video::ComputeOutputSize(video::Size{source_.width, source_.height},
                                     video::LayerBounds{L.max_pixels, L.box_width, L.box_height});
    if (L.out.size.width == 0 || L.out.size.height == 0) {
      return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_start: the source has no size (%ux%u)",
                        static_cast<unsigned>(source_.width), static_cast<unsigned>(source_.height));
    }
    const uint32_t bitrate =
        lc.bitrate_bps != 0 ? lc.bitrate_bps : (l == 0 ? encpolicy::kMinBitrateLayer0 * 40 : 300'000u);
    L.planner.emplace(bitrate, false); // the live flag follows the encoder that opens (OnOpened)
    video_rings_[l] = std::make_unique<ring::PacketRing>(ring::VideoRingConfig(l), &signal_);
    pump_.AddStream(video_rings_[l].get(), IM_PACKET_VIDEO, l);
    L.norm.Reserve(ring::VideoRingConfig(l).arena_bytes / 2);
  }

  catalog_ = EncoderCatalog();
  std::vector<im_encoder_info> probe;
  probe.reserve(catalog_.size());
  for (const EncoderEntry &e : catalog_) {
    probe.push_back(e.info);
  }
  encpolicy::CandidateQuery q;
  q.capture_path = source_.capture_path;
  q.capture_adapter = CaptureAdapterIndex(source_);
  q.forced = cfg.encoder;
  q.test_source = source_.test;
  candidates_.resize(std::max<std::size_t>(probe.size(), 1));
  candidates_.resize(encpolicy::BuildCandidates(probe, q, candidates_));
  const bool forced = cfg.encoder != IM_ENC_AUTO;
  log::Debug("session", "candidates",
             {{"count", candidates_.size()},
              {"first", candidates_.empty() ? std::string_view() : encpolicy::EncoderToken(candidates_[0].kind)},
              {"capture_adapter",
               q.capture_adapter == encpolicy::kNoAdapter ? int64_t{-1} : static_cast<int64_t>(q.capture_adapter)},
              {"forced", forced}});
  if (candidates_.empty()) {
    return abi::Failf(IM_ERR_NO_ENCODER, forced ? abi::token::kForcedFailed : abi::token::kLayerFailed,
                      "im_start: no encoder %s (layer %.*s)",
                      forced ? "of the forced kind is available" : "is available",
                      static_cast<int>(RidView(layers_[0].rid).size()), RidView(layers_[0].rid).data());
  }
  if (const im_status st = StartFrames(); st != IM_OK) {
    return st;
  }

  const encpolicy::Selection sel = encpolicy::SelectEncoders(candidates_, profile_, layer_count_, forced, opener_);
  if (sel.status != IM_OK) {
    const uint32_t failed = std::min<uint32_t>(sel.failed_layer, layer_count_ - 1);
    for (uint32_t l = 0; l < layer_count_; ++l) {
      layers_[l].enc.reset();
    }
    return abi::Failf(sel.status, sel.token, "im_start: no encoder opened for layer %.*s",
                      static_cast<int>(RidView(layers_[failed].rid).size()), RidView(layers_[failed].rid).data());
  }
  profile_ = sel.profile;

  std::array<pace::LayerConfig, IM_MAX_LAYERS> pcfg{};
  for (uint32_t l = 0; l < layer_count_; ++l) {
    Layer &L = layers_[l];
    // The planner learns the encoder's live flag; the first open doesn't count for the re-init spacing (§9.5).
    L.planner.emplace(L.planner->target_bitrate(), (L.enc_flags & IM_ENCF_LIVE_BITRATE) != 0);
    UpdateLayerInfo(l);
    pcfg[l].fps = L.fps;
    pcfg[l].gop_ms = L.gop_ms;
    pcfg[l].active = (cfg.layers[l].flags & IM_LAYER_INACTIVE) == 0;
    pcfg[l].hardware = (L.enc_flags & IM_ENCF_HARDWARE) != 0;
  }
  pacer_.emplace(epoch_, std::span<const pace::LayerConfig>(pcfg.data(), layer_count_));
  PublishStats(epoch_); // im_get_stats names the encoders from the start; the video thread refreshes it

  if (const im_status st = StartCapture(cfg); st != IM_OK) {
    return st;
  }
  // §7.1: no OS border for the test pattern, and for a capture only when it runs borderless (WGC that Windows let
  // drop the border; README WA60's DD always).
  if (!capture_ || capture_->borderless()) {
    info_.flags |= IM_SESS_NO_OS_INDICATOR;
  }
  return IM_OK;
}

im_status Session::StartCapture(const im_start_config &cfg) {
  if (!capture::Captures(source_)) {
    return IM_OK; // the supervisor's test pattern
  }
  capture::CaptureOptions o;
  o.hide_cursor = (cfg.flags & IM_START_HIDE_CURSOR) != 0; // §7.8: drawn unless asked
  o.keep_border = (cfg.flags & IM_START_KEEP_BORDER) != 0;
  o.fps = layers_[0].fps; // §7.3: MinUpdateInterval from layer 0's fps
  if (const im_status st = capture::StartSourceCapture(source_, frames_->slots(), o, &sink_, &capture_); st != IM_OK) {
    return st;
  }
  // The source may have changed size between the pick and the start: the frames carry the new size, so it is a resize
  // like any other (SOURCE_RESIZED at once, the re-init once it is stable).
  const video::Size at = capture_->size();
  if (at.width != 0 && at.height != 0 && !(at == src_size_)) {
    reported_size_.store(capture::PackSize(at), std::memory_order_relaxed);
    resize_reported_.store(true, std::memory_order_release);
  }
  watcher_.emplace(src_size_, capture_->window() != 0, epoch_);
  // The path that runs, which can differ from the pick's (README WA60): im_session_info and the stats report it.
  {
    std::lock_guard<std::mutex> lock(info_mu_);
    info_.capture_path = capture_->capture_path();
  }
  log::Info("session", "capture started",
            {{"path", CapturePathName(info_.capture_path)},
             {"borderless", capture_->borderless()},
             {"window", capture_->window() != 0},
             {"w", at.width},
             {"h", at.height},
             {"fps", o.fps},
             {"cursor", !o.hide_cursor}});
  return IM_OK;
}

im_status Session::StartFrames() {
  // The GPU path (§8; README WF2) when the session's first candidate takes pixels: one device per session, on that
  // candidate's adapter (§8.1), created before any encoder opens, since FFmpeg wraps it (§9.2). Its later candidates
  // use the same device (README WA44 decides a cross-adapter one's copy, §9.1 step 3). A session whose first
  // candidate takes nothing (the test hooks' IM_ENC_FAKE on a test: source, which the catalog lists
  // before the probe's real encoders) gets no device: it stays off WARP like a session of fakes only (frames.h), and
  // a later candidate that takes pixels fails to open in it (open_failed), so the fallback moves on.
  // A source whose capture writes into slots (a WGC pick, README WF12) keeps the GPU path all the same: on the first
  // candidate that takes pixels, or on the default adapter when none does (an IM_ENC_FAKE session under test hooks).
  // A Desktop Duplication source (README WA60) puts the device on its output's adapter whatever the candidates say
  // (§7.4, §8.1: DD duplicates an output only on its own adapter); an encoder of another adapter is then skipped
  // (open_failed, other_adapter) and a software one reads back from this device.
  const bool need_slots = capture::NeedsSlots(source_);
  const EncoderEntry *pixels = nullptr;
  for (const encpolicy::Candidate &c : candidates_) {
    if (c.probe_index >= catalog_.size()) {
      continue;
    }
    if (catalog_[c.probe_index].input == FrameInput::kNv12) {
      pixels = &catalog_[c.probe_index];
      break;
    }
    if (!need_slots) {
      break; // only the first candidate decides
    }
  }
  if (pixels != nullptr || need_slots) {
    gpu::DeviceRequest request = pixels != nullptr ? pixels->device : gpu::DeviceRequest{};
    if (const uint64_t luid = capture::OutputAdapterLuid(source_); luid != 0) {
      request.adapter = gpu::DeviceRequest::Adapter::kLuid;
      request.luid = luid;
    }
    if (const im_status st = gpu::Device::Create(request, &gpu_); st != IM_OK) {
      return st;
    }
  }
  std::array<VideoFrames::LayerSetup, IM_MAX_LAYERS> setup{};
  for (uint32_t l = 0; l < layer_count_; ++l) {
    setup[l].out = layers_[l].out;
    setup[l].fps = layers_[l].fps;
  }
  VideoFrames::Config fc;
  fc.source = video::Size{source_.width, source_.height};
  fc.pattern = source_.capture_path == IM_CAPTURE_TEST;
  fc.layers = std::span<const VideoFrames::LayerSetup>(setup.data(), layer_count_);
  if (const im_status st = VideoFrames::Create(gpu_.get(), fc, &frames_); st != IM_OK) {
    return st;
  }
  static std::atomic<uint64_t> devices{0};
  device_ = devices.fetch_add(1, std::memory_order_relaxed) + 1; // every session opens its own device (§8.1)
  gpu_path_ = gpu_ != nullptr;
  log::Info("session", "frames", {{"gpu", gpu_path_}, {"w", source_.width}, {"h", source_.height}});
  return IM_OK;
}

im_status Session::StartAudio(const im_start_config &cfg, uint32_t init_flags) {
  if (audio_mode_ == IM_AUDIO_MODE_OFF) {
    return IM_OK;
  }
  opus_ring_ = std::make_unique<ring::PacketRing>(ring::kOpusRingConfig, &signal_);
  opus_writer_ = std::make_unique<RingWriter>(opus_ring_.get());
  pump_.AddStream(opus_ring_.get(), IM_PACKET_AUDIO, IM_NO_LAYER);
  const bool tap = (cfg.audio.flags & IM_AUDIOF_TAP_PCM) != 0;
  if (tap) {
    pcm_ring_ = std::make_unique<ring::PacketRing>(ring::kPcmRingConfig, &signal_);
    pcm_writer_ = std::make_unique<RingWriter>(pcm_ring_.get());
    pump_.AddStream(pcm_ring_.get(), IM_PACKET_PCM, IM_NO_LAYER);
  }
  audio::OpusStageConfig sc;
  sc.epoch_ns = epoch_;
  sc.bitrate_bps = cfg.audio.bitrate_bps;
  sc.frame_ms = cfg.audio.frame_ms;
  sc.flags = cfg.audio.flags;
  sc.audio = opus_writer_.get();
  sc.pcm = pcm_writer_.get();
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // The tone's packets carry M1's audio Marker under test hooks with IM_ENC_FAKE (04 §10.3, §18.2).
  sc.marker = audio_mode_ == IM_AUDIO_MODE_TEST_TONE && layer_count_ > 0 && layers_[0].kind == IM_ENC_FAKE;
#endif
  if (const im_status st = audio::OpusStage::Create(sc, &stage_); st != IM_OK) {
    return st;
  }

  if (audio_mode_ == IM_AUDIO_MODE_TEST_TONE) {
    // The first chunk at epoch_ns + the encoder's lookahead: the packets are stamped epoch_ns + k × 20 ms and the
    // beeps start on packet boundaries (04 §10.3).
    tone_ = std::make_unique<audio::TestToneSource>(epoch_, epoch_ + stage_->lookahead_ns(), stage_.get());
    audio_state_ = IM_AUDIO_STATE_OK;
    info_.flags |= IM_SESS_AUDIO;
    return IM_OK;
  }

  // IM_AUDIO_MODE_EXCLUDE_APPS: 05's mixer feeds the stage (04 §10.2). After IM_INIT_NO_AUDIO the engine touches no
  // audio API: session.cpp's Start refuses the mode before anything opens (README §3 item 35); this is its backstop.
  // Otherwise audio never fails a share (05 §4.3): without a mixer the session runs without audio packets and says so
  // (audio state off).
  if ((init_flags & IM_INIT_NO_AUDIO) != 0) {
    return abi::Fail(IM_ERR_BAD_STATE, {}, "im_start: audio is off (IM_INIT_NO_AUDIO)");
  }
  audio::AudioEngine &ae = audio_bridge::Engine();
  if (cfg.exclusions != nullptr) {
    if (const im_status st = ae.SetRules(cfg.exclusions); st != IM_OK) {
      log::Warn("session", "audio rules not applied", {{"status", st}, {"token", DetailToken(abi::LastErrorText())}});
      abi::ClearLastError();
    }
  }
  std::unique_ptr<audio::Mixer> mixer = ae.NewMixer();
  if (!mixer) {
    log::Warn("session", "no audio mixer: the share runs without audio");
    audio_state_ = IM_AUDIO_STATE_OFF;
    return IM_OK;
  }
  audio::MixerConfig mc{audio_mode_, cfg.audio.flags, stage_.get()};
  if (const im_status st = mixer->Start(mc); st != IM_OK) {
    log::Warn("session", "audio mixer did not start", {{"status", st}, {"token", DetailToken(abi::LastErrorText())}});
    abi::ClearLastError();
    audio_state_ = IM_AUDIO_STATE_OFF;
    return IM_OK;
  }
  mixer_ = std::move(mixer);
  info_.flags |= IM_SESS_AUDIO;
  return IM_OK;
}

template <class F> void Session::Launch(Thread &t, F &&body) {
  auto self = shared_from_this();
  t.th = std::thread([self, &t, body = std::forward<F>(body)]() mutable noexcept {
    api::Guard("session thread", [&] { body(); });
    {
      std::lock_guard<std::mutex> lock(self->exit_mu_);
      t.done.store(true, std::memory_order_release);
    }
    self->exit_cv_.notify_all();
  });
  t.started = true;
}

void Session::StartThreads() {
  Launch(supervisor_thread_, [this] { SupervisorLoop(); });
  if (has_video_) {
    Launch(video_thread_, [this] { VideoLoop(); });
  }
  if (tone_) {
    Launch(tone_thread_, [this] { ToneLoop(); });
  }
}

// ------------------------------------------------------------------------------------------- info, next, stats

void Session::FillInfo(im_session_info &info) const {
  {
    std::lock_guard<std::mutex> lock(info_mu_);
    info = info_;
  }
  info.struct_size = sizeof(im_session_info);
  info.audio_state = mixer_ ? mixer_->State() : audio_state_;
}

im_status Session::Next(uint32_t timeout_ms, im_packet &out) {
  std::lock_guard<std::mutex> consumer(consumer_mu_);
  ring::Packet p;
  const im_status st = pump_.Next(timeout_ms, &p);
  if (st != IM_OK) {
    return st;
  }
  out.kind = p.kind;
  out.layer = p.layer;
  out.flags = p.flags;
  out.capture_ts_ns = p.capture_ts_ns;
  out.data = p.data;
  out.size = p.size;
  out.duration_us = p.duration_us;
  out.width = p.width;
  out.height = p.height;
  out.profile = p.profile;
  out.event = p.event;
  out.status = p.status;
  return IM_OK;
}

im_status Session::Stats(std::string &out) const {
  StatsSnapshot s;
  {
    std::lock_guard<std::mutex> lock(stats_mu_);
    s = stats_;
  }
  s.layer_count = layer_count_;
  s.capture_path = CapturePathName(has_video_ ? info_.capture_path : 0);
  s.source_width = has_video_ ? source_.width : 0;
  s.source_height = has_video_ ? source_.height : 0;
  s.frames = frames_captured_.load(std::memory_order_relaxed);
  s.paused = paused_.load(std::memory_order_acquire);
  for (uint32_t l = 0; l < layer_count_; ++l) {
    const ring::RingStats rs = video_rings_[l]->Stats();
    s.layers[l].drops_ring = rs.ring_drops;
    s.layers[l].drops_stale = rs.stale_drops;
    std::memcpy(s.layers[l].rid, layers_[l].rid, sizeof s.layers[l].rid);
  }
  s.audio_state = AudioStateName(mixer_ ? mixer_->State() : audio_state_);
  if (stage_) {
    const audio::OpusStageStats as = stage_->stats();
    s.audio_packets = as.packets;
    s.audio_drops = as.drops + (opus_ring_ ? opus_ring_->Stats().stale_drops : 0);
    s.audio_bitrate_bps = as.bitrate_bps;
  }
  out = StatsJson(s);
  return IM_OK;
}

// ---------------------------------------------------------------------------------------------------- controls

im_status Session::Push(const Command &c) {
  if (ended_.load(std::memory_order_acquire)) {
    return IM_OK; // ENDED is queued: the call succeeds and changes nothing (the host stops the session next)
  }
  {
    std::lock_guard<std::mutex> lock(cmd_mu_);
    if (cmd_count_ == cmds_.size()) {
      return abi::Fail(IM_ERR_LIMIT, {}, "session: the command queue is full (64 commands not yet applied)");
    }
    cmds_[(cmd_head_ + cmd_count_) % cmds_.size()] = c;
    ++cmd_count_;
  }
  video_waiter_.Wake();
  return IM_OK;
}

im_status Session::RequestKeyframe(uint32_t layer) {
  if (!has_video_) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_request_keyframe: an audio-only session has no video layer");
  }
  if (layer == IM_ALL_LAYERS) {
    keyframe_bits_.fetch_or((1u << layer_count_) - 1, std::memory_order_acq_rel);
  } else {
    if (const im_status st = CheckLayer("im_request_keyframe", layer); st != IM_OK) {
      return st;
    }
    keyframe_bits_.fetch_or(1u << layer, std::memory_order_acq_rel);
  }
  video_waiter_.Wake();
  return IM_OK;
}

im_status Session::SetBitrate(uint32_t layer, uint32_t bps) {
  if (const im_status st = CheckLayer("im_set_bitrate", layer); st != IM_OK) {
    return st;
  }
  Command c;
  c.type = Command::Type::kBitrate;
  c.layer = layer;
  c.value = bps;
  return Push(c);
}

im_status Session::SetLayerActive(uint32_t layer, bool active) {
  if (const im_status st = CheckLayer("im_set_layer_active", layer); st != IM_OK) {
    return st;
  }
  Command c;
  c.type = Command::Type::kLayerActive;
  c.layer = layer;
  c.value = active ? 1u : 0u;
  return Push(c);
}

im_status Session::UpdateLayer(uint32_t layer, const im_layer_config &cfg) {
  if (const im_status st = CheckLayer("im_update_layer", layer); st != IM_OK) {
    return st;
  }
  if (std::strncmp(cfg.rid, layers_[layer].rid, sizeof cfg.rid) != 0) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_update_layer: cfg->rid is not layer %u's rid",
                      static_cast<unsigned>(layer));
  }
  if (cfg.bitrate_bps != 0 && !ValidLayerBitrate(layer, cfg.bitrate_bps)) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_update_layer: cfg->bitrate_bps %u is outside layer %u's range",
                      static_cast<unsigned>(cfg.bitrate_bps), static_cast<unsigned>(layer));
  }
  Command c;
  c.type = Command::Type::kUpdateLayer;
  c.layer = layer;
  c.cfg = cfg;
  return Push(c);
}

im_status Session::SetProfile(uint32_t profile) {
  if (!has_video_) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_set_profile: an audio-only session has no video layer");
  }
  Command c;
  c.type = Command::Type::kProfile;
  c.value = profile;
  return Push(c);
}

im_status Session::SetContentHint(uint32_t hint) {
  if (!has_video_) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_set_content_hint: an audio-only session has no video layer");
  }
  Command c;
  c.type = Command::Type::kContentHint;
  c.value = hint;
  return Push(c);
}

im_status Session::SetPaused(bool paused) {
  const bool old = paused_.exchange(paused, std::memory_order_acq_rel);
  // Audio first: the effective mute is the user's mute OR pause (04 §8.5); the tone fades in 10 ms.
  if (tone_) {
    tone_->tone().SetPaused(paused);
  }
  if (mixer_) {
    mixer_->SetPaused(paused);
  }
  if (!has_video_) {
    return IM_OK;
  }
  Command c;
  c.type = Command::Type::kPaused;
  c.value = paused ? 1u : 0u;
  const im_status st = Push(c);
  if (st != IM_OK) {
    // A failed call changes nothing (IM_ERR_LIMIT: the queue is full): the flag, the audio and the pacer agree again.
    paused_.store(old, std::memory_order_release);
    if (tone_) {
      tone_->tone().SetPaused(old);
    }
    if (mixer_) {
      mixer_->SetPaused(old);
    }
  }
  return st;
}

bool Session::SetToneMuted(bool muted) noexcept {
  if (!tone_) {
    return false;
  }
  // Its own flag beside the pause (04 §8.5): effective mute = user mute OR pause, each cleared only by its own call.
  tone_->tone().SetMuted(muted);
  return true;
}

im_status Session::SetAudioBitrate(uint32_t bps) {
  if (!stage_) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_set_audio_bitrate: the session has no audio");
  }
  return stage_->SetBitrate(bps);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
im_status Session::Inject(uint32_t layer, InjectedFault fault) {
  if (fault == InjectedFault::kDeviceRemoved) {
    if (!has_video_) {
      return abi::Fail(IM_ERR_INVALID_ARG, {}, "inject: an audio-only session has no device");
    }
    device_removed_.store(true, std::memory_order_release);
  } else {
    if (const im_status st = CheckLayer("inject", layer); st != IM_OK) {
      return st;
    }
    faults_[layer].store(kFaultEncodeError, std::memory_order_release);
  }
  video_waiter_.Wake();
  return IM_OK;
}
#endif

// ------------------------------------------------------------------------------------------------- the video thread

int64_t Session::NextDeadline() const noexcept {
  int64_t at = pacer_ ? pacer_->NextWakeNs() : kNever;
  for (uint32_t l = 0; l < layer_count_; ++l) {
    if (layers_[l].planner) {
      at = std::min(at, layers_[l].planner->NextDeadlineNs());
    }
  }
  at = std::min(at, next_stats_);
  if (retry_at_ != kNever) {
    // A full ring's IDR waits for room (§6.2): the pacer keeps asking for it, so poll every kRoomRetryNs rather than
    // spin. New frames still wake the thread.
    at = std::max(at, retry_at_);
  }
  return at;
}

void Session::VideoLoop() noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni video", engine::os::ThreadClass::kCapture);
  next_stats_ = QpcNs() + kStatsEveryNs;
  while (!stopping_.load(std::memory_order_acquire)) {
    const int64_t now = QpcNs();
    if (!video_ended_) {
      api::Guard("session video", [&] {
        if (device_removed_.load(std::memory_order_acquire)) {
          // 04 §9.8: a removed or reset device ends the session at once (no recovery in M2); the probe's cache is
          // stale and the next start opens a new device.
          EndDeviceRemoved("test hooks");
          return;
        }
        CountCaptured(now);
        ApplyCommands(now);
        ApplySourceSize();
        ApplyRingRequests();
        if (!pacer_->paused() && !paused_.load(std::memory_order_acquire)) {
          // While paused the slots keep the frozen frame (§8.5): new frames wait in them, the newest is taken on
          // resume. paused_ is set by im_set_paused itself, so no wake-up that checks it after the call takes a
          // frame, also one that was already past ApplyCommands when the call queued its command; a wake-up already
          // past this check may take one more frame, published no later than its TakeFrame. Resume still waits for
          // the command (Apply's kPaused takes the newest frame then, uncounted).
          TakeFrame(now, true);
        }
        for (uint32_t l = 0; l < layer_count_ && !ended_.load(std::memory_order_acquire); ++l) {
          if (auto a = layers_[l].planner->Poll(now)) {
            Reinit(l, *a, now);
          }
        }
        if (now >= retry_at_) {
          retry_at_ = kNever;
        }
        std::array<pace::Encode, IM_MAX_LAYERS> encodes{};
        const std::size_t n = pacer_->Poll(now, encodes);
        for (std::size_t i = 0; i < n && !ended_.load(std::memory_order_acquire); ++i) {
          EncodeOne(encodes[i], now);
        }
        PublishLimits();
        if (now >= next_stats_) {
          PublishStats(now);
          next_stats_ = now + kStatsEveryNs;
        }
      });
      if (ended_.load(std::memory_order_acquire)) {
        video_ended_ = true; // ENDED is queued: no more media (the session waits for im_stop)
        ReleaseEncoders();
      }
    }
    video_waiter_.WaitUntil(video_ended_ ? engine::os::Waiter::kNoDeadline : NextDeadline());
  }
  engine::os::LeaveThread(token);
}

void Session::ApplyCommands(int64_t now) {
  for (;;) {
    Command c;
    {
      std::lock_guard<std::mutex> lock(cmd_mu_);
      if (cmd_count_ == 0) {
        break;
      }
      c = cmds_[cmd_head_];
      cmd_head_ = (cmd_head_ + 1) % cmds_.size();
      --cmd_count_;
    }
    Apply(c, now);
  }
  const uint32_t bits = keyframe_bits_.exchange(0, std::memory_order_acq_rel);
  for (uint32_t l = 0; l < layer_count_; ++l) {
    if ((bits & (1u << l)) != 0) {
      pacer_->RequestKeyframe(l);
    }
  }
}

void Session::Apply(const Command &c, int64_t now) {
  switch (c.type) {
  case Command::Type::kBitrate: {
    Layer &L = layers_[c.layer];
    switch (L.planner->SetBitrate(c.value)) {
    case encpolicy::BitrateDecision::kLive:
      if (L.enc) {
        L.enc->SetBitrate(c.value);
      }
      UpdateLayerInfo(c.layer);
      break;
    case encpolicy::BitrateDecision::kPending: // applied by the next allowed re-init (10 s spacing, §9.5)
    case encpolicy::BitrateDecision::kNoop:
      break;
    }
    break;
  }
  case Command::Type::kLayerActive:
    pacer_->SetLayerActive(c.layer, c.value != 0, now);
    break;
  case Command::Type::kUpdateLayer: {
    Layer &L = layers_[c.layer];
    const im_layer_config &cfg = c.cfg;
    uint32_t reasons = 0;
    if (cfg.fps != L.fps) {
      L.fps = cfg.fps;
      reasons |= encpolicy::reinit::kFps;
    }
    if (cfg.max_pixels != L.max_pixels || cfg.width != L.box_width || cfg.height != L.box_height) {
      L.max_pixels = cfg.max_pixels;
      L.box_width = cfg.width;
      L.box_height = cfg.height;
      const video::OutputSize out =
          video::ComputeOutputSize(src_size_, video::LayerBounds{L.max_pixels, L.box_width, L.box_height});
      if (!(out.size == L.out.size)) {
        reasons |= encpolicy::reinit::kSize; // no re-init when the output size stays (§7.7)
      }
    }
    if (cfg.gop_ms != L.gop_ms) {
      L.gop_ms = cfg.gop_ms;
      pacer_->SetGop(c.layer, cfg.gop_ms);
    }
    if (reasons != 0) {
      L.planner->Request(reasons);
    }
    if (cfg.bitrate_bps != 0) {
      // With a re-init pending, a re-init encoder's new bitrate rides along with it (§9.5).
      if (L.planner->SetBitrate(cfg.bitrate_bps) == encpolicy::BitrateDecision::kLive && L.enc) {
        L.enc->SetBitrate(cfg.bitrate_bps);
        UpdateLayerInfo(c.layer);
      }
    }
    break;
  }
  case Command::Type::kProfile: {
    // §9.6: one profile for every layer. The session moves only when every layer's encoder can encode the asked
    // profile; otherwise it stays (High asked of a session that runs a CB-only encoder on some layer, OpenH264, stays
    // CB), im_set_profile has returned IM_OK, and the packets' profile tells 01 the truth.
    bool all = true;
    for (uint32_t l = 0; l < layer_count_; ++l) {
      all = all && encpolicy::Supports(candidates_[layers_[l].candidate], c.value);
    }
    if (!all) {
      log::Info("session", "profile kept", {{"asked", c.value}, {"profile", profile_}});
      break;
    }
    profile_ = c.value;
    for (uint32_t l = 0; l < layer_count_; ++l) {
      if (layers_[l].profile != profile_) {
        layers_[l].planner->Request(encpolicy::reinit::kProfile);
      }
    }
    break;
  }
  case Command::Type::kContentHint:
    if (c.value != content_hint_) { // the same hint again is a no-op (§9.5)
      content_hint_ = c.value;
      for (uint32_t l = 0; l < layer_count_; ++l) {
        layers_[l].planner->Request(encpolicy::reinit::kContentHint);
      }
    }
    break;
  case Command::Type::kPaused:
    if (c.value == 0 && pacer_->paused()) {
      // Resume (§8.5): the newest frame first, while the pacer still ignores frames, so the ones captured during the
      // pause count as neither seen nor dropped; the pacer then encodes it at once.
      TakeFrame(now, false);
    }
    pacer_->SetPaused(c.value != 0, now);
    break;
  }
}

void Session::ApplySourceSize() {
  if (!settle_pending_.exchange(false, std::memory_order_acq_rel)) {
    return;
  }
  const video::Size size = capture::UnpackSize(settled_size_.load(std::memory_order_acquire));
  if (size.width == 0 || size.height == 0) {
    return;
  }
  src_size_ = size;
  uint32_t reinit = 0;
  for (uint32_t l = 0; l < layer_count_; ++l) {
    Layer &L = layers_[l];
    // §7.7: only a layer whose output size changes re-inits (REINIT|KEYFRAME); the others keep fitting the content.
    if (capture::ReinitForSize(size, video::LayerBounds{L.max_pixels, L.box_width, L.box_height}, L.out.size)) {
      L.planner->Request(encpolicy::reinit::kSize);
      reinit |= 1u << l;
    }
  }
  log::Info("session", "source size settled", {{"w", size.width}, {"h", size.height}, {"reinit_layers", reinit}});
}

void Session::CountCaptured(int64_t now) noexcept {
  const uint64_t captured = frames_captured_.load(std::memory_order_relaxed);
  // The capture side wakes this thread for every frame, so the arrivals are recorded within a wake-up; more than the
  // meter holds can't be counted within its window anyway.
  const uint64_t arrived = std::min<uint64_t>(captured - captured_seen_, 1024);
  for (uint64_t i = 0; i < arrived; ++i) {
    captured_meter_.Add(now);
  }
  captured_seen_ = captured;
}

void Session::TakeFrame(int64_t now, bool count_skipped) {
  const VideoFrames::Taken t = frames_->Take();
  if (!t.fresh) {
    return;
  }
  const auto skipped = count_skipped ? static_cast<uint32_t>(std::min<uint64_t>(t.skipped, 0xFFFFFFFFu)) : 0u;
  pacer_->OnFrame(t.capture_ts_ns, now, skipped);
}

void Session::ApplyRingRequests() {
  for (uint32_t l = 0; l < layer_count_; ++l) {
    ring::PacketRing &r = *video_rings_[l];
    if (r.TakeKeyframeRequest()) {
      pacer_->RequestKeyframe(l); // a stale drop at dequeue (§6.2): coalesced like a PLI
    }
    Layer &L = layers_[l];
    if (r.NeedsKeyframe()) {
      if (!L.ring_idr_asked) {
        pacer_->RequireKeyframe(l); // the full ring dropped an AU: the next frame of the layer is an IDR (§6.2)
        L.ring_idr_asked = true;
      }
    } else {
      L.ring_idr_asked = false;
    }
  }
}

im_status Session::OpenLayer(uint32_t layer, const encpolicy::Candidate &c, uint32_t profile) {
  Layer &L = layers_[layer];
  if (c.probe_index >= catalog_.size() || catalog_[c.probe_index].open == nullptr) {
    return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "session: the candidate has no encoder");
  }
  L.out = video::ComputeOutputSize(src_size_, video::LayerBounds{L.max_pixels, L.box_width, L.box_height});
  EncoderParams p;
  p.layer = layer;
  p.rid = L.rid[0];
  p.kind = c.kind;
  p.profile = profile;
  p.width = L.out.size.width;
  p.height = L.out.size.height;
  p.fps = L.fps;
  p.bitrate_bps = L.planner ? L.planner->target_bitrate() : 0;
  p.gop_ms = L.gop_ms;
  p.content_hint = content_hint_;
  p.epoch_ns = epoch_;
  const EncoderEntry &entry = catalog_[c.probe_index];
  const FrameInput input = entry.input;
  if (input == FrameInput::kNv12) {
    if (!frames_ || !frames_->has_device()) {
      return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed, "session: the encoder takes pixels, no device");
    }
    p.device = frames_->device();
    if (entry.device.adapter == gpu::DeviceRequest::Adapter::kLuid && p.device->luid() != entry.device.luid) {
      // One device per session, on its first pixel candidate's adapter (§8.1): an encoder of another adapter would get
      // a device it can't use, and the copy across adapters (§9.1 step 3) isn't built, so the order moves on (a
      // software encoder reads back from any device). Until it is, Candidate::cross_adapter_copy goes unread here.
      log::Info("encode", "encoder skipped",
                {{"layer", RidView(L.rid)}, {"encoder", encpolicy::EncoderToken(c.kind)}, {"why", "other_adapter"}});
      return abi::Fail(IM_ERR_NO_ENCODER, abi::token::kOpenFailed,
                       "session: the encoder is on another adapter than the session's device");
    }
  }
  std::unique_ptr<VideoEncoder> enc;
  L.enc.reset(); // "replaces whatever the layer had": a session limit counts the old one otherwise
  im_status st = entry.open(p, &enc);
  if (st == IM_OK && input == FrameInput::kNv12) {
    // The layer's converter drops the old encoder's targets and follows a new output size or frame rate (a re-init,
    // §7.7, §9.5).
    st = frames_->Configure(layer, L.out, L.fps, src_size_);
    if (st != IM_OK) {
      enc.reset();
    }
  }
  if (st != IM_OK) {
    log::Info("encode", "encoder open failed",
              {{"layer", RidView(L.rid)}, {"encoder", encpolicy::EncoderToken(c.kind)}, {"status", st}});
    return st;
  }
  L.enc = std::move(enc);
  L.kind = c.kind;
  L.enc_flags = c.flags;
  L.input = input;
  L.profile = profile;
  for (std::size_t i = 0; i < candidates_.size(); ++i) {
    if (candidates_[i].probe_index == c.probe_index) {
      L.candidate = i;
      break;
    }
  }
  L.norm.Reset();
  L.reinit_next = true;
  log::Info("encode", "encoder opened",
            {{"layer", RidView(L.rid)},
             {"encoder", encpolicy::EncoderToken(c.kind)},
             {"w", p.width},
             {"h", p.height},
             {"fps", p.fps},
             {"bitrate", p.bitrate_bps},
             {"profile", profile}});
  return IM_OK;
}

void Session::OnOpened(uint32_t layer, int64_t now, bool count_reinit) {
  Layer &L = layers_[layer];
  const bool live = (L.enc_flags & IM_ENCF_LIVE_BITRATE) != 0;
  L.planner->OnReopened(now, L.planner->target_bitrate(), live);
  if (count_reinit) {
    ++L.reinits;
  }
  UpdateLayerInfo(layer);
}

void Session::UpdateLayerInfo(uint32_t layer) {
  const Layer &L = layers_[layer];
  im_layer_info li{};
  std::memcpy(li.rid, L.rid, sizeof li.rid);
  li.encoder = L.kind;
  li.width = L.out.size.width;
  li.height = L.out.size.height;
  li.fps = L.fps;
  li.bitrate_bps = L.planner ? L.planner->bitrate() : 0;
  li.profile = L.profile;
  li.flags = L.enc_flags;
  std::lock_guard<std::mutex> lock(info_mu_);
  info_.layers[layer] = li;
}

void Session::Reinit(uint32_t layer, const encpolicy::ReinitAction &a, int64_t now) {
  Layer &L = layers_[layer];
  // One profile for every layer (§9.6): a layer re-opens with the session profile when its encoder can encode it;
  // otherwise it stays, and its packets tell 01 the truth.
  uint32_t profile = profile_;
  if (!encpolicy::Supports(candidates_[L.candidate], profile)) {
    profile = L.profile;
  }
  const encpolicy::Candidate c = candidates_[L.candidate];
  const im_status st = OpenLayer(layer, c, profile);
  if (st == IM_ERR_ENCODER_LOST || (st != IM_OK && DeviceLost())) {
    // The converter's new geometry met a removed device, or the open failed on one (§8.1): no candidate can help
    // (§9.8).
    EndDeviceRemoved("re-init");
    return;
  }
  if (st != IM_OK) {
    const std::string detail(abi::LastErrorText());
    abi::ClearLastError();
    log::Warn("encode", "re-init failed", {{"layer", RidView(L.rid)}, {"status", st}, {"detail", detail}});
    Recover(layer, encpolicy::Failure::kReopenFailed, st, now);
    return;
  }
  if ((a.reasons & encpolicy::reinit::kFps) != 0) {
    pacer_->SetFps(layer, L.fps, now);
    if (layer == 0 && capture_) {
      capture_->SetFps(L.fps); // §7.3: MinUpdateInterval follows layer 0's fps
    }
  }
  pacer_->Reopen(layer, now);
  ++L.reinits;
  UpdateLayerInfo(layer);
  log::Info("encode", "re-init",
            {{"layer", RidView(L.rid)}, {"reasons", a.reasons}, {"bitrate", a.bitrate_bps}, {"profile", profile}});
}

void Session::Recover(uint32_t layer, encpolicy::Failure why, im_status cause, int64_t now) {
  Layer &L = layers_[layer];
  const uint32_t old_kind = L.kind;
  L.enc.reset();
  if (DeviceLost()) {
    EndDeviceRemoved("recover"); // the failure was the device's (§8.1): no candidate can help (§9.8)
    return;
  }
  const encpolicy::Recovery r =
      encpolicy::RecoverLayer(candidates_, L.candidate, layer, L.profile, why, cause, opener_);
  if (DeviceLost()) {
    // The device went while the candidates were tried: RecoverLayer takes their failures (a converter's
    // IM_ERR_ENCODER_LOST among them) for open failures, and a candidate that did open would fail next (§9.8).
    EndDeviceRemoved("recover");
    return;
  }
  abi::ClearLastError();
  switch (r.outcome) {
  case encpolicy::Recovery::Outcome::kSame:
    break;
  case encpolicy::Recovery::Outcome::kChanged:
    if (r.kind_changed) {
      // Queued before the new encoder's first packet (§9.8): im_next_packet hands out events first.
      const std::string doc = EventJson([&](json::Writer &w) {
        w.Key("layer").String(RidView(L.rid));
        w.Key("from").String(encpolicy::EncoderToken(old_kind));
        w.Key("to").String(encpolicy::EncoderToken(r.kind));
        w.Key("codec").String(encpolicy::EncoderCodec(r.kind));
        w.Key("reason").String(r.reason);
      });
      events_.Push(IM_EVENT_ENCODER_CHANGED, IM_ERR_ENCODER_LOST, layer, QpcNs(), doc);
      log::Warn("encode", "encoder changed",
                {{"layer", RidView(L.rid)},
                 {"from", encpolicy::EncoderToken(old_kind)},
                 {"to", encpolicy::EncoderToken(r.kind)},
                 {"reason", r.reason}});
    }
    pacer_->SetHardware(layer, (L.enc_flags & IM_ENCF_HARDWARE) != 0);
    break;
  case encpolicy::Recovery::Outcome::kExhausted: {
    char detail[96];
    std::snprintf(detail, sizeof detail, "no encoder left for layer %.*s", static_cast<int>(RidView(L.rid).size()),
                  RidView(L.rid).data());
    End(IM_ERR_ENCODER_LOST, "encoder_lost", detail);
    return;
  }
  }
  pacer_->Reopen(layer, now);
  OnOpened(layer, now, true);
}

im_status Session::ConvertFor(Layer &L, uint32_t layer, bool black, gpu::Nv12Target *target) {
  if (const im_status st = L.enc->NextTarget(target); st != IM_OK) {
    return st;
  }
  return frames_->Convert(layer, black, *target);
}

void Session::EncodeOne(const pace::Encode &e, int64_t now) {
  if (e.layer >= layer_count_) {
    return;
  }
  Layer &L = layers_[e.layer];
  if (!L.enc) {
    return;
  }
  ring::PacketRing &r = *video_rings_[e.layer];
  if (r.NeedsKeyframe()) {
    // The ring refuses everything but an IDR, and the IDR waits until there is room for it (§6.2).
    const uint32_t need = std::max<uint32_t>(L.last_idr_bytes + L.last_idr_bytes / 2, 64u << 10);
    if (!e.idr || !r.HasRoom(std::min<uint32_t>(need, r.config().arena_bytes / 2))) {
      pacer_->RequireKeyframe(e.layer);
      retry_at_ = std::min(retry_at_, now + kRoomRetryNs);
      return;
    }
  }
  // Every capture path goes through here: two AUs of a layer are at least one RTP tick apart (kMinAuSpacingNs). A
  // static IDR stamped now, followed by a new frame that arrived a little late, is the case the pacer's 1 µs misses.
  const int64_t ts = L.last_ts == 0 ? e.ts_ns : std::max(e.ts_ns, L.last_ts + kMinAuSpacingNs);
  EncodeInput in;
  in.ts_ns = ts;
  in.idr = e.idr || L.reinit_next;
  in.refresh = e.refresh;
  in.black = e.input == pace::Input::kLast && e.frame == 0;
  in.index = L.index;
  if (ts >= L.next_flash) {
    in.flash = true;
    while (L.next_flash <= ts) {
      L.next_flash += kFlashEveryNs;
    }
  }
  EncodedAu au;
  gpu::Nv12Target picture;
  const int64_t t0 = QpcNs();
  im_status st = IM_OK;
  if (L.input == FrameInput::kNv12) {
    // The pacer's frame is the reader's slot: the newest one taken (kLatest), or for kLast the layer's last frame
    // again, which is that slot too except while paused, when the slots keep the newest frame taken before the pause.
    st = ConvertFor(L, e.layer, in.black, &picture);
    in.picture = &picture;
  }
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (st == IM_OK && faults_[e.layer].exchange(kFaultNone, std::memory_order_acq_rel) == kFaultEncodeError) {
    st = abi::Fail(IM_ERR_INTERNAL, {}, "test hooks: an injected encode error");
  } else if (st == IM_OK) {
    st = L.enc->Encode(in, &au);
  }
#else
  if (st == IM_OK) {
    st = L.enc->Encode(in, &au);
  }
#endif
  const int64_t t1 = QpcNs();
  ++L.index;
  if (st == IM_ERR_ENCODER_LOST || (st != IM_OK && DeviceLost())) {
    // A TDR may first show up as an ordinary error (an FFmpeg pool's ENOMEM): with the device gone it is device loss
    // all the same (§8.1).
    EndDeviceRemoved("encode");
    return;
  }
  if (st != IM_OK) {
    log::Warn("encode", "encode error", {{"layer", RidView(L.rid)}, {"status", st}, {"detail", abi::LastErrorText()}});
    Recover(e.layer, encpolicy::Failure::kEncodeError, st, now);
    return;
  }
  L.last_ts = ts;
  L.au_times.Sent(ts, e.refresh);
  L.encode_ns.Add(t1 - t0);
  if (au.data.empty()) {
    return; // the encoder holds the frame: its AU comes with a later Encode, stamped with this frame's time
  }
  // 04 §9.2: the AU's own frame, which is an earlier one than the frame just sent when the encoder holds frames back
  // (EncodedAu::capture_ts_ns); its capture time and refresh flag go on the packet.
  const AuTimes::Stamp stamp = L.au_times.ForAu(au.capture_ts_ns, ts, e.refresh);
  h264::NormalizeOptions no;
  no.session_profile = L.profile;
  no.encoder_keyframe = au.keyframe ? 1 : 0;
  h264::AuInfo info;
  const std::span<const uint8_t> norm = L.norm.Normalize(au.data, no, &info);
  if (info.profile_mismatch) {
    log::Warn("encode", "profile mismatch", {{"layer", RidView(L.rid)}, {"profile", info.profile}});
    Recover(e.layer, encpolicy::Failure::kBadStream, IM_ERR_INTERNAL, now);
    return;
  }
  if (!info.has_vcl) {
    return;
  }
  ring::PacketMeta m;
  m.flags = (info.keyframe ? IM_PKT_KEYFRAME : 0u) | (stamp.refresh ? IM_PKT_REFRESH : 0u) |
            (L.reinit_next && info.keyframe ? IM_PKT_REINIT : 0u);
  m.capture_ts_ns = stamp.capture_ts_ns;
  m.width = info.width;
  m.height = info.height;
  m.profile = info.profile;
  if (r.Push(m, norm) != ring::PushResult::kOk) {
    return; // counted by the ring; DISCONTINUITY and the IDR follow (§6.2)
  }
  const int64_t pushed = QpcNs();
  L.au_times.Pushed(m.capture_ts_ns);
  if ((m.flags & IM_PKT_REINIT) != 0) {
    L.reinit_next = false;
  }
  if (info.keyframe) {
    ++L.keyframes;
    L.last_idr_bytes = static_cast<uint32_t>(norm.size());
    L.ring_idr_asked = false;
  }
  if (stamp.refresh) {
    ++L.refreshes;
  }
  L.bytes.Add(pushed, static_cast<uint32_t>(norm.size()));
  L.latency_ns.Add(pushed - m.capture_ts_ns);
}

void Session::PublishLimits() {
  std::array<pace::LimitChange, pace::Pacer::kMaxLimitChanges> changes{};
  const std::size_t n = pacer_->TakeLimitChanges(changes);
  for (std::size_t i = 0; i < n; ++i) {
    const pace::LimitChange &c = changes[i];
    if (c.layer < layer_count_) {
      limit_fps10_[c.layer].store(static_cast<uint32_t>(std::max(c.fps, 0.0) * 10.0 + 0.5), std::memory_order_relaxed);
      limit_[c.layer].store(static_cast<uint8_t>(c.limit), std::memory_order_release);
    }
  }
  if (n != 0) {
    supervisor_waiter_.Wake();
  }
}

void Session::PublishStats(int64_t now) {
  StatsSnapshot s;
  s.layer_count = layer_count_;
  // fpsIn: the captured frames over the last 2 s (the pacer's own count stops while paused, §8.5).
  const int64_t from = std::max(now - kStatsWindowNs, epoch_);
  const double window_s = static_cast<double>(now - from) / 1e9;
  const double fps_in = window_s > 0 ? static_cast<double>(captured_meter_.CountSince(from)) / window_s : 0.0;
  for (uint32_t l = 0; l < layer_count_; ++l) {
    const Layer &L = layers_[l];
    const pace::LayerStats ps = pacer_->Stats(l, now);
    LayerStatsSnapshot &o = s.layers[l];
    std::memcpy(o.rid, L.rid, sizeof o.rid);
    o.encoder = encpolicy::EncoderToken(L.kind);
    o.codec = encpolicy::EncoderCodec(L.kind);
    o.hw = (L.enc_flags & IM_ENCF_HARDWARE) != 0;
    o.width = L.out.size.width;
    o.height = L.out.size.height;
    o.active = ps.active;
    o.fps_in = fps_in; // every layer sees the same captured frames
    o.fps_out = ps.fps_out;
    o.bitrate_bps = L.bytes.BitsPerSecond(now);
    o.target_bps = L.planner->bitrate();
    o.keyframes = L.keyframes;
    o.refreshes = ps.refreshes;
    o.reinits = L.reinits;
    o.drops_pacer = ps.pacer_drops;
    o.encode_p50 = L.encode_ns.PercentileMs(50);
    o.encode_p95 = L.encode_ns.PercentileMs(95);
    o.encode_p99 = L.encode_ns.PercentileMs(99);
    o.latency_p50 = L.latency_ns.PercentileMs(50);
    o.latency_p95 = L.latency_ns.PercentileMs(95);
    o.limit = pace::LimitToken(ps.limit);
  }
  // The caller of im_get_stats never makes this thread wait: a busy lock skips one snapshot.
  std::unique_lock<std::mutex> lock(stats_mu_, std::try_to_lock);
  if (lock.owns_lock()) {
    stats_ = s;
  }
}

// ---------------------------------------------------------------------------------------------- the supervisor

bool Session::PublishPattern(const PatternGrid &grid, uint64_t k) {
  // Its capture time is its grid time, so the flashes land on epoch_ns + k × 1 s exactly.
  const int64_t ts = grid.FrameNs(k);
  const im_status st = frames_->PublishPattern(gpu::PatternFrame{k, ts - epoch_}, ts);
  if (st == IM_ERR_ENCODER_LOST || (st != IM_OK && DeviceLost())) {
    EndDeviceRemoved("test pattern");
    return false;
  }
  if (st != IM_OK) {
    const std::string detail(abi::LastErrorText());
    abi::ClearLastError();
    log::Error("session", "test pattern failed", {{"status", st}, {"detail", detail}});
    End(IM_ERR_INTERNAL, "internal", detail);
    return false;
  }
  frames_captured_.fetch_add(1, std::memory_order_relaxed);
  video_waiter_.Wake();
  return true;
}

void Session::SupervisorLoop() noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni supervisor", engine::os::ThreadClass::kNormal);
  // A source a capture area captures delivers its own frames (the sink); the others get the test pattern.
  const bool pattern =
      has_video_ && !capture_ && source_.capture_path == IM_CAPTURE_TEST && source_.pattern != Pattern::kNone;
  const PatternGrid grid{epoch_, source_.pattern_hz, source_.pattern_late_ns};
  uint64_t next_k = 0; // the next pattern frame
  bool pattern_done = !pattern;
  while (!stopping_.load(std::memory_order_acquire)) {
    const int64_t now = QpcNs();
    if (!pattern_done && (capture_stopped_.load(std::memory_order_acquire) || ended_.load(std::memory_order_acquire))) {
      pattern_done = true;
    }
    if (!pattern_done && now >= grid.DueNs(next_k)) {
      // The newest frame that is due; the ones the timer missed are skipped, as a capture source keeps only the newest.
      const uint64_t k = grid.NewestDue(now, next_k);
      api::Guard("session test pattern", [&] {
        if (!PublishPattern(grid, k)) {
          pattern_done = true; // the session ended
        }
      });
      next_k = k + 1;
      if (source_.pattern == Pattern::kStatic) {
        pattern_done = true; // a static screen: the initial frame only
      }
    }
    int64_t capture_due = kNever;
    api::Guard("session capture", [&] { capture_due = SuperviseCapture(now); });
    api::Guard("session supervisor", [&] { SuperviseEvents(now); });
    int64_t deadline = std::min(now + kSupervisorPollNs, capture_due);
    if (!pattern_done) {
      deadline = std::min(deadline, grid.DueNs(next_k));
    }
    supervisor_waiter_.WaitUntil(deadline);
  }
  engine::os::LeaveThread(token);
}

void Session::SuperviseEvents(int64_t now) {
  if (ended_.load(std::memory_order_acquire)) {
    return; // nothing after ENDED (the queue refuses it anyway)
  }
  for (uint32_t l = 0; l < layer_count_; ++l) {
    const auto lim = static_cast<pace::Limit>(limit_[l].load(std::memory_order_acquire));
    if (lim == reported_limit_[l]) {
      continue;
    }
    reported_limit_[l] = lim;
    const double fps = static_cast<double>(limit_fps10_[l].load(std::memory_order_relaxed)) / 10.0;
    const std::string doc = EventJson([&](json::Writer &w) {
      w.Key("layer").String(RidView(layers_[l].rid));
      w.Key("limit").String(pace::LimitToken(lim));
      w.Key("fps").Fixed(fps, 1);
    });
    events_.Push(IM_EVENT_LIMIT_CHANGED, IM_OK, l, now, doc);
    log::Info("session", "limit changed", {{"layer", RidView(layers_[l].rid)}, {"limit", pace::LimitToken(lim)}});
  }
  // RING_OVERFLOW: at most one per stream and second, with the drops since the last one.
  const uint32_t streams = layer_count_ + (opus_ring_ ? 1u : 0u) + (pcm_ring_ ? 1u : 0u);
  for (uint32_t i = 0; i < streams && i < ring::kMaxStreams; ++i) {
    ring::PacketRing *r =
        i < layer_count_ ? video_rings_[i].get() : (i == layer_count_ ? opus_ring_.get() : pcm_ring_.get());
    if (r == nullptr) {
      continue;
    }
    overflow_pending_[i] += r->TakeOverflowDrops();
    if (overflow_pending_[i] == 0 ||
        (overflow_sent_at_[i] != 0 && now - overflow_sent_at_[i] < kOverflowEventEveryNs)) {
      continue;
    }
    const uint64_t dropped = overflow_pending_[i];
    overflow_pending_[i] = 0;
    overflow_sent_at_[i] = now;
    const std::string_view name = StreamName(i);
    const std::string doc = EventJson([&](json::Writer &w) {
      w.Key("stream").String(name);
      w.Key("dropped").Uint(dropped);
    });
    events_.Push(IM_EVENT_RING_OVERFLOW, IM_OK, i < layer_count_ ? i : IM_NO_LAYER, now, doc);
    log::Warn("session", "ring overflow", {{"stream", name}, {"dropped", dropped}});
  }
  if (const uint64_t lost = events_.TakeDropped(); lost != 0) {
    log::Warn("session", "events dropped", {{"dropped", lost}});
  }
}

int64_t Session::SuperviseCapture(int64_t now) {
  if (!capture_ || !watcher_ || capture_halted_) {
    return kNever;
  }
  if (!ended_.load(std::memory_order_acquire)) {
    if (source_closed_.load(std::memory_order_acquire)) {
      // §7.3, §7.7: the window was closed or the monitor removed.
      End(IM_ERR_SOURCE_GONE, "source_gone", "the shared window was closed or the screen was disconnected");
    } else if (capture_failed_.load(std::memory_order_acquire)) {
      im_status st = IM_ERR_INTERNAL;
      std::string detail;
      {
        std::lock_guard<std::mutex> lock(failure_mu_);
        if (failure_status_ != IM_OK) {
          st = failure_status_;
        }
        detail = failure_detail_;
      }
      log::Warn("session", "capture failed", {{"status", st}, {"detail", detail}});
      if (st == IM_ERR_ENCODER_LOST || DeviceLost()) {
        EndDeviceRemoved("capture"); // §8.1, §9.8
      } else if (st == IM_ERR_SOURCE_GONE) {
        End(IM_ERR_SOURCE_GONE, "source_gone", detail);
      } else if (st == IM_ERR_PERMISSION_DENIED) {
        End(IM_ERR_PERMISSION_DENIED, "permission_denied", detail);
      } else {
        End(IM_ERR_INTERNAL, "internal", detail);
      }
    }
  }
  if (ended_.load(std::memory_order_acquire)) {
    // Nothing is captured once the session ended (the privacy-relevant part); im_stop frees the rest. This is not one
    // of the capture's threads, so its Stop may wait for a callback in flight.
    capture_->Stop();
    capture_halted_ = true;
    log::Info("session", "capture stopped", {{"why", "ended"}});
    return kNever;
  }

  std::optional<video::Size> reported;
  if (resize_reported_.exchange(false, std::memory_order_acq_rel)) {
    reported = capture::UnpackSize(reported_size_.load(std::memory_order_relaxed));
  }
  const capture::SourceWatcher::Step step = watcher_->Poll(reported, now, [&] { return capture_->minimized(); });
  if (step.resized) {
    // §5.6: SOURCE_RESIZED when the change is first seen; the re-init follows once the size is stable for 1 s.
    const video::Size sz = *step.resized;
    const std::string doc = EventJson([&](json::Writer &w) {
      w.Key("width").Uint(sz.width);
      w.Key("height").Uint(sz.height);
    });
    events_.Push(IM_EVENT_SOURCE_RESIZED, IM_OK, IM_NO_LAYER, now, doc);
    log::Info("session", "source resized", {{"w", sz.width}, {"h", sz.height}});
  }
  if (step.settled) {
    settled_size_.store(capture::PackSize(*step.settled), std::memory_order_release);
    settle_pending_.store(true, std::memory_order_release);
    video_waiter_.Wake();
  }
  if (step.minimize != capture::MinimizeTracker::Change::kNone) {
    const bool minimized = step.minimize == capture::MinimizeTracker::Change::kMinimized;
    events_.Push(minimized ? IM_EVENT_SOURCE_MINIMIZED : IM_EVENT_SOURCE_RESTORED, IM_OK, IM_NO_LAYER, now, "{}");
    log::Info("session", minimized ? "source minimized" : "source restored");
  }
  return watcher_->NextDueNs();
}

// ---------------------------------------------------------------------------------------------- the test tone

void Session::ToneLoop() noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni tone", engine::os::ThreadClass::kProAudio);
  while (!stopping_.load(std::memory_order_acquire) && !ended_.load(std::memory_order_acquire)) {
    const int64_t deadline = tone_->Pump(QpcNs());
    tone_waiter_.WaitUntil(deadline);
  }
  engine::os::LeaveThread(token);
}

// --------------------------------------------------------------------------------------------------- end, stop

void Session::End(im_status status, std::string_view reason, std::string_view detail) noexcept {
  if (ended_.exchange(true, std::memory_order_acq_rel)) {
    return;
  }
  capture_stopped_.store(true, std::memory_order_release);
  const std::string doc = EventJson([&](json::Writer &w) {
    w.Key("reason").String(reason);
    w.Key("detail").String(detail);
  });
  events_.Push(IM_EVENT_ENDED, status, IM_NO_LAYER, QpcNs(), doc);
  log::Warn("session", "ended", {{"status", status}, {"reason", reason}});
  supervisor_waiter_.Wake();
  tone_waiter_.Wake();
  video_waiter_.Wake(); // the video thread closes its encoders (ReleaseEncoders)
}

void Session::ReleaseEncoders() noexcept {
  // On the video thread, the encoders' only user once the session runs. §9.8: an ended session holds no encoder while
  // the host has yet to call im_stop (an NVENC session counts against the system's limit; a removed device's FFmpeg
  // contexts are dead); the frames and the device go with ReleaseVideo, once no thread can use them.
  uint32_t released = 0;
  for (uint32_t l = 0; l < layer_count_; ++l) {
    if (layers_[l].enc) {
      layers_[l].enc.reset();
      ++released;
    }
  }
  if (released != 0) {
    log::Info("session", "encoders released", {{"layers", released}});
  }
}

void Session::EndDeviceRemoved(std::string_view what) noexcept {
  // 04 §9.8: no recovery in M2; the probe's cache is stale and the next start opens a new device.
  log::Warn("session", "device removed", {{"where", what}, {"detail", abi::LastErrorText()}});
  abi::ClearLastError();
  MarkProbeCacheStale();
  End(IM_ERR_ENCODER_LOST, "encoder_lost", "device_removed: the encoder's device was removed or reset");
}

bool Session::DeviceLost() const noexcept {
  if (!gpu_) {
    return false; // the test hooks' IM_ENC_FAKE sessions: no device to lose
  }
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (g_device_removed_for_test.load(std::memory_order_acquire)) {
    return true;
  }
#endif
  return gpu_->Removed();
}

void Session::JoinThreads(int64_t deadline_ns) noexcept {
  {
    std::unique_lock<std::mutex> lock(exit_mu_);
    for (Thread *t : {&video_thread_, &supervisor_thread_, &tone_thread_}) {
      if (!t->started) {
        continue;
      }
      while (!t->done.load(std::memory_order_acquire)) {
        const int64_t left = deadline_ns - QpcNs();
        if (left <= 0) {
          break;
        }
        exit_cv_.wait_for(lock, std::chrono::nanoseconds(left));
      }
    }
  }
  for (Thread *t : {&video_thread_, &supervisor_thread_, &tone_thread_}) {
    if (!t->started || !t->th.joinable()) {
      continue;
    }
    if (t->done.load(std::memory_order_acquire)) {
      t->th.join();
    } else {
      // Stuck (a driver call): abandoned, never killed (§6.3). Its reference keeps the session alive.
      log::Error("session", "thread abandoned at stop", {{"bound_ms", kStopBoundNs / 1'000'000}});
      t->th.detach();
    }
  }
}

void Session::Stop() noexcept {
  if (stop_once_.exchange(true, std::memory_order_acq_rel)) {
    return;
  }
  const int64_t deadline = QpcNs() + kStopBoundNs;
  // Capture first (the privacy-relevant part, within one frame interval), then the consumer, then the mixer. Once
  // the capture's Stop returns it copies no frame and calls no sink; its slots and the device go in ReleaseVideo.
  if (capture_) {
    capture_->Stop();
  }
  capture_stopped_.store(true, std::memory_order_release);
  pump_.Stop(); // a blocked im_next_packet returns IM_ENDED within one wake-up
  if (mixer_) {
    api::Guard("session: Mixer::Stop", [&] { mixer_->Stop(); }); // ≤ 500 ms; no OnChunk after it
  }
  stopping_.store(true, std::memory_order_release);
  video_waiter_.Wake();
  supervisor_waiter_.Wake();
  tone_waiter_.Wake();
  JoinThreads(deadline);
  {
    std::lock_guard<std::mutex> consumer(consumer_mu_); // the consumer has returned
    pump_.Release();
  }
  ReleaseVideo();
  log::Info("session", "stopped", {{"device", device_}});
}

void Session::ReleaseVideo() noexcept {
  const auto gone = [](const Thread &t) {
    return !t.started || (t.done.load(std::memory_order_acquire) && !t.th.joinable());
  };
  if (!gone(video_thread_)) {
    return; // an abandoned video thread keeps the encoders, the frames and the device (the destructor frees them)
  }
  for (Layer &L : layers_) {
    L.enc.reset(); // the encoders go first: they hold the device (FFmpeg's hardware device wraps it, §9.2)
  }
  if (!gone(supervisor_thread_)) {
    return; // an abandoned supervisor may still draw the test pattern into the slots, or poll the capture
  }
  capture_.reset(); // stopped by Stop already; it holds the session's device through its frame pool
  log::Debug("session", "video released");
  frames_.reset();
  gpu_.reset();
}

} // namespace im::session
