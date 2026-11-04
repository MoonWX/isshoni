#include "audio/probe_flags.h"

#include <chrono>
#include <memory>
#include <utility>

#include "audio/wasapi_capture.h"

namespace im::audio {
namespace {

// Shared-mode float32 48 kHz stereo (05 §8.2's first format), the int16 form the conversion flags accept too.
WAVEFORMATEX Float32Stereo() noexcept {
  WAVEFORMATEX f{};
  f.wFormatTag = WAVE_FORMAT_IEEE_FLOAT;
  f.nChannels = 2;
  f.nSamplesPerSec = 48000;
  f.wBitsPerSample = 32;
  f.nBlockAlign = 8;
  f.nAvgBytesPerSec = 48000 * 8;
  return f;
}

constexpr REFERENCE_TIME kProcessBuffer = 1'000'000;  // 100 ms (S1's process-loopback buffer)
constexpr REFERENCE_TIME kEndpointBuffer = 2'000'000; // 200 ms

} // namespace

HRESULT ActivateProcessLoopback(uint32_t pid, bool include_tree, uint32_t timeout_ms, win::ComPtr<IAudioClient> &out) {
  out.Reset();
  // The capture streams' activation (wasapi_capture.h): its handler holds its own reference, so a completion after a
  // timeout touches only the handler and the Completion.
  std::unique_ptr<LoopbackActivation> act;
  if (const HRESULT hr = LoopbackActivation::Start(pid, include_tree, &act); FAILED(hr)) {
    return hr;
  }
  if (!act->completion()->WaitFor(std::chrono::milliseconds(timeout_ms))) {
    return HRESULT_FROM_WIN32(ERROR_TIMEOUT);
  }
  return act->Result(out);
}

bool ProcessLoopbackWorks(uint32_t pid, uint32_t timeout_ms) {
  const WAVEFORMATEX fmt = Float32Stereo();
  const DWORD base = AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK;
  const DWORD convert = AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;
  for (const DWORD extra : {DWORD{0}, convert}) {
    // A client whose Initialize failed is not reused: each attempt activates a fresh one (S1).
    win::ComPtr<IAudioClient> client;
    if (FAILED(ActivateProcessLoopback(pid, true, timeout_ms, client))) {
      return false; // activation itself failed or timed out: no format makes it work
    }
    if (SUCCEEDED(client->Initialize(AUDCLNT_SHAREMODE_SHARED, base | extra, kProcessBuffer, 0, &fmt, nullptr))) {
      return true;
    }
  }
  return false;
}

bool EndpointLoopbackWorks(IMMDeviceEnumerator *en) {
  if (en == nullptr) {
    return false;
  }
  win::ComPtr<IMMDevice> dev;
  win::ComPtr<IAudioClient> client;
  if (FAILED(en->GetDefaultAudioEndpoint(eRender, eConsole, dev.Put())) ||
      FAILED(dev->Activate(__uuidof(IAudioClient), CLSCTX_ALL, nullptr, client.PutVoid()))) {
    return false;
  }
  WAVEFORMATEX *mix = nullptr;
  if (FAILED(client->GetMixFormat(&mix)) || mix == nullptr) {
    return false;
  }
  const HRESULT hr =
      client->Initialize(AUDCLNT_SHAREMODE_SHARED, AUDCLNT_STREAMFLAGS_LOOPBACK, kEndpointBuffer, 0, mix, nullptr);
  CoTaskMemFree(mix);
  return SUCCEEDED(hr);
}

namespace {

// A session notification that ignores what it hears: the probe only checks that registering works.
class NullSessionNotifier final : public win::Sink<IAudioSessionNotification> {
public:
  HRESULT STDMETHODCALLTYPE OnSessionCreated(IAudioSessionControl *) override { return S_OK; }
};

} // namespace

bool SessionEventsWork(IMMDeviceEnumerator *en) {
  if (en == nullptr) {
    return false;
  }
  win::ComPtr<IMMDeviceCollection> coll;
  UINT n = 0;
  if (FAILED(en->EnumAudioEndpoints(eRender, DEVICE_STATE_ACTIVE, coll.Put())) || FAILED(coll->GetCount(&n)) ||
      n == 0) {
    return false;
  }
  for (UINT i = 0; i < n; ++i) {
    win::ComPtr<IMMDevice> dev;
    win::ComPtr<IAudioSessionManager2> mgr;
    win::ComPtr<IAudioSessionEnumerator> se;
    if (FAILED(coll->Item(i, dev.Put())) ||
        FAILED(dev->Activate(__uuidof(IAudioSessionManager2), CLSCTX_ALL, nullptr, mgr.PutVoid())) ||
        FAILED(mgr->GetSessionEnumerator(se.Put()))) {
      return false;
    }
    auto sink = win::ComPtr<IAudioSessionNotification>::Attach(new NullSessionNotifier());
    if (FAILED(mgr->RegisterSessionNotification(sink.Get()))) {
      return false;
    }
    mgr->UnregisterSessionNotification(sink.Get());
  }
  return true;
}

uint32_t ActiveEndpoints(IMMDeviceEnumerator *en, EDataFlow flow) {
  if (en == nullptr) {
    return 0;
  }
  win::ComPtr<IMMDeviceCollection> coll;
  UINT n = 0;
  if (FAILED(en->EnumAudioEndpoints(flow, DEVICE_STATE_ACTIVE, coll.Put())) || FAILED(coll->GetCount(&n))) {
    return 0;
  }
  return static_cast<uint32_t>(n);
}

} // namespace im::audio
