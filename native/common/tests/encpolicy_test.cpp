// The encoder policy (docs/m2/04-windows-engine.md §5.6, §9.1, §9.5–§9.8, §18.1): candidate order for DD and WGC,
// single and hybrid GPUs; OpenH264 forces CB on every layer; preview-layer fallback; live versus re-init; 10 s
// spacing with the latest value winning; 1 s coalescing of size/fps/profile/content-hint re-inits; failure → next
// candidate; a live encoder whose probe saw an IDR after a bitrate change is treated as re-init; the §5.6 encoder
// token of every kind.
#include <gtest/gtest.h>

#include <algorithm>
#include <cstring>
#include <map>
#include <set>
#include <string>
#include <utility>
#include <vector>

#include "im/encpolicy/candidates.h"
#include "im/encpolicy/encoders.h"
#include "im/encpolicy/reinit.h"
#include "isshoni_media.h"

namespace {

using namespace im::encpolicy;

constexpr int64_t kSec = 1'000'000'000;
constexpr int64_t kMs = 1'000'000;

constexpr uint32_t kNvidia = 0; // adapter indexes in the probe
constexpr uint32_t kIntel = 1;
constexpr uint32_t kAmd = 2;

im_encoder_info Enc(uint32_t kind, uint32_t adapter, im_status status = IM_OK, uint32_t extra_flags = 0) {
  im_encoder_info e;
  std::memset(&e, 0, sizeof(e));
  e.kind = kind;
  e.adapter_index = adapter;
  e.status = status;
  const EncoderTraits &t = Traits(kind);
  e.flags = (status == IM_OK ? IM_ENCF_AVAILABLE : 0u) | (t.hardware ? IM_ENCF_HARDWARE : 0u) |
            (t.live_bitrate ? IM_ENCF_LIVE_BITRATE : 0u) | (t.high ? IM_ENCF_HIGH : 0u) | (t.cb ? IM_ENCF_CB : 0u) |
            extra_flags;
  return e;
}

using Order = std::vector<std::pair<uint32_t, uint32_t>>; // (kind, adapter)

Order OrderOf(const std::vector<im_encoder_info> &probe, const CandidateQuery &q,
              std::vector<Candidate> *out = nullptr) {
  std::vector<Candidate> c(16);
  c.resize(BuildCandidates(probe, q, c));
  Order o;
  for (const Candidate &x : c) {
    o.emplace_back(x.kind, x.adapter_index);
    EXPECT_EQ(probe[x.probe_index].kind, x.kind);
  }
  if (out != nullptr) {
    *out = c;
  }
  return o;
}

CandidateQuery Query(uint32_t path, uint32_t adapter, uint32_t forced = IM_ENC_AUTO, bool test_source = false) {
  CandidateQuery q;
  q.capture_path = path;
  q.capture_adapter = adapter;
  q.forced = forced;
  q.test_source = test_source;
  return q;
}

TEST(EncPolicy, CandidateOrderSingleGpu) {
  // One NVIDIA GPU; the probe lists in any order: the policy sorts.
  const std::vector<im_encoder_info> probe = {
      Enc(IM_ENC_MF_SW, kNoAdapter),
      Enc(IM_ENC_OPENH264, kNoAdapter, IM_ERR_DEPENDENCY), // not turned on ("disabled")
      Enc(IM_ENC_MF_HW, kNvidia),
      Enc(IM_ENC_NVENC, kNvidia),
  };
  const Order want = {{IM_ENC_NVENC, kNvidia}, {IM_ENC_MF_HW, kNvidia}, {IM_ENC_MF_SW, kNoAdapter}};
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_DD, kNvidia)), want);
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia)), want);
  // OpenH264 turned on comes last.
  std::vector<im_encoder_info> with = probe;
  with[1].status = IM_OK;
  Order with_want = want;
  with_want.emplace_back(IM_ENC_OPENH264, kNoAdapter);
  EXPECT_EQ(OrderOf(with, Query(IM_CAPTURE_WGC, kNvidia)), with_want);
  // An unavailable encoder takes no part.
  std::vector<im_encoder_info> broken = probe;
  broken[3].status = IM_ERR_DEPENDENCY; // driver_too_old
  EXPECT_EQ(OrderOf(broken, Query(IM_CAPTURE_WGC, kNvidia)),
            (Order{{IM_ENC_MF_HW, kNvidia}, {IM_ENC_MF_SW, kNoAdapter}}));
}

TEST(EncPolicy, CandidateOrderHybridGpu) {
  // A laptop: dGPU NVIDIA (adapter 0), the internal panel on the iGPU Intel (adapter 1).
  const std::vector<im_encoder_info> probe = {
      Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_QSV, kIntel),       Enc(IM_ENC_MF_HW, kNvidia),
      Enc(IM_ENC_MF_HW, kIntel),  Enc(IM_ENC_MF_SW, kNoAdapter),
  };
  // DD of the internal panel: the iGPU owns the output; the dGPU's encoders need the CPU copy and come after.
  std::vector<Candidate> c;
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_DD, kIntel), &c), (Order{{IM_ENC_QSV, kIntel},
                                                                     {IM_ENC_MF_HW, kIntel},
                                                                     {IM_ENC_NVENC, kNvidia},
                                                                     {IM_ENC_MF_HW, kNvidia},
                                                                     {IM_ENC_MF_SW, kNoAdapter}}));
  ASSERT_EQ(c.size(), 5u);
  EXPECT_FALSE(c[0].cross_adapter_copy);
  EXPECT_FALSE(c[1].cross_adapter_copy);
  EXPECT_TRUE(c[2].cross_adapter_copy) << "DD on another adapter: GPU -> CPU -> GPU";
  EXPECT_TRUE(c[3].cross_adapter_copy);
  EXPECT_FALSE(c[4].cross_adapter_copy) << "software encoders read back anyway";
  // WGC of a window on the external monitor (dGPU): NVENC first, WGC copies across adapters itself.
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia), &c), (Order{{IM_ENC_NVENC, kNvidia},
                                                                       {IM_ENC_MF_HW, kNvidia},
                                                                       {IM_ENC_QSV, kIntel},
                                                                       {IM_ENC_MF_HW, kIntel},
                                                                       {IM_ENC_MF_SW, kNoAdapter}}));
  for (const Candidate &x : c) {
    EXPECT_FALSE(x.cross_adapter_copy);
  }
}

TEST(EncPolicy, CandidateOrderThreeAdaptersAndUnknownCaptureAdapter) {
  const std::vector<im_encoder_info> probe = {
      Enc(IM_ENC_MF_HW, kAmd),    Enc(IM_ENC_AMF, kAmd),         Enc(IM_ENC_QSV, kIntel),
      Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_MF_SW, kNoAdapter), Enc(IM_ENC_MF_HW, kNvidia),
  };
  // Capture on the AMD card: its AMF, its MF; then vendor encoders of the others in adapter order, then their MF.
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kAmd)), (Order{{IM_ENC_AMF, kAmd},
                                                                {IM_ENC_MF_HW, kAmd},
                                                                {IM_ENC_NVENC, kNvidia},
                                                                {IM_ENC_QSV, kIntel},
                                                                {IM_ENC_MF_HW, kNvidia},
                                                                {IM_ENC_MF_SW, kNoAdapter}}));
  // Unknown capture adapter: every adapter counts as "other", in adapter order.
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kNoAdapter)), (Order{{IM_ENC_NVENC, kNvidia},
                                                                      {IM_ENC_QSV, kIntel},
                                                                      {IM_ENC_AMF, kAmd},
                                                                      {IM_ENC_MF_HW, kNvidia},
                                                                      {IM_ENC_MF_HW, kAmd},
                                                                      {IM_ENC_MF_SW, kNoAdapter}}));
}

TEST(EncPolicy, ForcedEncoderAndTheFake) {
  const std::vector<im_encoder_info> probe = {
      Enc(IM_ENC_NVENC, kNvidia),
      Enc(IM_ENC_MF_HW, kNvidia),
      Enc(IM_ENC_MF_SW, kNoAdapter),
      Enc(IM_ENC_FAKE, kNoAdapter),
  };
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia, IM_ENC_MF_SW)), (Order{{IM_ENC_MF_SW, kNoAdapter}}));
  EXPECT_TRUE(OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia, IM_ENC_QSV)).empty()) << "forced and absent: no candidate";
  // The fake encoder (test hooks): first for a test: source, otherwise only when forced.
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_TEST, kNvidia, IM_ENC_AUTO, true)).front().first,
            static_cast<uint32_t>(IM_ENC_FAKE));
  for (const auto &[kind, adapter] : OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia))) {
    EXPECT_NE(kind, static_cast<uint32_t>(IM_ENC_FAKE));
    (void)adapter;
  }
  EXPECT_EQ(OrderOf(probe, Query(IM_CAPTURE_WGC, kNvidia, IM_ENC_FAKE)), (Order{{IM_ENC_FAKE, kNoAdapter}}));
  // The output span bounds what is written.
  std::vector<Candidate> two(2);
  EXPECT_EQ(BuildCandidates(probe, Query(IM_CAPTURE_WGC, kNvidia), two), 2u);
}

// ------------------------------------------------------------------------------------------------ selection

class FakeOpener final : public Opener {
public:
  // (kind, profile) pairs that fail to open, optionally only for some layers; session_limit: IM_ERR_LIMIT.
  struct Rule {
    uint32_t kind;
    uint32_t profile; // 0 = any
    uint32_t layer;   // IM_NO_LAYER = any
    im_status status;
    int times; // fails this many times (-1 = always)
  };
  std::vector<Rule> rules;
  std::vector<std::string> log;
  std::map<uint32_t, uint32_t> open; // layer -> kind
  int waits = 0;
  uint32_t waited_ms = 0;

  im_status Open(uint32_t layer, const Candidate &c, uint32_t profile) override {
    for (Rule &r : rules) {
      if (r.kind == c.kind && (r.profile == 0 || r.profile == profile) &&
          (r.layer == IM_NO_LAYER || r.layer == layer) && r.times != 0) {
        if (r.times > 0) {
          --r.times;
        }
        log.push_back("fail " + std::to_string(layer) + " " + std::string(EncoderToken(c.kind)));
        return r.status;
      }
    }
    open[layer] = c.kind;
    log.push_back("open " + std::to_string(layer) + " " + std::string(EncoderToken(c.kind)) +
                  (profile == IM_PROFILE_CB ? " cb" : " high"));
    return IM_OK;
  }
  void Close(uint32_t layer) override {
    open.erase(layer);
    log.push_back("close " + std::to_string(layer));
  }
  void Wait(uint32_t ms) override {
    ++waits;
    waited_ms += ms;
  }
};

std::vector<Candidate> Candidates(const std::vector<im_encoder_info> &probe, uint32_t forced = IM_ENC_AUTO) {
  std::vector<Candidate> c(16);
  c.resize(BuildCandidates(probe, Query(IM_CAPTURE_WGC, kNvidia, forced), c));
  return c;
}

TEST(EncPolicy, SelectionHappyPath) {
  const auto c = Candidates({Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_MF_SW, kNoAdapter)});
  FakeOpener o;
  const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
  EXPECT_EQ(s.status, IM_OK);
  EXPECT_EQ(s.profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
  EXPECT_EQ(s.layer_count, 2u);
  EXPECT_EQ(s.layers[0].kind, static_cast<uint32_t>(IM_ENC_NVENC));
  EXPECT_EQ(s.layers[1].kind, static_cast<uint32_t>(IM_ENC_NVENC)) << "each layer takes the first that opens";
  EXPECT_EQ(o.waits, 0);
  // Audio only: nothing to open.
  FakeOpener none;
  EXPECT_EQ(SelectEncoders(c, IM_PROFILE_HIGH, 0, false, none).status, IM_OK);
  EXPECT_TRUE(none.log.empty());
}

TEST(EncPolicy, PreviewLayerFallback) {
  const auto c = Candidates({Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_MF_HW, kNvidia), Enc(IM_ENC_MF_SW, kNoAdapter)});
  {
    // NVENC's session limit hits the preview layer: it takes the next encoder that supports the profile.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, 1, IM_ERR_LIMIT, -1});
    o.rules.push_back({IM_ENC_MF_HW, 0, 1, IM_ERR_NO_ENCODER, -1});
    const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
    ASSERT_EQ(s.status, IM_OK);
    EXPECT_EQ(s.layers[0].kind, static_cast<uint32_t>(IM_ENC_NVENC));
    EXPECT_EQ(s.layers[1].kind, static_cast<uint32_t>(IM_ENC_MF_SW));
    EXPECT_EQ(s.profile, static_cast<uint32_t>(IM_PROFILE_HIGH));
    EXPECT_EQ(o.waits, 0);
  }
  {
    // Every option fails once for the preview layer: one retry after 500 ms.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, 1, IM_ERR_LIMIT, 1});
    o.rules.push_back({IM_ENC_MF_HW, 0, 1, IM_ERR_NO_ENCODER, 1});
    o.rules.push_back({IM_ENC_MF_SW, 0, 1, IM_ERR_NO_ENCODER, 1});
    const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
    ASSERT_EQ(s.status, IM_OK);
    EXPECT_EQ(o.waits, 1);
    EXPECT_EQ(o.waited_ms, kLayerRetryWaitMs);
    EXPECT_EQ(s.layers[1].kind, static_cast<uint32_t>(IM_ENC_NVENC));
  }
  {
    // A layer never silently disappears: nothing opens for it → IM_ERR_NO_ENCODER layer_failed, nothing left open.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, 1, IM_ERR_LIMIT, -1});
    o.rules.push_back({IM_ENC_MF_HW, 0, 1, IM_ERR_NO_ENCODER, -1});
    o.rules.push_back({IM_ENC_MF_SW, 0, 1, IM_ERR_NO_ENCODER, -1});
    const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
    EXPECT_EQ(s.status, IM_ERR_NO_ENCODER);
    EXPECT_EQ(s.token, "layer_failed");
    EXPECT_EQ(s.failed_layer, 1u);
    EXPECT_EQ(o.waits, 1);
    EXPECT_TRUE(o.open.empty()) << "layer 0's encoder was closed again";
  }
  {
    // Layer 0 gets no retry wait.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    o.rules.push_back({IM_ENC_MF_HW, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    o.rules.push_back({IM_ENC_MF_SW, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
    EXPECT_EQ(s.status, IM_ERR_NO_ENCODER);
    EXPECT_EQ(s.failed_layer, 0u);
    EXPECT_EQ(o.waits, 0);
  }
}

TEST(EncPolicy, OpenH264ForcesCbOnEveryLayer) {
  std::vector<im_encoder_info> probe = {Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_MF_SW, kNoAdapter),
                                        Enc(IM_ENC_OPENH264, kNoAdapter)};
  const auto c = Candidates(probe);
  {
    // The preview layer opens only with OpenH264 (CB only): the whole session becomes CB.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, 1, IM_ERR_LIMIT, -1});
    o.rules.push_back({IM_ENC_MF_SW, 0, 1, IM_ERR_NO_ENCODER, -1});
    const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 2, false, o);
    ASSERT_EQ(s.status, IM_OK);
    EXPECT_EQ(s.profile, static_cast<uint32_t>(IM_PROFILE_CB)) << "one profile for every layer";
    EXPECT_EQ(s.layers[0].kind, static_cast<uint32_t>(IM_ENC_NVENC));
    EXPECT_EQ(s.layers[1].kind, static_cast<uint32_t>(IM_ENC_OPENH264));
    // Layer 0 was opened with High, closed, and re-opened with CB.
    const std::vector<std::string> &l = o.log;
    EXPECT_NE(std::find(l.begin(), l.end(), "open 0 nvenc high"), l.end());
    EXPECT_NE(std::find(l.begin(), l.end(), "close 0"), l.end());
    EXPECT_NE(std::find(l.begin(), l.end(), "open 0 nvenc cb"), l.end());
    EXPECT_EQ(l.back(), "open 1 openh264 cb");
  }
  {
    // Only OpenH264: High is asked for, the session runs CB.
    const auto only = Candidates({Enc(IM_ENC_OPENH264, kNoAdapter)});
    FakeOpener o;
    const Selection s = SelectEncoders(only, IM_PROFILE_HIGH, 2, false, o);
    ASSERT_EQ(s.status, IM_OK);
    EXPECT_EQ(s.profile, static_cast<uint32_t>(IM_PROFILE_CB));
    EXPECT_EQ(o.waits, 0) << "no High candidate at all: straight to CB";
  }
  {
    // Without a CB-only candidate a High failure is final (no pointless CB round).
    const auto hw = Candidates({Enc(IM_ENC_NVENC, kNvidia)});
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    const Selection s = SelectEncoders(hw, IM_PROFILE_HIGH, 1, false, o);
    EXPECT_EQ(s.status, IM_ERR_NO_ENCODER);
    EXPECT_EQ(o.log.size(), 1u);
  }
  // Supports: probe flags first, traits when the probe recorded neither.
  Candidate x;
  x.kind = IM_ENC_OPENH264;
  EXPECT_FALSE(Supports(x, IM_PROFILE_HIGH));
  EXPECT_TRUE(Supports(x, IM_PROFILE_CB));
  x.kind = IM_ENC_NVENC;
  x.flags = IM_ENCF_CB; // a probe that found no High
  EXPECT_FALSE(Supports(x, IM_PROFILE_HIGH));
  EXPECT_TRUE(Supports(x, IM_PROFILE_CB));
}

TEST(EncPolicy, ForcedEncoderFailureIsForcedFailed) {
  const auto c = Candidates({Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_MF_SW, kNoAdapter)}, IM_ENC_NVENC);
  ASSERT_EQ(c.size(), 1u);
  FakeOpener o;
  o.rules.push_back({IM_ENC_NVENC, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
  const Selection s = SelectEncoders(c, IM_PROFILE_HIGH, 1, true, o);
  EXPECT_EQ(s.status, IM_ERR_NO_ENCODER);
  EXPECT_EQ(s.token, "forced_failed") << "never a silent fallback";
  // No candidate at all (forced kind absent).
  FakeOpener o2;
  EXPECT_EQ(SelectEncoders({}, IM_PROFILE_HIGH, 1, true, o2).token, "forced_failed");
  EXPECT_EQ(SelectEncoders({}, IM_PROFILE_HIGH, 1, false, o2).token, "layer_failed");
}

TEST(EncPolicy, FailureMovesToTheNextCandidate) {
  const auto c = Candidates({Enc(IM_ENC_NVENC, kNvidia), Enc(IM_ENC_OPENH264, kNoAdapter), Enc(IM_ENC_MF_HW, kNvidia),
                             Enc(IM_ENC_MF_SW, kNoAdapter)});
  // Order: nvenc, mf-hw, mf-sw, openh264.
  ASSERT_EQ(c.size(), 4u);
  ASSERT_EQ(c[0].kind, static_cast<uint32_t>(IM_ENC_NVENC));
  {
    // An encode error: the same encoder re-opens once.
    FakeOpener o;
    const Recovery r = RecoverLayer(c, 0, 0, IM_PROFILE_HIGH, Failure::kEncodeError, IM_OK, o);
    EXPECT_EQ(r.outcome, Recovery::Outcome::kSame);
    EXPECT_EQ(r.candidate, 0u);
    EXPECT_FALSE(r.kind_changed);
  }
  {
    // The re-open fails: the next candidate, ENCODER_CHANGED with reason encode_error.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    const Recovery r = RecoverLayer(c, 0, 0, IM_PROFILE_HIGH, Failure::kEncodeError, IM_OK, o);
    EXPECT_EQ(r.outcome, Recovery::Outcome::kChanged);
    EXPECT_EQ(r.kind, static_cast<uint32_t>(IM_ENC_MF_HW));
    EXPECT_TRUE(r.kind_changed);
    EXPECT_EQ(r.reason, "encode_error");
  }
  {
    // A session limit on the re-open: reason session_limit.
    FakeOpener o;
    o.rules.push_back({IM_ENC_NVENC, 0, IM_NO_LAYER, IM_ERR_LIMIT, -1});
    const Recovery r = RecoverLayer(c, 0, 1, IM_PROFILE_HIGH, Failure::kEncodeError, IM_OK, o);
    EXPECT_EQ(r.reason, "session_limit");
    EXPECT_EQ(r.kind, static_cast<uint32_t>(IM_ENC_MF_HW));
  }
  {
    // A profile mismatch (bad stream): no re-open of the same encoder.
    FakeOpener o;
    const Recovery r = RecoverLayer(c, 0, 0, IM_PROFILE_HIGH, Failure::kBadStream, IM_OK, o);
    EXPECT_EQ(r.outcome, Recovery::Outcome::kChanged);
    EXPECT_EQ(r.kind, static_cast<uint32_t>(IM_ENC_MF_HW));
    EXPECT_EQ(r.reason, "encode_error");
    EXPECT_EQ(o.log.front(), "open 0 mf-hw high");
  }
  {
    // A re-init's re-open failed: open_failed (or session_limit).
    FakeOpener o;
    EXPECT_EQ(RecoverLayer(c, 1, 0, IM_PROFILE_HIGH, Failure::kReopenFailed, IM_ERR_NO_ENCODER, o).reason,
              "open_failed");
    EXPECT_EQ(RecoverLayer(c, 1, 0, IM_PROFILE_HIGH, Failure::kReopenFailed, IM_ERR_LIMIT, o).reason, "session_limit");
  }
  {
    // Candidates that can't do the session profile are skipped (OpenH264 in a High session); exhausted → the
    // session ends with IM_ERR_ENCODER_LOST.
    FakeOpener o;
    o.rules.push_back({IM_ENC_MF_SW, 0, IM_NO_LAYER, IM_ERR_NO_ENCODER, -1});
    const Recovery r = RecoverLayer(c, 2, 0, IM_PROFILE_HIGH, Failure::kBadStream, IM_OK, o);
    EXPECT_EQ(r.outcome, Recovery::Outcome::kExhausted);
    // With CB the same position falls back to OpenH264.
    const Recovery cb = RecoverLayer(c, 2, 0, IM_PROFILE_CB, Failure::kBadStream, IM_OK, o);
    EXPECT_EQ(cb.outcome, Recovery::Outcome::kChanged);
    EXPECT_EQ(cb.kind, static_cast<uint32_t>(IM_ENC_OPENH264));
  }
  {
    // The same kind on another adapter is a change without ENCODER_CHANGED.
    const auto two = Candidates({Enc(IM_ENC_MF_HW, kNvidia), Enc(IM_ENC_MF_HW, kIntel)});
    FakeOpener o;
    const Recovery r = RecoverLayer(two, 0, 0, IM_PROFILE_HIGH, Failure::kBadStream, IM_OK, o);
    EXPECT_EQ(r.outcome, Recovery::Outcome::kChanged);
    EXPECT_FALSE(r.kind_changed);
  }
  FakeOpener o;
  EXPECT_EQ(RecoverLayer(c, 9, 0, IM_PROFILE_HIGH, Failure::kBadStream, IM_OK, o).outcome,
            Recovery::Outcome::kExhausted);
}

// ---------------------------------------------------------------------------------------- live versus re-init

TEST(EncPolicy, LiveVersusReinit) {
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_NVENC, kNvidia).flags), BitrateMode::kLive);
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_QSV, kIntel).flags), BitrateMode::kLive);
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_OPENH264, kNoAdapter).flags), BitrateMode::kLive);
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_AMF, kAmd).flags), BitrateMode::kReinit);
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_MF_HW, kNvidia).flags), BitrateMode::kReinit);
  EXPECT_EQ(BitrateModeFor(Enc(IM_ENC_MF_SW, kNoAdapter).flags), BitrateMode::kReinit);

  // A live encoder: applied before the next frame, the same value is a no-op.
  ReinitPlanner live(8'000'000, true);
  EXPECT_EQ(live.SetBitrate(8'000'000), BitrateDecision::kNoop);
  EXPECT_EQ(live.SetBitrate(6'000'000), BitrateDecision::kLive);
  EXPECT_EQ(live.bitrate(), 6'000'000u);
  EXPECT_FALSE(live.Poll(0).has_value()) << "no re-init for a live change";
  EXPECT_EQ(live.NextDeadlineNs(), ReinitPlanner::kNever);
}

TEST(EncPolicy, LiveEncoderThatEmitsAnIdrIsReinit) {
  // §9.10: the probe's live-bitrate check saw an IDR after a bitrate change (QSV's reset): the flag goes.
  const uint32_t probed = ApplyLiveBitrateCheck(Enc(IM_ENC_QSV, kIntel).flags, /*idr_after_change=*/true);
  EXPECT_EQ(probed & IM_ENCF_LIVE_BITRATE, 0u);
  EXPECT_EQ(BitrateModeFor(probed), BitrateMode::kReinit);
  EXPECT_EQ(ApplyLiveBitrateCheck(Enc(IM_ENC_NVENC, kNvidia).flags, false) & IM_ENCF_LIVE_BITRATE,
            static_cast<uint32_t>(IM_ENCF_LIVE_BITRATE));
  ReinitPlanner p(8'000'000, BitrateModeFor(probed) == BitrateMode::kLive);
  EXPECT_FALSE(p.live());
  EXPECT_EQ(p.SetBitrate(4'000'000), BitrateDecision::kPending);
}

TEST(EncPolicy, ReinitSpacingTenSecondsLatestValueWins) {
  ReinitPlanner p(8'000'000, false);
  // The first bitrate re-init may run at once.
  EXPECT_EQ(p.SetBitrate(6'000'000), BitrateDecision::kPending);
  auto a = p.Poll(1 * kSec);
  ASSERT_TRUE(a.has_value());
  EXPECT_EQ(a->reasons, reinit::kBitrate);
  EXPECT_EQ(a->bitrate_bps, 6'000'000u);
  EXPECT_EQ(p.bitrate(), 6'000'000u);
  // Inside the 10 s window: stored, the latest value wins.
  EXPECT_EQ(p.SetBitrate(5'000'000), BitrateDecision::kPending);
  EXPECT_EQ(p.SetBitrate(4'000'000), BitrateDecision::kPending);
  EXPECT_EQ(p.target_bitrate(), 4'000'000u);
  EXPECT_EQ(p.NextDeadlineNs(), 11 * kSec);
  EXPECT_FALSE(p.Poll(5 * kSec).has_value());
  EXPECT_FALSE(p.Poll(11 * kSec - 1).has_value());
  a = p.Poll(11 * kSec);
  ASSERT_TRUE(a.has_value());
  EXPECT_EQ(a->bitrate_bps, 4'000'000u);
  // Back to the current value inside the window cancels the pending change.
  EXPECT_EQ(p.SetBitrate(3'000'000), BitrateDecision::kPending);
  EXPECT_EQ(p.SetBitrate(4'000'000), BitrateDecision::kNoop);
  EXPECT_EQ(p.NextDeadlineNs(), ReinitPlanner::kNever);
  EXPECT_FALSE(p.Poll(30 * kSec).has_value());
  EXPECT_EQ(p.SetBitrate(4'000'000), BitrateDecision::kNoop) << "the current value is a no-op";
}

TEST(EncPolicy, ReinitCoalescingOneSecond) {
  ReinitPlanner p(8'000'000, true);
  p.Request(reinit::kFps);
  auto a = p.Poll(0);
  ASSERT_TRUE(a.has_value()) << "the first re-init runs at once";
  EXPECT_EQ(a->reasons, reinit::kFps);
  EXPECT_EQ(a->bitrate_bps, 8'000'000u);
  // Requests within the next second coalesce into one re-init at the 1 s mark.
  p.Request(reinit::kContentHint);
  p.Request(reinit::kProfile);
  p.Request(reinit::kContentHint);
  EXPECT_EQ(p.NextDeadlineNs(), 1 * kSec);
  EXPECT_FALSE(p.Poll(200 * kMs).has_value());
  EXPECT_FALSE(p.Poll(999 * kMs).has_value());
  a = p.Poll(1 * kSec);
  ASSERT_TRUE(a.has_value());
  EXPECT_EQ(a->reasons, reinit::kContentHint | reinit::kProfile);
  EXPECT_FALSE(p.Poll(5 * kSec).has_value());
  // kBitrate can't be requested this way.
  p.Request(reinit::kBitrate);
  EXPECT_EQ(p.pending_reasons(), 0u);
}

TEST(EncPolicy, PendingBitrateRidesAlongWithAReinit) {
  ReinitPlanner p(8'000'000, false);
  EXPECT_EQ(p.SetBitrate(7'000'000), BitrateDecision::kPending);
  ASSERT_TRUE(p.Poll(0).has_value());                            // the first bitrate re-init runs at once, at 0
  EXPECT_EQ(p.SetBitrate(5'000'000), BitrateDecision::kPending); // waits for 10 s
  p.Request(reinit::kSize);                                      // a stable resize at 2 s
  auto a = p.Poll(2 * kSec);
  ASSERT_TRUE(a.has_value());
  EXPECT_EQ(a->reasons, reinit::kSize | reinit::kBitrate) << "the pending bitrate goes with it";
  EXPECT_EQ(a->bitrate_bps, 5'000'000u);
  // That counted as the bitrate re-init: the next bitrate change waits until 12 s.
  EXPECT_EQ(p.SetBitrate(3'000'000), BitrateDecision::kPending);
  EXPECT_EQ(p.NextDeadlineNs(), 12 * kSec);
  // A size change pending with a bitrate inside the bitrate window: the size re-init doesn't wait for 10 s.
  p.Request(reinit::kFps);
  EXPECT_EQ(p.NextDeadlineNs(), 3 * kSec);
}

TEST(EncPolicy, ReopenAfterFallback) {
  // A re-init encoder with a pending bitrate falls back to a live encoder: it opens with the target and is live.
  ReinitPlanner p(8'000'000, false);
  EXPECT_EQ(p.SetBitrate(6'000'000), BitrateDecision::kPending);
  p.OnReopened(5 * kSec, p.target_bitrate(), true);
  EXPECT_EQ(p.bitrate(), 6'000'000u);
  EXPECT_TRUE(p.live());
  EXPECT_EQ(p.pending_reasons(), 0u);
  EXPECT_EQ(p.SetBitrate(4'000'000), BitrateDecision::kLive);
  // The other way round: a live encoder falls back to a re-init one; the re-open counts for the spacing.
  ReinitPlanner q(8'000'000, true);
  q.OnReopened(5 * kSec, 8'000'000, false);
  q.Request(reinit::kContentHint);
  EXPECT_EQ(q.NextDeadlineNs(), 6 * kSec);
}

// --------------------------------------------------------------------------------------------- tokens, limits

TEST(EncPolicy, EncoderTokenOfEveryKind) {
  const struct {
    uint32_t kind;
    const char *token;
    const char *codec;
    bool hardware;
  } want[] = {
      {IM_ENC_NVENC, "nvenc", "h264_nvenc", true},
      {IM_ENC_QSV, "qsv", "h264_qsv", true},
      {IM_ENC_AMF, "amf", "h264_amf", true},
      {IM_ENC_MF_HW, "mf-hw", "h264_mf", true},
      {IM_ENC_MF_SW, "mf-sw", "h264_mf", false},
      {IM_ENC_OPENH264, "openh264", "openh264", false},
      {IM_ENC_FAKE, "fake", "fake", false},
      {IM_ENC_VIDEOTOOLBOX, "videotoolbox", "videotoolbox", true},
      {IM_ENC_GSTREAMER, "gstreamer", "gstreamer", false},
  };
  std::set<std::string> tokens;
  for (const auto &w : want) {
    EXPECT_EQ(EncoderToken(w.kind), w.token);
    EXPECT_EQ(EncoderCodec(w.kind), w.codec);
    EXPECT_EQ(Traits(w.kind).hardware, w.hardware) << w.token;
    EXPECT_EQ(EncoderKindFromToken(w.token), w.kind);
    tokens.insert(w.token);
  }
  EXPECT_EQ(tokens.size(), 9u) << "one token per kind";
  EXPECT_EQ(EncoderToken(IM_ENC_AUTO), "auto");
  EXPECT_EQ(EncoderKindFromToken("auto"), static_cast<uint32_t>(IM_ENC_AUTO));
  EXPECT_EQ(EncoderToken(42), "");
  EXPECT_EQ(EncoderKindFromToken("h264_nvenc"), kUnknownKind) << "the codec name is not a token";
  EXPECT_EQ(EncoderKindFromToken(""), kUnknownKind);
  EXPECT_EQ(EncoderKindFromToken("NVENC"), kUnknownKind) << "tokens are lower case";
  // §9.1's table.
  EXPECT_TRUE(Traits(IM_ENC_NVENC).vendor);
  EXPECT_TRUE(Traits(IM_ENC_QSV).vendor);
  EXPECT_TRUE(Traits(IM_ENC_AMF).vendor);
  EXPECT_FALSE(Traits(IM_ENC_MF_HW).vendor);
  EXPECT_FALSE(Traits(IM_ENC_OPENH264).high) << "OpenH264 is CB only";
  EXPECT_TRUE(Traits(IM_ENC_OPENH264).cb);
}

TEST(EncPolicy, ControlLimits) {
  EXPECT_TRUE(ValidVideoBitrate(0, 100'000));
  EXPECT_TRUE(ValidVideoBitrate(0, 50'000'000));
  EXPECT_FALSE(ValidVideoBitrate(0, 99'999));
  EXPECT_FALSE(ValidVideoBitrate(0, 50'000'001));
  EXPECT_TRUE(ValidVideoBitrate(1, 50'000));
  EXPECT_TRUE(ValidVideoBitrate(1, 2'000'000));
  EXPECT_FALSE(ValidVideoBitrate(1, 2'000'001));
  EXPECT_FALSE(ValidVideoBitrate(2, 49'999));
  EXPECT_FALSE(ValidVideoBitrate(0, 0));
  EXPECT_TRUE(ValidOpusBitrate(32'000));
  EXPECT_TRUE(ValidOpusBitrate(510'000));
  EXPECT_FALSE(ValidOpusBitrate(510'001));
  EXPECT_TRUE(ValidFps(1));
  EXPECT_TRUE(ValidFps(240));
  EXPECT_FALSE(ValidFps(0));
  EXPECT_FALSE(ValidFps(241));
  EXPECT_TRUE(ValidGopMs(0));
  EXPECT_TRUE(ValidGopMs(1000));
  EXPECT_TRUE(ValidGopMs(10000));
  EXPECT_FALSE(ValidGopMs(999));
  EXPECT_FALSE(ValidGopMs(10001));
}

} // namespace
