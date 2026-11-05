// C++/WinRT for the capture area (docs/m2/04-windows-engine.md §7.1–§7.3, §13.1): the projection headers from the
// Windows SDK (10.0.26100 declares the 24H2 capture properties), runtimeobject.lib through #pragma comment (the
// desktop import library of the Ro* and Windows*String functions, through the api-ms-win-core-winrt-* sets of
// deploy/windows/dll-imports.txt; never windowsapp.lib, see native/windows/CMakeLists.txt), and the helpers the picker
// and the WGC capture share. Every API newer than the engine's floor (build 19041) is checked at run time with
// ApiInformation before use. Include this header only from the capture area's .cpp files.
#pragma once

#include <windows.h>

// unknwn.h before winrt/base.h: C++/WinRT's interop with classic COM interfaces (IInitializeWithWindow,
// IDirect3DDxgiInterfaceAccess) needs ::IUnknown declared first (WIN32_LEAN_AND_MEAN leaves it out of windows.h).
#include <unknwn.h>

#include <d3d11.h>
#include <dxgi.h>
#include <inspectable.h>
#include <shobjidl_core.h>
#include <windows.graphics.directx.direct3d11.interop.h>

#include <winrt/base.h>

#include <winrt/Windows.Foundation.Metadata.h>
#include <winrt/Windows.Foundation.h>
#include <winrt/Windows.Graphics.Capture.h>
#include <winrt/Windows.Graphics.DirectX.Direct3D11.h>
#include <winrt/Windows.Graphics.DirectX.h>
#include <winrt/Windows.Graphics.h>
#include <winrt/Windows.Security.Authorization.AppCapabilityAccess.h>

#include <cstdint>
#include <memory>

#include "isshoni_media.h"

// C++/WinRT's other imports come from the classic libraries on the link line: ole32 (Co*, RoGetAgileReference),
// oleaut32 (Sys*String), kernel32 and d3d11 (CreateDirect3D11DeviceFromDXGIDevice).
#pragma comment(lib, "runtimeobject.lib")

namespace im::capture {

namespace wgc = winrt::Windows::Graphics::Capture;

inline constexpr wchar_t kSessionClass[] = L"Windows.Graphics.Capture.GraphicsCaptureSession";
inline constexpr wchar_t kAccessClass[] = L"Windows.Graphics.Capture.GraphicsCaptureAccess";

// ApiInformation::IsPropertyPresent on GraphicsCaptureSession (false when the query itself fails).
bool SessionHasProperty(const wchar_t *property) noexcept;

// GraphicsCaptureSession::IsSupported() (false when it throws).
bool WgcSupported() noexcept;

// The status for a failed WinRT or COM call `what` (sets the thread's detail): E_ACCESSDENIED is
// IM_ERR_PERMISSION_DENIED with token capture_denied (Windows' screen-capture privacy setting or GraphicsCaptureAccess,
// 04 §5.7); RO_E_CLOSED and an invalid window handle (the picked window closed meanwhile) are IM_ERR_SOURCE_GONE;
// anything else IM_ERR_INTERNAL.
im_status FailHr(HRESULT hr, const char *what) noexcept;

// Waits for a WinRT async operation while pumping the thread's messages (the picker's STA must keep its window and
// COM calls alive; on an MTA thread the pump finds nothing), up to timeout_ms or until `cancel` is signaled.
// Attach sets the operation's Completed handler (only once per operation); Wait may be called again after a cancel,
// for the cancelled operation's completion.
class AsyncWait {
public:
  enum class Result : uint8_t { kCompleted, kCancelled, kTimeout, kFailed };

  AsyncWait();
  AsyncWait(const AsyncWait &) = delete;
  AsyncWait &operator=(const AsyncWait &) = delete;

  template <class Op> void Attach(const Op &op) {
    // The handler may run after this object is gone (a timed-out wait): it keeps the event alive itself.
    std::shared_ptr<winrt::handle> ev = event_;
    op.Completed([ev](auto &&, auto &&) { ::SetEvent(ev->get()); });
  }

  Result Wait(uint32_t timeout_ms, HANDLE cancel) noexcept;

private:
  std::shared_ptr<winrt::handle> event_;
};

inline constexpr uint32_t kBorderlessWaitMs = 2000; // the picker (WGC's start uses 0: the kept answer only)

// Whether borderless capture is allowed (04 §7.1): GraphicsCaptureSession has IsBorderRequired and
// GraphicsCaptureAccess::RequestAccessAsync(Borderless) returns Allowed. Asked once per process (an Allowed or a final
// denial is kept; a query that failed or timed out is asked again next time), waiting at most wait_ms, with
// AsyncWait (so the picker's STA may call it); wait_ms 0 asks nothing and gives the kept answer (false while there is
// none: a quick probe's < 100 ms, README WF12). Never throws.
bool BorderlessAllowed(uint32_t wait_ms = kBorderlessWaitMs) noexcept;

} // namespace im::capture
