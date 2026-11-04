#include "capture/winrt_support.h"

#include <atomic>

#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::capture {
namespace {

namespace meta = winrt::Windows::Foundation::Metadata;
namespace access = winrt::Windows::Security::Authorization::AppCapabilityAccess;

constexpr uint32_t kAccessWaitMs = 2000;

unsigned long Hex(HRESULT hr) noexcept { return static_cast<unsigned long>(hr); }

// BorderlessAllowed's answer: 0 not asked (or asked without an answer), 1 allowed, 2 denied for good.
std::atomic<int> g_borderless{0};

} // namespace

bool SessionHasProperty(const wchar_t *property) noexcept {
  try {
    return meta::ApiInformation::IsPropertyPresent(kSessionClass, property);
  } catch (...) {
    return false;
  }
}

bool WgcSupported() noexcept {
  try {
    return wgc::GraphicsCaptureSession::IsSupported();
  } catch (...) {
    return false;
  }
}

im_status FailHr(HRESULT hr, const char *what) noexcept {
  if (hr == E_ACCESSDENIED) {
    return abi::Failf(IM_ERR_PERMISSION_DENIED, abi::token::kCaptureDenied,
                      "capture: %s: Windows denied screen capture (0x%08lx)", what, Hex(hr));
  }
  if (hr == RO_E_CLOSED || hr == HRESULT_FROM_WIN32(ERROR_INVALID_WINDOW_HANDLE)) {
    return abi::Failf(IM_ERR_SOURCE_GONE, {}, "capture: %s: the picked source is gone (0x%08lx)", what, Hex(hr));
  }
  return abi::Failf(IM_ERR_INTERNAL, {}, "capture: %s failed (0x%08lx)", what, Hex(hr));
}

AsyncWait::AsyncWait() : event_(std::make_shared<winrt::handle>(::CreateEventW(nullptr, TRUE, FALSE, nullptr))) {
  if (!*event_) {
    winrt::throw_last_error();
  }
}

AsyncWait::Result AsyncWait::Wait(uint32_t timeout_ms, HANDLE cancel) noexcept {
  const ULONGLONG deadline = ::GetTickCount64() + timeout_ms;
  HANDLE handles[2] = {event_->get(), cancel};
  const DWORD count = cancel != nullptr ? 2 : 1;
  for (;;) {
    const ULONGLONG now = ::GetTickCount64();
    const DWORD left = now >= deadline ? 0 : static_cast<DWORD>(deadline - now);
    const DWORD r = ::MsgWaitForMultipleObjectsEx(count, handles, left, QS_ALLINPUT, MWMO_INPUTAVAILABLE);
    if (r == WAIT_OBJECT_0) {
      return Result::kCompleted;
    }
    if (count == 2 && r == WAIT_OBJECT_0 + 1) {
      return Result::kCancelled;
    }
    if (r == WAIT_OBJECT_0 + count) {
      MSG msg;
      while (::PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
        ::TranslateMessage(&msg);
        ::DispatchMessageW(&msg);
      }
      continue;
    }
    if (r == WAIT_TIMEOUT) {
      return Result::kTimeout;
    }
    return Result::kFailed;
  }
}

bool BorderlessAllowed() noexcept {
  const int known = g_borderless.load(std::memory_order_acquire);
  if (known != 0) {
    return known == 1;
  }
  try {
    if (!SessionHasProperty(L"IsBorderRequired") || !meta::ApiInformation::IsTypePresent(kAccessClass)) {
      g_borderless.store(2, std::memory_order_release); // an older Windows: the border stays (04 §7.1)
      return false;
    }
    auto op = wgc::GraphicsCaptureAccess::RequestAccessAsync(wgc::GraphicsCaptureAccessKind::Borderless);
    AsyncWait wait;
    wait.Attach(op);
    if (wait.Wait(kAccessWaitMs, nullptr) != AsyncWait::Result::kCompleted) {
      op.Cancel();
      log::Info("capture", "borderless access: no answer", {{"wait_ms", kAccessWaitMs}});
      return false;
    }
    if (op.Status() != winrt::Windows::Foundation::AsyncStatus::Completed) {
      log::Info("capture", "borderless access: query failed", {{"hr", static_cast<int64_t>(op.ErrorCode().value)}});
      return false;
    }
    const access::AppCapabilityAccessStatus status = op.GetResults();
    const bool allowed = status == access::AppCapabilityAccessStatus::Allowed;
    // UserPromptRequired may change after a prompt elsewhere: ask again next time; the other denials are final.
    if (allowed || status != access::AppCapabilityAccessStatus::UserPromptRequired) {
      g_borderless.store(allowed ? 1 : 2, std::memory_order_release);
    }
    log::Info("capture", "borderless access", {{"allowed", allowed}, {"status", static_cast<int64_t>(status)}});
    return allowed;
  } catch (const winrt::hresult_error &e) {
    log::Info("capture", "borderless access: error", {{"hr", static_cast<int64_t>(e.code().value)}});
    return false;
  } catch (...) {
    return false;
  }
}

} // namespace im::capture
