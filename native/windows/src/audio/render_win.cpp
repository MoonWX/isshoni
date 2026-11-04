#include "audio/render_win.h"

#include <cstring>
#include <system_error>
#include <thread>
#include <utility>

#include "api/guard.h"
#include "engine/os.h"
#include "im/abi/status.h"

namespace im::audio {
namespace {

constexpr REFERENCE_TIME kRenderBuffer = 1'000'000; // 100 ms

// The DLL's private audio session (AudioSessionGuid of Initialize): the probe, the sound check's own chime and the test
// signals share it, apart from any session of the app's own playback.
constexpr GUID kPrivateSession = {0x6c1f3b2e, 0x9a54, 0x4d1b, {0x8e, 0x0f, 0x3a, 0x7c, 0x5d, 0x2b, 0x9e, 0x41}};

} // namespace

HRESULT WasapiRender::Open(const std::wstring &endpoint_id, std::unique_ptr<WasapiRender> *out) {
  out->reset();
  win::ComPtr<IMMDeviceEnumerator> en;
  HRESULT hr =
      CoCreateInstance(__uuidof(MMDeviceEnumerator), nullptr, CLSCTX_ALL, __uuidof(IMMDeviceEnumerator), en.PutVoid());
  if (FAILED(hr)) {
    return hr;
  }
  win::ComPtr<IMMDevice> dev;
  hr = endpoint_id.empty() ? en->GetDefaultAudioEndpoint(eRender, eConsole, dev.Put())
                           : en->GetDevice(endpoint_id.c_str(), dev.Put());
  if (FAILED(hr)) {
    return hr;
  }
  std::unique_ptr<WasapiRender> r(new WasapiRender());
  hr = dev->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr, r->client_.PutVoid());
  if (FAILED(hr)) {
    return hr;
  }
  WAVEFORMATEX fmt{};
  fmt.wFormatTag = WAVE_FORMAT_IEEE_FLOAT;
  fmt.nChannels = 2;
  fmt.nSamplesPerSec = kSignalRate;
  fmt.wBitsPerSample = 32;
  fmt.nBlockAlign = 8;
  fmt.nAvgBytesPerSec = kSignalRate * 8;
  hr = r->client_->Initialize(AUDCLNT_SHAREMODE_SHARED,
                              AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY |
                                  AUDCLNT_STREAMFLAGS_NOPERSIST,
                              kRenderBuffer, 0, &fmt, &kPrivateSession);
  if (FAILED(hr)) {
    return hr;
  }
  if (FAILED(hr = r->client_->GetBufferSize(&r->buffer_frames_)) ||
      FAILED(hr = r->client_->GetService(__uuidof(IAudioRenderClient), r->render_.PutVoid())) ||
      FAILED(hr = r->client_->GetService(__uuidof(IAudioClock), r->clock_.PutVoid())) ||
      FAILED(hr = r->clock_->GetFrequency(&r->clock_freq_))) {
    return hr;
  }
  if (r->clock_freq_ == 0) {
    return E_UNEXPECTED;
  }
  win::ComPtr<ISimpleAudioVolume> vol;
  if (SUCCEEDED(r->client_->GetService(__uuidof(ISimpleAudioVolume), vol.PutVoid()))) {
    vol->SetMasterVolume(1.0f, nullptr); // 05 §11.2 step 2: a Volume Mixer setting can't fail the probe
    vol->SetMute(FALSE, nullptr);
  }
  LPWSTR wid = nullptr;
  if (SUCCEEDED(dev->GetId(&wid)) && wid != nullptr) {
    r->id_ = win::Utf8(wid);
    CoTaskMemFree(wid);
  }
  *out = std::move(r);
  return S_OK;
}

WasapiRender::~WasapiRender() {
  if (started_ && client_) {
    client_->Stop();
  }
}

int32_t WasapiRender::Writable(uint32_t *frames) noexcept {
  UINT32 padding = 0;
  const HRESULT hr = client_->GetCurrentPadding(&padding);
  *frames = SUCCEEDED(hr) && padding <= buffer_frames_ ? buffer_frames_ - padding : 0;
  return static_cast<int32_t>(hr);
}

int32_t WasapiRender::Write(const float *frames, uint32_t n) noexcept {
  BYTE *data = nullptr;
  HRESULT hr = render_->GetBuffer(n, &data);
  if (FAILED(hr)) {
    return static_cast<int32_t>(hr);
  }
  std::memcpy(data, frames, std::size_t{n} * 2 * sizeof(float));
  hr = render_->ReleaseBuffer(n, 0);
  return static_cast<int32_t>(hr);
}

int32_t WasapiRender::Start() noexcept {
  const HRESULT hr = client_->Start();
  started_ = SUCCEEDED(hr);
  return static_cast<int32_t>(hr);
}

bool WasapiRender::Position(uint64_t *frames, int64_t *qpc_ns) noexcept {
  UINT64 pos = 0;
  UINT64 qpc100 = 0;
  if (FAILED(clock_->GetPosition(&pos, &qpc100))) {
    return false;
  }
  // GetFrequency's units to frames, without overflow: whole seconds, then the rest.
  *frames = pos / clock_freq_ * kSignalRate + (pos % clock_freq_) * kSignalRate / clock_freq_;
  *qpc_ns = static_cast<int64_t>(qpc100) * 100; // 100 ns QPC units: the engine clock (05 §10.3)
  return true;
}

void WasapiRender::Wait(uint32_t ms) noexcept { Sleep(ms); }

im_status RunOnMta(const char *name, const std::function<im_status(std::string *detail)> &fn) {
  im_status st = IM_ERR_INTERNAL;
  std::string detail = "the audio thread failed";
  try {
    std::thread t([&] {
      const uintptr_t token = engine::os::EnterThread(name, engine::os::ThreadClass::kNormal);
      api::Guard(name, [&] {
        const win::ComApartment com;
        if (FAILED(com.hr())) {
          st = IM_ERR_INTERNAL;
          detail = "COM is not available on the audio thread (" + win::Hr(com.hr()) + ")";
          return;
        }
        detail.clear();
        st = fn(&detail);
      });
      engine::os::LeaveThread(token);
    });
    t.join();
  } catch (const std::system_error &e) {
    return abi::Failf(IM_ERR_INTERNAL, {}, "%s: the thread did not start: %s", name, e.what());
  }
  if (st != IM_OK && st < 0) {
    return abi::Fail(st, {}, detail);
  }
  return st;
}

} // namespace im::audio
