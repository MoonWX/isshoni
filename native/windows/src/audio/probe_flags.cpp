#include "audio/probe_flags.h"

#if defined(_MSC_VER)
#pragma warning(push, 3)
#endif
#include <audioclientactivationparams.h> // Windows SDK 10.0.20348 and later (04 §13.1 pins 10.0.26100)
#if defined(_MSC_VER)
#pragma warning(pop)
#endif

#include <cstring> // PropVariantInit is a memset
#include <utility>

namespace im::audio {
namespace {

using ActivateAudioInterfaceAsyncFn = HRESULT(STDAPICALLTYPE *)(LPCWSTR, REFIID, PROPVARIANT *,
                                                                IActivateAudioInterfaceCompletionHandler *,
                                                                IActivateAudioInterfaceAsyncOperation **);

// mmdevapi.dll's ActivateAudioInterfaceAsync, loaded from System32 at run time (no import-table entry).
ActivateAudioInterfaceAsyncFn ActivateFn() noexcept {
  static const auto fn = reinterpret_cast<ActivateAudioInterfaceAsyncFn>(
      reinterpret_cast<void *>(win::SystemProc(L"mmdevapi.dll", "ActivateAudioInterfaceAsync")));
  return fn;
}

// The completion handler of one activation: it keeps the result and signals done. Agile (IAgileObject), so Windows
// calls it on any MTA thread without marshaling.
class CompletionHandler final : public win::Sink<IActivateAudioInterfaceCompletionHandler> {
public:
  CompletionHandler() : done_(CreateEventW(nullptr, TRUE, FALSE, nullptr)) {}

  HRESULT STDMETHODCALLTYPE ActivateCompleted(IActivateAudioInterfaceAsyncOperation *op) override {
    HRESULT activated = E_FAIL;
    win::ComPtr<IUnknown> unk;
    HRESULT hr = op != nullptr ? op->GetActivateResult(&activated, unk.Put()) : E_POINTER;
    if (SUCCEEDED(hr) && SUCCEEDED(activated) && unk) {
      win::ComPtr<IAudioClient> client;
      hr = unk.As(client);
      if (SUCCEEDED(hr)) {
        client_ = std::move(client);
      }
    } else if (SUCCEEDED(hr)) {
      hr = FAILED(activated) ? activated : E_NOINTERFACE;
    }
    result_ = hr;
    SetEvent(done_.get());
    return S_OK;
  }

  HANDLE done() const noexcept { return done_.get(); }
  // Valid once done is signaled.
  HRESULT result() const noexcept { return result_; }
  win::ComPtr<IAudioClient> TakeClient() noexcept { return std::exchange(client_, {}); }

private:
  bool Agile(REFIID riid) const noexcept override { return IsEqualIID(riid, __uuidof(IAgileObject)) != 0; }

  win::Handle done_;
  HRESULT result_ = E_PENDING;
  win::ComPtr<IAudioClient> client_;
};

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
  const ActivateAudioInterfaceAsyncFn activate = ActivateFn();
  if (activate == nullptr) {
    return E_NOTIMPL;
  }
  AUDIOCLIENT_ACTIVATION_PARAMS params{};
  params.ActivationType = AUDIOCLIENT_ACTIVATION_TYPE_PROCESS_LOOPBACK;
  params.ProcessLoopbackParams.TargetProcessId = static_cast<DWORD>(pid);
  params.ProcessLoopbackParams.ProcessLoopbackMode = include_tree ? PROCESS_LOOPBACK_MODE_INCLUDE_TARGET_PROCESS_TREE
                                                                  : PROCESS_LOOPBACK_MODE_EXCLUDE_TARGET_PROCESS_TREE;
  PROPVARIANT pv;
  PropVariantInit(&pv);
  pv.vt = VT_BLOB;
  pv.blob.cbSize = sizeof params;
  pv.blob.pBlobData = reinterpret_cast<BYTE *>(&params); // not owned: no PropVariantClear

  // The handler lives until its last reference goes: ours (released below) and Windows' own while the activation
  // runs, so a completion after a timeout touches only the handler.
  auto handler = win::ComPtr<CompletionHandler>::Attach(new CompletionHandler());
  if (handler->done() == nullptr) {
    return HRESULT_FROM_WIN32(GetLastError());
  }
  win::ComPtr<IActivateAudioInterfaceAsyncOperation> op;
  HRESULT hr = activate(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, __uuidof(IAudioClient), &pv, handler.Get(), op.Put());
  if (FAILED(hr)) {
    return hr;
  }
  switch (WaitForSingleObject(handler->done(), timeout_ms)) {
  case WAIT_OBJECT_0:
    hr = handler->result();
    if (SUCCEEDED(hr)) {
      out = handler->TakeClient();
      if (!out) {
        hr = E_NOINTERFACE;
      }
    }
    return hr;
  case WAIT_TIMEOUT:
    return HRESULT_FROM_WIN32(ERROR_TIMEOUT);
  default:
    return HRESULT_FROM_WIN32(GetLastError());
  }
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
