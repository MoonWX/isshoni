// openh264_test (docs/m2/04-windows-engine.md §9.9, §18.2; README WA51): Cisco's real OpenH264 DLL, fetched by the
// test, through the engine (a win-x64-ci test binary, IM_TEST_HOOKS).
//
// The DLL: the test downloads encode/openh264_pin.h's URL over HTTPS (WinHTTP; the windows-native job has outbound
// network for it, 06 §5.1), decompresses it with its own bzip2 decoder (openh264_fetch.h) bounded at the pinned size
// + 1, and checks the pinned size and SHA-256: a mismatch fails, wherever it runs. A verified copy is kept in the
// build folder (<build>/windows/openh264/) and checked again on the next run instead of downloading. Without network
// the cases are skipped, except in CI (CI=true), where they fail.
//
// It asserts:
//   - the download has the pinned size and SHA-256, and a tampered copy (one bit flipped) is rejected by the same
//     check and leaves nothing behind (the CI fetch's own guard; the app's installer is Go's InstallOpenH264, whose
//     tampered, short, long and bomb cases run in internal/media/native's tests);
//   - im_set_openh264_path: a missing file is IM_ERR_DEPENDENCY "missing"; the real DLL with the expected version moved
//     (the test hook) is IM_ERR_DEPENDENCY "version" and leaves OpenH264 off; the real one loads (im_build_info:
//     loaded, "2.7.0"); NULL turns it off again;
//   - a full probe tests it: IM_OK, available, Constrained Baseline only, live bitrate (the probe's live check saw no
//     IDR), software (no adapter);
//   - test:pattern 1280×720@30 for 3 s through the whole session (the pattern drawn on WARP, converted, read back,
//     encoded by OpenH264) and back through OpenH264's decoder: PSNR ≥ 35 dB on the pattern's static half (its bars
//     and ramp, against gpu/cpu_model.h's BT.709 luma of pattern_layout.h's pixels); every packet's profile key 42e0
//     and the SPS's profile_idc 66 with constraint byte 0xE0; the first packet KEYFRAME|REINIT, every keyframe SPS,
//     PPS, then IDR slices only, and no SPS, PPS or IDR slice outside keyframes;
//   - a live bitrate step (1 Mbit/s → 500 kbit/s at 1.5 s) brings no REINIT or KEYFRAME packet in the 2 s after it,
//     and the bytes of those 2 s are within ±25 % of the new target.
#include <gtest/gtest.h>

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS

#include <windows.h>

#include <winhttp.h>

#include <algorithm>
#include <chrono>
#include <cmath>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <filesystem>
#include <fstream>
#include <iterator>
#include <memory>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "encode/openh264_api.h"
#include "encode/openh264_pin.h"
#include "gpu/cpu_model.h"
#include "gpu/pattern_layout.h"
#include "im/h264/nal.h"
#include "im/h264/sps.h"
#include "isshoni_media.h"
#include "openh264_fetch.h"
#include "session/sources.h"
#include "testhooks/testhooks.h"

#pragma comment(lib, "winhttp.lib") // the test's own download; the engine never downloads (04 §9.9)

namespace fs = std::filesystem;
namespace pin = im::encode::openh264;
using im::openh264_test::Bzip2Decompress;
using im::openh264_test::Bzip2Result;
using im::openh264_test::Sha256;

namespace {

using Clock = std::chrono::steady_clock;

constexpr uint32_t kWidth = 1280;
constexpr uint32_t kHeight = 720;
constexpr uint32_t kHz = 30;
constexpr uint32_t kStartBps = 1'000'000;
constexpr uint32_t kStepBps = 500'000;
constexpr double kMinPsnr = 35.0;
constexpr std::size_t kMaxCompressed = 10u << 20; // the app's limit (04 §9.9 step 1)

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

bool InCi() { return GetEnv(L"CI").value_or(L"") == L"true"; }

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

std::string Utf8(const fs::path &p) {
  const std::u8string u = p.u8string();
  return std::string(u.begin(), u.end());
}

std::wstring Widen(std::string_view s) {
  const int n = MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()), nullptr, 0);
  std::wstring w(static_cast<std::size_t>(std::max(n, 0)), L'\0');
  MultiByteToWideChar(CP_UTF8, 0, s.data(), static_cast<int>(s.size()), w.data(), n);
  return w;
}

std::vector<uint8_t> ReadAll(const fs::path &p) {
  std::ifstream f(p, std::ios::binary);
  return std::vector<uint8_t>(std::istreambuf_iterator<char>(f), {});
}

// The pinned size and hash of the decompressed DLL.
bool MatchesPin(std::span<const uint8_t> dll, std::string *why) {
  if (dll.size() != pin::kOpenH264Size) {
    *why = "size " + std::to_string(dll.size()) + ", want " + std::to_string(pin::kOpenH264Size);
    return false;
  }
  const std::string got = Sha256::Of(dll);
  if (got != pin::kOpenH264Sha256) {
    *why = "SHA-256 " + got + ", want " + pin::kOpenH264Sha256;
    return false;
  }
  return true;
}

// Writes dll into dir as the pinned file name only when it matches the pin (temp file, then rename); a mismatch
// writes nothing.
bool InstallVerified(std::span<const uint8_t> dll, const fs::path &dir, fs::path *out, std::string *why) {
  if (!MatchesPin(dll, why)) {
    return false;
  }
  std::error_code ec;
  fs::create_directories(dir, ec);
  const fs::path tmp = dir / (std::string(pin::kOpenH264FileName) + ".tmp");
  {
    std::ofstream f(tmp, std::ios::binary | std::ios::trunc);
    f.write(reinterpret_cast<const char *>(dll.data()), static_cast<std::streamsize>(dll.size()));
    if (!f) {
      *why = "writing the temp file failed";
      f.close();
      fs::remove(tmp, ec);
      return false;
    }
  }
  const fs::path final_path = dir / pin::kOpenH264FileName;
  fs::rename(tmp, final_path, ec);
  if (ec) {
    *why = "rename: " + ec.message();
    fs::remove(tmp, ec);
    return false;
  }
  *out = final_path;
  return true;
}

// GET url over WinHTTP into *body, at most kMaxCompressed bytes.
bool Download(const char *url, std::vector<uint8_t> *body, std::string *why) {
  const std::wstring wurl = Widen(url);
  URL_COMPONENTS uc{};
  uc.dwStructSize = sizeof uc;
  wchar_t host[256] = {};
  wchar_t path[1024] = {};
  uc.lpszHostName = host;
  uc.dwHostNameLength = static_cast<DWORD>(std::size(host));
  uc.lpszUrlPath = path;
  uc.dwUrlPathLength = static_cast<DWORD>(std::size(path));
  if (!WinHttpCrackUrl(wurl.c_str(), 0, 0, &uc)) {
    *why = "WinHttpCrackUrl failed";
    return false;
  }
  struct Handle {
    HINTERNET h = nullptr;
    ~Handle() {
      if (h != nullptr) {
        WinHttpCloseHandle(h);
      }
    }
  };
  Handle inet{WinHttpOpen(L"isshoni-openh264-test", WINHTTP_ACCESS_TYPE_AUTOMATIC_PROXY, WINHTTP_NO_PROXY_NAME,
                          WINHTTP_NO_PROXY_BYPASS, 0)};
  if (inet.h == nullptr) {
    *why = "WinHttpOpen failed (" + std::to_string(GetLastError()) + ")";
    return false;
  }
  WinHttpSetTimeouts(inet.h, 10000, 10000, 30000, 30000);
  Handle conn{WinHttpConnect(inet.h, host, uc.nPort, 0)};
  const DWORD flags = uc.nScheme == INTERNET_SCHEME_HTTPS ? WINHTTP_FLAG_SECURE : 0;
  Handle request{conn.h == nullptr ? nullptr
                                   : WinHttpOpenRequest(conn.h, L"GET", path, nullptr, WINHTTP_NO_REFERER,
                                                        WINHTTP_DEFAULT_ACCEPT_TYPES, flags)};
  if (request.h == nullptr ||
      !WinHttpSendRequest(request.h, WINHTTP_NO_ADDITIONAL_HEADERS, 0, WINHTTP_NO_REQUEST_DATA, 0, 0, 0) ||
      !WinHttpReceiveResponse(request.h, nullptr)) {
    *why = "the request failed (" + std::to_string(GetLastError()) + ")";
    return false;
  }
  DWORD status = 0;
  DWORD size = sizeof status;
  if (!WinHttpQueryHeaders(request.h, WINHTTP_QUERY_STATUS_CODE | WINHTTP_QUERY_FLAG_NUMBER,
                           WINHTTP_HEADER_NAME_BY_INDEX, &status, &size, WINHTTP_NO_HEADER_INDEX) ||
      status != 200) {
    *why = "HTTP status " + std::to_string(status);
    return false;
  }
  body->clear();
  for (;;) {
    char buf[64 * 1024];
    DWORD got = 0;
    if (!WinHttpReadData(request.h, buf, static_cast<DWORD>(sizeof buf), &got)) {
      *why = "reading the body failed (" + std::to_string(GetLastError()) + ")";
      return false;
    }
    if (got == 0) {
      return true;
    }
    if (body->size() + got > kMaxCompressed) {
      *why = "the compressed body is over 10 MiB";
      return false;
    }
    body->insert(body->end(), buf, buf + got);
  }
}

// The verified DLL: the cached copy when it still matches, else a fresh download.
struct Fetched {
  fs::path path;
  std::vector<uint8_t> dll;
  std::string why; // empty on success
  bool network = true;
};

Fetched Fetch() {
  Fetched f;
  const fs::path dir = fs::path(IM_TEST_OUT_DIR) / "openh264";
  const fs::path cached = dir / pin::kOpenH264FileName;
  std::error_code ec;
  if (fs::exists(cached, ec)) {
    f.dll = ReadAll(cached);
    std::string why;
    if (MatchesPin(f.dll, &why)) {
      f.path = cached;
      std::printf("openh264_test: the cached download matches the pin\n");
      return f;
    }
    std::printf("openh264_test: the cached copy doesn't match the pin (%s); downloading again\n", why.c_str());
    fs::remove(cached, ec);
  }
  std::vector<uint8_t> bz;
  if (!Download(pin::kOpenH264Url, &bz, &f.why)) {
    f.network = false;
    return f;
  }
  // The decompressed bytes are bounded at the pinned size + 1, like the app's installer (04 §9.9 step 2).
  if (const Bzip2Result r = Bzip2Decompress(bz, &f.dll, pin::kOpenH264Size + 1); r != Bzip2Result::kOk) {
    f.why = r == Bzip2Result::kTooLarge ? "the download decompresses to more than the pinned size"
                                        : "the download is not a valid bzip2 stream";
    return f;
  }
  if (!InstallVerified(f.dll, dir, &f.path, &f.why)) {
    return f;
  }
  std::printf("openh264_test: downloaded %zu compressed bytes; the DLL matches the pin\n", bz.size());
  return f;
}

// The video packets of layer f.
struct Packet {
  uint32_t flags = 0;
  uint32_t profile = 0;
  int64_t ts_ns = 0;
  std::vector<uint8_t> data;
};

int64_t EngineNow() {
  im_clock_sample c{};
  c.struct_size = sizeof c;
  im_clock(&c);
  return c.mono_ns;
}

class OpenH264 : public ::testing::Test {
protected:
  static void SetUpTestSuite() { fetched_ = new Fetched(Fetch()); }
  static void TearDownTestSuite() {
    delete fetched_;
    fetched_ = nullptr;
  }

  void SetUp() override {
    ASSERT_NE(fetched_, nullptr);
    if (!fetched_->network) {
      if (InCi()) {
        FAIL() << "downloading " << pin::kOpenH264Url << " failed: " << fetched_->why
               << " (the windows-native job needs outbound network for openh264_test)";
      }
      GTEST_SKIP() << "no download of Cisco's OpenH264: " << fetched_->why;
    }
    ASSERT_TRUE(fetched_->why.empty()) << "Cisco's download is not the pinned release: " << fetched_->why;
    path_ = Utf8(fetched_->path);
    im::testhooks::Reset();
    pin::ResetForTest();
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO;
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
    initialized_ = true;
  }

  void TearDown() override {
    if (initialized_) {
      im_shutdown();
    }
    pin::ResetForTest();
    im::testhooks::Reset();
  }

  static std::string BuildInfo() {
    std::string doc(64 * 1024, '\0');
    const int32_t n = im_build_info(doc.data(), static_cast<int32_t>(doc.size()));
    doc.resize(n > 0 ? static_cast<std::size_t>(n) : 0);
    return doc;
  }

  void TurnOn() {
    ASSERT_EQ(im_set_openh264_path(path_.c_str()), IM_OK) << LastError();
    im::testhooks::Options o;
    o.pattern = im::session::Pattern::kMoving;
    o.pattern_width = kWidth;
    o.pattern_height = kHeight;
    o.pattern_hz = kHz;
    im::testhooks::Configure(o);
  }

  // One session of `seconds` forced to OpenH264 (Constrained Baseline), stepping f's bitrate at step_at_s (0 = no
  // step); returns f's packets.
  std::vector<Packet> Run(double seconds, double step_at_s, int64_t *step_ns) {
    im_start_config c{};
    c.struct_size = sizeof c;
    c.flags = IM_START_ALLOW_LISTED_SOURCE;
    c.source_id = "test:pattern";
    c.profile = IM_PROFILE_CB;
    c.content_hint = IM_CONTENT_AUTO;
    c.encoder = IM_ENC_OPENH264;
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
    EXPECT_EQ(info.layers[0].encoder, IM_ENC_OPENH264);
    EXPECT_EQ(info.layers[0].profile, IM_PROFILE_CB);
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

  static Fetched *fetched_;
  std::string path_;
  bool initialized_ = false;
};

Fetched *OpenH264::fetched_ = nullptr;

// Keyframes are SPS, PPS, then IDR slices only; other AUs carry no SPS, PPS or IDR slice; every SPS is CB's.
void CheckStream(const std::vector<Packet> &pkts) {
  ASSERT_FALSE(pkts.empty()) << "no video packet";
  EXPECT_EQ(pkts.front().flags & (IM_PKT_KEYFRAME | IM_PKT_REINIT), IM_PKT_KEYFRAME | IM_PKT_REINIT)
      << "the first packet is KEYFRAME|REINIT";
  for (std::size_t i = 0; i < pkts.size(); ++i) {
    const Packet &p = pkts[i];
    EXPECT_EQ(p.profile, IM_PROFILE_CB) << "packet " << i << ": the profile key is 42e0";
    if (i > 0) {
      EXPECT_GT(p.ts_ns, pkts[i - 1].ts_ns) << "packet " << i;
    }
    std::vector<uint8_t> types;
    im::h264::ForEachNal(std::span<const uint8_t>(p.data), [&](std::span<const uint8_t> nal) {
      const uint8_t t = im::h264::NalType(nal);
      types.push_back(t);
      if (t == im::h264::nal::kSps) {
        im::h264::Sps sps;
        EXPECT_EQ(im::h264::ParseSps(nal, &sps), im::h264::SpsError::kOk) << "packet " << i;
        EXPECT_EQ(sps.profile_idc, 66) << "packet " << i;
        EXPECT_EQ(sps.constraint_flags, 0xE0) << "packet " << i;
        EXPECT_EQ(sps.width, kWidth) << "packet " << i;
        EXPECT_EQ(sps.height, kHeight) << "packet " << i;
      }
      return true;
    });
    ASSERT_FALSE(types.empty()) << "packet " << i << " holds no NAL unit";
    if ((p.flags & IM_PKT_KEYFRAME) != 0) {
      ASSERT_GE(types.size(), 3u) << "packet " << i;
      EXPECT_EQ(types[0], im::h264::nal::kSps) << "packet " << i << ": SPS first";
      EXPECT_EQ(types[1], im::h264::nal::kPps) << "packet " << i << ": then PPS";
      for (std::size_t k = 2; k < types.size(); ++k) {
        EXPECT_EQ(types[k], im::h264::nal::kIdr) << "packet " << i << " unit " << k;
      }
    } else {
      for (const uint8_t t : types) {
        EXPECT_EQ(t, im::h264::nal::kSlice) << "packet " << i << ": NAL type " << int{t} << " outside a keyframe";
      }
    }
  }
}

// OpenH264's decoder over the packets: the last decoded picture's luma (kWidth × kHeight), and how many pictures came
// out.
struct Decoded {
  std::vector<uint8_t> y;
  int pictures = 0;
  int errors = 0;
};

Decoded DecodeAll(const std::vector<Packet> &pkts) {
  Decoded d;
  std::shared_ptr<const pin::Library> lib;
  if (pin::Acquire(&lib) != IM_OK || lib->create_decoder == nullptr) {
    ADD_FAILURE() << "OpenH264's decoder is not available: " << LastError();
    return d;
  }
  ISVCDecoder *dec = nullptr;
  if (lib->create_decoder(&dec) != 0 || dec == nullptr) {
    ADD_FAILURE() << "WelsCreateDecoder failed";
    return d;
  }
  SDecodingParam param{};
  param.eEcActiveIdc = ERROR_CON_DISABLE;
  param.sVideoProperty.size = sizeof param.sVideoProperty;
  param.sVideoProperty.eVideoBsType = VIDEO_BITSTREAM_AVC;
  EXPECT_EQ(dec->Initialize(&param), 0);
  for (const Packet &p : pkts) {
    unsigned char *planes[3] = {};
    SBufferInfo bi{};
    const DECODING_STATE st = dec->DecodeFrameNoDelay(p.data.data(), static_cast<int>(p.data.size()), planes, &bi);
    if (st != dsErrorFree) {
      ++d.errors;
      continue;
    }
    if (bi.iBufferStatus != 1 || planes[0] == nullptr) {
      continue;
    }
    ++d.pictures;
    const SSysMEMBuffer &b = bi.UsrData.sSystemBuffer;
    if (b.iWidth != static_cast<int>(kWidth) || b.iHeight != static_cast<int>(kHeight)) {
      ADD_FAILURE() << "decoded " << b.iWidth << "x" << b.iHeight;
      continue;
    }
    d.y.resize(static_cast<std::size_t>(kWidth) * kHeight);
    for (uint32_t row = 0; row < kHeight; ++row) {
      std::memcpy(d.y.data() + static_cast<std::size_t>(row) * kWidth,
                  planes[0] + static_cast<std::ptrdiff_t>(row) * b.iStride[0], kWidth);
    }
  }
  dec->Uninitialize();
  lib->destroy_decoder(dec);
  return d;
}

// The PSNR of y's static half (rows [0, H/2): the color bars and the ramp) against the pattern's model.
double StaticPsnr(const std::vector<uint8_t> &y) {
  const im::gpu::PatternLayout l = im::gpu::MakePatternLayout(kWidth, kHeight);
  double se = 0;
  std::size_t n = 0;
  for (uint32_t row = 0; row < l.ramp_end; ++row) {
    for (uint32_t x = 0; x < kWidth; ++x) {
      const uint32_t argb = im::gpu::PatternPixel(l, im::gpu::PatternFrame{}, x, row);
      const im::gpu::model::Rgb rgb{static_cast<double>((argb >> 16) & 0xFF) / 255.0,
                                    static_cast<double>((argb >> 8) & 0xFF) / 255.0,
                                    static_cast<double>(argb & 0xFF) / 255.0};
      const double want = im::gpu::model::Code(im::gpu::model::ToYcc(rgb).y);
      const double diff = static_cast<double>(y[static_cast<std::size_t>(row) * kWidth + x]) - want;
      se += diff * diff;
      ++n;
    }
  }
  const double mse = se / static_cast<double>(n);
  return mse <= 0 ? 99.0 : 10.0 * std::log10(255.0 * 255.0 / mse);
}

TEST_F(OpenH264, DownloadHasThePinnedHash) {
  std::string why;
  EXPECT_TRUE(MatchesPin(fetched_->dll, &why)) << why;
  // A tampered copy fails the same check and leaves nothing behind.
  std::vector<uint8_t> tampered = fetched_->dll;
  tampered[tampered.size() / 2] ^= 0x01;
  const fs::path dir = fs::path(IM_TEST_OUT_DIR) / "openh264-tampered";
  std::error_code ec;
  fs::remove_all(dir, ec);
  fs::path out;
  EXPECT_FALSE(InstallVerified(tampered, dir, &out, &why));
  EXPECT_NE(why.find("SHA-256"), std::string::npos) << why;
  EXPECT_TRUE(!fs::exists(dir, ec) || fs::is_empty(dir, ec)) << "a rejected copy left a file behind";
  fs::remove_all(dir, ec);
}

TEST_F(OpenH264, SetPathChecksTheFileAndTheVersion) {
  // Off by default: build info says not loaded.
  EXPECT_NE(BuildInfo().find("\"openh264\":{\"loaded\":false"), std::string::npos) << BuildInfo();
  // A missing file.
  const std::string missing = Utf8(fs::path(IM_TEST_OUT_DIR) / "openh264" / "missing-openh264.dll");
  EXPECT_EQ(im_set_openh264_path(missing.c_str()), IM_ERR_DEPENDENCY);
  EXPECT_EQ(LastError().rfind("missing", 0), 0u) << LastError();
  // Another version: the real DLL against a moved expectation.
  pin::SetExpectedVersionForTest(pin::kPinMajor, pin::kPinMinor + 1, 0);
  EXPECT_EQ(im_set_openh264_path(path_.c_str()), IM_ERR_DEPENDENCY);
  EXPECT_EQ(LastError().rfind("version", 0), 0u) << LastError();
  EXPECT_NE(BuildInfo().find("\"openh264\":{\"loaded\":false"), std::string::npos) << BuildInfo();
  // The probe still says off.
  im_caps caps{};
  caps.struct_size = sizeof caps;
  ASSERT_EQ(im_probe(IM_PROBE_QUICK, &caps), IM_OK) << LastError();
  bool seen = false;
  for (uint32_t i = 0; i < caps.encoder_count; ++i) {
    if (caps.encoders[i].kind == IM_ENC_OPENH264) {
      seen = true;
      EXPECT_EQ(caps.encoders[i].status, IM_ERR_DEPENDENCY);
      EXPECT_STREQ(caps.encoders[i].reason, "disabled");
    }
  }
  EXPECT_TRUE(seen);
  // The pinned version loads.
  pin::SetExpectedVersionForTest(pin::kPinMajor, pin::kPinMinor, pin::kPinRevision);
  ASSERT_EQ(im_set_openh264_path(path_.c_str()), IM_OK) << LastError();
  EXPECT_NE(BuildInfo().find("\"openh264\":{\"loaded\":true,\"version\":\"" IM_OPENH264_VERSION "\""),
            std::string::npos)
      << BuildInfo();
  // NULL turns it off.
  ASSERT_EQ(im_set_openh264_path(nullptr), IM_OK) << LastError();
  EXPECT_NE(BuildInfo().find("\"openh264\":{\"loaded\":false"), std::string::npos) << BuildInfo();
}

TEST_F(OpenH264, FullProbeTestsIt) {
  TurnOn();
  im_caps caps{};
  caps.struct_size = sizeof caps;
  ASSERT_EQ(im_probe(IM_PROBE_FULL, &caps), IM_OK) << LastError();
  const im_encoder_info *e = nullptr;
  for (uint32_t i = 0; i < caps.encoder_count; ++i) {
    if (caps.encoders[i].kind == IM_ENC_OPENH264) {
      e = &caps.encoders[i];
    }
  }
  ASSERT_NE(e, nullptr);
  EXPECT_EQ(e->status, IM_OK) << e->reason;
  EXPECT_NE(e->flags & IM_ENCF_AVAILABLE, 0u);
  EXPECT_NE(e->flags & IM_ENCF_CB, 0u);
  EXPECT_EQ(e->flags & IM_ENCF_HIGH, 0u) << "OpenH264 is CB only (04 §9.1)";
  EXPECT_NE(e->flags & IM_ENCF_LIVE_BITRATE, 0u) << "the probe's live-bitrate check saw an IDR";
  EXPECT_EQ(e->flags & IM_ENCF_HARDWARE, 0u);
  EXPECT_EQ(e->adapter_index, 0xFFFFFFFFu);
  EXPECT_STREQ(e->codec, "openh264");
  EXPECT_GT(e->test_encode_us, 0u);
  std::printf("openh264_test: probe test encode %u us\n", e->test_encode_us);
  // A quick probe afterwards keeps the full probe's result.
  im_caps quick{};
  quick.struct_size = sizeof quick;
  ASSERT_EQ(im_probe(IM_PROBE_QUICK, &quick), IM_OK) << LastError();
  for (uint32_t i = 0; i < quick.encoder_count; ++i) {
    if (quick.encoders[i].kind == IM_ENC_OPENH264) {
      EXPECT_NE(quick.encoders[i].flags & IM_ENCF_AVAILABLE, 0u);
    }
  }
}

TEST_F(OpenH264, SessionEncodesThePattern) {
  TurnOn();
  int64_t step_ns = 0;
  const std::vector<Packet> pkts = Run(3.0, 0, &step_ns);
  CheckStream(pkts);
  std::printf("openh264_test: %zu packets in 3 s\n", pkts.size());
  EXPECT_GE(pkts.size(), static_cast<std::size_t>(kHz * 3 / 2)) << "at least half the frame rate";
  const Decoded d = DecodeAll(pkts);
  EXPECT_EQ(d.errors, 0);
  EXPECT_GE(d.pictures, static_cast<int>(pkts.size()) - 1) << "every packet decodes to a picture";
  ASSERT_FALSE(d.y.empty()) << "nothing decoded";
  const double psnr = StaticPsnr(d.y);
  std::printf("openh264_test: PSNR of the static half after 3 s: %.2f dB\n", psnr);
  EXPECT_GE(psnr, kMinPsnr);
}

TEST_F(OpenH264, LiveBitrateStep) {
  TurnOn();
  int64_t step_ns = 0;
  const std::vector<Packet> pkts = Run(4.5, 1.5, &step_ns);
  CheckStream(pkts);
  ASSERT_GT(step_ns, 0);
  // Live (04 §9.5): no REINIT or KEYFRAME from the step until 2 s after it (the first periodic IDR comes at f's 4 s
  // GOP, after that window).
  const int64_t window_end = step_ns + 2'000'000'000LL;
  uint64_t bytes = 0;
  bool covered = false;
  for (const Packet &p : pkts) {
    if (p.ts_ns < step_ns) {
      continue;
    }
    if (p.ts_ns >= window_end) {
      covered = true;
      break;
    }
    EXPECT_EQ(p.flags & (IM_PKT_REINIT | IM_PKT_KEYFRAME), 0u) << "a live bitrate change brings no IDR";
    bytes += p.data.size();
  }
  ASSERT_TRUE(covered) << "the run ended before 2 s after the step";
  const double bps = static_cast<double>(bytes) * 8.0 / 2.0;
  std::printf("openh264_test: after the step to %u bit/s: %.0f bit/s over 2 s\n", kStepBps, bps);
  EXPECT_GE(bps, kStepBps * 0.75);
  EXPECT_LE(bps, kStepBps * 1.25);
}

} // namespace

#else

TEST(OpenH264, NeedsTestHooks) { GTEST_SKIP() << "openh264_test needs an IM_TEST_HOOKS build (win-x64-ci)"; }

#endif // IM_TEST_HOOKS
