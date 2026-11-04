// Windows' LoopbackIo (loopback_io.h) and the startup probe's backend (startup_probe.h): the capture streams of
// wasapi_capture.h read through a ring, the DLL's render client (render_win.h) and the session list, on the calling
// thread (COM MTA).
#include "audio/loopback_io.h"

#include <chrono>
#include <utility>
#include <vector>

#include "audio/render_win.h"
#include "audio/startup_probe.h"
#include "audio/wasapi_capture.h"
#include "audio/win_audio.h"
#include "engine/os.h"
#include "im/audiomix/capture_ring.h"
#include "im/log/ring.h"

namespace im::audio {
namespace {

const HRESULT kNotFound = HRESULT_FROM_WIN32(ERROR_NOT_FOUND); // GetDefaultAudioEndpoint without an endpoint

OpenOutcome OutcomeOf(const OpenResult &r) {
  if (r.hr == 0 && r.client) {
    return OpenOutcome::kOk;
  }
  const auto hr = static_cast<HRESULT>(r.hr);
  if (hr == AUDCLNT_E_DEVICE_IN_USE || r.last_failure == static_cast<int32_t>(AUDCLNT_E_DEVICE_IN_USE)) {
    return OpenOutcome::kBusy;
  }
  if (hr == kNotFound) {
    return OpenOutcome::kNoDevice;
  }
  if (hr == E_NOTIMPL || (r.failed_attempts > 0 && !r.timed_out && !r.aborted)) {
    return OpenOutcome::kUnsupported; // no activation function, or no format initializes (05 §11.3 step 3)
  }
  return OpenOutcome::kFailed;
}

class WinLoopbackIo final : public LoopbackIo {
public:
  explicit WinLoopbackIo(uint32_t activation_timeout_ms) : timeout_(activation_timeout_ms) {}
  ~WinLoopbackIo() override {
    CloseCapture();
    CloseRender();
  }

  OpenOutcome OpenProcessCapture(uint32_t pid, std::string *format) noexcept override {
    return Open(OpenRequest{0, audiopolicy::StreamKind::kInclude, pid}, format);
  }

  OpenOutcome OpenEndpointCapture(std::string *format) noexcept override {
    return Open(OpenRequest{0, audiopolicy::StreamKind::kEndpoint, 0}, format);
  }

  int32_t ReadCapture(std::vector<float> &stereo) noexcept override {
    if (!client_ || !ring_) {
      return static_cast<int32_t>(E_UNEXPECTED);
    }
    const int32_t hr = client_->Drain(*ring_);
    float buf[2 * 960];
    for (uint32_t n = ring_->Peek(buf, 960); n > 0; n = ring_->Peek(buf, 960)) {
      stereo.insert(stereo.end(), buf, buf + 2 * n);
      ring_->Consume(n);
    }
    return hr;
  }

  void CloseCapture() noexcept override {
    client_.reset();
    ring_.reset();
  }

  OpenOutcome OpenRender(RenderTarget **out, std::string *endpoint_id) noexcept override {
    CloseRender();
    const HRESULT hr = WasapiRender::Open(std::wstring(), &render_);
    if (FAILED(hr) || !render_) {
      render_.reset();
      if (hr == AUDCLNT_E_DEVICE_IN_USE) {
        return OpenOutcome::kBusy;
      }
      return OpenOutcome::kNoDevice; // no endpoint, or a client that can't initialize: nothing can play now
    }
    *out = render_.get();
    *endpoint_id = render_->endpoint_id();
    return OpenOutcome::kOk;
  }

  void CloseRender() noexcept override { render_.reset(); }

  // Whether pid has a session on an active render endpoint (05 §7.2's enumeration, without registrations).
  bool SessionSeen(uint32_t pid) noexcept override {
    if (!en_ && FAILED(CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL,
                                        __uuidof(IMMDeviceEnumerator), en_.PutVoid()))) {
      return false;
    }
    win::ComPtr<IMMDeviceCollection> coll;
    UINT n = 0;
    if (FAILED(en_->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE, coll.Put())) || FAILED(coll->GetCount(&n))) {
      return false;
    }
    for (UINT i = 0; i < n; ++i) {
      win::ComPtr<IMMDevice> dev;
      win::ComPtr<IAudioSessionManager2> mgr;
      win::ComPtr<IAudioSessionEnumerator> se;
      int count = 0;
      if (FAILED(coll->Item(i, dev.Put())) ||
          FAILED(dev->Activate(__uuidof(IAudioSessionManager2), CLSCTX_ALL, nullptr, mgr.PutVoid())) ||
          FAILED(mgr->GetSessionEnumerator(se.Put())) || FAILED(se->GetCount(&count))) {
        continue;
      }
      for (int k = 0; k < count; ++k) {
        win::ComPtr<IAudioSessionControl> ctl;
        win::ComPtr<IAudioSessionControl2> ctl2;
        DWORD p = 0;
        if (SUCCEEDED(se->GetSession(k, ctl.Put())) && SUCCEEDED(ctl.As(ctl2)) && SUCCEEDED(ctl2->GetProcessId(&p)) &&
            p == pid) {
          return true;
        }
      }
    }
    return false;
  }

  uint32_t SelfPid() const noexcept override { return static_cast<uint32_t>(GetCurrentProcessId()); }
  int64_t NowNs() noexcept override { return engine::os::QpcNs(); }
  void Wait(uint32_t ms) noexcept override { Sleep(ms); }

private:
  OpenOutcome Open(const OpenRequest &req, std::string *format) noexcept {
    CloseCapture();
    OpenResult r = OpenCaptureStream(
        req,
        [this](const std::shared_ptr<Completion> &c) {
          return c->WaitFor(std::chrono::milliseconds(timeout_)) ? WaitResult::kDone : WaitResult::kTimeout;
        },
        nullptr);
    const OpenOutcome o = OutcomeOf(r);
    if (o != OpenOutcome::kOk) {
      log::Info("audio", "audio check capture did not open",
                {{"hr", static_cast<int64_t>(r.hr)}, {"failed_attempts", static_cast<int64_t>(r.failed_attempts)}});
      return o;
    }
    if (const int32_t hr = r.client->Start(); hr < 0) {
      return OpenOutcome::kFailed;
    }
    client_ = std::move(r.client);
    ring_ = std::make_unique<audiomix::CaptureRing>();
    *format = r.format;
    return OpenOutcome::kOk;
  }

  const uint32_t timeout_;
  std::unique_ptr<CaptureClient> client_;
  std::unique_ptr<audiomix::CaptureRing> ring_;
  std::unique_ptr<WasapiRender> render_;
  win::ComPtr<IMMDeviceEnumerator> en_;
};

class WinProbeBackend final : public ProbeBackend {
public:
  ProbeReport Run(const std::atomic<bool> &abort) noexcept override {
    const uintptr_t token = engine::os::EnterThread("isshoni audio probe", engine::os::ThreadClass::kNormal);
    ProbeReport r;
    {
      const win::ComApartment com;
      if (SUCCEEDED(com.hr())) {
        try {
          const std::unique_ptr<LoopbackIo> io = NewWindowsLoopbackIo(kProbeActivationMs);
          r = RunStartupProbe(*io, &abort);
        } catch (...) {
          r = ProbeReport{}; // skipped: an allocation failed
        }
      }
    }
    engine::os::LeaveThread(token);
    return r;
  }
};

} // namespace

std::unique_ptr<LoopbackIo> NewWindowsLoopbackIo(uint32_t activation_timeout_ms) {
  return std::make_unique<WinLoopbackIo>(activation_timeout_ms);
}

std::shared_ptr<ProbeBackend> NewWindowsProbeBackend() { return std::make_shared<WinProbeBackend>(); }

} // namespace im::audio
