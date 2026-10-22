#include "session/session_object.h"

#include <algorithm>
#include <chrono>
#include <cstdio>
#include <cstring>
#include <exception>
#include <utility>

#include "api/guard.h"
#include "audio_bridge/audio_bridge.h"
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
#endif

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
  q.capture_adapter = encpolicy::kNoAdapter;
  q.forced = cfg.encoder;
  q.test_source = source_.test;
  candidates_.resize(std::max<std::size_t>(probe.size(), 1));
  candidates_.resize(encpolicy::BuildCandidates(probe, q, candidates_));
  const bool forced = cfg.encoder != IM_ENC_AUTO;
  if (candidates_.empty()) {
    return abi::Failf(IM_ERR_NO_ENCODER, forced ? abi::token::kForcedFailed : abi::token::kLayerFailed,
                      "im_start: no encoder %s (layer %.*s)",
                      forced ? "of the forced kind is available" : "is available",
                      static_cast<int>(RidView(layers_[0].rid).size()), RidView(layers_[0].rid).data());
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

  static std::atomic<uint64_t> devices{0};
  device_ = devices.fetch_add(1, std::memory_order_relaxed) + 1; // every session opens its own device (§8.1)
  info_.flags |= IM_SESS_NO_OS_INDICATOR; // the test pattern: no OS border; WGC borderless and DD set it too (§7)
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

  // IM_AUDIO_MODE_EXCLUDE_APPS: 05's mixer feeds the stage (04 §10.2). Audio never fails a share (05 §4.3): without
  // a mixer the session runs without audio packets and says so (audio state off).
  if ((init_flags & IM_INIT_NO_AUDIO) != 0) {
    audio_state_ = IM_AUDIO_STATE_OFF;
    return IM_OK;
  }
  audio::AudioEngine &ae = audio_bridge::Engine();
  if (cfg.exclusions != nullptr) {
    if (const im_status st = ae.SetRules(cfg.exclusions); st != IM_OK) {
      log::Warn("session", "audio rules not applied", {{"status", st}, {"detail", abi::LastErrorText()}});
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
    log::Warn("session", "audio mixer did not start", {{"status", st}, {"detail", abi::LastErrorText()}});
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
  s.capture_path = CapturePathName(has_video_ ? source_.capture_path : 0);
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
  paused_.store(paused, std::memory_order_release);
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
  return Push(c);
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
          MarkProbeCacheStale();
          End(IM_ERR_ENCODER_LOST, "encoder_lost", "device_removed: the encoder's device was removed or reset");
          return;
        }
        ApplyCommands(now);
        ApplyRingRequests();
        const uint64_t seq = frame_seq_.load(std::memory_order_acquire);
        if (seq != seen_seq_) {
          const int64_t ts = frame_ts_[seq % kFrameSlots].load(std::memory_order_acquire);
          const auto skipped = static_cast<uint32_t>(std::min<uint64_t>(seq - seen_seq_ - 1, 0xFFFFFFFFu));
          seen_seq_ = seq;
          pacer_->OnFrame(ts, now, skipped);
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
      const video::OutputSize out = video::ComputeOutputSize(
          video::Size{source_.width, source_.height}, video::LayerBounds{L.max_pixels, L.box_width, L.box_height});
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
  case Command::Type::kProfile:
    profile_ = c.value;
    for (uint32_t l = 0; l < layer_count_; ++l) {
      if (layers_[l].profile != profile_) {
        layers_[l].planner->Request(encpolicy::reinit::kProfile);
      }
    }
    break;
  case Command::Type::kContentHint:
    if (c.value != content_hint_) { // the same hint again is a no-op (§9.5)
      content_hint_ = c.value;
      for (uint32_t l = 0; l < layer_count_; ++l) {
        layers_[l].planner->Request(encpolicy::reinit::kContentHint);
      }
    }
    break;
  case Command::Type::kPaused:
    pacer_->SetPaused(c.value != 0, now);
    break;
  }
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
  L.out = video::ComputeOutputSize(video::Size{source_.width, source_.height},
                                   video::LayerBounds{L.max_pixels, L.box_width, L.box_height});
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
  std::unique_ptr<VideoEncoder> enc;
  L.enc.reset(); // "replaces whatever the layer had": a session limit counts the old one otherwise
  const im_status st = catalog_[c.probe_index].open(p, &enc);
  if (st != IM_OK) {
    log::Info("encode", "encoder open failed",
              {{"layer", RidView(L.rid)}, {"encoder", encpolicy::EncoderToken(c.kind)}, {"status", st}});
    return st;
  }
  L.enc = std::move(enc);
  L.kind = c.kind;
  L.enc_flags = c.flags;
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
  if (st != IM_OK) {
    const std::string detail(abi::LastErrorText());
    abi::ClearLastError();
    log::Warn("encode", "re-init failed", {{"layer", RidView(L.rid)}, {"status", st}, {"detail", detail}});
    Recover(layer, encpolicy::Failure::kReopenFailed, st, now);
    return;
  }
  if ((a.reasons & encpolicy::reinit::kFps) != 0) {
    pacer_->SetFps(layer, L.fps, now);
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
  const encpolicy::Recovery r =
      encpolicy::RecoverLayer(candidates_, L.candidate, layer, L.profile, why, cause, opener_);
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
  EncodeInput in;
  in.ts_ns = e.ts_ns;
  in.idr = e.idr || L.reinit_next;
  in.refresh = e.refresh;
  in.black = e.input == pace::Input::kLast && e.frame == 0;
  in.index = L.index;
  if (e.ts_ns >= L.next_flash) {
    in.flash = true;
    while (L.next_flash <= e.ts_ns) {
      L.next_flash += kFlashEveryNs;
    }
  }
  EncodedAu au;
  const int64_t t0 = QpcNs();
  im_status st = IM_OK;
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  if (faults_[e.layer].exchange(kFaultNone, std::memory_order_acq_rel) == kFaultEncodeError) {
    st = abi::Fail(IM_ERR_INTERNAL, {}, "test hooks: an injected encode error");
  } else {
    st = L.enc->Encode(in, &au);
  }
#else
  st = L.enc->Encode(in, &au);
#endif
  const int64_t t1 = QpcNs();
  ++L.index;
  if (st == IM_ERR_ENCODER_LOST) {
    MarkProbeCacheStale();
    End(IM_ERR_ENCODER_LOST, "encoder_lost", "device_removed: the encoder's device was removed or reset");
    return;
  }
  if (st != IM_OK) {
    log::Warn("encode", "encode error", {{"layer", RidView(L.rid)}, {"status", st}, {"detail", abi::LastErrorText()}});
    Recover(e.layer, encpolicy::Failure::kEncodeError, st, now);
    return;
  }
  L.encode_ns.Add(t1 - t0);
  if (au.data.empty()) {
    return;
  }
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
  m.flags = (info.keyframe ? IM_PKT_KEYFRAME : 0u) | (e.refresh ? IM_PKT_REFRESH : 0u) |
            (L.reinit_next && info.keyframe ? IM_PKT_REINIT : 0u);
  m.capture_ts_ns = e.ts_ns;
  m.width = info.width;
  m.height = info.height;
  m.profile = info.profile;
  if (r.Push(m, norm) != ring::PushResult::kOk) {
    return; // counted by the ring; DISCONTINUITY and the IDR follow (§6.2)
  }
  const int64_t pushed = QpcNs();
  if ((m.flags & IM_PKT_REINIT) != 0) {
    L.reinit_next = false;
  }
  if (info.keyframe) {
    ++L.keyframes;
    L.last_idr_bytes = static_cast<uint32_t>(norm.size());
    L.ring_idr_asked = false;
  }
  if (e.refresh) {
    ++L.refreshes;
  }
  L.bytes.Add(pushed, static_cast<uint32_t>(norm.size()));
  L.latency_ns.Add(pushed - e.ts_ns);
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
    o.fps_in = ps.fps_in;
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

int64_t Session::PatternFrameNs(uint64_t k) const noexcept {
  const uint64_t hz = std::max<uint32_t>(source_.pattern_hz, 1);
  return epoch_ + static_cast<int64_t>(k / hz * 1'000'000'000ull + (k % hz) * 1'000'000'000ull / hz);
}

void Session::SupervisorLoop() noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni supervisor", engine::os::ThreadClass::kNormal);
  const bool pattern = has_video_ && source_.capture_path == IM_CAPTURE_TEST && source_.pattern != Pattern::kNone;
  const uint64_t hz = std::max<uint32_t>(source_.pattern_hz, 1);
  uint64_t next_k = 0; // the next pattern frame
  bool pattern_done = !pattern;
  while (!stopping_.load(std::memory_order_acquire)) {
    const int64_t now = QpcNs();
    if (!pattern_done && (capture_stopped_.load(std::memory_order_acquire) || ended_.load(std::memory_order_acquire))) {
      pattern_done = true;
    }
    if (!pattern_done && now >= PatternFrameNs(next_k)) {
      // The newest frame that is due; the ones the timer missed are skipped, as a capture source keeps only the
      // newest. Its capture time is its grid time, so the flashes land on epoch_ns + k × 1 s exactly.
      const auto elapsed = static_cast<uint64_t>(now - epoch_);
      uint64_t k = elapsed / 1'000'000'000ull * hz + (elapsed % 1'000'000'000ull) * hz / 1'000'000'000ull;
      while (k > next_k && PatternFrameNs(k) > now) {
        --k;
      }
      k = std::max(k, next_k);
      const uint64_t seq = frame_seq_.load(std::memory_order_relaxed) + 1;
      frame_ts_[seq % kFrameSlots].store(PatternFrameNs(k), std::memory_order_release);
      frame_seq_.store(seq, std::memory_order_release);
      frames_captured_.fetch_add(1, std::memory_order_relaxed);
      video_waiter_.Wake();
      next_k = k + 1;
      if (source_.pattern == Pattern::kStatic) {
        pattern_done = true; // a static screen: the initial frame only
      }
    }
    api::Guard("session supervisor", [&] { SuperviseEvents(now); });
    int64_t deadline = now + kSupervisorPollNs;
    if (!pattern_done) {
      deadline = std::min(deadline, PatternFrameNs(next_k));
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
  // Capture first (the privacy-relevant part, within one frame interval), then the consumer, then the mixer.
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
  if (!video_thread_.started || (video_thread_.done.load(std::memory_order_acquire) && !video_thread_.th.joinable())) {
    for (Layer &L : layers_) {
      L.enc.reset(); // the encoders and their device go now; an abandoned video thread keeps them
    }
  }
  log::Info("session", "stopped", {{"device", device_}});
}

} // namespace im::session
