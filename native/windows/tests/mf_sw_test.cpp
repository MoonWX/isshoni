// mf_sw_test (docs/m2/04-windows-engine.md §9.1-§9.5, §18.2; README WA44): Microsoft's software H.264 MFT through
// libavcodec's h264_mf (hw_encoding=0), in a whole session: test:pattern at 1280×720 and 30 Hz drawn on the session's
// device (WARP on the GPU-less runner), converted, read back into system memory and encoded, for about 4 s per case.
//
// It runs when Media Foundation works on the machine (MFStartup, IM_OS_MEDIA_FOUNDATION) and the H.264 encoder MFT is
// registered; otherwise it is skipped with the reason logged. On Windows Server, where Microsoft's H.264 encoder MFT
// is not supported ("Minimum supported server: None supported") yet can enumerate and open, a probe failure is skipped
// too, but only when FFmpeg's log shows the MFT itself refused during MF software's own probe (h264_mf's error with
// the MFT's HRESULT, before MF software's failed open or failed encode call), which the skip prints; any other failure
// (no AU, an AU without SPS/PPS/IDR, a failure of ours) still fails, and a client Windows must pass in full. A failed
// probe prints the engine's encode log. The FFmpeg folder is ffmpeg_load_test's (ISSHONI_TEST_FFMPEG_DIR; required in
// CI).
//
// It asserts, for a High and a Constrained Baseline session forced to IM_ENC_MF_SW:
//   - the probe lists MF software as available, without zero copy or live bitrate (04 §9.1, §9.5);
//   - the session's first packet is KEYFRAME|REINIT, and every keyframe access unit is SPS, PPS, then the IDR slice
//     (04 §9.4), with only IDR slices in a keyframe and none in the others;
//   - the packets' profile key and the SPS's profile_idc are the session's (6400 / 100, 42e0 / 66, constraint byte
//     0xE0 for CB);
//   - a bitrate step (2 Mbit/s → 1 Mbit/s) is a re-init: exactly one REINIT|KEYFRAME packet follows it, and the bytes
//     of the 2 s after that packet are within ±25 % of the new target.
// The pictures' quality (openh264_test's PSNR) needs a decoder, which the pinned FFmpeg build doesn't have
// (encoders only); README WA51's OpenH264 decoder can check it later.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <windows.h>

#include <mfapi.h>
#include <mftransform.h>
#include <versionhelpers.h>

#include <algorithm>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "encode/selector.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"
#include "session/encoder.h"
#include "session/sources.h"
#include "testhooks/testhooks.h"

#pragma comment(lib, "mfuuid.lib") // MFT_CATEGORY_VIDEO_ENCODER, MFVideoFormat_*: GUIDs only, in the test binary

namespace fs = std::filesystem;

namespace {

using Clock = std::chrono::steady_clock;

constexpr uint32_t kWidth = 1280;
constexpr uint32_t kHeight = 720;
constexpr uint32_t kHz = 30;
constexpr uint32_t kStartBps = 2'000'000;
constexpr uint32_t kStepBps = 1'000'000;

std::optional<std::wstring> GetEnv(const wchar_t *name) {
  const DWORD n = GetEnvironmentVariableW(name, nullptr, 0);
  if (n == 0) {
    return std::nullopt;
  }
  std::wstring v(n, L'\0');
  const DWORD m = GetEnvironmentVariableW(name, v.data(), n);
  v.resize(m);
  return v;
}

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

// Whether a synchronous software MFT turns NV12 into H.264 here (Windows Server can have Media Foundation without
// the codecs).
bool H264EncoderMftRegistered() {
  if (FAILED(MFStartup(MF_VERSION, MFSTARTUP_LITE))) {
    return false;
  }
  MFT_REGISTER_TYPE_INFO in{MFMediaType_Video, MFVideoFormat_NV12};
  MFT_REGISTER_TYPE_INFO out{MFMediaType_Video, MFVideoFormat_H264};
  IMFActivate **activates = nullptr;
  UINT32 count = 0;
  const HRESULT hr = MFTEnumEx(MFT_CATEGORY_VIDEO_ENCODER, MFT_ENUM_FLAG_SYNCMFT | MFT_ENUM_FLAG_SORTANDFILTER, &in,
                               &out, &activates, &count);
  for (UINT32 i = 0; i < count; ++i) {
    activates[i]->Release();
  }
  CoTaskMemFree(activates);
  MFShutdown();
  return SUCCEEDED(hr) && count > 0;
}

// The engine's "encode" and "ffmpeg" log lines since the last drain: the probe's own lines and h264_mf's errors,
// which end in the MFT's HRESULT ("failed processing input: <hr>").
std::string EncodeLog() {
  std::vector<char> buf(64 * 1024);
  std::string out;
  for (;;) {
    const int32_t n = im_read_logs(buf.data(), static_cast<int32_t>(buf.size()));
    if (n <= 0) {
      break;
    }
    std::string_view all(buf.data(), static_cast<std::size_t>(n));
    while (!all.empty()) {
      const std::size_t eol = all.find('\n');
      const std::string_view line = all.substr(0, eol);
      if (line.find("\"component\":\"ffmpeg\"") != std::string_view::npos ||
          line.find("\"component\":\"encode\"") != std::string_view::npos) {
        out.append(line);
        out.push_back('\n');
      }
      if (eol == std::string_view::npos) {
        break;
      }
      all.remove_prefix(eol + 1);
    }
  }
  return out;
}

// h264_mf's "ffmpeg" line showing that the encoder MFT itself refused while MF software was probed (its messages that
// end in the MFT's HRESULT), or "" when there is none. Only lines inside MF software's own probe count: after the
// previous encoder's "encoder probed" record and before MF software's failure record, which must be "probe open
// failed" or "probe: test encode failed" with "ok":false (an encode call failed). A test encode that ran but gave no
// AU, or an AU without SPS/PPS/IDR ("ok":true), is ours and never matches; nor does MF hardware's h264_mf error.
std::string MftRefusal(const std::string &encode_log) {
  static constexpr std::string_view kMarkers[] = {"failed processing input",  "failed processing output",
                                                  "could not start stream",   "failed draining",
                                                  "could not set input type", "could not set output type"};
  const auto has = [](std::string_view line, std::string_view s) { return line.find(s) != std::string_view::npos; };
  std::string candidate; // the first marker line since the last "encoder probed" record
  std::string_view rest(encode_log);
  while (!rest.empty()) {
    const std::size_t eol = rest.find('\n');
    const std::string_view line = rest.substr(0, eol);
    if (has(line, "\"component\":\"ffmpeg\"")) {
      if (candidate.empty()) {
        for (const std::string_view m : kMarkers) {
          if (has(line, m)) {
            candidate = std::string(line);
            break;
          }
        }
      }
    } else if (has(line, "\"component\":\"encode\"")) {
      if (has(line, "\"msg\":\"encoder probed\"")) {
        candidate.clear(); // the previous encoder's probe ended; its h264_mf errors are not MF software's
      } else if (has(line, "\"encoder\":\"mf-sw\"") &&
                 (has(line, "\"msg\":\"probe open failed\"") ||
                  (has(line, "\"msg\":\"probe: test encode failed\"") && has(line, "\"ok\":false")))) {
        return candidate;
      }
    }
    if (eol == std::string_view::npos) {
      break;
    }
    rest.remove_prefix(eol + 1);
  }
  return {}; // MF software's failure record is missing (the ring dropped it?): not provably the MFT's refusal
}

struct Packet {
  uint32_t flags = 0;
  uint32_t profile = 0;
  int64_t ts_ns = 0; // capture_ts_ns (engine clock)
  std::vector<uint8_t> data;
};

int64_t EngineNow() {
  im_clock_sample c{};
  c.struct_size = sizeof c;
  im_clock(&c);
  return c.mono_ns;
}

class MfSoftware : public ::testing::Test {
protected:
  void SetUp() override {
    const std::optional<std::wstring> dir = GetEnv(L"ISSHONI_TEST_FFMPEG_DIR");
    if (!dir || dir->empty()) {
      if (GetEnv(L"CI").value_or(L"") == L"true") {
        FAIL() << "ISSHONI_TEST_FFMPEG_DIR is not set: the windows-native job must point it at the locked FFmpeg bin";
      }
      GTEST_SKIP() << "ISSHONI_TEST_FFMPEG_DIR is not set (the pinned FFmpeg build's bin folder)";
    }
    const std::u8string u8 = fs::path(*dir).u8string();
    dir_.assign(u8.begin(), u8.end());
    im::testhooks::Reset();
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO;
    p.log_level = IM_LOG_DEBUG;
    p.ffmpeg_dir = dir_.c_str();
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
    initialized_ = true;

    (void)EncodeLog(); // im_init's records out of the ring (512 records; a full ring drops new ones): the probe's fit

    im_caps caps{};
    caps.struct_size = sizeof caps;
    ASSERT_EQ(im_probe(IM_PROBE_FULL, &caps), IM_OK) << LastError();
    if ((caps.os_flags & IM_OS_MEDIA_FOUNDATION) == 0) {
      std::printf("mf_sw_test: skipped: MFStartup fails on this machine (no Media Foundation)\n");
      GTEST_SKIP() << "Media Foundation is not available here (MFStartup fails)";
    }
    const im_encoder_info *mf = nullptr;
    for (uint32_t i = 0; i < caps.encoder_count; ++i) {
      if (caps.encoders[i].kind == IM_ENC_MF_SW) {
        mf = &caps.encoders[i];
      }
    }
    ASSERT_NE(mf, nullptr) << "the probe always lists MF software";
    std::string why;
    if (mf->status != IM_OK) {
      why = EncodeLog();
      std::printf("mf_sw_test: MF software's probe failed (status %d, '%s'); the engine's encode log:\n%s", mf->status,
                  mf->reason, why.c_str());
      EXPECT_EQ(mf->flags & IM_ENCF_AVAILABLE, 0u) << "a failed probe entry is not available";
      if (!H264EncoderMftRegistered()) {
        std::printf("mf_sw_test: skipped: no H.264 encoder MFT is registered (probe reason '%s')\n", mf->reason);
        GTEST_SKIP() << "Media Foundation has no H.264 encoder MFT here";
      }
      // Microsoft's H.264 encoder MFT: "Minimum supported server: None supported". On Windows Server (GitHub's
      // windows-2025 runner) it can enumerate and open, then refuse to encode. Skipped only when FFmpeg's log shows
      // the MFT itself refused in MF software's own probe, with its HRESULT (MftRefusal); anything else (no AU, no
      // SPS/PPS/IDR, an h264_mf error from MF hardware's probe) still fails below.
      if (const std::string refusal = MftRefusal(why); IsWindowsServer() && !refusal.empty()) {
        std::printf("mf_sw_test: skipped: Windows Server; the H.264 encoder MFT refused: %s\n", refusal.c_str());
        GTEST_SKIP() << "Microsoft's H.264 encoder MFT is not supported on Windows Server and refused here: "
                     << refusal;
      }
    }
    ASSERT_EQ(mf->status, IM_OK) << "MF software failed its probe: " << mf->reason << "; the engine's encode log:\n"
                                 << why;
    EXPECT_NE(mf->flags & IM_ENCF_AVAILABLE, 0u);
    EXPECT_EQ(mf->flags & IM_ENCF_ZERO_COPY, 0u) << "MF software reads back (04 §9.1)";
    EXPECT_EQ(mf->flags & IM_ENCF_LIVE_BITRATE, 0u) << "MF software re-inits for a bitrate change (04 §9.5)";
    EXPECT_NE(mf->flags & IM_ENCF_HIGH, 0u);
    EXPECT_NE(mf->flags & IM_ENCF_CB, 0u);

    // The probe's encoders in the session's catalog: README WA44's wiring of session/encoder_catalog.cpp lists them
    // there itself; this puts them first either way.
    im::session::SetCatalogPrefixForTest(im::encode::CatalogEntries());
    im::testhooks::Options o;
    o.pattern = im::session::Pattern::kMoving;
    o.pattern_width = kWidth;
    o.pattern_height = kHeight;
    o.pattern_hz = kHz;
    im::testhooks::Configure(o);
  }

  void TearDown() override {
    im::session::SetCatalogPrefixForTest({});
    if (initialized_) {
      im_shutdown();
    }
    im::testhooks::Reset();
  }

  // Runs one session for `seconds`, stepping f's bitrate at step_at_s (0 = no step); returns f's packets.
  std::vector<Packet> Run(uint32_t profile, double seconds, double step_at_s, int64_t *step_ns) {
    im_start_config c{};
    c.struct_size = sizeof c;
    c.flags = IM_START_ALLOW_LISTED_SOURCE;
    c.source_id = "test:pattern";
    c.profile = profile;
    c.content_hint = IM_CONTENT_AUTO;
    c.encoder = IM_ENC_MF_SW;
    c.layer_count = 1;
    std::memcpy(c.layers[0].rid, "f", 2);
    c.layers[0].max_pixels = kWidth * kHeight;
    c.layers[0].fps = kHz;
    c.layers[0].bitrate_bps = kStartBps;
    c.audio.mode = IM_AUDIO_MODE_OFF;
    im_session s = 0;
    im_session_info info{};
    info.struct_size = sizeof info;
    const im_status st = im_start(&c, &s, &info);
    EXPECT_EQ(st, IM_OK) << LastError();
    std::vector<Packet> out;
    if (st != IM_OK) {
      return out;
    }
    EXPECT_EQ(info.layers[0].encoder, IM_ENC_MF_SW);
    EXPECT_EQ(info.layers[0].width, kWidth);
    EXPECT_EQ(info.layers[0].height, kHeight);
    const auto start = Clock::now();
    const auto end = start + std::chrono::milliseconds(static_cast<int64_t>(seconds * 1000));
    bool stepped = step_at_s <= 0;
    while (Clock::now() < end) {
      if (!stepped && Clock::now() >= start + std::chrono::milliseconds(static_cast<int64_t>(step_at_s * 1000))) {
        EXPECT_EQ(im_set_bitrate(s, 0, kStepBps), IM_OK) << LastError();
        *step_ns = EngineNow();
        stepped = true;
      }
      im_packet p{};
      p.struct_size = sizeof p;
      const im_status r = im_next_packet(s, 50, &p);
      if (r == IM_ENDED) {
        ADD_FAILURE() << "the session ended early";
        break;
      }
      if (r != IM_OK) {
        continue;
      }
      if (p.kind == IM_PACKET_EVENT) {
        EXPECT_NE(p.event, IM_EVENT_ENDED) << "status " << p.status;
        continue;
      }
      if (p.kind != IM_PACKET_VIDEO || p.layer != 0) {
        continue;
      }
      Packet k;
      k.flags = p.flags;
      k.profile = p.profile;
      k.ts_ns = p.capture_ts_ns;
      k.data.assign(p.data, p.data + p.size);
      out.push_back(std::move(k));
    }
    EXPECT_EQ(im_stop(s), IM_OK);
    return out;
  }

  std::string dir_;
  bool initialized_ = false;
};

// Every access unit: keyframes are SPS, PPS, then IDR slices only; others carry no SPS, PPS or IDR slice.
void CheckStream(const std::vector<Packet> &pkts, uint32_t profile) {
  ASSERT_FALSE(pkts.empty()) << "no video packet";
  EXPECT_EQ(pkts.front().flags & (IM_PKT_KEYFRAME | IM_PKT_REINIT), IM_PKT_KEYFRAME | IM_PKT_REINIT)
      << "the first packet is KEYFRAME|REINIT";
  const uint8_t profile_idc = profile == IM_PROFILE_CB ? 66 : 100;
  for (std::size_t i = 0; i < pkts.size(); ++i) {
    const Packet &p = pkts[i];
    EXPECT_EQ(p.profile, profile) << "packet " << i;
    std::vector<uint8_t> types;
    im::h264::ForEachNal(std::span<const uint8_t>(p.data), [&](std::span<const uint8_t> nal) {
      const uint8_t t = im::h264::NalType(nal);
      types.push_back(t);
      if (t == im::h264::nal::kSps) {
        im::h264::Sps sps;
        EXPECT_EQ(im::h264::ParseSps(nal, &sps), im::h264::SpsError::kOk) << "packet " << i;
        EXPECT_EQ(sps.profile_idc, profile_idc) << "packet " << i;
        if (profile == IM_PROFILE_CB) {
          EXPECT_EQ(sps.constraint_flags, 0xE0) << "packet " << i;
        }
      }
      return true;
    });
    ASSERT_FALSE(types.empty()) << "packet " << i << " holds no NAL unit";
    const bool key = (p.flags & IM_PKT_KEYFRAME) != 0;
    if (key) {
      ASSERT_GE(types.size(), 3u) << "packet " << i;
      EXPECT_EQ(types[0], im::h264::nal::kSps) << "packet " << i;
      EXPECT_EQ(types[1], im::h264::nal::kPps) << "packet " << i;
      for (std::size_t k = 2; k < types.size(); ++k) {
        EXPECT_EQ(types[k], im::h264::nal::kIdr) << "packet " << i << " unit " << k;
      }
    } else {
      for (const uint8_t t : types) {
        EXPECT_TRUE(t == im::h264::nal::kSlice) << "packet " << i << ": NAL type " << int{t} << " outside a keyframe";
      }
    }
  }
}

TEST_F(MfSoftware, HighSession) {
  int64_t step_ns = 0;
  const std::vector<Packet> pkts = Run(IM_PROFILE_HIGH, 3.0, 0, &step_ns);
  CheckStream(pkts, IM_PROFILE_HIGH);
  std::printf("mf_sw_test: High: %zu packets in 3 s\n", pkts.size());
  EXPECT_GE(pkts.size(), static_cast<std::size_t>(kHz * 3 / 2)) << "at least half the frame rate";
}

TEST_F(MfSoftware, ConstrainedBaselineSession) {
  int64_t step_ns = 0;
  const std::vector<Packet> pkts = Run(IM_PROFILE_CB, 3.0, 0, &step_ns);
  CheckStream(pkts, IM_PROFILE_CB);
}

TEST_F(MfSoftware, BitrateStepIsOneReinit) {
  int64_t step_ns = 0;
  const std::vector<Packet> pkts = Run(IM_PROFILE_HIGH, 4.5, 1.5, &step_ns);
  CheckStream(pkts, IM_PROFILE_HIGH);
  ASSERT_GT(step_ns, 0);
  // The packets after the step: exactly one REINIT|KEYFRAME (the first re-init isn't held back by the 10 s spacing,
  // 04 §9.5), then the rate over the 2 s after it.
  std::size_t reinit = pkts.size();
  int reinits = 0;
  for (std::size_t i = 1; i < pkts.size(); ++i) {
    if ((pkts[i].flags & IM_PKT_REINIT) != 0) {
      ++reinits;
      EXPECT_NE(pkts[i].flags & IM_PKT_KEYFRAME, 0u) << "a REINIT packet is a keyframe";
      if (reinit == pkts.size()) {
        reinit = i;
      }
    }
  }
  EXPECT_EQ(reinits, 1) << "one bitrate step, one re-init";
  ASSERT_LT(reinit, pkts.size());
  // A frame captured up to one interval before the step may be the first one the re-opened encoder takes.
  EXPECT_GE(pkts[reinit].ts_ns + 1'000'000'000LL / kHz, step_ns) << "the re-init follows the step";
  uint64_t bytes = 0;
  const int64_t window_end = pkts[reinit].ts_ns + 2'000'000'000LL;
  bool covered = false;
  for (std::size_t i = reinit; i < pkts.size(); ++i) {
    if (pkts[i].ts_ns >= window_end) {
      covered = true;
      break;
    }
    bytes += pkts[i].data.size();
  }
  ASSERT_TRUE(covered) << "the run ended before 2 s after the re-init";
  const double bps = static_cast<double>(bytes) * 8.0 / 2.0;
  std::printf("mf_sw_test: after the step to %u bit/s: %.0f bit/s over 2 s\n", kStepBps, bps);
  EXPECT_GE(bps, kStepBps * 0.75);
  EXPECT_LE(bps, kStepBps * 1.25);
}

} // namespace

#else

TEST(MfSoftware, NeedsTestHooks) { GTEST_SKIP() << "mf_sw_test needs an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS
