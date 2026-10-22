// audio_bridge_test (docs/m2/04-windows-engine.md §18.2, §10.1-§10.3, 05 §4.3, §10.5; README WA34): 04's audio wiring
// through the C ABI against a fake AudioEngine and a fake Mixer (05's real ones need audio devices, which CI runners
// don't have). It asserts:
// - im_init and im_shutdown call Init (with im_init's flags) and Shutdown, Shutdown after every session's Mixer::Stop;
// - every audio export forwards its arguments (the rules, flags and shared_pid, the buffers and their caps) and
//   returns what the AudioEngine answered, its detail included; refused arguments never reach it
//   (IM_AUDIO_MODE_EVERYTHING in im_start and im_audio_set_mode: no mixer, the fake's mode unchanged);
// - im_start copies cfg->exclusions (shared_pid included) and hands the copy to SetRules before Mixer::Start, with
//   the audio flags and the session's sink in the MixerConfig; without exclusions it calls no SetRules;
// - modes OFF and TEST_TONE create no mixer;
// - im_set_paused and IM_START_PAUSED reach Mixer::SetPaused, im_audio_set_muted reaches SetMuted, each only by its
//   own call;
// - the fake mixer's chunks come out as Opus packets exactly 20 ms apart (the first stamped at or after epoch_ns, the
//   first chunk's time minus the encoder's lookahead) and, with IM_AUDIOF_TAP_PCM, as PCM packets exactly 10 ms apart
//   holding the mixer's samples byte for byte; Opus packets decode to the mixer's tone (im_test_opus_decode);
// - audio-only sessions; muted and paused, silent frames keep flowing on the unbroken grid and the PCM tap is digital
//   zero; a mixer resync (OnGap) is one REINIT per stream, never an event;
// - audio never fails a share: refused rules, no mixer or a mixer that doesn't start leave a running share without
//   audio packets; Mixer::State is the session's audio state (im_session_info, im_get_stats); im_stop stops the mixer;
//   a second audio session is IM_ERR_LIMIT without a new mixer;
// - im_test_opus_decode decodes with the DLL's libopus (before im_init too), and im_build_info names that libopus.
//
// The test links the engine's objects (im_engine) and points audio_bridge::Engine() at the fake. The cases with a
// video layer need the test hooks (test:pattern with IM_ENC_FAKE: win-x64-ci, win-x64-asan) and skip without them.
// Calls are ordered by their position in the fake's call log, never by clock readings (Windows' clocks tick).
#include <gtest/gtest.h>

#include <opus.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstddef>
#include <cstdint>
#include <cstdlib>
#include <cstring>
#include <iterator>
#include <limits>
#include <map>
#include <memory>
#include <mutex>
#include <numbers>
#include <optional>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include "audio/mixer.h"
#include "audio_bridge/audio_bridge.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/audio/pcm_sink.h"
#include "isshoni_media.h"

namespace {

using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr int64_t kMs = 1'000'000;
constexpr int64_t kChunkNs = im::audio::kChunkNs; // 10 ms
constexpr int64_t kFrameNs = 2 * kChunkNs;        // one Opus packet
constexpr uint32_t kChunkFrames = im::audio::kChunkFrames;
constexpr uint32_t kChunkSamples = im::audio::kChunkSamples;
constexpr int32_t kMaxDecodeFrames = 5760; // 120 ms, the most one Opus packet holds
constexpr double kToneHz = 1000.0;         // the fake mixer's signal: a 1 kHz sine at 0.25 on both channels
constexpr double kToneLevel = 0.25;
constexpr double kToneRms = kToneLevel / std::numbers::sqrt2;
constexpr int64_t kDefaultDelayNs = 50 * kMs; // the fake stamps its chunks 50 ms in the past, as 05's L0 (05 §10.3)
constexpr int64_t kResyncStepNs = 35 * kMs;   // a resync's new grid starts 35 ms later than the old one would go on
constexpr std::size_t kNone = std::numeric_limits<std::size_t>::max();

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

bool StartsWith(std::string_view s, std::string_view prefix) { return s.substr(0, prefix.size()) == prefix; }

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

// The Opus encoder's lookahead on the engine clock (04 §10.3: 312 samples, 6.5 ms), which packet times subtract.
int64_t LookaheadNs() {
  int err = OPUS_OK;
  OpusEncoder *enc = opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO, &err);
  if (enc == nullptr) {
    ADD_FAILURE() << "opus_encoder_create: " << opus_strerror(err);
    return 0;
  }
  opus_int32 la = 0;
  EXPECT_EQ(opus_encoder_ctl(enc, OPUS_GET_LOOKAHEAD(&la)), OPUS_OK);
  opus_encoder_destroy(enc);
  return int64_t{la} * 1'000'000'000 / 48000;
}

// ----------------------------------------------------------------------------------------- the fake AudioEngine

// A rule as the AudioEngine saw it during the call (05 §4.3: it copies; the caller's memory may go afterwards).
struct RuleCopy {
  uint32_t kind = 0;
  uint32_t source = 0;
  std::string value;
  std::optional<std::string> app_id;
  std::optional<std::string> name;
  bool operator==(const RuleCopy &) const = default;
};

struct ExclusionsCopy {
  const im_exclusions *ptr = nullptr; // the pointer the call got
  const im_rule *rules_ptr = nullptr;
  uint32_t struct_size = 0;
  uint32_t flags = 0;
  uint32_t shared_pid = 0;
  std::vector<RuleCopy> rules;
};

ExclusionsCopy Copy(const im_exclusions *ex) {
  ExclusionsCopy c;
  c.ptr = ex;
  if (ex == nullptr) {
    return c;
  }
  c.rules_ptr = ex->rules;
  c.struct_size = ex->struct_size;
  c.flags = ex->flags;
  c.shared_pid = ex->shared_pid;
  for (uint32_t i = 0; i < ex->rule_count; ++i) {
    const im_rule &r = ex->rules[i];
    RuleCopy rc;
    rc.kind = r.kind;
    rc.source = r.source;
    rc.value = r.value != nullptr ? r.value : "";
    if (r.app_id != nullptr) {
      rc.app_id = r.app_id;
    }
    if (r.name != nullptr) {
      rc.name = r.name;
    }
    c.rules.push_back(std::move(rc));
  }
  return c;
}

// How loud a chunk of the fake mixer is: the whole tone, a 10 ms fade, or digital zero.
enum class Level : uint8_t { kFull, kRamp, kSilent };

// One chunk the fake mixer handed to the session's sink.
struct Chunk {
  int64_t ts = 0;
  std::size_t calls = 0; // the calls logged before the mixer read its flags for this chunk (orders it without a clock)
  Level level = Level::kFull;
  std::vector<float> frames;
};

struct ListCall {
  uint32_t flags = 0;
  char *buf = nullptr;
  int32_t cap = 0;
};

struct SoundCheckCall {
  uint32_t helper_pid = 0;
  int32_t timeout_ms = 0;
  char *buf = nullptr;
  int32_t cap = 0;
};

constexpr std::string_view kAppsJson = R"({"v":1,"seq":3,"state":"watching","apps":[],"events":[]})";
constexpr std::string_view kSoundCheckJson = R"({"v":1,"result":"heard"})";

class FakeAudioEngine;

// 05's Mixer as 04 sees it (05 §10.5): Start starts a mixer thread that hands the sink a 10 ms chunk of a 1 kHz tone
// on an exact grid, stamped L in the past (chunk time = tick - L, 05 §10.3), calling OnGap before its first chunk and
// at a resync; pause and the engine's user mute each fade the tone to digital zero in 10 ms (effective mute = user mute
// OR paused, 05 §9.6); Stop joins the thread, so no chunk follows it.
class FakeMixer final : public im::audio::Mixer {
public:
  static constexpr uint32_t kRampFrames = kChunkFrames; // the 10 ms fade

  explicit FakeMixer(FakeAudioEngine *engine) noexcept : engine_(engine) {}
  ~FakeMixer() override;
  FakeMixer(const FakeMixer &) = delete;
  FakeMixer &operator=(const FakeMixer &) = delete;

  im_status Start(const im::audio::MixerConfig &cfg) override;
  void Stop() override;
  void SetPaused(bool paused) override;
  uint32_t State() const override;

private:
  void Loop() noexcept;
  void Join() noexcept;

  FakeAudioEngine *const engine_;
  im::audio::PcmSink *sink_ = nullptr;
  int64_t delay_ns_ = kDefaultDelayNs;
  uint32_t resync_after_ = 0;
  std::atomic<bool> paused_{false};
  std::atomic<bool> stop_{false};
  im::engine::os::Waiter waiter_;
  std::thread thread_;
  // The mixer thread's.
  uint32_t gain_ = kRampFrames;
  uint64_t n_ = 0; // the tone's sample index
};

// The process-wide fake (static, so a thread an earlier failure abandoned never reaches freed memory). Every call is
// logged in order; Init, Shutdown and NewMixer arrive on the engine worker, the exports on the caller's thread and
// the chunks on the mixer thread, so everything is behind one mutex.
class FakeAudioEngine final : public im::audio::AudioEngine {
public:
  struct MixerKnobs {
    im_status start = IM_OK;
    int64_t delay_ns = kDefaultDelayNs;
    uint32_t resync_after = 0;
  };

  static FakeAudioEngine &Instance() {
    static FakeAudioEngine *const engine = new FakeAudioEngine();
    return *engine;
  }

  // Back to the defaults and an empty log. No mixer may be running.
  void Reset() {
    std::lock_guard<std::mutex> lock(mu_);
    calls_.clear();
    chunks_.clear();
    gaps_.clear();
    result_ = IM_OK;
    rules_result_ = IM_OK;
    makes_mixers_ = true;
    knobs_ = MixerKnobs{};
    rules_.reset();
    watch_.reset();
    mixer_cfg_.reset();
    list_.reset();
    sound_check_.reset();
    levels_.reset();
    muted_.store(false);
    state_.store(IM_AUDIO_STATE_OK);
  }

  // ---- knobs, set by a test before the calls they affect
  // What every forwarded call (not Init, Shutdown or NewMixer) answers; a failure sets the detail kRefused.
  void SetResult(im_status st) {
    std::lock_guard<std::mutex> lock(mu_);
    result_ = st;
  }
  void SetRulesResult(im_status st) {
    std::lock_guard<std::mutex> lock(mu_);
    rules_result_ = st;
  }
  void SetMakesMixers(bool on) {
    std::lock_guard<std::mutex> lock(mu_);
    makes_mixers_ = on;
  }
  void SetMixerStart(im_status st) {
    std::lock_guard<std::mutex> lock(mu_);
    knobs_.start = st;
  }
  void SetDelay(int64_t ns) {
    std::lock_guard<std::mutex> lock(mu_);
    knobs_.delay_ns = ns;
  }
  void SetResyncAfter(uint32_t chunks) {
    std::lock_guard<std::mutex> lock(mu_);
    knobs_.resync_after = chunks;
  }
  void SetMixerState(uint32_t value) { state_.store(value); }

  static constexpr const char *kRefused = "fake: refused by the audio engine";

  // ---- what happened
  std::vector<std::string> Calls() const {
    std::lock_guard<std::mutex> lock(mu_);
    return calls_;
  }
  std::size_t Count(std::string_view call) const {
    std::lock_guard<std::mutex> lock(mu_);
    return static_cast<std::size_t>(std::count(calls_.begin(), calls_.end(), call));
  }
  // The position of the first such call in the log, or kNone.
  std::size_t Index(std::string_view call) const {
    std::lock_guard<std::mutex> lock(mu_);
    const auto it = std::find(calls_.begin(), calls_.end(), call);
    return it == calls_.end() ? kNone : static_cast<std::size_t>(it - calls_.begin());
  }
  // A test's own entry in the log, to order the calls against what it did.
  void Mark(std::string what) { Log("test: " + std::move(what)); }
  std::size_t CallCount() const {
    std::lock_guard<std::mutex> lock(mu_);
    return calls_.size();
  }
  std::optional<ExclusionsCopy> LastRules() const {
    std::lock_guard<std::mutex> lock(mu_);
    return rules_;
  }
  std::optional<ExclusionsCopy> LastWatch() const {
    std::lock_guard<std::mutex> lock(mu_);
    return watch_;
  }
  std::optional<im::audio::MixerConfig> LastMixerConfig() const {
    std::lock_guard<std::mutex> lock(mu_);
    return mixer_cfg_;
  }
  std::optional<ListCall> LastList() const {
    std::lock_guard<std::mutex> lock(mu_);
    return list_;
  }
  std::optional<SoundCheckCall> LastSoundCheck() const {
    std::lock_guard<std::mutex> lock(mu_);
    return sound_check_;
  }
  std::optional<im_audio_levels *> LastLevels() const {
    std::lock_guard<std::mutex> lock(mu_);
    return levels_;
  }
  std::vector<Chunk> Chunks() const {
    std::lock_guard<std::mutex> lock(mu_);
    return chunks_;
  }
  std::vector<int64_t> Gaps() const {
    std::lock_guard<std::mutex> lock(mu_);
    return gaps_;
  }
  bool muted() const noexcept { return muted_.load(); }
  uint32_t state() const noexcept { return state_.load(); }

  // ---- the fake mixer's side
  void Log(std::string call) {
    std::lock_guard<std::mutex> lock(mu_);
    calls_.push_back(std::move(call));
  }
  MixerKnobs OnMixerStart(const im::audio::MixerConfig &cfg) {
    std::lock_guard<std::mutex> lock(mu_);
    calls_.push_back("Mixer::Start");
    mixer_cfg_ = cfg;
    return knobs_;
  }
  void RecordChunk(Chunk c) {
    std::lock_guard<std::mutex> lock(mu_);
    chunks_.push_back(std::move(c));
  }
  void RecordGap(int64_t ts) {
    std::lock_guard<std::mutex> lock(mu_);
    gaps_.push_back(ts);
  }

  // ---- im::audio::AudioEngine
  im_status Init(uint32_t init_flags) override {
    Log("Init(" + std::to_string(init_flags) + ")");
    return IM_OK;
  }
  void Shutdown() override { Log("Shutdown"); }
  uint32_t ProbeFlags() override {
    Log("ProbeFlags");
    return 0;
  }
  int32_t ListAppsJson(uint32_t flags, char *buf, int32_t cap) override {
    const im_status st = Record("ListAppsJson", [&] { list_ = ListCall{flags, buf, cap}; });
    return st != IM_OK ? Refused(st) : im::abi::CopyOut(kAppsJson, buf, cap);
  }
  im_status SetRules(const im_exclusions *ex) override {
    im_status st = Record("SetRules", [&] { rules_ = Copy(ex); });
    {
      std::lock_guard<std::mutex> lock(mu_);
      st = rules_result_ != IM_OK ? rules_result_ : st;
    }
    return st != IM_OK ? Refused(st) : IM_OK;
  }
  im_status Watch(const im_exclusions *ex) override {
    const im_status st = Record("Watch", [&] { watch_ = Copy(ex); });
    return st != IM_OK ? Refused(st) : IM_OK;
  }
  im_status Unwatch() override {
    const im_status st = Record("Unwatch", [] {});
    return st != IM_OK ? Refused(st) : IM_OK;
  }
  im_status SetMode(uint32_t mode) override {
    const im_status st = Record("SetMode(" + std::to_string(mode) + ")", [] {});
    return st != IM_OK ? Refused(st) : IM_OK;
  }
  im_status SetMuted(bool on) override {
    im_status st = IM_OK;
    {
      std::lock_guard<std::mutex> lock(mu_);
      st = result_;
      if (st == IM_OK) {
        muted_.store(on); // before the log entry: a chunk mixed after the entry sees it
      }
      calls_.push_back(on ? "SetMuted(1)" : "SetMuted(0)");
    }
    return st != IM_OK ? Refused(st) : IM_OK;
  }
  im_status LevelsGet(im_audio_levels *out) override {
    const im_status st = Record("LevelsGet", [&] { levels_ = out; });
    if (st != IM_OK) {
      return Refused(st);
    }
    // The export checked struct_size against the v1.0 size, which holds every field written here.
    out->report_seq = 9;
    out->qpc_ns = 123456789;
    out->mix_peak_db = -6.0f;
    out->mix_rms_db = -9.0f;
    out->count = 1;
    std::memcpy(out->apps[0].app_id, "exe:game.exe", sizeof "exe:game.exe");
    out->apps[0].peak_db = -12.0f;
    out->apps[0].rms_db = -15.0f;
    out->apps[0].source = 0;
    return IM_OK;
  }
  int32_t SoundCheck(uint32_t helper_pid, int32_t timeout_ms, char *buf, int32_t cap) override {
    const im_status st = Record("SoundCheck", [&] { sound_check_ = SoundCheckCall{helper_pid, timeout_ms, buf, cap}; });
    return st != IM_OK ? Refused(st) : im::abi::CopyOut(kSoundCheckJson, buf, cap);
  }
  std::unique_ptr<im::audio::Mixer> NewMixer() override {
    bool make = false;
    {
      std::lock_guard<std::mutex> lock(mu_);
      calls_.push_back("NewMixer");
      make = makes_mixers_;
    }
    return make ? std::make_unique<FakeMixer>(this) : nullptr;
  }

private:
  FakeAudioEngine() = default;

  template <class F> im_status Record(std::string call, F &&fill) {
    std::lock_guard<std::mutex> lock(mu_);
    calls_.push_back(std::move(call));
    fill();
    return result_;
  }
  static int32_t Refused(im_status st) { return im::abi::Fail(st, {}, kRefused); }

  mutable std::mutex mu_;
  std::vector<std::string> calls_;
  std::vector<Chunk> chunks_;
  std::vector<int64_t> gaps_;
  im_status result_ = IM_OK;
  im_status rules_result_ = IM_OK;
  bool makes_mixers_ = true;
  MixerKnobs knobs_;
  std::optional<ExclusionsCopy> rules_;
  std::optional<ExclusionsCopy> watch_;
  std::optional<im::audio::MixerConfig> mixer_cfg_;
  std::optional<ListCall> list_;
  std::optional<SoundCheckCall> sound_check_;
  std::optional<im_audio_levels *> levels_;
  std::atomic<bool> muted_{false};
  std::atomic<uint32_t> state_{IM_AUDIO_STATE_OK};
};

FakeMixer::~FakeMixer() {
  Join(); // a mixer whose Start failed, or that is destroyed without Stop
  engine_->Log("~Mixer");
}

im_status FakeMixer::Start(const im::audio::MixerConfig &cfg) {
  const FakeAudioEngine::MixerKnobs k = engine_->OnMixerStart(cfg);
  if (k.start != IM_OK) {
    return im::abi::Fail(k.start, {}, "fake mixer: start refused");
  }
  if (cfg.sink == nullptr) {
    return im::abi::Fail(IM_ERR_INVALID_ARG, {}, "fake mixer: no sink");
  }
  sink_ = cfg.sink;
  delay_ns_ = k.delay_ns;
  resync_after_ = k.resync_after;
  thread_ = std::thread([this] { Loop(); });
  return IM_OK;
}

void FakeMixer::Stop() {
  Join();
  engine_->Log("Mixer::Stop");
}

void FakeMixer::SetPaused(bool paused) {
  paused_.store(paused); // before the log entry: a chunk mixed after the entry sees it
  engine_->Log(paused ? "Mixer::SetPaused(1)" : "Mixer::SetPaused(0)");
}

uint32_t FakeMixer::State() const { return engine_->state(); }

void FakeMixer::Join() noexcept {
  stop_.store(true, std::memory_order_release);
  waiter_.Wake();
  if (thread_.joinable()) {
    thread_.join();
  }
}

void FakeMixer::Loop() noexcept {
  std::vector<float> buf(kChunkSamples);
  int64_t next = im::engine::os::QpcNs() - delay_ns_; // the first chunk is mixed now, stamped L in the past
  sink_->OnGap(next);
  engine_->RecordGap(next);
  uint64_t chunks = 0;
  bool resynced = false;
  while (!stop_.load(std::memory_order_acquire)) {
    const int64_t due = next + delay_ns_;
    if (im::engine::os::QpcNs() < due) {
      waiter_.WaitUntil(due);
      continue;
    }
    if (resync_after_ != 0 && chunks == resync_after_ && !resynced) {
      resynced = true;
      next += kResyncStepNs; // a mixer resync (05 §9.1): a new grid, later than the old one would go on
      sink_->OnGap(next);
      engine_->RecordGap(next);
      continue;
    }
    Chunk c;
    c.ts = next;
    c.calls = engine_->CallCount(); // first: the flags read below are at least as new as these calls
    const bool silent = paused_.load() || engine_->muted();
    const uint32_t gain_before = gain_;
    for (uint32_t i = 0; i < kChunkFrames; ++i) {
      if (silent && gain_ > 0) {
        --gain_;
      } else if (!silent && gain_ < kRampFrames) {
        ++gain_;
      }
      float v = 0.0f; // digital zero once faded out
      if (gain_ > 0) {
        const double phase = 2.0 * std::numbers::pi * kToneHz * static_cast<double>(n_ % 48000) / 48000.0;
        v = static_cast<float>(kToneLevel * std::sin(phase) * static_cast<double>(gain_) / kRampFrames);
      }
      buf[2 * i] = v;
      buf[2 * i + 1] = v;
      ++n_;
    }
    if (gain_before == 0 && gain_ == 0) {
      c.level = Level::kSilent;
    } else if (gain_before == kRampFrames && gain_ == kRampFrames) {
      c.level = Level::kFull;
    } else {
      c.level = Level::kRamp;
    }
    sink_->OnChunk(buf.data(), next);
    c.frames = buf;
    engine_->RecordChunk(std::move(c));
    next += kChunkNs;
    ++chunks;
  }
}

FakeAudioEngine &Fake() { return FakeAudioEngine::Instance(); }

// ---------------------------------------------------------------------------------------------- packets

struct Pkt {
  uint32_t kind = 0;
  uint32_t layer = 0;
  uint32_t flags = 0;
  int64_t ts = 0;
  uint32_t duration_us = 0;
  int32_t event = 0;
  std::vector<uint8_t> data;

  bool silent_pcm() const {
    std::vector<float> f(data.size() / sizeof(float));
    std::memcpy(f.data(), data.data(), f.size() * sizeof(float));
    return std::all_of(f.begin(), f.end(), [](float v) { return v == 0.0f; });
  }
};

// Pulls packets for d (or until the session ends).
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
    k.duration_us = p.duration_us;
    k.event = p.event;
    k.data.assign(p.data, p.data + p.size);
    out.push_back(std::move(k));
  }
  return out;
}

void Append(std::vector<Pkt> &to, std::vector<Pkt> more) {
  to.insert(to.end(), std::make_move_iterator(more.begin()), std::make_move_iterator(more.end()));
}

std::vector<Pkt> OfKind(const std::vector<Pkt> &pkts, uint32_t kind) {
  std::vector<Pkt> out;
  std::copy_if(pkts.begin(), pkts.end(), std::back_inserter(out), [&](const Pkt &p) { return p.kind == kind; });
  return out;
}

// One stream on one unbroken grid: REINIT on the first packet only, no DISCONTINUITY, capture times exactly step
// apart.
::testing::AssertionResult OnGrid(const std::vector<Pkt> &pkts, int64_t step) {
  if (pkts.empty()) {
    return ::testing::AssertionFailure() << "no packets";
  }
  if (pkts[0].flags != IM_PKT_REINIT) {
    return ::testing::AssertionFailure() << "the first packet's flags are " << pkts[0].flags << ", not REINIT";
  }
  for (std::size_t i = 1; i < pkts.size(); ++i) {
    if (pkts[i].flags != 0) {
      return ::testing::AssertionFailure() << "packet " << i << " has flags " << pkts[i].flags;
    }
    if (pkts[i].ts - pkts[i - 1].ts != step) {
      return ::testing::AssertionFailure()
             << "packet " << i << " is " << pkts[i].ts - pkts[i - 1].ts << " ns after the one before, not " << step;
    }
  }
  return ::testing::AssertionSuccess();
}

// The chunks whose flags the fake mixer read after the call logged at index from and before the one at index to were
// digital zero, at least `least` of them. The first and the last may be a fade: each call sets its flag just before
// its log entry, so a chunk mixed in between reads the new flag but counts as mixed before the call.
::testing::AssertionResult SilentBetween(const std::vector<Chunk> &chunks, std::size_t from, std::size_t to,
                                         std::size_t least) {
  std::vector<Level> window;
  for (const Chunk &c : chunks) {
    if (c.calls > from && c.calls <= to) {
      window.push_back(c.level);
    }
  }
  if (window.size() < least + 2) {
    return ::testing::AssertionFailure() << "only " << window.size() << " chunks between the calls";
  }
  for (std::size_t i = 1; i + 1 < window.size(); ++i) {
    if (window[i] != Level::kSilent) {
      return ::testing::AssertionFailure()
             << "chunk " << i << " of " << window.size() << " between the calls has sound";
    }
  }
  return ::testing::AssertionSuccess();
}

// The fake's chunks by capture time.
std::map<int64_t, const Chunk *> ByTime(const std::vector<Chunk> &chunks) {
  std::map<int64_t, const Chunk *> m;
  for (const Chunk &c : chunks) {
    m[c.ts] = &c;
  }
  return m;
}

// Every PCM packet holds exactly the samples of the fake's chunk with its capture time.
::testing::AssertionResult PcmIsTheMix(const std::vector<Pkt> &pcm, const std::map<int64_t, const Chunk *> &chunks) {
  for (std::size_t i = 0; i < pcm.size(); ++i) {
    const auto it = chunks.find(pcm[i].ts);
    if (it == chunks.end()) {
      return ::testing::AssertionFailure() << "PCM packet " << i << " has no chunk at its capture time";
    }
    if (pcm[i].data.size() != kChunkSamples * sizeof(float) ||
        std::memcmp(pcm[i].data.data(), it->second->frames.data(), pcm[i].data.size()) != 0) {
      return ::testing::AssertionFailure() << "PCM packet " << i << " differs from the mixer's chunk";
    }
  }
  return ::testing::AssertionSuccess();
}

struct Decoded {
  double rms = 0;
  double peak = 0;
};

// Decodes the Opus packets in order with im_test_opus_decode (one stream, the decoder's state kept).
std::vector<Decoded> DecodeOpus(const std::vector<Pkt> &opus) {
  std::vector<float> pcm(2 * kMaxDecodeFrames);
  std::vector<Decoded> out;
  for (const Pkt &p : opus) {
    const int32_t n =
        im_test_opus_decode(p.data.data(), static_cast<int32_t>(p.data.size()), pcm.data(), kMaxDecodeFrames);
    EXPECT_EQ(n, 960) << LastError();
    Decoded d;
    if (n > 0) {
      double sum = 0;
      for (int32_t i = 0; i < 2 * n; ++i) {
        const double v = pcm[static_cast<std::size_t>(i)];
        sum += v * v;
        d.peak = std::max(d.peak, std::fabs(v));
      }
      d.rms = std::sqrt(sum / (2.0 * n));
    }
    out.push_back(d);
  }
  return out;
}

// Whether the Opus packet stamped ts decodes only from chunks of the given level: its own two chunks (from ts +
// lookahead) and the two before, which the encoder's lookahead and overlap reach into.
bool PacketIs(int64_t ts, int64_t lookahead_ns, Level level, const std::map<int64_t, const Chunk *> &chunks) {
  const int64_t first = ts + lookahead_ns;
  for (int64_t t = first - 2 * kChunkNs; t <= first + kChunkNs; t += kChunkNs) {
    const auto it = chunks.find(t);
    if (it == chunks.end() || it->second->level != level) {
      return false;
    }
  }
  return true;
}

// The decoded Opus stream follows the mixer: packets made of the whole tone carry its level, packets made of digital
// zero are silent. Returns how many packets of each kind it checked.
std::pair<std::size_t, std::size_t> ExpectDecodedFollowsTheMix(const std::vector<Pkt> &opus, int64_t lookahead_ns,
                                                               const std::map<int64_t, const Chunk *> &chunks) {
  const std::vector<Decoded> dec = DecodeOpus(opus);
  std::size_t loud = 0, quiet = 0;
  for (std::size_t i = 2; i < opus.size() && i < dec.size(); ++i) { // the decoder settles in two packets
    if (PacketIs(opus[i].ts, lookahead_ns, Level::kFull, chunks)) {
      ++loud;
      EXPECT_NEAR(dec[i].rms, kToneRms, 0.25 * kToneRms) << "packet " << i;
    } else if (PacketIs(opus[i].ts, lookahead_ns, Level::kSilent, chunks)) {
      ++quiet;
      EXPECT_LT(dec[i].peak, 0.01) << "packet " << i << " decodes to sound, but its chunks were digital zero";
    }
  }
  return {loud, quiet};
}

// ---------------------------------------------------------------------------------------------- configs

im_rule Rule(uint32_t kind, uint32_t source, const char *value, const char *app_id = nullptr,
             const char *name = nullptr) {
  return im_rule{kind, source, value, app_id, name};
}

// An audio-only session with 05's mixer (04 §10.3: no source, no layers).
im_start_config AudioOnly(uint32_t audio_flags = 0, const im_exclusions *ex = nullptr) {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.audio.mode = IM_AUDIO_MODE_EXCLUDE_APPS;
  c.audio.flags = audio_flags;
  c.exclusions = ex;
  return c;
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
// test:pattern through IM_ENC_FAKE, one layer f (720p30), and the given audio.
im_start_config WithVideo(uint32_t audio_mode, uint32_t audio_flags = 0) {
  im_start_config c{};
  c.struct_size = sizeof c;
  c.flags = IM_START_ALLOW_LISTED_SOURCE;
  c.source_id = "test:pattern";
  c.profile = IM_PROFILE_HIGH;
  c.content_hint = IM_CONTENT_AUTO;
  c.encoder = IM_ENC_AUTO;
  c.layer_count = 1;
  std::memcpy(c.layers[0].rid, "f", 2);
  c.layers[0].max_pixels = 1280 * 720;
  c.layers[0].fps = 30;
  c.layers[0].bitrate_bps = 2'000'000;
  c.audio.mode = audio_mode;
  c.audio.flags = audio_flags;
  return c;
}
#endif

class AudioBridge : public ::testing::Test {
protected:
  void SetUp() override {
    im_shutdown();
    Fake().Reset();
    im::audio_bridge::SetEngineForTest(&Fake());
    ASSERT_EQ(Init(0), IM_OK) << LastError();
    lookahead_ns_ = LookaheadNs();
    ASSERT_GT(lookahead_ns_, 0);
  }
  void TearDown() override {
    im_shutdown();
    im::audio_bridge::SetEngineForTest(nullptr);
  }

  static im_status Init(uint32_t flags) {
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = flags;
    p.log_level = IM_LOG_DEBUG;
    return im_init(&p);
  }

  // Starts cfg, logs "test: im_start returned" in the fake's log and keeps epoch_ns.
  im_session Start(const im_start_config &cfg, im_session_info *info = nullptr) {
    im_session s = 0;
    im_session_info local{};
    local.struct_size = sizeof local;
    const im_status st = im_start(&cfg, &s, &local);
    Fake().Mark("im_start returned");
    EXPECT_EQ(st, IM_OK) << LastError();
    if (info != nullptr) {
      *info = local;
    }
    epoch_ = local.epoch_ns;
    return s;
  }

  int64_t epoch_ = 0;
  int64_t lookahead_ns_ = 0;
};

// ---------------------------------------------------------------------------------------------- init, shutdown

TEST_F(AudioBridge, InitAndShutdownCallTheAudioEngine) {
  EXPECT_EQ(Init(0), IM_ERR_BAD_STATE) << "a second im_init";
  im_shutdown();
  im_shutdown(); // not initialized any more: nothing to shut down
  // 05's engine decides what IM_INIT_NO_AUDIO means (no watcher, no audio API): the flags reach it as given.
  ASSERT_EQ(Init(IM_INIT_NO_AUDIO), IM_OK) << LastError();
  im_shutdown();
  EXPECT_EQ(Fake().Calls(), (std::vector<std::string>{"Init(0)", "Shutdown", "Init(1)", "Shutdown"}));
}

TEST_F(AudioBridge, ShutdownStopsTheMixerBeforeTheAudioEngine) {
  const im_session s = Start(AudioOnly());
  ASSERT_NE(s, 0u);
  im_shutdown(); // without im_stop
  const std::size_t stop = Fake().Index("Mixer::Stop");
  ASSERT_NE(stop, kNone) << "im_shutdown didn't stop the session's mixer";
  EXPECT_LT(stop, Fake().Index("Shutdown"));
  EXPECT_EQ(Fake().Count("Shutdown"), 1u);
}

// ---------------------------------------------------------------------------------------------- the exports

TEST_F(AudioBridge, AudioExportsForwardTheirArguments) {
  const im_rule rules[] = {
      Rule(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "discord.exe", "voice:discord", "Discord"),
      Rule(IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "4242", "isshoni"),
      Rule(IM_RULE_BROWSER, IM_RULE_SRC_DATA, "chrome.exe"),
      Rule(IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, "C:\\Users\\u\\AppData\\Local\\isshoni\\WebView2", "", nullptr),
  };
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS | IM_EXCLF_FAST_PATH | 0x100u; // unknown bits are ignored, not dropped
  ex.rules = rules;
  ex.rule_count = 4;
  ex.shared_pid = 31337;
  const std::vector<RuleCopy> want = {
      {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_BUILTIN, "discord.exe", "voice:discord", "Discord"},
      {IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "4242", "isshoni", std::nullopt},
      {IM_RULE_BROWSER, IM_RULE_SRC_DATA, "chrome.exe", std::nullopt, std::nullopt},
      {IM_RULE_WEBVIEW_DATA_DIR, IM_RULE_SRC_SELF, "C:\\Users\\u\\AppData\\Local\\isshoni\\WebView2", "", std::nullopt},
  };

  // im_audio_watch and im_set_exclusions: the rules, flags and shared_pid, during the call.
  ASSERT_EQ(im_audio_watch(&ex), IM_OK) << LastError();
  const std::optional<ExclusionsCopy> watch = Fake().LastWatch();
  ASSERT_TRUE(watch.has_value());
  EXPECT_EQ(watch->rules, want);
  EXPECT_EQ(watch->flags, ex.flags);
  EXPECT_EQ(watch->shared_pid, 31337u);
  ex.shared_pid = 0; // a monitor
  ASSERT_EQ(im_set_exclusions(&ex), IM_OK) << LastError();
  const std::optional<ExclusionsCopy> rules_seen = Fake().LastRules();
  ASSERT_TRUE(rules_seen.has_value());
  EXPECT_EQ(rules_seen->rules, want);
  EXPECT_EQ(rules_seen->flags, ex.flags);
  EXPECT_EQ(rules_seen->shared_pid, 0u);
  ASSERT_EQ(im_audio_unwatch(), IM_OK) << LastError();

  // im_audio_set_mode and im_audio_set_muted.
  ASSERT_EQ(im_audio_set_mode(IM_AUDIO_MODE_OFF), IM_OK) << LastError();
  ASSERT_EQ(im_audio_set_mode(IM_AUDIO_MODE_EXCLUDE_APPS), IM_OK) << LastError();
  ASSERT_EQ(im_audio_set_muted(1), IM_OK) << LastError();
  EXPECT_TRUE(Fake().muted());
  ASSERT_EQ(im_audio_set_muted(0), IM_OK) << LastError();
  EXPECT_FALSE(Fake().muted());

  // im_list_audio_apps: flags, buffer and cap as given; the snprintf answer comes back unchanged.
  std::vector<char> buf(256, 'x');
  EXPECT_EQ(im_list_audio_apps(0, buf.data(), static_cast<int32_t>(buf.size())), static_cast<int32_t>(kAppsJson.size()))
      << LastError();
  EXPECT_EQ(std::string(buf.data()), kAppsJson);
  std::optional<ListCall> list = Fake().LastList();
  ASSERT_TRUE(list.has_value());
  EXPECT_EQ(list->flags, 0u);
  EXPECT_EQ(list->buf, buf.data());
  EXPECT_EQ(list->cap, 256);
  EXPECT_EQ(im_list_audio_apps(5, nullptr, 0), static_cast<int32_t>(kAppsJson.size())) << "the size query";
  list = Fake().LastList();
  ASSERT_TRUE(list.has_value());
  EXPECT_EQ(list->flags, 5u) << "the reserved flags reach 05 as given";
  EXPECT_EQ(list->buf, nullptr);
  EXPECT_EQ(list->cap, 0);
  EXPECT_EQ(im_list_audio_apps(0, buf.data(), 8), static_cast<int32_t>(kAppsJson.size()));
  EXPECT_EQ(std::string(buf.data()), kAppsJson.substr(0, 7)) << "cut with the snprintf rule";

  // im_audio_levels_get: the caller's struct itself, a larger struct_size included.
  struct Bigger {
    im_audio_levels levels;
    uint32_t newer_field;
  } big{};
  big.levels.struct_size = sizeof big;
  big.newer_field = 0xABCD;
  ASSERT_EQ(im_audio_levels_get(&big.levels), IM_OK) << LastError();
  const std::optional<im_audio_levels *> levels_seen = Fake().LastLevels();
  ASSERT_TRUE(levels_seen.has_value());
  EXPECT_EQ(*levels_seen, &big.levels);
  EXPECT_EQ(big.levels.report_seq, 9u);
  EXPECT_EQ(big.levels.qpc_ns, 123456789);
  EXPECT_EQ(big.levels.count, 1u);
  EXPECT_EQ(std::string(big.levels.apps[0].app_id), "exe:game.exe");
  EXPECT_EQ(big.newer_field, 0xABCDu);

  // im_audio_sound_check: the helper's pid, the timeout and the buffer.
  std::vector<char> sc(128);
  EXPECT_EQ(im_audio_sound_check(777, 2500, sc.data(), static_cast<int32_t>(sc.size())),
            static_cast<int32_t>(kSoundCheckJson.size()))
      << LastError();
  EXPECT_EQ(std::string(sc.data()), kSoundCheckJson);
  const std::optional<SoundCheckCall> check = Fake().LastSoundCheck();
  ASSERT_TRUE(check.has_value());
  EXPECT_EQ(check->helper_pid, 777u);
  EXPECT_EQ(check->timeout_ms, 2500);
  EXPECT_EQ(check->buf, sc.data());
  EXPECT_EQ(check->cap, 128);
  EXPECT_EQ(im_audio_sound_check(0, 10000, nullptr, 0), static_cast<int32_t>(kSoundCheckJson.size()));
  EXPECT_EQ(Fake().LastSoundCheck()->helper_pid, 0u) << "0: the DLL plays the chime itself";

  EXPECT_EQ(Fake().Calls(),
            (std::vector<std::string>{"Init(0)", "Watch", "SetRules", "Unwatch", "SetMode(0)", "SetMode(1)",
                                      "SetMuted(1)", "SetMuted(0)", "ListAppsJson", "ListAppsJson", "ListAppsJson",
                                      "LevelsGet", "SoundCheck", "SoundCheck"}));
}

TEST_F(AudioBridge, AudioExportsReturnWhatTheAudioEngineAnswered) {
  const im_rule rule = Rule(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, "game.exe");
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.rules = &rule;
  ex.rule_count = 1;
  Fake().SetResult(IM_ERR_BAD_STATE);
  const std::string detail = FakeAudioEngine::kRefused;
  im_audio_levels levels{};
  levels.struct_size = sizeof levels;
  char buf[64];
  const std::vector<std::pair<const char *, int32_t>> got = {
      {"im_audio_watch", im_audio_watch(&ex)},
      {"im_audio_unwatch", im_audio_unwatch()},
      {"im_set_exclusions", im_set_exclusions(&ex)},
      {"im_audio_set_mode", im_audio_set_mode(IM_AUDIO_MODE_OFF)},
      {"im_audio_set_muted", im_audio_set_muted(1)},
      {"im_audio_levels_get", im_audio_levels_get(&levels)},
      {"im_list_audio_apps", im_list_audio_apps(0, buf, static_cast<int32_t>(sizeof buf))},
      {"im_audio_sound_check", im_audio_sound_check(1, 1000, buf, static_cast<int32_t>(sizeof buf))},
  };
  for (const auto &[fn, st] : got) {
    EXPECT_EQ(st, IM_ERR_BAD_STATE) << fn;
  }
  // The detail is the AudioEngine's, on the caller's thread.
  EXPECT_EQ(im_audio_sound_check(1, 1000, buf, static_cast<int32_t>(sizeof buf)), IM_ERR_BAD_STATE);
  EXPECT_EQ(LastError(), detail);
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_EXCLUDE_APPS), IM_ERR_BAD_STATE);
  EXPECT_EQ(LastError(), detail);
  EXPECT_FALSE(Fake().muted()) << "a refused mute changed nothing";
}

TEST_F(AudioBridge, RefusedArgumentsNeverReachTheAudioEngine) {
  // IM_AUDIO_MODE_EVERYTHING is reserved and refused in M2 (README §3 item 15): no mixer, the fake's mode unchanged.
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_EVERYTHING), IM_ERR_INVALID_ARG);
  EXPECT_EQ(im_audio_set_mode(IM_AUDIO_MODE_TEST_TONE), IM_ERR_INVALID_ARG) << "the tone is a start mode only";
  EXPECT_EQ(im_audio_set_mode(7), IM_ERR_INVALID_ARG);
  im_start_config cfg = AudioOnly();
  cfg.audio.mode = IM_AUDIO_MODE_EVERYTHING;
  im_session s = 1;
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_INVALID_ARG);
  EXPECT_EQ(s, 0u);
  // A rule set the checks refuse, a bad bool, a timeout out of range.
  const im_rule bad = Rule(IM_RULE_INSTANCE, IM_RULE_SRC_SELF, "not-a-pid");
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.rules = &bad;
  ex.rule_count = 1;
  EXPECT_EQ(im_set_exclusions(&ex), IM_ERR_INVALID_ARG);
  EXPECT_EQ(im_audio_watch(&ex), IM_ERR_INVALID_ARG);
  cfg = AudioOnly(0, &ex);
  EXPECT_EQ(im_start(&cfg, &s, nullptr), IM_ERR_INVALID_ARG);
  EXPECT_EQ(im_audio_set_muted(2), IM_ERR_INVALID_ARG);
  char buf[64];
  EXPECT_EQ(im_audio_sound_check(1, 999, buf, static_cast<int32_t>(sizeof buf)), IM_ERR_INVALID_ARG);
  EXPECT_EQ(Fake().Calls(), std::vector<std::string>{"Init(0)"});
}

// ---------------------------------------------------------------------------------------------- im_start

TEST_F(AudioBridge, StartHandsACopyOfTheExclusionsToSetRulesBeforeTheMixerStarts) {
  const im_rule rules[] = {
      Rule(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, "game.exe", "exe:game.exe", "A game"),
      Rule(IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe"),
  };
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.flags = IM_EXCLF_KEEP_OUT_MIC_USERS;
  ex.rules = rules;
  ex.rule_count = 2;
  ex.shared_pid = 4321; // the shared window's process (im_source_info.pid)
  const uint32_t flags = IM_AUDIOF_TAP_PCM | IM_AUDIOF_TEST_FAIL_PROBE;
  const im_session s = Start(AudioOnly(flags, &ex));
  ASSERT_NE(s, 0u);

  const std::optional<ExclusionsCopy> seen = Fake().LastRules();
  ASSERT_TRUE(seen.has_value());
  EXPECT_NE(seen->ptr, &ex) << "im_start's job reads its own copy, never the caller's memory (04 §5.1, §6.1)";
  EXPECT_NE(seen->rules_ptr, rules);
  EXPECT_EQ(seen->struct_size, sizeof(im_exclusions));
  EXPECT_EQ(seen->flags, IM_EXCLF_KEEP_OUT_MIC_USERS);
  EXPECT_EQ(seen->shared_pid, 4321u);
  EXPECT_EQ(seen->rules, (std::vector<RuleCopy>{
                             {IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, "game.exe", "exe:game.exe", "A game"},
                             {IM_RULE_STOP, IM_RULE_SRC_DATA, "explorer.exe", std::nullopt, std::nullopt},
                         }));

  const std::size_t set_rules = Fake().Index("SetRules");
  const std::size_t new_mixer = Fake().Index("NewMixer");
  const std::size_t start = Fake().Index("Mixer::Start");
  const std::size_t returned = Fake().Index("test: im_start returned");
  ASSERT_NE(start, kNone);
  EXPECT_LT(set_rules, new_mixer);
  EXPECT_LT(new_mixer, start);
  EXPECT_LT(start, returned) << "the mixer starts inside im_start (05 §10.5)";
  EXPECT_EQ(Fake().Count("SetRules"), 1u);
  const std::optional<im::audio::MixerConfig> mc = Fake().LastMixerConfig();
  ASSERT_TRUE(mc.has_value());
  EXPECT_EQ(mc->mode, IM_AUDIO_MODE_EXCLUDE_APPS);
  EXPECT_EQ(mc->flags, flags) << "the audio flags reach the mixer as given";
  EXPECT_NE(mc->sink, nullptr);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(AudioBridge, WithoutExclusionsTheStartKeepsTheRules) {
  // NULL keeps the process-wide set of the last im_set_exclusions or im_audio_watch (05 §4.2): no SetRules.
  const im_session s = Start(AudioOnly());
  ASSERT_NE(s, 0u);
  EXPECT_EQ(Fake().Count("SetRules"), 0u);
  EXPECT_EQ(Fake().Count("NewMixer"), 1u);
  EXPECT_EQ(Fake().Count("Mixer::Start"), 1u);
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(AudioBridge, ModeOffAndTheTestToneCreateNoMixer) {
  // Audio-only with audio off captures nothing: refused before anything starts.
  im_start_config off = AudioOnly();
  off.audio.mode = IM_AUDIO_MODE_OFF;
  im_session s = 0;
  EXPECT_EQ(im_start(&off, &s, nullptr), IM_ERR_INVALID_ARG);

  // The test tone is 04's: it feeds the Opus stage itself.
  im_start_config tone = AudioOnly();
  tone.audio.mode = IM_AUDIO_MODE_TEST_TONE;
  im_session_info info{};
  s = Start(tone, &info);
  ASSERT_NE(s, 0u);
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_FALSE(OfKind(Pull(s, milliseconds(300)), IM_PACKET_AUDIO).empty()) << "the tone's packets";
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(Fake().Count("NewMixer"), 0u);
  EXPECT_EQ(Fake().Count("SetRules"), 0u);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
TEST_F(AudioBridge, VideoWithAudioOffCreatesNoMixer) {
  im_session_info info{};
  const im_session s = Start(WithVideo(IM_AUDIO_MODE_OFF), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_OFF);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(600));
  EXPECT_FALSE(OfKind(pkts, IM_PACKET_VIDEO).empty());
  EXPECT_TRUE(OfKind(pkts, IM_PACKET_AUDIO).empty());
  EXPECT_TRUE(OfKind(pkts, IM_PACKET_PCM).empty());
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(Fake().Count("NewMixer"), 0u);
}
#else
TEST_F(AudioBridge, VideoWithAudioOffCreatesNoMixer) { GTEST_SKIP() << "needs an IM_TEST_HOOKS build (win-x64-ci)"; }
#endif

// ---------------------------------------------------------------------------------------------- packets

TEST_F(AudioBridge, MixerChunksComeOutAsOpusEvery20msAndPcmEvery10ms) {
  Fake().SetMixerState(IM_AUDIO_STATE_STARTING); // 05's startup probe runs asynchronously (05 §11.2)
  Fake().SetDelay(100 * kMs);                    // L: the mixer's first chunks are older than epoch_ns
  im_session_info info{};
  const im_session s = Start(AudioOnly(IM_AUDIOF_TAP_PCM), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.layer_count, 0u) << "audio-only";
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_STARTING) << "the mixer's state";
  Fake().SetMixerState(IM_AUDIO_STATE_OK);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(1500));
  ASSERT_EQ(im_stop(s), IM_OK);

  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<Pkt> pcm = OfKind(pkts, IM_PACKET_PCM);
  EXPECT_EQ(opus.size() + pcm.size(), pkts.size()) << "an audio-only session sends audio only, and no event";
  ASSERT_GE(opus.size(), 50u);
  ASSERT_GE(pcm.size(), 100u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs)) << "Opus";
  EXPECT_TRUE(OnGrid(pcm, kChunkNs)) << "PCM";
  for (const Pkt &p : opus) {
    EXPECT_EQ(p.layer, IM_NO_LAYER);
    EXPECT_EQ(p.duration_us, 20000u);
    EXPECT_GT(p.data.size(), 0u);
    EXPECT_GE(p.ts, epoch_);
  }
  for (const Pkt &p : pcm) {
    EXPECT_EQ(p.layer, IM_NO_LAYER);
    EXPECT_EQ(p.duration_us, 10000u);
    EXPECT_EQ(p.data.size(), 3840u);
    EXPECT_GE(p.ts, epoch_);
  }

  // The start at epoch_ns (04 §10.3): the mixer's first chunks are stamped before it and dropped; the tap starts with
  // the first chunk at or after epoch_ns, Opus with the first whose packet (chunk time - lookahead) is.
  const std::vector<Chunk> chunks = Fake().Chunks();
  ASSERT_FALSE(chunks.empty());
  EXPECT_LT(chunks.front().ts, epoch_) << "the fake stamps its chunks 100 ms in the past";
  const auto first_pcm = std::find_if(chunks.begin(), chunks.end(), [&](const Chunk &c) { return c.ts >= epoch_; });
  const auto first_opus =
      std::find_if(chunks.begin(), chunks.end(), [&](const Chunk &c) { return c.ts - lookahead_ns_ >= epoch_; });
  ASSERT_NE(first_opus, chunks.end());
  EXPECT_EQ(pcm.front().ts, first_pcm->ts);
  EXPECT_EQ(opus.front().ts, first_opus->ts - lookahead_ns_);

  // What the tap carries is the mix, byte for byte; the Opus stream decodes to the mixer's tone.
  const auto by_time = ByTime(chunks);
  EXPECT_TRUE(PcmIsTheMix(pcm, by_time));
  const auto [loud, quiet] = ExpectDecodedFollowsTheMix(opus, lookahead_ns_, by_time);
  EXPECT_GE(loud, 40u);
  EXPECT_EQ(quiet, 0u);
}

TEST_F(AudioBridge, WithoutTheTapThereIsNoPcm) {
  const im_session s = Start(AudioOnly());
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(700));
  ASSERT_EQ(im_stop(s), IM_OK);
  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  EXPECT_GE(opus.size(), 25u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs));
  EXPECT_EQ(opus.size(), pkts.size()) << "Opus only: no PCM without IM_AUDIOF_TAP_PCM, and no event";
}

TEST_F(AudioBridge, PauseReachesTheMixerAndSilentFramesKeepFlowing) {
  const im_session s = Start(AudioOnly(IM_AUDIOF_TAP_PCM));
  ASSERT_NE(s, 0u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(400));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(500)));
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(400)));
  ASSERT_EQ(im_stop(s), IM_OK);

  // im_set_paused reaches Mixer::SetPaused, and only it: the user's mute is another flag (05 §4.3).
  const std::size_t pause = Fake().Index("Mixer::SetPaused(1)");
  const std::size_t resume = Fake().Index("Mixer::SetPaused(0)");
  ASSERT_NE(pause, kNone);
  ASSERT_NE(resume, kNone);
  EXPECT_LT(pause, resume);
  EXPECT_EQ(Fake().Count("SetMuted(1)") + Fake().Count("SetMuted(0)"), 0u);

  // Silent frames keep flowing on the unbroken grid: 50 Opus and 100 PCM packets a second, no REINIT, no gap.
  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<Pkt> pcm = OfKind(pkts, IM_PACKET_PCM);
  EXPECT_GE(opus.size(), 55u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs)) << "Opus";
  EXPECT_TRUE(OnGrid(pcm, kChunkNs)) << "PCM";

  // The mixer faded out after the pause and in again after the resume; the tap is its digital zero meanwhile.
  const std::vector<Chunk> chunks = Fake().Chunks();
  EXPECT_TRUE(SilentBetween(chunks, pause, resume, 20));
  const auto by_time = ByTime(chunks);
  EXPECT_TRUE(PcmIsTheMix(pcm, by_time));
  const std::size_t zero_pcm =
      static_cast<std::size_t>(std::count_if(pcm.begin(), pcm.end(), [](const Pkt &p) { return p.silent_pcm(); }));
  EXPECT_GE(zero_pcm, 20u) << "the PCM tap shows digital zero while paused";
  EXPECT_FALSE(pcm.back().silent_pcm()) << "sound again after the resume";
  const auto [loud, quiet] = ExpectDecodedFollowsTheMix(opus, lookahead_ns_, by_time);
  EXPECT_GE(loud, 20u);
  EXPECT_GE(quiet, 5u) << "the paused stretch decodes to silence";
}

TEST_F(AudioBridge, UserMuteKeepsSilentFramesFlowingThroughAPause) {
  const im_session s = Start(AudioOnly(IM_AUDIOF_TAP_PCM));
  ASSERT_NE(s, 0u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(300));
  ASSERT_EQ(im_audio_set_muted(1), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(300)));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(200)));
  ASSERT_EQ(im_set_paused(s, 0), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(300)));
  ASSERT_EQ(im_audio_set_muted(0), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(300)));
  ASSERT_EQ(im_stop(s), IM_OK);

  // Each flag only by its own call: the resume didn't touch the user's mute.
  EXPECT_EQ(Fake().Count("SetMuted(1)"), 1u);
  EXPECT_EQ(Fake().Count("SetMuted(0)"), 1u);
  const std::size_t mute = Fake().Index("SetMuted(1)");
  const std::size_t unmute = Fake().Index("SetMuted(0)");
  EXPECT_LT(mute, Fake().Index("Mixer::SetPaused(1)"));
  EXPECT_LT(Fake().Index("Mixer::SetPaused(0)"), unmute);

  // Muted from the mute to the unmute, the resume included; the frames kept flowing all along.
  const std::vector<Chunk> chunks = Fake().Chunks();
  EXPECT_TRUE(SilentBetween(chunks, mute, unmute, 50)) << "sound while the user's mute was set";
  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<Pkt> pcm = OfKind(pkts, IM_PACKET_PCM);
  EXPECT_GE(opus.size(), 60u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs)) << "Opus";
  EXPECT_TRUE(OnGrid(pcm, kChunkNs)) << "PCM";
  EXPECT_TRUE(PcmIsTheMix(pcm, ByTime(chunks)));
}

TEST_F(AudioBridge, StartPausedPausesTheMixerBeforeImStartReturns) {
  // A large L, so that what the mixer mixed before the pause reached it is long before epoch_ns.
  Fake().SetDelay(200 * kMs);
  im_start_config cfg = AudioOnly(IM_AUDIOF_TAP_PCM);
  cfg.flags = IM_START_PAUSED;
  const im_session s = Start(cfg);
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(1200));
  ASSERT_EQ(im_stop(s), IM_OK);
  const std::size_t start = Fake().Index("Mixer::Start");
  const std::size_t pause = Fake().Index("Mixer::SetPaused(1)");
  ASSERT_NE(pause, kNone);
  EXPECT_LT(start, pause);
  EXPECT_LT(pause, Fake().Index("test: im_start returned"));

  // No sound leaves the engine, and the silent frames flow from the first packet on.
  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  const std::vector<Pkt> pcm = OfKind(pkts, IM_PACKET_PCM);
  EXPECT_GE(opus.size(), 40u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs)) << "Opus";
  EXPECT_TRUE(OnGrid(pcm, kChunkNs)) << "PCM";
  for (const Pkt &p : pcm) {
    EXPECT_TRUE(p.silent_pcm()) << "a PCM packet at " << p.ts - epoch_ << " ns after the epoch has sound";
  }
  const std::vector<Decoded> dec = DecodeOpus(opus);
  for (std::size_t i = 2; i < dec.size(); ++i) { // the process's one decoder settles from the last stream in two
    EXPECT_LT(dec[i].peak, 0.01) << "packet " << i;
  }
}

TEST_F(AudioBridge, AMixerResyncIsOneReinitAndNoEvent) {
  Fake().SetResyncAfter(30);
  const im_session s = Start(AudioOnly(IM_AUDIOF_TAP_PCM));
  ASSERT_NE(s, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(1200));
  ASSERT_EQ(im_stop(s), IM_OK);
  const std::vector<int64_t> gaps = Fake().Gaps();
  ASSERT_EQ(gaps.size(), 2u) << "the first chunk's OnGap and the resync's";
  ASSERT_GE(gaps[1], epoch_) << "the resync came before the first packet";

  for (const uint32_t kind : {IM_PACKET_AUDIO, IM_PACKET_PCM}) {
    SCOPED_TRACE(kind == IM_PACKET_AUDIO ? "Opus" : "PCM");
    const std::vector<Pkt> stream = OfKind(pkts, kind);
    const int64_t step = kind == IM_PACKET_AUDIO ? kFrameNs : kChunkNs;
    const int64_t new_grid = kind == IM_PACKET_AUDIO ? gaps[1] - lookahead_ns_ : gaps[1];
    const auto it = std::find_if(stream.begin(), stream.end(), [&](const Pkt &p) { return p.ts >= new_grid; });
    ASSERT_NE(it, stream.end());
    ASSERT_NE(it, stream.begin());
    EXPECT_EQ(it->ts, new_grid) << "the new grid's first chunk starts the stream's next packet";
    EXPECT_TRUE(OnGrid(std::vector<Pkt>(stream.begin(), it), step)) << "before the resync";
    EXPECT_TRUE(OnGrid(std::vector<Pkt>(it, stream.end()), step)) << "from the resync on: REINIT, then the grid";
  }
  EXPECT_TRUE(OfKind(pkts, IM_PACKET_EVENT).empty()) << "audio has no events (04 §10.1)";
}

// ---------------------------------------------------------------------------------------------- audio never fails

TEST_F(AudioBridge, RefusedRulesDoNotStopTheShare) {
  Fake().SetRulesResult(IM_ERR_INTERNAL);
  DrainLogs();
  const im_rule rule = Rule(IM_RULE_APP_EXCLUDE, IM_RULE_SRC_USER, "game.exe");
  im_exclusions ex{};
  ex.struct_size = sizeof ex;
  ex.rules = &rule;
  ex.rule_count = 1;
  im_session_info info{};
  const im_session s = Start(AudioOnly(0, &ex), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(LastError(), "") << "a successful im_start leaves no detail";
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u) << "the mixer still runs";
  EXPECT_LT(Fake().Index("SetRules"), Fake().Index("Mixer::Start"));
  EXPECT_FALSE(OfKind(Pull(s, milliseconds(400)), IM_PACKET_AUDIO).empty());
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_NE(DrainLogs().find("\"msg\":\"audio rules not applied\""), std::string::npos);
}

TEST_F(AudioBridge, NoMixerMeansASilentShareWithoutAudioPackets) {
  Fake().SetMakesMixers(false);
  im_session_info info{};
  const im_session s = Start(AudioOnly(IM_AUDIOF_TAP_PCM), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_OFF);
  EXPECT_TRUE(Pull(s, milliseconds(300)).empty()) << "no packet and no event";
  EXPECT_EQ(im_set_paused(s, 1), IM_OK) << "control calls still work";
  EXPECT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(Fake().Count("Mixer::Start"), 0u);
}

TEST_F(AudioBridge, AMixerThatDoesNotStartLeavesTheShareRunning) {
  Fake().SetMixerStart(IM_ERR_INTERNAL);
  DrainLogs();
  im_session_info info{};
  const im_session s = Start(AudioOnly(), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(LastError(), "");
  EXPECT_EQ(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_OFF);
  EXPECT_TRUE(Pull(s, milliseconds(300)).empty());
  EXPECT_EQ(im_stop(s), IM_OK);
  // The session let go of the mixer it couldn't start, and never stopped it.
  EXPECT_NE(Fake().Index("~Mixer"), kNone);
  EXPECT_EQ(Fake().Count("Mixer::Stop"), 0u);
  EXPECT_NE(DrainLogs().find("\"msg\":\"audio mixer did not start\""), std::string::npos);
}

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
TEST_F(AudioBridge, VideoFlowsWhenTheMixerDoesNotStart) {
  Fake().SetMixerStart(IM_ERR_INTERNAL);
  im_session_info info{};
  const im_session s = Start(WithVideo(IM_AUDIO_MODE_EXCLUDE_APPS), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.flags & IM_SESS_AUDIO, 0u);
  const std::vector<Pkt> pkts = Pull(s, milliseconds(800));
  const std::vector<Pkt> video = OfKind(pkts, IM_PACKET_VIDEO);
  ASSERT_FALSE(video.empty());
  EXPECT_EQ(video.front().flags & (IM_PKT_KEYFRAME | IM_PKT_REINIT), IM_PKT_KEYFRAME | IM_PKT_REINIT);
  EXPECT_TRUE(OfKind(pkts, IM_PACKET_AUDIO).empty());
  EXPECT_TRUE(OfKind(pkts, IM_PACKET_EVENT).empty()) << "audio never ends a share and has no events";
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(AudioBridge, VideoAndTheMixersAudioInOneSession) {
  im_session_info info{};
  const im_session s = Start(WithVideo(IM_AUDIO_MODE_EXCLUDE_APPS, IM_AUDIOF_TAP_PCM), &info);
  ASSERT_NE(s, 0u);
  EXPECT_NE(info.flags & IM_SESS_AUDIO, 0u);
  EXPECT_EQ(info.layer_count, 1u);
  std::vector<Pkt> pkts = Pull(s, milliseconds(500));
  ASSERT_EQ(im_set_paused(s, 1), IM_OK) << LastError();
  Append(pkts, Pull(s, milliseconds(500)));
  ASSERT_EQ(im_stop(s), IM_OK);
  EXPECT_NE(Fake().Index("Mixer::SetPaused(1)"), kNone) << "im_set_paused pauses the mixer too (04 §10.1)";
  EXPECT_FALSE(OfKind(pkts, IM_PACKET_VIDEO).empty());
  const std::vector<Pkt> opus = OfKind(pkts, IM_PACKET_AUDIO);
  EXPECT_GE(opus.size(), 40u);
  EXPECT_TRUE(OnGrid(opus, kFrameNs)) << "Opus, through the pause";
  EXPECT_TRUE(OnGrid(OfKind(pkts, IM_PACKET_PCM), kChunkNs)) << "PCM";
  for (const Pkt &p : pkts) {
    EXPECT_GE(p.ts, epoch_) << "kind " << p.kind;
  }
}
#else
TEST_F(AudioBridge, VideoFlowsWhenTheMixerDoesNotStart) { GTEST_SKIP() << "needs an IM_TEST_HOOKS build (win-x64-ci)"; }
TEST_F(AudioBridge, VideoAndTheMixersAudioInOneSession) { GTEST_SKIP() << "needs an IM_TEST_HOOKS build (win-x64-ci)"; }
#endif

// ---------------------------------------------------------------------------------------------- state, limits, stop

TEST_F(AudioBridge, TheMixersStateIsTheSessionsAudioState) {
  Fake().SetMixerState(IM_AUDIO_STATE_NO_DEVICE);
  im_session_info info{};
  const im_session s = Start(AudioOnly(), &info);
  ASSERT_NE(s, 0u);
  EXPECT_EQ(info.audio_state, IM_AUDIO_STATE_NO_DEVICE);
  Pull(s, milliseconds(300));
  Fake().SetMixerState(IM_AUDIO_STATE_DEGRADED);
  im_session_info now{};
  now.struct_size = sizeof now;
  ASSERT_EQ(im_get_session_info(s, &now), IM_OK) << LastError();
  EXPECT_EQ(now.audio_state, IM_AUDIO_STATE_DEGRADED);
  std::vector<char> buf(16384);
  const int32_t n = im_get_stats(s, buf.data(), static_cast<int32_t>(buf.size()));
  ASSERT_GT(n, 0) << LastError();
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  const std::string audio = "\"audio\":{\"state\":\"degraded\",\"packets\":";
  const std::size_t at = doc.find(audio);
  ASSERT_NE(at, std::string::npos) << doc;
  EXPECT_GT(std::strtoull(doc.c_str() + at + audio.size(), nullptr, 10), 0u) << "the mixer's packets count: " << doc;
  EXPECT_EQ(im_stop(s), IM_OK);
}

TEST_F(AudioBridge, StopStopsTheMixerAndNoChunkFollows) {
  const im_session s = Start(AudioOnly());
  ASSERT_NE(s, 0u);
  Pull(s, milliseconds(200));
  ASSERT_EQ(im_stop(s), IM_OK);
  EXPECT_EQ(Fake().Count("Mixer::Stop"), 1u);
  const std::size_t chunks = Fake().Chunks().size();
  std::this_thread::sleep_for(milliseconds(100));
  EXPECT_EQ(Fake().Chunks().size(), chunks) << "a chunk after Mixer::Stop returned";
  EXPECT_EQ(im_set_paused(s, 1), IM_ERR_INVALID_ARG) << "the handle is gone";
  EXPECT_EQ(Fake().Count("Mixer::SetPaused(1)"), 0u);
}

TEST_F(AudioBridge, OneAudioSessionPerProcess) {
  const im_session a = Start(AudioOnly());
  ASSERT_NE(a, 0u);
  im_start_config cfg = AudioOnly();
  im_session b = 0;
  EXPECT_EQ(im_start(&cfg, &b, nullptr), IM_ERR_LIMIT);
  EXPECT_TRUE(StartsWith(LastError(), "session_limit: ")) << LastError();
  EXPECT_EQ(Fake().Count("NewMixer"), 1u) << "the refused start made no mixer";
  EXPECT_EQ(im_stop(a), IM_OK);
  // The next one gets a mixer of its own.
  const im_session c = Start(AudioOnly());
  ASSERT_NE(c, 0u);
  EXPECT_EQ(Fake().Count("NewMixer"), 2u);
  EXPECT_EQ(im_stop(c), IM_OK);
}

// ---------------------------------------------------------------------------------------------- the DLL's libopus

// Encodes n 20 ms frames of a 1 kHz sine at kToneLevel with a libopus encoder of the stage's kind.
std::vector<std::vector<uint8_t>> EncodeTone(int frames) {
  int err = OPUS_OK;
  OpusEncoder *enc = opus_encoder_create(48000, 2, OPUS_APPLICATION_AUDIO, &err);
  EXPECT_NE(enc, nullptr) << opus_strerror(err);
  std::vector<std::vector<uint8_t>> out;
  if (enc == nullptr) {
    return out;
  }
  std::vector<float> in(2 * 960);
  std::vector<uint8_t> pkt(1275);
  for (int k = 0; k < frames; ++k) {
    for (int i = 0; i < 960; ++i) {
      const double n = static_cast<double>(k * 960 + i);
      const auto v = static_cast<float>(kToneLevel * std::sin(2.0 * std::numbers::pi * kToneHz * n / 48000.0));
      in[static_cast<std::size_t>(2 * i)] = v;
      in[static_cast<std::size_t>(2 * i + 1)] = v;
    }
    const opus_int32 bytes = opus_encode_float(enc, in.data(), 960, pkt.data(), static_cast<opus_int32>(pkt.size()));
    EXPECT_GT(bytes, 0) << opus_strerror(bytes);
    if (bytes > 0) {
      out.emplace_back(pkt.begin(), pkt.begin() + bytes);
    }
  }
  opus_encoder_destroy(enc);
  return out;
}

TEST(AudioBridgeOpus, TestOpusDecodeUsesTheDllsLibopus) {
  im_shutdown(); // im_test_opus_decode works before im_init (04 §5.3)
  const std::vector<std::vector<uint8_t>> pkts = EncodeTone(25);
  ASSERT_EQ(pkts.size(), 25u);
  std::vector<float> pcm(2 * kMaxDecodeFrames);
  double sum = 0;
  std::size_t samples = 0;
  for (std::size_t k = 0; k < pkts.size(); ++k) {
    const int32_t n =
        im_test_opus_decode(pkts[k].data(), static_cast<int32_t>(pkts[k].size()), pcm.data(), kMaxDecodeFrames);
    ASSERT_EQ(n, 960) << LastError();
    if (k >= 5) { // the decoder's state carries from packet to packet; the first ones settle it
      for (int32_t i = 0; i < 2 * n; ++i) {
        const double v = pcm[static_cast<std::size_t>(i)];
        sum += v * v;
      }
      samples += static_cast<std::size_t>(2 * n);
    }
  }
  EXPECT_NEAR(std::sqrt(sum / static_cast<double>(samples)), kToneRms, 0.1 * kToneRms);

  // Too little room for the packet, and a malformed packet: IM_ERR_INVALID_ARG with the export's name.
  EXPECT_EQ(im_test_opus_decode(pkts[0].data(), static_cast<int32_t>(pkts[0].size()), pcm.data(), 480),
            IM_ERR_INVALID_ARG);
  EXPECT_TRUE(StartsWith(LastError(), "im_test_opus_decode: ")) << LastError();
  const uint8_t malformed[] = {0x03}; // a code 3 packet without its frame count byte (RFC 6716 §3.2.5)
  EXPECT_EQ(im_test_opus_decode(malformed, 1, pcm.data(), kMaxDecodeFrames), IM_ERR_INVALID_ARG);
  EXPECT_TRUE(StartsWith(LastError(), "im_test_opus_decode: ")) << LastError();
  // A good packet decodes again afterwards.
  EXPECT_EQ(im_test_opus_decode(pkts[1].data(), static_cast<int32_t>(pkts[1].size()), pcm.data(), kMaxDecodeFrames),
            960)
      << LastError();
  EXPECT_EQ(LastError(), "");
}

TEST(AudioBridgeOpus, ConcurrentDecodesAreSerialized) {
  const std::vector<std::vector<uint8_t>> pkts = EncodeTone(4);
  ASSERT_EQ(pkts.size(), 4u);
  std::atomic<int> bad{0};
  std::vector<std::thread> threads;
  for (int t = 0; t < 4; ++t) {
    threads.emplace_back([&] {
      std::vector<float> pcm(2 * 960);
      for (int i = 0; i < 50; ++i) {
        const std::vector<uint8_t> &p = pkts[static_cast<std::size_t>(i % 4)];
        if (im_test_opus_decode(p.data(), static_cast<int32_t>(p.size()), pcm.data(), 960) != 960) {
          ++bad;
        }
      }
    });
  }
  for (std::thread &t : threads) {
    t.join();
  }
  EXPECT_EQ(bad.load(), 0);
}

TEST(AudioBridgeOpus, BuildInfoNamesTheLinkedLibopus) {
  const std::string want = std::string("\"opus\":\"") + opus_get_version_string() + "\"";
  EXPECT_EQ(im::audio_bridge::OpusVersion(), std::string_view(opus_get_version_string()));
  std::vector<char> buf(4096);
  const int32_t n = im_build_info(buf.data(), static_cast<int32_t>(buf.size()));
  ASSERT_GT(n, 0) << LastError();
  const std::string doc(buf.data(), static_cast<std::size_t>(n));
  EXPECT_NE(doc.find(want), std::string::npos) << doc;
  EXPECT_TRUE(StartsWith(opus_get_version_string(), "libopus ")) << opus_get_version_string();
}

} // namespace
