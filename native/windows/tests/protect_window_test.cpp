// protect_window_test (docs/m2/04-windows-engine.md §11, E12; README WA70): im_protect_window on Windows.
//
// It asserts, through the C ABI:
// - a top-level window of this process gets WDA_EXCLUDEFROMCAPTURE (GetWindowDisplayAffinity reads it back), a second
//   call is fine, IM_PROTECT_OFF sets WDA_NONE again, and another thread of the process may make the call (the Go
//   binding calls from its own locked thread);
// - NULL, unknown flags, a handle that is no window (a destroyed one), a child window and another process's window
//   (the desktop window) are IM_ERR_INVALID_ARG and leave the affinity alone;
// - below build 19041 the call is IM_ERR_UNSUPPORTED_OS with the token os_too_old (window::ProtectOnBuild's seam);
// - the engine's log records the outcome but never the window's title (README §4 "Logging");
// - Desktop Duplication leaves a protected window out of the desktop image (04 §11: "the content behind it shows")
//   and shows it again once unprotected. A solid magenta window is sampled at five points of a fresh duplication's
//   images. That case skips where Desktop Duplication can't run here (no output, a secure desktop, a remote session)
//   and where even the unprotected window doesn't reach the image (another window above it): without that control,
//   an absence would prove nothing. WGC's half of 04 §11's guarantee, and both paths through a real engine session,
//   are isshoni-enginetest protect-check's (04 §14), run on the owner's PC.
// (Calls before im_init are not_initialized: exports_test covers every export.)
#include <gtest/gtest.h>

#include <windows.h>

#include <d3d11.h>
#include <dxgi1_2.h>
#include <wrl/client.h>

#include <algorithm>
#include <cctype>
#include <chrono>
#include <cstdint>
#include <cstdio>
#include <iterator>
#include <string>
#include <thread>
#include <vector>

#include "capture/outputs_win.h"
#include "isshoni_media.h"
#include "window/protect.h"

#ifndef WDA_EXCLUDEFROMCAPTURE
#define WDA_EXCLUDEFROMCAPTURE 0x00000011
#endif

namespace {

using Microsoft::WRL::ComPtr;
using std::chrono::milliseconds;
using Clock = std::chrono::steady_clock;

constexpr const wchar_t *kTitle = L"WA70 protected window title";
constexpr const char *kTitleLower = "wa70 protected window title";
constexpr const wchar_t *kClass = L"isshoni-wa70-protect-test";
constexpr DWORD kNoAffinity = 0xFFFFFFFFu;

std::string LastError() {
  char buf[1100];
  const int32_t n = im_last_error(buf, static_cast<int32_t>(sizeof buf));
  return n < 0 ? std::string("<im_last_error failed>")
               : std::string(buf, std::min<std::size_t>(static_cast<std::size_t>(n), sizeof buf - 1));
}

// Checks a call's status and its detail: the 04 §5.7 token in front ("<token>: ") when token is set, and a fragment
// of the text when contains is set.
::testing::AssertionResult Fails(int32_t got, int32_t want, const char *token = nullptr,
                                 const char *contains = nullptr) {
  const std::string detail = LastError();
  if (got != want) {
    return ::testing::AssertionFailure() << "status " << got << ", want " << want << " (detail: " << detail << ")";
  }
  if (token != nullptr && detail.rfind(std::string(token) + ": ", 0) != 0) {
    return ::testing::AssertionFailure() << "detail \"" << detail << "\" doesn't start with \"" << token << ": \"";
  }
  if (contains != nullptr && detail.find(contains) == std::string::npos) {
    return ::testing::AssertionFailure() << "detail \"" << detail << "\" lacks \"" << contains << "\"";
  }
  return ::testing::AssertionSuccess();
}

// Everything in the engine's log ring.
std::string DrainLogs() {
  std::vector<char> buf(64 * 1024);
  std::string all;
  for (int i = 0; i < 64; ++i) {
    const int32_t n = im_read_logs(buf.data(), static_cast<int32_t>(buf.size()));
    if (n <= 0) {
      break;
    }
    all.append(buf.data(), static_cast<std::size_t>(n));
  }
  return all;
}

std::string Lower(std::string s) {
  std::transform(s.begin(), s.end(), s.begin(), [](unsigned char c) { return static_cast<char>(std::tolower(c)); });
  return s;
}

DWORD AffinityOf(HWND w) {
  DWORD a = 0;
  return GetWindowDisplayAffinity(w, &a) != FALSE ? a : kNoAffinity;
}

// Runs the calling thread's pending messages (the test's windows belong to it).
void Pump() {
  MSG msg;
  while (PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE) != FALSE) {
    TranslateMessage(&msg);
    DispatchMessageW(&msg);
  }
}

void PumpFor(milliseconds d) {
  const auto until = Clock::now() + d;
  while (Clock::now() < until) {
    Pump();
    Sleep(10);
  }
}

// The test's window class: a solid magenta background, which DefWindowProcW paints on WM_ERASEBKGND.
bool RegisterTestClass() {
  static const bool ok = [] {
    WNDCLASSEXW wc{};
    wc.cbSize = sizeof wc;
    wc.lpfnWndProc = DefWindowProcW;
    wc.hInstance = GetModuleHandleW(nullptr);
    wc.hCursor = LoadCursorW(nullptr, IDC_ARROW);
    wc.hbrBackground = CreateSolidBrush(RGB(255, 0, 255));
    wc.lpszClassName = kClass;
    return RegisterClassExW(&wc) != 0 || GetLastError() == ERROR_CLASS_ALREADY_EXISTS;
  }();
  return ok;
}

// A top-level popup of this thread, hidden until shown; topmost and never activated, so that it covers the desktop
// without taking the focus.
HWND CreateTestWindow(int x, int y, int w, int h) {
  if (!RegisterTestClass()) {
    return nullptr;
  }
  return CreateWindowExW(WS_EX_TOPMOST | WS_EX_TOOLWINDOW | WS_EX_NOACTIVATE, kClass, kTitle, WS_POPUP, x, y, w, h,
                         nullptr, nullptr, GetModuleHandleW(nullptr), nullptr);
}

// The engine started for one test, with one hidden test window, both gone after it.
class Protect : public ::testing::Test {
protected:
  void SetUp() override {
    im_init_params p{};
    p.struct_size = sizeof p;
    p.abi_version = IM_ABI_VERSION;
    p.flags = IM_INIT_NO_AUDIO; // CI runners have no audio device; nothing here needs one
    p.log_level = IM_LOG_DEBUG;
    ASSERT_EQ(im_init(&p), IM_OK) << LastError();
    build_ = im::capture::WindowsBuild();
    window_ = CreateTestWindow(0, 0, 64, 64);
    ASSERT_NE(window_, nullptr) << "CreateWindowExW failed: " << GetLastError();
  }
  void TearDown() override {
    if (window_ != nullptr) {
      DestroyWindow(window_);
    }
    Pump();
    im_shutdown();
  }
  bool TooOld() const { return build_ != 0 && build_ < im::window::kExcludeFromCaptureBuild; }

  uint32_t build_ = 0;
  HWND window_ = nullptr;
};

TEST_F(Protect, SetsAndClearsTheAffinity) {
  if (TooOld()) {
    GTEST_SKIP() << "build " << build_ << " has no WDA_EXCLUDEFROMCAPTURE";
  }
  ASSERT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_NONE));
  ASSERT_EQ(im_protect_window(window_, 0), IM_OK) << LastError();
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_EXCLUDEFROMCAPTURE));
  EXPECT_EQ(im_protect_window(window_, 0), IM_OK) << "a second call: " << LastError();
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_EXCLUDEFROMCAPTURE));
  ASSERT_EQ(im_protect_window(window_, IM_PROTECT_OFF), IM_OK) << LastError();
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_NONE));

  // A shown window, protected from another thread of the process.
  ShowWindow(window_, SW_SHOWNOACTIVATE);
  Pump();
  im_status st = IM_ERR_INTERNAL;
  std::string detail;
  std::thread other([&] {
    st = im_protect_window(window_, 0);
    detail = LastError();
  });
  other.join();
  EXPECT_EQ(st, IM_OK) << detail;
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_EXCLUDEFROMCAPTURE));
}

TEST_F(Protect, RefusesWhatItCannotProtect) {
  EXPECT_TRUE(Fails(im_protect_window(nullptr, 0), IM_ERR_INVALID_ARG));
  EXPECT_TRUE(Fails(im_protect_window(window_, 0x2u), IM_ERR_INVALID_ARG, nullptr, "flags"));
  EXPECT_TRUE(Fails(im_protect_window(window_, 0x80000000u | IM_PROTECT_OFF), IM_ERR_INVALID_ARG, nullptr, "flags"));
  if (TooOld()) {
    GTEST_SKIP() << "build " << build_ << " has no WDA_EXCLUDEFROMCAPTURE";
  }

  // Another process's window: the desktop window belongs to the window station's session process.
  const HWND desktop = GetDesktopWindow();
  ASSERT_NE(desktop, nullptr);
  DWORD pid = 0;
  GetWindowThreadProcessId(desktop, &pid);
  ASSERT_NE(pid, GetCurrentProcessId());
  const DWORD before = AffinityOf(desktop);
  EXPECT_TRUE(Fails(im_protect_window(desktop, 0), IM_ERR_INVALID_ARG, nullptr, "another process"));
  EXPECT_EQ(AffinityOf(desktop), before);

  // A child window of ours.
  const HWND child =
      CreateWindowExW(0, L"STATIC", L"", WS_CHILD, 0, 0, 16, 16, window_, nullptr, GetModuleHandleW(nullptr), nullptr);
  ASSERT_NE(child, nullptr) << GetLastError();
  EXPECT_TRUE(Fails(im_protect_window(child, 0), IM_ERR_INVALID_ARG, nullptr, "top-level"));
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_NONE)) << "the child's parent was protected";

  // A handle that is no window any more.
  HWND gone = CreateTestWindow(0, 0, 8, 8);
  ASSERT_NE(gone, nullptr);
  ASSERT_NE(DestroyWindow(gone), FALSE);
  Pump();
  EXPECT_TRUE(Fails(im_protect_window(gone, 0), IM_ERR_INVALID_ARG, nullptr, "not a window"));
}

TEST_F(Protect, OldWindowsIsUnsupported) {
  EXPECT_TRUE(Fails(im::window::ProtectOnBuild(window_, 0, 18363), IM_ERR_UNSUPPORTED_OS, "os_too_old"));
  EXPECT_TRUE(Fails(im::window::ProtectOnBuild(window_, IM_PROTECT_OFF, 19040), IM_ERR_UNSUPPORTED_OS, "os_too_old"));
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_NONE));
  if (TooOld()) {
    GTEST_SKIP() << "build " << build_ << " has no WDA_EXCLUDEFROMCAPTURE";
  }
  EXPECT_EQ(im::window::ProtectOnBuild(window_, 0, im::window::kExcludeFromCaptureBuild), IM_OK) << LastError();
  EXPECT_EQ(im::window::ProtectOnBuild(window_, IM_PROTECT_OFF, 0), IM_OK) << "build unknown: " << LastError();
  EXPECT_EQ(AffinityOf(window_), static_cast<DWORD>(WDA_NONE));
}

TEST_F(Protect, LogsNoTitle) {
  if (TooOld()) {
    GTEST_SKIP() << "build " << build_ << " has no WDA_EXCLUDEFROMCAPTURE";
  }
  (void)DrainLogs(); // im_init's records
  ASSERT_EQ(im_protect_window(window_, 0), IM_OK) << LastError();
  ASSERT_EQ(im_protect_window(window_, IM_PROTECT_OFF), IM_OK) << LastError();
  (void)im_protect_window(GetDesktopWindow(), 0);
  const std::string logs = DrainLogs();
  EXPECT_NE(logs.find("\"protected\""), std::string::npos) << logs;
  EXPECT_NE(logs.find("\"unprotected\""), std::string::npos) << logs;
  EXPECT_EQ(Lower(logs).find(kTitleLower), std::string::npos) << logs;
  EXPECT_EQ(logs.find("isshoni-wa70"), std::string::npos) << "the window class was logged: " << logs;
}

// ------------------------------------------------------------------------------------- Desktop Duplication

// What one look through Desktop Duplication found at the sample points.
struct Look {
  std::string skip; // set when Desktop Duplication can't run here (the reason)
  int magenta = 0;  // sample points that showed the window's color in some image
  int images = 0;
};

bool IsMagenta(const uint8_t *bgra) { return bgra[2] >= 240 && bgra[1] <= 15 && bgra[0] >= 240; }

std::string Hex(HRESULT hr) {
  char buf[16];
  std::snprintf(buf, sizeof buf, "0x%08lx", static_cast<unsigned long>(hr));
  return buf;
}

// Duplicates the output that shows rect (in desktop coordinates) and samples five points inside it in every image
// that arrives within about one second; each image is copied to the CPU. A fresh duplication's first image is the
// whole desktop, so a static screen still gives one.
Look LookThroughDD(const RECT &rect) {
  Look out;
  ComPtr<IDXGIFactory1> factory;
  if (FAILED(CreateDXGIFactory1(IID_PPV_ARGS(&factory)))) {
    out.skip = "CreateDXGIFactory1 failed";
    return out;
  }
  const POINT center{(rect.left + rect.right) / 2, (rect.top + rect.bottom) / 2};
  ComPtr<IDXGIAdapter1> adapter;
  ComPtr<IDXGIOutput1> output;
  DXGI_OUTPUT_DESC desc{};
  for (UINT a = 0; output == nullptr && factory->EnumAdapters1(a, adapter.ReleaseAndGetAddressOf()) == S_OK; ++a) {
    ComPtr<IDXGIOutput> o;
    for (UINT i = 0; adapter->EnumOutputs(i, o.ReleaseAndGetAddressOf()) == S_OK; ++i) {
      DXGI_OUTPUT_DESC d{};
      if (SUCCEEDED(o->GetDesc(&d)) && d.AttachedToDesktop != FALSE &&
          PtInRect(&d.DesktopCoordinates, center) != FALSE && SUCCEEDED(o.As(&output))) {
        desc = d;
        break;
      }
    }
  }
  if (output == nullptr) {
    out.skip = "no DXGI output shows the test window";
    return out;
  }
  if (desc.Rotation != DXGI_MODE_ROTATION_IDENTITY && desc.Rotation != DXGI_MODE_ROTATION_UNSPECIFIED) {
    out.skip = "the output is rotated";
    return out;
  }
  ComPtr<ID3D11Device> device;
  ComPtr<ID3D11DeviceContext> context;
  HRESULT hr = D3D11CreateDevice(adapter.Get(), D3D_DRIVER_TYPE_UNKNOWN, nullptr, 0, nullptr, 0, D3D11_SDK_VERSION,
                                 &device, nullptr, &context);
  if (FAILED(hr)) {
    out.skip = "D3D11CreateDevice failed: " + Hex(hr);
    return out;
  }
  ComPtr<IDXGIOutputDuplication> dup;
  hr = output->DuplicateOutput(device.Get(), &dup);
  if (FAILED(hr)) {
    // E_ACCESSDENIED (a secure desktop), DXGI_ERROR_UNSUPPORTED (a remote or Basic Display session),
    // DXGI_ERROR_NOT_CURRENTLY_AVAILABLE (too many duplications), DXGI_ERROR_SESSION_DISCONNECTED.
    out.skip = "DuplicateOutput failed: " + Hex(hr);
    return out;
  }
  const LONG ox = desc.DesktopCoordinates.left;
  const LONG oy = desc.DesktopCoordinates.top;
  const LONG w = rect.right - rect.left;
  const LONG h = rect.bottom - rect.top;
  const POINT points[5] = {{center.x - ox, center.y - oy},
                           {rect.left + w / 4 - ox, rect.top + h / 4 - oy},
                           {rect.right - w / 4 - ox, rect.top + h / 4 - oy},
                           {rect.left + w / 4 - ox, rect.bottom - h / 4 - oy},
                           {rect.right - w / 4 - ox, rect.bottom - h / 4 - oy}};
  bool seen[5] = {};
  const auto until = Clock::now() + milliseconds(1000);
  while (Clock::now() < until) {
    Pump();
    DXGI_OUTDUPL_FRAME_INFO info{};
    ComPtr<IDXGIResource> resource;
    hr = dup->AcquireNextFrame(100, &info, &resource);
    if (hr == DXGI_ERROR_WAIT_TIMEOUT) {
      continue;
    }
    if (FAILED(hr)) {
      if (out.images == 0) {
        out.skip = "AcquireNextFrame failed: " + Hex(hr);
      }
      break;
    }
    ComPtr<ID3D11Texture2D> image;
    if (SUCCEEDED(resource.As(&image))) {
      D3D11_TEXTURE2D_DESC td{};
      image->GetDesc(&td);
      td.Usage = D3D11_USAGE_STAGING;
      td.BindFlags = 0;
      td.CPUAccessFlags = D3D11_CPU_ACCESS_READ;
      td.MiscFlags = 0;
      td.MipLevels = 1;
      td.ArraySize = 1;
      ComPtr<ID3D11Texture2D> staging;
      if (td.Format == DXGI_FORMAT_B8G8R8A8_UNORM && SUCCEEDED(device->CreateTexture2D(&td, nullptr, &staging))) {
        context->CopyResource(staging.Get(), image.Get());
        D3D11_MAPPED_SUBRESOURCE m{};
        if (SUCCEEDED(context->Map(staging.Get(), 0, D3D11_MAP_READ, 0, &m))) {
          ++out.images;
          for (int i = 0; i < 5; ++i) {
            const POINT p = points[i];
            if (p.x >= 0 && p.y >= 0 && static_cast<UINT>(p.x) < td.Width && static_cast<UINT>(p.y) < td.Height) {
              const auto *row = static_cast<const uint8_t *>(m.pData) + static_cast<std::size_t>(p.y) * m.RowPitch;
              seen[i] = seen[i] || IsMagenta(row + static_cast<std::size_t>(p.x) * 4);
            }
          }
          context->Unmap(staging.Get(), 0);
        }
      } else if (td.Format != DXGI_FORMAT_B8G8R8A8_UNORM && out.images == 0) {
        out.skip = "the duplicated image is not BGRA8";
      }
    }
    dup->ReleaseFrame();
  }
  if (out.images == 0 && out.skip.empty()) {
    out.skip = "Desktop Duplication delivered no image within 1 s";
  }
  out.magenta = static_cast<int>(std::count(std::begin(seen), std::end(seen), true));
  return out;
}

TEST_F(Protect, DesktopDuplicationLeavesTheWindowOut) {
  if (TooOld()) {
    GTEST_SKIP() << "build " << build_ << " has no WDA_EXCLUDEFROMCAPTURE";
  }
  // Desktop Duplication needs a per-monitor DPI aware process (04 §7.4, as isshoni-enginetest makes itself); the
  // call fails harmlessly when the awareness is already set. The window is created inside the thread's scope, so it
  // is per-monitor aware too and every coordinate below is physical.
  (void)SetProcessDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
  im::capture::ThreadDpiScope dpi;
  const POINT origin{0, 0};
  const HMONITOR mon = MonitorFromPoint(origin, MONITOR_DEFAULTTOPRIMARY);
  MONITORINFO mi{};
  mi.cbSize = sizeof mi;
  ASSERT_NE(GetMonitorInfoW(mon, &mi), FALSE);
  const LONG mw = mi.rcMonitor.right - mi.rcMonitor.left;
  const LONG mh = mi.rcMonitor.bottom - mi.rcMonitor.top;
  const HWND win = CreateTestWindow(mi.rcMonitor.left + mw / 4, mi.rcMonitor.top + mh / 4, mw / 2, mh / 2);
  ASSERT_NE(win, nullptr) << GetLastError();
  struct Destroy {
    HWND w;
    ~Destroy() { DestroyWindow(w); }
  } destroy{win};
  ASSERT_EQ(im_protect_window(win, 0), IM_OK) << LastError();
  ShowWindow(win, SW_SHOWNOACTIVATE);
  PumpFor(milliseconds(300));
  RECT rect{};
  ASSERT_NE(GetWindowRect(win, &rect), FALSE);
  const Look hidden = LookThroughDD(rect);
  if (!hidden.skip.empty()) {
    GTEST_SKIP() << "Desktop Duplication doesn't run here: " << hidden.skip;
  }

  ASSERT_EQ(im_protect_window(win, IM_PROTECT_OFF), IM_OK) << LastError();
  InvalidateRect(win, nullptr, TRUE);
  PumpFor(milliseconds(300));
  const Look shown = LookThroughDD(rect);
  if (!shown.skip.empty()) {
    GTEST_SKIP() << "Desktop Duplication doesn't run here: " << shown.skip;
  }
  if (shown.magenta < 5) {
    GTEST_SKIP() << "the unprotected window reached only " << shown.magenta << " of 5 sample points in " << shown.images
                 << " images (another window above it?): the protected look proves nothing";
  }
  EXPECT_EQ(hidden.magenta, 0) << "the protected window showed at " << hidden.magenta << " of 5 points in "
                               << hidden.images << " images";
}

} // namespace
