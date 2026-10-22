// One session (docs/m2/04-windows-engine.md §6): its streams, its threads and its state machine.
//
//   Start (engine worker) ──► RUNNING ⇄ PAUSED ──► ENDED (the ENDED event is queued last) ──► Stop ──► freed
//                               └── Stop (any state) ───────────────────────────────────────────┘
//
// Streams (§6.2): one SPSC packet ring per video layer (producer: the video thread), one for Opus and one for the PCM
// tap (producer: the test-tone thread or 05's mixer thread, through the Opus stage), and the event queue; the packet
// pump drains them for im_next_packet (events first, then the oldest capture time).
//
// Threads (§6.1):
// - video (MMCSS "Capture"): drains the command queue, feeds captured frames to the pacer (§8.4), encodes what it asks
//   for, runs the normalizer (§9.4) and pushes into the rings; re-inits through the re-init planner (§9.5); recovers a
//   failed encoder with the next candidate (ENCODER_CHANGED first, §9.8); ends the session when a device is removed
//   or no candidate is left. It never waits on the consumer.
// - supervisor: the CPU test pattern's frame timer (frames on a grid anchored at epoch_ns, so the pattern's flash
//   instants are the test tone's beeps), and the real-time flags into events: LIMIT_CHANGED (every edge of the
//   pacer's overload rule), RING_OVERFLOW (at most one per stream and second).
// - test tone (MMCSS "Pro Audio", IM_AUDIO_MODE_TEST_TONE only): 04 §10.3's tone on an exact 10 ms grid into the
//   Opus stage. With IM_AUDIO_MODE_EXCLUDE_APPS 05's mixer feeds the stage instead (audio::AudioEngine::NewMixer).
//
// Control calls validate, queue a command (64 entries, a mutex held for the push only) and wake the video thread;
// keyframe requests are coalesced into a bit mask. Stop stops capture first, makes a blocked consumer return
// IM_ENDED at once, stops the mixer and joins the threads within 2 s (a thread that doesn't exit is abandoned with an
// error log: it keeps the session alive through its reference, never touches freed memory and is never killed).
#pragma once

#include <array>
#include <atomic>
#include <condition_variable>
#include <cstddef>
#include <cstdint>
#include <limits>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include "api/owned.h"
#include "audio/mixer.h"
#include "engine/os.h"
#include "im/audio/opus_stage.h"
#include "im/audio/test_tone.h"
#include "im/encpolicy/candidates.h"
#include "im/encpolicy/reinit.h"
#include "im/h264/annexb.h"
#include "im/os/clock.h"
#include "im/pace/pacer.h"
#include "im/ring/event_queue.h"
#include "im/ring/packet_ring.h"
#include "im/ring/pump.h"
#include "im/ring/signal.h"
#include "im/video/scale.h"
#include "isshoni_media.h"
#include "session/encoder.h"
#include "session/session.h"
#include "session/sources.h"
#include "session/stats.h"

namespace im::session {

inline constexpr uint32_t kCommandQueueSize = 64;               // 04 §6.1
inline constexpr int64_t kStopBoundNs = 2'000'000'000;          // im_stop (04 §6.4)
inline constexpr int64_t kSupervisorPollNs = 50'000'000;        // flags into events
inline constexpr int64_t kOverflowEventEveryNs = 1'000'000'000; // RING_OVERFLOW per stream
inline constexpr int64_t kStatsEveryNs = 250'000'000;           // the video thread's stats snapshot
inline constexpr int64_t kRoomRetryNs = 5'000'000;              // a full ring's IDR waits for room (§6.2)
inline constexpr int64_t kFlashEveryNs = 1'000'000'000;         // the test pattern's flash, the tone's beep (§10.3)
// The least capture_ts_ns step between two access units of a layer: one 90 kHz RTP tick, ceil(1e9 / 90000) ns. The
// pacer's own bump (§8.4) is 1 µs, and two AUs 1 µs apart share one RTP timestamp: the SFU and the viewer would take
// a static IDR and the late new frame right after it for one frame, and the IDR would be lost.
inline constexpr int64_t kMinAuSpacingNs = 11'112;

// The engine clock (QPC ns) as im::os::Clock, for the pump's stale rule (§6.2).
class EngineClock final : public os::Clock {
public:
  int64_t NowNs() const noexcept override { return engine::os::QpcNs(); }
};

class Session final : public std::enable_shared_from_this<Session> {
public:
  static constexpr int64_t kNever = std::numeric_limits<int64_t>::max();

  Session();
  ~Session();
  Session(const Session &) = delete;
  Session &operator=(const Session &) = delete;

  // On the engine worker, before the session gets a handle: opens the encoders (the candidate order, one profile,
  // §9.1, §9.6), the audio (the test tone, or 05's mixer through audio_bridge::Engine()) and starts the threads.
  // source is null for an audio-only session. On failure nothing is left running and the thread's detail is set.
  im_status Start(const api::StartRequest &req, const SourceSpec *source, uint32_t init_flags);

  // im_get_session_info.
  void FillInfo(im_session_info &info) const;
  // Whether the session runs audio (it counts against the one audio session of 04 §6.4).
  bool wants_audio() const noexcept { return audio_mode_ != IM_AUDIO_MODE_OFF; }
  uint64_t device() const noexcept { return device_; }

  // im_next_packet (one caller at a time).
  im_status Next(uint32_t timeout_ms, im_packet &out);

  // The control calls.
  im_status RequestKeyframe(uint32_t layer);
  im_status SetBitrate(uint32_t layer, uint32_t bps);
  im_status SetLayerActive(uint32_t layer, bool active);
  im_status UpdateLayer(uint32_t layer, const im_layer_config &cfg);
  im_status SetProfile(uint32_t profile);
  im_status SetContentHint(uint32_t hint);
  im_status SetPaused(bool paused);
  im_status SetAudioBitrate(uint32_t bps);
  im_status Stats(std::string &out) const;

  // im_stop: ≤ 2 s, idempotent.
  void Stop() noexcept;

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // Test hooks: a fault the layer's encoder reports next (04 §18.2).
  im_status Inject(uint32_t layer, InjectedFault fault);
#endif

private:
  struct Command {
    enum class Type : uint8_t { kBitrate, kLayerActive, kUpdateLayer, kProfile, kContentHint, kPaused };
    Type type = Type::kBitrate;
    uint32_t layer = 0;
    uint32_t value = 0;
    im_layer_config cfg{};
  };

  // One video layer, owned by the video thread once the session runs (by the worker before).
  struct Layer {
    char rid[4] = {};
    uint32_t max_pixels = 0; // im_layer_config's bounds (0 = none)
    uint32_t box_width = 0;
    uint32_t box_height = 0;
    uint32_t fps = 60;
    uint32_t gop_ms = 0;
    video::OutputSize out; // the encoder's size
    std::unique_ptr<VideoEncoder> enc;
    std::size_t candidate = 0; // into candidates_
    uint32_t kind = IM_ENC_AUTO;
    uint32_t enc_flags = 0; // IM_ENCF_* of the encoder
    uint32_t profile = 0;   // the profile the encoder was opened with
    h264::Normalizer norm;
    std::optional<encpolicy::ReinitPlanner> planner;
    bool reinit_next = true;     // the next packet that gets in carries REINIT
    bool ring_idr_asked = false; // RequireKeyframe was issued for a ring that waits for a keyframe
    uint32_t index = 0;          // the next access unit's index (the fake's Marker frame)
    int64_t next_flash = 0;      // the next flash instant (epoch_ns + k × 1 s)
    int64_t last_ts = 0;         // the last encoded AU's capture_ts_ns (0: none yet)
    uint32_t last_idr_bytes = 0;
    uint64_t keyframes = 0;
    uint64_t refreshes = 0;
    uint64_t reinits = 0;
    ByteWindow bytes;
    Samples encode_ns;
    Samples latency_ns;
  };

  // The packet writer of the Opus stage: one of the session's audio rings.
  class RingWriter final : public audio::PacketWriter {
  public:
    explicit RingWriter(ring::PacketRing *ring) noexcept : ring_(ring) {}
    bool Write(const audio::AudioPacket &p) noexcept override;

  private:
    ring::PacketRing *ring_;
  };

  // encpolicy's view of this session's encoders.
  class Opener final : public encpolicy::Opener {
  public:
    explicit Opener(Session *s) noexcept : s_(s) {}
    im_status Open(uint32_t layer, const encpolicy::Candidate &c, uint32_t profile) override;
    void Close(uint32_t layer) override;
    void Wait(uint32_t ms) override;

  private:
    Session *s_;
  };

  struct Thread {
    std::thread th;
    std::atomic<bool> done{false};
    bool started = false;
  };

  im_status StartVideo(const im_start_config &cfg);
  im_status StartAudio(const im_start_config &cfg, uint32_t init_flags);
  void StartThreads();
  template <class F> void Launch(Thread &t, F &&body);
  void JoinThreads(int64_t deadline_ns) noexcept;

  // The video thread.
  void VideoLoop() noexcept;
  void ApplyCommands(int64_t now);
  void Apply(const Command &c, int64_t now);
  void ApplyRingRequests();
  void EncodeOne(const pace::Encode &e, int64_t now);
  void Reinit(uint32_t layer, const encpolicy::ReinitAction &a, int64_t now);
  void Recover(uint32_t layer, encpolicy::Failure why, im_status cause, int64_t now);
  im_status OpenLayer(uint32_t layer, const encpolicy::Candidate &c, uint32_t profile);
  void OnOpened(uint32_t layer, int64_t now, bool count_reinit);
  void UpdateLayerInfo(uint32_t layer);
  void PublishLimits();
  void PublishStats(int64_t now);
  int64_t NextDeadline() const noexcept;

  // The supervisor.
  void SupervisorLoop() noexcept;
  int64_t PatternFrameNs(uint64_t k) const noexcept;
  int64_t PatternDueNs(uint64_t k) const noexcept;
  void SuperviseEvents(int64_t now);

  // The test-tone thread.
  void ToneLoop() noexcept;

  // Queues ENDED once (any thread) and stops the session's work; resources stay until Stop.
  void End(im_status status, std::string_view reason, std::string_view detail) noexcept;
  im_status Push(const Command &c);
  im_status CheckLayer(const char *fn, uint32_t layer) const noexcept;
  std::string_view StreamName(uint32_t stream) const noexcept;

  // ---- set up by Start, read-only afterwards
  int64_t epoch_ = 0;
  uint64_t device_ = 0;
  SourceSpec source_;
  bool has_video_ = false;
  uint32_t layer_count_ = 0;
  uint32_t audio_mode_ = IM_AUDIO_MODE_OFF;
  std::vector<EncoderEntry> catalog_;
  std::vector<encpolicy::Candidate> candidates_;

  // ---- streams (declaration order: the pump points at the signal, the events and the clock)
  EngineClock clock_;
  ring::WakeSignal signal_;
  ring::EventQueue events_;
  std::array<std::unique_ptr<ring::PacketRing>, IM_MAX_LAYERS> video_rings_;
  std::unique_ptr<ring::PacketRing> opus_ring_;
  std::unique_ptr<ring::PacketRing> pcm_ring_;
  std::unique_ptr<RingWriter> opus_writer_;
  std::unique_ptr<RingWriter> pcm_writer_;
  ring::PacketPump pump_;
  std::mutex consumer_mu_; // held by im_next_packet; Stop takes it before it releases the last packet

  // ---- audio
  std::unique_ptr<audio::OpusStage> stage_;
  std::unique_ptr<audio::TestToneSource> tone_;
  std::unique_ptr<audio::Mixer> mixer_;
  uint32_t audio_state_ = IM_AUDIO_STATE_OFF; // without a mixer (the tone: ok)

  // ---- capture: the CPU test pattern (published by the supervisor, read by the video thread)
  static constexpr uint32_t kFrameSlots = 8;
  std::atomic<uint64_t> frame_seq_{0};
  std::array<std::atomic<int64_t>, kFrameSlots> frame_ts_{};

  // ---- the video thread's
  std::array<Layer, IM_MAX_LAYERS> layers_;
  std::optional<pace::Pacer> pacer_;
  Opener opener_{this};
  uint32_t profile_ = 0;      // the session profile asked for (start, im_set_profile)
  uint32_t content_hint_ = 0; // IM_CONTENT_*
  uint64_t seen_seq_ = 0;
  int64_t retry_at_ = kNever;
  int64_t next_stats_ = 0;
  bool video_ended_ = false;

  // ---- commands and flags from other threads
  std::mutex cmd_mu_;
  std::array<Command, kCommandQueueSize> cmds_{};
  std::size_t cmd_head_ = 0;
  std::size_t cmd_count_ = 0;
  std::atomic<uint32_t> keyframe_bits_{0};                   // im_request_keyframe, per layer
  std::array<std::atomic<uint8_t>, IM_MAX_LAYERS> faults_{}; // test hooks: 1 = the next encode fails
  std::atomic<bool> device_removed_{false};                  // test hooks: the device is gone

  // ---- shared with callers
  mutable std::mutex info_mu_;
  im_session_info info_{}; // guarded by info_mu_
  mutable std::mutex stats_mu_;
  StatsSnapshot stats_;                                            // guarded by stats_mu_ (the video part)
  std::array<std::atomic<uint8_t>, IM_MAX_LAYERS> limit_{};        // pace::Limit, published by the video thread
  std::array<std::atomic<uint32_t>, IM_MAX_LAYERS> limit_fps10_{}; // fpsOut × 10 when it changed
  std::atomic<uint64_t> frames_captured_{0};
  std::atomic<bool> paused_{false};
  std::atomic<bool> ended_{false};
  std::atomic<bool> stopping_{false};
  std::atomic<bool> capture_stopped_{false};
  std::atomic<bool> stop_once_{false};

  // ---- the supervisor's
  std::array<pace::Limit, IM_MAX_LAYERS> reported_limit_{};
  std::array<uint64_t, ring::kMaxStreams> overflow_pending_{};
  std::array<int64_t, ring::kMaxStreams> overflow_sent_at_{};

  // ---- threads
  engine::os::Waiter video_waiter_;
  engine::os::Waiter supervisor_waiter_;
  engine::os::Waiter tone_waiter_;
  Thread video_thread_;
  Thread supervisor_thread_;
  Thread tone_thread_;
  std::mutex exit_mu_;
  std::condition_variable exit_cv_;
};

} // namespace im::session
