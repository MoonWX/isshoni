#include "encode/probe.h"

#include <windows.h>

#include <d3d11.h>
#include <dxgi1_6.h>
#include <mfapi.h>
#include <wrl/client.h>

#include <algorithm>
#include <array>
#include <cstdint>
#include <cstdio>
#include <iterator>
#include <memory>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "audio_bridge/audio_bridge.h"
#include "encode/encode.h"
#include "encode/ffmpeg_api.h"
#include "encode/ffmpeg_encoder.h"
#include "encode/selector.h"
#include "engine/delayload.h"
#include "engine/os.h"
#include "gpu/device.h"
#include "im/abi/status.h"
#include "im/abi/utf8.h"
#include "im/encpolicy/candidates.h"
#include "im/encpolicy/encoders.h"
#include "im/h264/nal.h"
#include "im/log/ring.h"
#include "session/session.h"

namespace im::encode {
namespace {

using Microsoft::WRL::ComPtr;

constexpr uint32_t kVendorNvidia = 0x10DE;
constexpr uint32_t kVendorIntel = 0x8086;
constexpr uint32_t kVendorAmd = 0x1002;
constexpr uint32_t kVendorAmdAlt = 0x1022;
constexpr uint32_t kMinSupportedBuild = 19045; // Windows 10 22H2
constexpr uint32_t kBuildWgc = 18362;          // GraphicsCaptureSession::IsSupported() (1903)
constexpr uint32_t kBuild2004 = 19041;         // IsCursorCaptureEnabled, WDA_EXCLUDEFROMCAPTURE
constexpr uint32_t kBuild24H2 = 26100;         // MinUpdateInterval, IncludeSecondaryWindows

int64_t NowNs() noexcept { return engine::os::QpcNs(); }

// ---------------------------------------------------------------------------------------------------- 1. the OS

struct OsVersion {
  uint32_t build = 0;
  uint32_t ubr = 0;
};

OsVersion ReadOsVersion() noexcept {
  OsVersion v;
  // RtlGetVersion: GetVersionEx lies to processes without a compatibility manifest.
  using RtlGetVersionFn = LONG(WINAPI *)(OSVERSIONINFOW *);
  if (const HMODULE ntdll = GetModuleHandleW(L"ntdll.dll")) {
    if (const FARPROC p = GetProcAddress(ntdll, "RtlGetVersion")) {
      OSVERSIONINFOW info{};
      info.dwOSVersionInfoSize = sizeof info;
      if (reinterpret_cast<RtlGetVersionFn>(reinterpret_cast<void *>(p))(&info) == 0) {
        v.build = info.dwBuildNumber;
      }
    }
  }
  // The UBR from the registry, through advapi32 loaded from System32 (no new static import, 06 §5.1).
  using RegGetValueFn = LONG(WINAPI *)(HKEY, LPCWSTR, LPCWSTR, DWORD, LPDWORD, PVOID, LPDWORD);
  if (const HMODULE advapi = LoadLibraryExW(L"advapi32.dll", nullptr, LOAD_LIBRARY_SEARCH_SYSTEM32)) {
    if (const FARPROC p = GetProcAddress(advapi, "RegGetValueW")) {
      DWORD ubr = 0;
      DWORD size = sizeof ubr;
      if (reinterpret_cast<RegGetValueFn>(reinterpret_cast<void *>(p))(
              HKEY_LOCAL_MACHINE, L"SOFTWARE\\Microsoft\\Windows NT\\CurrentVersion", L"UBR", RRF_RT_REG_DWORD, nullptr,
              &ubr, &size) == ERROR_SUCCESS) {
        v.ubr = ubr;
      }
    }
    FreeLibrary(advapi);
  }
  return v;
}

// Whether MFStartup works (not on N editions without the Media Feature Pack): mfplat.dll's delay-loaded imports
// resolve from System32 first (engine/delayload.h), so a missing DLL is an answer, not an exception.
bool MediaFoundationWorks() noexcept {
  if (engine::LoadDelayedImports("mfplat.dll") != S_OK) {
    return false;
  }
  if (FAILED(MFStartup(MF_VERSION, MFSTARTUP_LITE))) {
    return false;
  }
  MFShutdown();
  return true;
}

// Whether any active display path has advanced color (HDR) on.
bool AdvancedColorOn() noexcept {
  try {
    UINT32 paths = 0;
    UINT32 modes = 0;
    if (GetDisplayConfigBufferSizes(QDC_ONLY_ACTIVE_PATHS, &paths, &modes) != ERROR_SUCCESS) {
      return false;
    }
    std::vector<DISPLAYCONFIG_PATH_INFO> p(paths);
    std::vector<DISPLAYCONFIG_MODE_INFO> m(modes);
    if (QueryDisplayConfig(QDC_ONLY_ACTIVE_PATHS, &paths, p.data(), &modes, m.data(), nullptr) != ERROR_SUCCESS) {
      return false;
    }
    for (UINT32 i = 0; i < paths; ++i) {
      DISPLAYCONFIG_GET_ADVANCED_COLOR_INFO info{};
      info.header.type = DISPLAYCONFIG_DEVICE_INFO_GET_ADVANCED_COLOR_INFO;
      info.header.size = sizeof info;
      info.header.adapterId = p[i].targetInfo.adapterId;
      info.header.id = p[i].targetInfo.id;
      if (DisplayConfigGetDeviceInfo(&info.header) == ERROR_SUCCESS && info.advancedColorEnabled) {
        return true;
      }
    }
  } catch (...) {
    // out of memory: no answer
  }
  return false;
}

// Whether DuplicateOutput1 works on at least one output (04 §7.4). It needs a per-monitor DPI-aware process
// (isshoni.exe and isshoni-enginetest are); elsewhere the trial fails and the flag stays clear.
bool DuplicationWorks(std::span<const ComPtr<IDXGIAdapter1>> adapters) noexcept {
  for (const ComPtr<IDXGIAdapter1> &a : adapters) {
    ComPtr<IDXGIOutput> out;
    if (FAILED(a->EnumOutputs(0, &out))) {
      continue;
    }
    ComPtr<ID3D11Device> dev;
    const D3D_FEATURE_LEVEL levels[] = {D3D_FEATURE_LEVEL_11_1, D3D_FEATURE_LEVEL_11_0};
    if (FAILED(D3D11CreateDevice(a.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr, D3D11_CREATE_DEVICE_BGRA_SUPPORT, levels,
                                 static_cast<UINT>(std::size(levels)), D3D11_SDK_VERSION, &dev, nullptr, nullptr))) {
      continue;
    }
    for (UINT i = 0;; ++i) {
      ComPtr<IDXGIOutput> o;
      if (a->EnumOutputs(i, &o) == DXGI_ERROR_NOT_FOUND) {
        break;
      }
      ComPtr<IDXGIOutput5> o5;
      if (FAILED(o.As(&o5))) {
        continue;
      }
      const DXGI_FORMAT formats[] = {DXGI_FORMAT_B8G8R8A8_UNORM};
      ComPtr<IDXGIOutputDuplication> dup;
      if (SUCCEEDED(o5->DuplicateOutput1(dev.Get(), 0, 1, formats, &dup))) {
        return true;
      }
    }
  }
  return false;
}

uint32_t OsFlags(uint32_t build, std::span<const ComPtr<IDXGIAdapter1>> adapters) noexcept {
  uint32_t f = 0;
  if (build >= kMinSupportedBuild) {
    f |= IM_OS_SUPPORTED;
  }
  if (build >= kBuildWgc) {
    // GraphicsCaptureSession::IsSupported() and the picker: README WA50's C++/WinRT asks Windows itself; until then
    // the build that introduced them decides.
    f |= IM_OS_WGC | IM_OS_WGC_PICKER;
  }
  if (build >= kBuild2004) {
    f |= IM_OS_WGC_CURSOR_TOGGLE | IM_OS_EXCLUDE_FROM_CAPTURE;
  }
  if (build >= kBuild24H2) {
    f |= IM_OS_WGC_MIN_INTERVAL | IM_OS_WGC_SECONDARY_WINDOWS;
  }
  // IM_OS_WGC_BORDERLESS needs RequestAccessAsync(Borderless) (C++/WinRT): README WA50.
  if (DuplicationWorks(adapters)) {
    f |= IM_OS_DESKTOP_DUPLICATION;
  }
  if (MediaFoundationWorks()) {
    f |= IM_OS_MEDIA_FOUNDATION;
  }
  if (AdvancedColorOn()) {
    f |= IM_OS_HDR_DISPLAY;
  }
  return f;
}

// --------------------------------------------------------------------------------------------- 2. the adapters

uint64_t LuidValue(const LUID &l) noexcept {
  return (static_cast<uint64_t>(static_cast<uint32_t>(l.HighPart)) << 32) | l.LowPart;
}

// Fills caps.adapters; returns the DXGI adapters in the same order.
std::vector<ComPtr<IDXGIAdapter1>> ProbeAdapters(im_caps &caps) {
  std::vector<ComPtr<IDXGIAdapter1>> out;
  ComPtr<IDXGIFactory1> factory;
  if (FAILED(CreateDXGIFactory1(IID_PPV_ARGS(&factory)))) {
    return out;
  }
  ComPtr<IDXGIFactory6> f6;
  factory.As(&f6); // absent before Windows 10 1803: EnumAdapters1's order
  for (UINT i = 0; out.size() < IM_MAX_ADAPTERS; ++i) {
    ComPtr<IDXGIAdapter1> a;
    const HRESULT hr = f6 ? f6->EnumAdapterByGpuPreference(i, DXGI_GPU_PREFERENCE_UNSPECIFIED, IID_PPV_ARGS(&a))
                          : factory->EnumAdapters1(i, &a);
    if (hr == DXGI_ERROR_NOT_FOUND || FAILED(hr)) {
      break;
    }
    DXGI_ADAPTER_DESC1 desc{};
    if (FAILED(a->GetDesc1(&desc))) {
      continue;
    }
    im_adapter_info &info = caps.adapters[out.size()];
    info = im_adapter_info{};
    info.luid = LuidValue(desc.AdapterLuid);
    info.vendor_id = desc.VendorId;
    info.device_id = desc.DeviceId;
    LARGE_INTEGER umd{};
    if (SUCCEEDED(a->CheckInterfaceSupport(__uuidof(IDXGIDevice), &umd))) {
      info.driver_version = static_cast<uint64_t>(umd.QuadPart);
    }
    const std::wstring_view name(desc.Description);
    abi::CopyFixed(info.name,
                   abi::Utf16ToUtf8(std::u16string_view(reinterpret_cast<const char16_t *>(name.data()), name.size())));
    if ((desc.Flags & DXGI_ADAPTER_FLAG_SOFTWARE) != 0 ||
        (desc.VendorId == gpu::kVendorMicrosoft && desc.DeviceId == gpu::kBasicRenderDriverDevice)) {
      info.flags |= IM_ADAPTER_SOFTWARE;
    }
    ComPtr<IDXGIOutput> o;
    if (SUCCEEDED(a->EnumOutputs(0, &o))) {
      info.flags |= IM_ADAPTER_HAS_OUTPUTS;
    }
    out.push_back(a);
  }
  caps.adapter_count = static_cast<uint32_t>(out.size());
  return out;
}

// ---------------------------------------------------------------------------------------------- 3. the encoders

uint32_t VendorKind(uint32_t vendor) noexcept {
  switch (vendor) {
  case kVendorNvidia:
    return IM_ENC_NVENC;
  case kVendorIntel:
    return IM_ENC_QSV;
  case kVendorAmd:
  case kVendorAmdAlt:
    return IM_ENC_AMF;
  default:
    return IM_ENC_AUTO;
  }
}

struct Planned {
  uint32_t kind = IM_ENC_AUTO;
  uint32_t adapter = encpolicy::kNoAdapter;
};

// 04 §9.1's order without a capture adapter: per hardware adapter its vendor encoder, then MF hardware; then MF
// software and OpenH264 (and the test hooks' fake), which always keep their slots.
std::vector<Planned> PlanEncoders(const im_caps &caps) {
  std::vector<Planned> tail{{IM_ENC_MF_SW, encpolicy::kNoAdapter}, {IM_ENC_OPENH264, encpolicy::kNoAdapter}};
#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
  // The win-x64-ci DLL's sessions on test: sources use the test hooks' fake (04 §18.2); the probe lists it, tested,
  // so Go's Conformance finds the encoder it forces.
  tail.push_back({IM_ENC_FAKE, encpolicy::kNoAdapter});
#endif
  std::vector<Planned> out;
  for (uint32_t i = 0; i < caps.adapter_count; ++i) {
    const im_adapter_info &a = caps.adapters[i];
    if ((a.flags & IM_ADAPTER_SOFTWARE) != 0) {
      continue;
    }
    if (const uint32_t k = VendorKind(a.vendor_id); k != IM_ENC_AUTO) {
      out.push_back({k, i});
    }
    out.push_back({IM_ENC_MF_HW, i});
  }
  const std::size_t room = IM_MAX_ENCODERS - tail.size();
  if (out.size() > room) {
    out.resize(room);
  }
  out.insert(out.end(), tail.begin(), tail.end());
  return out;
}

// The 04 §5.7 token at the start of this thread's detail ("open_failed: …"), or "" when there is none.
std::string_view DetailToken() noexcept {
  const std::string_view text = abi::LastErrorText();
  const auto colon = text.find(':');
  if (colon == std::string_view::npos) {
    return {};
  }
  const std::string_view token = text.substr(0, colon);
  for (const abi::TokenInfo &t : abi::Tokens()) {
    if (t.token == token) {
      return t.token; // a view of the static table, valid after the detail changes
    }
  }
  return {};
}

void SetStatus(im_encoder_info &info, im_status status, std::string_view token) noexcept {
  info.status = status;
  info.flags &= ~IM_ENCF_AVAILABLE;
  abi::CopyFixed(info.reason, status == IM_OK ? std::string_view{} : token);
}

// What one AU holds.
struct AuFacts {
  bool sps = false;
  bool pps = false;
  bool idr = false;
};

AuFacts Inspect(std::span<const uint8_t> au) noexcept {
  AuFacts f;
  h264::ForEachNal(au, [&](std::span<const uint8_t> nal) {
    switch (h264::NalType(nal)) {
    case h264::nal::kSps:
      f.sps = true;
      break;
    case h264::nal::kPps:
      f.pps = true;
      break;
    case h264::nal::kIdr:
      f.idr = true;
      break;
    default:
      break;
    }
    return true;
  });
  return f;
}

// One test session of an encoder: the frames it encoded and the AUs it produced, in order.
class TestRun {
public:
  explicit TestRun(FfmpegEncoder &enc) noexcept : enc_(enc) {}

  // Encodes one mid-gray frame; appends the AU when the encoder produced one. False on an error (detail set).
  bool Frame(bool idr, bool *produced) noexcept {
    *produced = false;
    gpu::Nv12Target t;
    if (enc_.NextTarget(&t) != IM_OK || enc_.FillGray(t) != IM_OK) {
      return false;
    }
    session::EncodeInput in;
    in.ts_ns = ts_;
    in.idr = idr;
    in.picture = &t;
    in.index = index_++;
    ts_ += 1'000'000'000LL / kTestFps;
    session::EncodedAu au;
    if (enc_.Encode(in, &au) != IM_OK) {
      return false;
    }
    if (!au.data.empty()) {
      try {
        aus_.emplace_back(au.data.begin(), au.data.end());
      } catch (...) {
        return false;
      }
      *produced = true;
    }
    return true;
  }

  const std::vector<std::vector<uint8_t>> &aus() const noexcept { return aus_; }

private:
  FfmpegEncoder &enc_;
  int64_t ts_ = 0;
  uint32_t index_ = 0;
  std::vector<std::vector<uint8_t>> aus_;
};

// The most frames an encoder may hold back before its first AU (some MF and QSV configurations hold one).
constexpr int kMaxDelayFrames = 3;

session::EncoderParams TestParams(uint32_t kind, uint32_t profile, gpu::Device *device) noexcept {
  session::EncoderParams p;
  p.layer = 0;
  p.rid = 'f';
  p.kind = kind;
  p.profile = profile;
  p.width = kTestWidth;
  p.height = kTestHeight;
  p.fps = kTestFps;
  p.bitrate_bps = kTestBitrate;
  p.content_hint = IM_CONTENT_AUTO;
  p.epoch_ns = 0;
  p.device = device;
  return p;
}

// Step 3 for one libavcodec encoder on its device: the test encode, the trial opens and the live-bitrate check.
void TestEncoder(im_encoder_info &info, gpu::Device &device) noexcept {
  const encpolicy::EncoderTraits &traits = encpolicy::Traits(info.kind);
  const int64_t t0 = NowNs();
  std::unique_ptr<FfmpegEncoder> enc;
  OpenFacts facts;
  uint32_t profile = IM_PROFILE_HIGH;
  im_status st = FfmpegEncoder::Create(TestParams(info.kind, profile, &device), &enc, &facts);
  if (st != IM_OK && st != IM_ERR_DEPENDENCY && st != IM_ERR_LIMIT && st != IM_ERR_ENCODER_LOST && traits.cb) {
    // An encoder that can't do High may still do CB (04 §9.6: the session then runs CB).
    abi::ClearLastError();
    profile = IM_PROFILE_CB;
    st = FfmpegEncoder::Create(TestParams(info.kind, profile, &device), &enc, &facts);
  }
  if (st != IM_OK) {
    const std::string_view token = DetailToken();
    log::Info("encode", "probe open failed",
              {{"encoder", encpolicy::EncoderToken(info.kind)}, {"status", st}, {"detail", abi::LastErrorText()}});
    SetStatus(info, st, token.empty() ? abi::token::kOpenFailed : token);
    abi::ClearLastError();
    return;
  }
  if (facts.open_us > kSlowOpenUs) {
    enc.reset();
    SetStatus(info, IM_ERR_NO_ENCODER, abi::token::kSlowOpen);
    log::Info("encode", "probe: slow open", {{"encoder", encpolicy::EncoderToken(info.kind)}, {"us", facts.open_us}});
    return;
  }
  if (facts.zero_copy) {
    info.flags |= IM_ENCF_ZERO_COPY;
  }
  info.flags |= profile == IM_PROFILE_HIGH ? IM_ENCF_HIGH : IM_ENCF_CB;

  // The IDR, allowing the encoder a few frames of delay before its first AU.
  TestRun run(*enc);
  bool produced = false;
  bool ok = run.Frame(true, &produced);
  int delay = 0;
  while (ok && !produced && delay < kMaxDelayFrames) {
    ++delay;
    ok = run.Frame(false, &produced);
  }
  const AuFacts first = run.aus().empty() ? AuFacts{} : Inspect(run.aus().front());
  if (!ok || !first.sps || !first.pps || !first.idr) {
    log::Info("encode", "probe: test encode failed",
              {{"encoder", encpolicy::EncoderToken(info.kind)},
               {"ok", ok},
               {"aus", run.aus().size()},
               {"sps", first.sps},
               {"pps", first.pps},
               {"idr", first.idr}});
    abi::ClearLastError();
    enc.reset();
    SetStatus(info, IM_ERR_NO_ENCODER, abi::token::kTestEncodeFailed);
    return;
  }

  // 04 §9.10's live-bitrate check: two P frames, a 50 % change as §9.5 applies it, two more frames (plus the
  // encoder's delay, so their AUs come out); an IDR in either of those AUs makes it a re-init encoder.
  if ((info.flags & IM_ENCF_LIVE_BITRATE) != 0) {
    bool live_ok = run.Frame(false, &produced) && run.Frame(false, &produced);
    const std::size_t before = run.aus().size();
    live_ok = live_ok && enc->SetBitrate(kTestBitrate / 2) == IM_OK;
    for (int i = 0; live_ok && i < 2 + delay; ++i) {
      live_ok = run.Frame(false, &produced);
    }
    bool idr_after = false;
    const std::size_t after = run.aus().size();
    for (std::size_t i = std::max(before, after >= 2 ? after - 2 : 0); i < after; ++i) {
      idr_after = idr_after || Inspect(run.aus()[i]).idr;
    }
    if (!live_ok) {
      abi::ClearLastError();
      idr_after = true; // a change it can't take live makes it a re-init encoder too
    }
    info.flags = encpolicy::ApplyLiveBitrateCheck(info.flags, idr_after);
    if (idr_after) {
      log::Info("encode", "live_bitrate_idr", {{"encoder", encpolicy::EncoderToken(info.kind)}, {"ok", live_ok}});
    }
  }
  enc.reset();
  info.test_encode_us = static_cast<uint32_t>(std::clamp<int64_t>((NowNs() - t0) / 1000, 0, UINT32_MAX));

  // The other profile, by a trial open (04 §9.10: "High and CB from trial opens").
  if (profile == IM_PROFILE_HIGH && traits.cb) {
    std::unique_ptr<FfmpegEncoder> cb;
    if (FfmpegEncoder::Create(TestParams(info.kind, IM_PROFILE_CB, &device), &cb, nullptr) == IM_OK) {
      info.flags |= IM_ENCF_CB;
    }
    abi::ClearLastError();
  }
  info.status = IM_OK;
  info.flags |= IM_ENCF_AVAILABLE;
  abi::CopyFixed(info.reason, std::string_view{});
}

// Step 3: every planned encoder's entry.
void ProbeEncoders(bool full, im_caps &caps, int64_t start_ns) {
  const std::vector<Planned> plan = PlanEncoders(caps);
  const bool mf = (caps.os_flags & IM_OS_MEDIA_FOUNDATION) != 0;
  ffmpeg::LoadResult why;
  const ffmpeg::Api *api = ffmpeg::Load(ffmpeg::DefaultDir(), &why);
  if (api == nullptr) {
    abi::ClearLastError();
  }
  // One device per adapter, shared by the adapter's encoders; MF software uses the default adapter's.
  std::array<std::unique_ptr<gpu::Device>, IM_MAX_ADAPTERS> devices;
  std::unique_ptr<gpu::Device> default_device;
  bool timed_out = false;

  caps.encoder_count = 0;
  for (const Planned &p : plan) {
    im_encoder_info &info = caps.encoders[caps.encoder_count++];
    info = im_encoder_info{};
    info.kind = p.kind;
    info.adapter_index = p.adapter;
    const encpolicy::EncoderTraits &traits = encpolicy::Traits(p.kind);
    abi::CopyFixed(info.codec, traits.codec);
    info.flags = IM_ENCF_FORCED_IDR;
    if (traits.hardware) {
      info.flags |= IM_ENCF_HARDWARE;
    }
    if (traits.live_bitrate) {
      info.flags |= IM_ENCF_LIVE_BITRATE;
    }

    if (p.kind == IM_ENC_OPENH264) {
      ProbeOpenH264(full, info);
      continue;
    }
    if (p.kind == IM_ENC_FAKE) {
      info.flags |= IM_ENCF_AVAILABLE | IM_ENCF_HIGH | IM_ENCF_CB;
      info.status = IM_OK;
      continue;
    }
    if (api == nullptr) {
      SetStatus(info, why.status, why.token);
      continue;
    }
    if ((p.kind == IM_ENC_MF_HW || p.kind == IM_ENC_MF_SW) && !mf) {
      SetStatus(info, IM_ERR_DEPENDENCY, abi::token::kNoMediaFoundation);
      continue;
    }
    if (!FfmpegEncoder::BuildHasEncoder(p.kind)) {
      SetStatus(info, IM_ERR_NO_ENCODER, abi::token::kOpenFailed);
      continue;
    }
    if (!full) {
      // IM_PROBE_QUICK: the build has it; untested (no IM_ENCF_AVAILABLE), the profiles from the table.
      info.status = IM_OK;
      if (FfmpegEncoder::BuildListsZeroCopy(p.kind)) {
        info.flags |= IM_ENCF_ZERO_COPY;
      }
      info.flags |= (traits.high ? IM_ENCF_HIGH : 0u) | (traits.cb ? IM_ENCF_CB : 0u);
      continue;
    }
    if (timed_out || (NowNs() - start_ns) / 1'000'000 > kEncoderBudgetMs) {
      timed_out = true;
      SetStatus(info, IM_ERR_TIMEOUT, abi::token::kEngineTimeout);
      continue;
    }
    std::unique_ptr<gpu::Device> *slot = p.adapter < devices.size() ? &devices[p.adapter] : &default_device;
    if (!*slot) {
      gpu::DeviceRequest req;
      if (p.adapter < caps.adapter_count) {
        req.adapter = gpu::DeviceRequest::Adapter::kLuid;
        req.luid = caps.adapters[p.adapter].luid;
      }
      if (const im_status st = gpu::Device::Create(req, slot); st != IM_OK) {
        abi::ClearLastError();
        SetStatus(info, IM_ERR_NO_ENCODER, abi::token::kOpenFailed);
        continue;
      }
    }
    TestEncoder(info, **slot);
    const int64_t adapter = p.adapter == encpolicy::kNoAdapter ? -1 : static_cast<int64_t>(p.adapter);
    log::Info("encode", "encoder probed",
              {{"encoder", encpolicy::EncoderToken(p.kind)},
               {"adapter", adapter},
               {"status", info.status},
               {"reason", std::string_view(info.reason)},
               {"flags", info.flags},
               {"us", info.test_encode_us}});
  }
}

im_status ProbeAll(uint32_t flags, im_caps &caps, bool os_and_audio) {
  const int64_t t0 = NowNs();
  const bool full = (flags & IM_PROBE_FULL) != 0;
  caps = im_caps{};
  caps.struct_size = sizeof caps;
  std::vector<ComPtr<IDXGIAdapter1>> adapters = ProbeAdapters(caps);
  if (os_and_audio) {
    const OsVersion v = ReadOsVersion();
    caps.os_build = v.build;
    caps.os_ubr = v.ubr;
    caps.os_flags = OsFlags(v.build, adapters);
  } else if (MediaFoundationWorks()) {
    caps.os_flags |= IM_OS_MEDIA_FOUNDATION; // the encoders need to know
  }
  adapters.clear();
  ProbeEncoders(full, caps, t0);
  if (os_and_audio && full) {
    // Step 4: 05's flags; the call posts to 05's watcher, the worker does no WASAPI work (04 §10.2).
    caps.audio_flags = audio_bridge::Engine().ProbeFlags();
  }
  caps.probe_ms = static_cast<uint32_t>(std::clamp<int64_t>((NowNs() - t0) / 1'000'000, 0, UINT32_MAX));
  return IM_OK;
}

} // namespace

im_status Probe(uint32_t flags, im_caps &caps) {
  try {
    const im_status st = ProbeAll(flags, caps, true);
    if (st != IM_OK) {
      return st;
    }
    const bool full = (flags & IM_PROBE_FULL) != 0;
    if (full) {
      StoreProbe(caps);
      session::ResetProbeCacheStale();
    }
    uint32_t available = 0;
    for (uint32_t i = 0; i < caps.encoder_count; ++i) {
      available += (caps.encoders[i].flags & IM_ENCF_AVAILABLE) != 0 ? 1 : 0;
    }
    log::Info("encode", "probe",
              {{"full", full},
               {"build", caps.os_build},
               {"ubr", caps.os_ubr},
               {"osFlags", caps.os_flags},
               {"audioFlags", caps.audio_flags},
               {"adapters", caps.adapter_count},
               {"encoders", caps.encoder_count},
               {"available", available},
               {"ms", caps.probe_ms}});
    return IM_OK;
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "im_probe: out of memory");
  }
}

im_status ProbeForCatalog(im_caps &caps) noexcept {
  try {
    return ProbeAll(IM_PROBE_FULL, caps, false);
  } catch (...) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "encode: out of memory probing the encoders");
  }
}

} // namespace im::encode
