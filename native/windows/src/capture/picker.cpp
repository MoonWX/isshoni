#include "capture/picker.h"

#include <algorithm>
#include <chrono>
#include <exception>
#include <future>
#include <string_view>
#include <thread>
#include <utility>

#include "capture/outputs_win.h"
#include "capture/window_system_win.h"
#include "engine/os.h"
#include "im/abi/status.h"
#include "im/log/ring.h"

namespace im::capture {
namespace {

constexpr wchar_t kOwnerClass[] = L"isshoni.picker.owner";
// How long Pick waits beyond the pick's own timeout for the picker thread: the cancel grace, the identity match and
// the borderless query (≤ 2 s), with margin. Past it the thread is cancelled, then given kCancelGraceMs more.
constexpr DWORD kAnswerMarginMs = 8'000;
constexpr DWORD kThreadReadyMs = 5'000;

HINSTANCE ThisModule() noexcept {
  HMODULE m = nullptr;
  ::GetModuleHandleExW(GET_MODULE_HANDLE_EX_FLAG_FROM_ADDRESS | GET_MODULE_HANDLE_EX_FLAG_UNCHANGED_REFCOUNT,
                       reinterpret_cast<LPCWSTR>(&ThisModule), &m);
  return m;
}

// The owner window for a pick without a parent: a 1×1 fully transparent layered tool window that takes no input. The
// class is registered once per process and never unregistered (the DLL is never unloaded, 04 §12.4).
HWND CreateOwnerWindow() noexcept {
  const HINSTANCE module = ThisModule();
  WNDCLASSEXW wc{};
  wc.cbSize = sizeof wc;
  wc.lpfnWndProc = ::DefWindowProcW;
  wc.hInstance = module;
  wc.lpszClassName = kOwnerClass;
  if (::RegisterClassExW(&wc) == 0 && ::GetLastError() != ERROR_CLASS_ALREADY_EXISTS) {
    return nullptr;
  }
  HWND w = ::CreateWindowExW(WS_EX_TOOLWINDOW | WS_EX_LAYERED | WS_EX_TRANSPARENT | WS_EX_NOACTIVATE, kOwnerClass, L"",
                             WS_POPUP, 0, 0, 1, 1, nullptr, nullptr, module, nullptr);
  if (w != nullptr) {
    ::SetLayeredWindowAttributes(w, 0, 0, LWA_ALPHA);
  }
  return w;
}

// Centres the owner window on the primary monitor (the primary may have changed since the last pick).
void PlaceOwnerWindow(HWND w) noexcept {
  MONITORINFO mi{};
  mi.cbSize = sizeof mi;
  if (!::GetMonitorInfoW(::MonitorFromPoint(POINT{0, 0}, MONITOR_DEFAULTTOPRIMARY), &mi)) {
    return;
  }
  const int x = mi.rcMonitor.left + (mi.rcMonitor.right - mi.rcMonitor.left) / 2;
  const int y = mi.rcMonitor.top + (mi.rcMonitor.bottom - mi.rcMonitor.top) / 2;
  ::SetWindowPos(w, nullptr, x, y, 1, 1, SWP_NOZORDER | SWP_NOACTIVATE);
}

// Copies the picker thread's detail for the caller's thread and clears it.
std::string TakeDetail() {
  std::string d(abi::LastErrorText());
  abi::ClearLastError();
  return d;
}

} // namespace

struct Picker::Request {
  HWND parent = nullptr;
  uint32_t timeout_ms = kDefaultPickTimeoutMs;
  winrt::handle done{::CreateEventW(nullptr, TRUE, FALSE, nullptr)};
  im_status status = IM_ERR_INTERNAL;
  std::string detail; // im_last_error's text, for the caller's thread
  PickResult result;
};

struct Picker::Thread {
  std::thread th;
  winrt::handle wake{::CreateEventW(nullptr, FALSE, FALSE, nullptr)};  // a request or quit
  winrt::handle cancel{::CreateEventW(nullptr, TRUE, FALSE, nullptr)}; // Cancel(), a timeout of Pick, Shutdown
  winrt::handle exited{::CreateEventW(nullptr, TRUE, FALSE, nullptr)}; // the thread's last act
  std::mutex mu;
  std::shared_ptr<Request> pending; // guarded by mu
  bool quit = false;                // guarded by mu
  HWND owner = nullptr;             // the thread's own

  bool ok() const noexcept { return wake && cancel && exited; }

  void Run(std::promise<std::string> ready) noexcept;
  void RunPick(Request &req) noexcept;
  im_status Describe(const wgc::GraphicsCaptureItem &item, PickResult *out);
};

void Picker::Thread::Run(std::promise<std::string> ready) noexcept {
  const uintptr_t token = engine::os::EnterThread("isshoni picker", engine::os::ThreadClass::kNormal);
  const HRESULT hr = ::CoInitializeEx(nullptr, COINIT_APARTMENTTHREADED | COINIT_DISABLE_OLE1DDE);
  if (FAILED(hr)) {
    ready.set_value("CoInitializeEx(STA) failed");
    engine::os::LeaveThread(token);
    ::SetEvent(exited.get());
    return;
  }
  // Frame bounds and monitor rectangles in physical pixels, like item.Size() (04 §7.4, §7.6).
  ::SetThreadDpiAwarenessContext(DPI_AWARENESS_CONTEXT_PER_MONITOR_AWARE_V2);
  owner = CreateOwnerWindow();
  if (owner == nullptr) {
    ready.set_value("the owner window could not be created");
  } else {
    ready.set_value(std::string());
    HANDLE wake_handle = wake.get();
    for (;;) {
      const DWORD r = ::MsgWaitForMultipleObjectsEx(1, &wake_handle, INFINITE, QS_ALLINPUT, MWMO_INPUTAVAILABLE);
      if (r == WAIT_OBJECT_0 + 1) {
        MSG msg;
        while (::PeekMessageW(&msg, nullptr, 0, 0, PM_REMOVE)) {
          ::TranslateMessage(&msg);
          ::DispatchMessageW(&msg);
        }
        continue;
      }
      if (r != WAIT_OBJECT_0) {
        break; // WAIT_FAILED: nothing sensible is left to do
      }
      std::shared_ptr<Request> req;
      bool stop = false;
      {
        std::lock_guard<std::mutex> lock(mu);
        req = std::move(pending);
        pending.reset();
        stop = quit;
      }
      if (req) {
        if (stop) {
          req->status = abi::Fail(IM_ERR_CANCELLED, {}, "im_pick_source: the engine is shutting down");
          req->detail = TakeDetail();
        } else {
          RunPick(*req);
        }
        ::SetEvent(req->done.get());
      }
      if (stop) {
        break;
      }
    }
    ::DestroyWindow(owner);
    owner = nullptr;
  }
  ::CoUninitialize();
  engine::os::LeaveThread(token);
  ::SetEvent(exited.get());
}

void Picker::Thread::RunPick(Request &req) noexcept {
  const bool own_window = req.parent == nullptr;
  im_status st = IM_ERR_INTERNAL;
  try {
    st = [&]() -> im_status {
      if (!WgcSupported()) {
        return abi::Fail(IM_ERR_UNSUPPORTED_OS, abi::token::kOsTooOld,
                         "im_pick_source: Windows.Graphics.Capture is not supported on this system");
      }
      if (!own_window && !::IsWindow(req.parent)) {
        return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_pick_source: params->parent_window is not a window");
      }
      HWND parent = req.parent;
      if (own_window) {
        PlaceOwnerWindow(owner);
        ::ShowWindow(owner, SW_SHOWNOACTIVATE);
        parent = owner;
      }
      wgc::GraphicsCapturePicker picker;
      winrt::check_hresult(picker.as<::IInitializeWithWindow>()->Initialize(parent));
      auto op = picker.PickSingleItemAsync();
      AsyncWait wait;
      wait.Attach(op);
      const AsyncWait::Result r = wait.Wait(req.timeout_ms, cancel.get());
      if (r != AsyncWait::Result::kCompleted) {
        op.Cancel();
        wait.Wait(kCancelGraceMs, nullptr);
        switch (r) {
        case AsyncWait::Result::kTimeout:
          return abi::Failf(IM_ERR_CANCELLED, {}, "im_pick_source: no pick within %u ms",
                            static_cast<unsigned>(req.timeout_ms));
        case AsyncWait::Result::kCancelled:
          return abi::Fail(IM_ERR_CANCELLED, {}, "im_pick_source: cancelled (im_cancel_pick)");
        default:
          return abi::Failf(IM_ERR_INTERNAL, {}, "im_pick_source: waiting for the picker failed (%lu)",
                            static_cast<unsigned long>(::GetLastError()));
        }
      }
      switch (op.Status()) {
      case winrt::Windows::Foundation::AsyncStatus::Completed:
        break;
      case winrt::Windows::Foundation::AsyncStatus::Canceled:
        return abi::Fail(IM_ERR_CANCELLED, {}, "im_pick_source: the pick was cancelled");
      case winrt::Windows::Foundation::AsyncStatus::Error:
        return FailHr(op.ErrorCode(), "PickSingleItemAsync");
      default:
        return abi::Fail(IM_ERR_INTERNAL, {}, "im_pick_source: the picker ended in an unknown state");
      }
      const wgc::GraphicsCaptureItem item = op.GetResults();
      if (!item) {
        return abi::Fail(IM_ERR_CANCELLED, {}, "im_pick_source: the user closed the picker");
      }
      return Describe(item, &req.result);
    }();
  } catch (const winrt::hresult_error &e) {
    st = FailHr(e.code(), "the picker");
  } catch (const std::exception &e) {
    st = abi::Failf(IM_ERR_INTERNAL, {}, "im_pick_source: unexpected exception: %s", e.what());
  } catch (...) {
    st = abi::Fail(IM_ERR_INTERNAL, {}, "im_pick_source: unexpected exception");
  }
  if (own_window) {
    ::ShowWindow(owner, SW_HIDE);
  }
  req.status = st;
  if (st != IM_OK) {
    req.result = PickResult{};
    req.detail = TakeDetail();
  }
}

im_status Picker::Thread::Describe(const wgc::GraphicsCaptureItem &item, PickResult *out) {
  const winrt::Windows::Graphics::SizeInt32 size = item.Size();
  if (size.Width <= 0 || size.Height <= 0) {
    return abi::Fail(IM_ERR_SOURCE_GONE, {}, "im_pick_source: the picked source has no size");
  }
  const winrt::hstring name = item.DisplayName();
  const PixelSize item_size{size.Width, size.Height};

  // §7.6 then §7.2 step 4: a window first, then a monitor of that size, else a window of unknown identity.
  Win32WindowSystem ws;
  WindowIdentity id = MatchWindow(ws, std::wstring_view(name), item_size);
  const std::vector<PixelSize> monitors = MonitorSizes();
  out->kind = ClassifyPick(id, monitors, item_size);
  out->self = id.identified() && IsSelfProcess(ProcessTable(), id.pid, ::GetCurrentProcessId());
  out->borderless = BorderlessAllowed();
  out->width = static_cast<uint32_t>(size.Width);
  out->height = static_cast<uint32_t>(size.Height);
  out->name = Utf8FromWide(std::wstring_view(name));
  auto picked = std::make_shared<PickedItem>();
  picked->item = item;
  picked->identity = id;

  // §7.1, §7.5: a monitor is mapped to its DXGI output (Desktop Duplication on Windows 10; on Windows 11 the output is
  // kept for the DD fallback and monitor_id).
  PickFacts facts;
  facts.kind = out->kind;
  facts.windows11 = WindowsBuild() >= kWindows11Build;
  facts.borderless = out->borderless;
  facts.self = out->self;
  MonitorMatch match;
  if (out->kind == IM_SOURCE_MONITOR) {
    Win32Outputs outputs;
    const std::vector<DisplayOutput> list = outputs.Outputs();
    match = MatchPickedMonitor(list, item_size, out->name);
    facts.match = &match;
    picked->output = match.output;
    // Step 5 logs at info; neither the item's name nor an output's is logged.
    log::Info("capture", match.output ? "monitor mapped" : "monitor not mapped",
              {{"outcome", MapOutcomeName(match.result.outcome)},
               {"outputs", list.size()},
               {"size_matches", match.result.size_matches},
               {"name_matches", match.result.name_matches},
               {"windows11", facts.windows11}});
  }
  out->plan = PlanPick(facts);
  out->picked = std::move(picked);
  // Never the title or the process (README §4 "Logging").
  log::Info("capture", "picked",
            {{"kind", out->kind},
             {"w", out->width},
             {"h", out->height},
             {"identified", id.identified()},
             {"pid_known", id.pid != 0},
             {"self", out->self},
             {"borderless", out->borderless},
             {"path", out->plan.capture_path}});
  return IM_OK;
}

Picker::Picker() = default;
Picker::~Picker() = default;

im_status Picker::Pick(HWND parent, uint32_t timeout_ms, PickResult *out) {
  auto req = std::make_shared<Request>();
  if (!req->done) {
    return abi::Fail(IM_ERR_INTERNAL, {}, "im_pick_source: CreateEvent failed");
  }
  req->parent = parent;
  req->timeout_ms = timeout_ms == 0 ? kDefaultPickTimeoutMs : std::min(timeout_ms, kMaxPickTimeoutMs);

  std::shared_ptr<Thread> t;
  {
    std::lock_guard<std::mutex> lock(mu_);
    if (busy_) {
      return abi::Fail(IM_ERR_LIMIT, abi::token::kPickBusy, "im_pick_source: another pick is open");
    }
    if (thread_ && ::WaitForSingleObject(thread_->exited.get(), 0) == WAIT_OBJECT_0) {
      // The thread's loop ended on its own (WAIT_FAILED): reap it and start a fresh one.
      if (thread_->th.joinable()) {
        thread_->th.join();
      }
      thread_.reset();
    }
    if (!thread_) {
      auto fresh = std::make_shared<Thread>();
      if (!fresh->ok()) {
        return abi::Fail(IM_ERR_INTERNAL, {}, "im_pick_source: CreateEvent failed");
      }
      std::promise<std::string> ready;
      std::future<std::string> answer = ready.get_future();
      fresh->th = std::thread([fresh, r = std::move(ready)]() mutable { fresh->Run(std::move(r)); });
      if (answer.wait_for(std::chrono::milliseconds(kThreadReadyMs)) != std::future_status::ready) {
        fresh->th.detach(); // it holds its own reference
        return abi::Fail(IM_ERR_INTERNAL, {}, "im_pick_source: the picker thread did not start");
      }
      const std::string why = answer.get();
      if (!why.empty()) {
        fresh->th.join();
        return abi::Failf(IM_ERR_INTERNAL, {}, "im_pick_source: picker thread: %s", why.c_str());
      }
      thread_ = std::move(fresh);
    }
    t = thread_;
    busy_ = true;
    ::ResetEvent(t->cancel.get());
    {
      std::lock_guard<std::mutex> tlock(t->mu);
      t->pending = req;
    }
    ::SetEvent(t->wake.get());
  }

  DWORD bound = req->timeout_ms + kCancelGraceMs + kAnswerMarginMs;
  DWORD r = ::WaitForSingleObject(req->done.get(), bound);
  if (r != WAIT_OBJECT_0) {
    ::SetEvent(t->cancel.get()); // the picker thread is late: cancel and give it the grace once more
    r = ::WaitForSingleObject(req->done.get(), kCancelGraceMs + kAnswerMarginMs);
  }
  std::shared_ptr<Thread> abandoned;
  {
    std::lock_guard<std::mutex> lock(mu_);
    busy_ = false;
    if (r != WAIT_OBJECT_0 && thread_ == t) {
      // The thread is stuck: abandon it so that the next pick gets a fresh one instead of queueing behind it.
      abandoned = std::move(thread_);
      thread_.reset();
    }
  }
  if (abandoned) {
    {
      std::lock_guard<std::mutex> tlock(abandoned->mu);
      abandoned->quit = true;
    }
    ::SetEvent(abandoned->cancel.get());
    ::SetEvent(abandoned->wake.get());
    if (abandoned->th.joinable()) {
      abandoned->th.detach(); // it holds its own reference and ends when it gets back to its loop; never killed
    }
  }
  if (r != WAIT_OBJECT_0) {
    log::Error("capture", "picker thread did not answer", {{"bound_ms", bound}});
    return abi::Fail(IM_ERR_TIMEOUT, abi::token::kEngineTimeout, "im_pick_source: the picker thread did not answer");
  }
  if (req->status != IM_OK) {
    return abi::Fail(req->status, {}, req->detail); // the detail already starts with its token
  }
  *out = std::move(req->result);
  return IM_OK;
}

void Picker::Cancel() noexcept {
  std::lock_guard<std::mutex> lock(mu_);
  if (busy_ && thread_) {
    ::SetEvent(thread_->cancel.get());
  }
}

void Picker::Shutdown() noexcept {
  std::shared_ptr<Thread> t;
  {
    std::lock_guard<std::mutex> lock(mu_);
    t = std::move(thread_);
    thread_.reset();
  }
  if (!t) {
    return;
  }
  {
    std::lock_guard<std::mutex> lock(t->mu);
    t->quit = true;
  }
  ::SetEvent(t->cancel.get());
  ::SetEvent(t->wake.get());
  // An open pick winds down within kCancelGraceMs; the identity match and the borderless query add little.
  if (::WaitForSingleObject(t->exited.get(), kCancelGraceMs + kAnswerMarginMs) == WAIT_OBJECT_0) {
    if (t->th.joinable()) {
      t->th.join();
    }
    return;
  }
  log::Error("capture", "picker thread abandoned at shutdown");
  if (t->th.joinable()) {
    t->th.detach(); // it holds its own reference; never killed
  }
}

Picker &ThePicker() noexcept {
  static Picker *const p = new Picker(); // never destroyed, like the engine
  return *p;
}

} // namespace im::capture
