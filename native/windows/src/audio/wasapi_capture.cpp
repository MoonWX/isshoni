#include "audio/wasapi_capture.h"

#if defined(_MSC_VER)
#pragma warning(push, 3)
#endif
#include <audioclientactivationparams.h> // Windows SDK 10.0.20348 and later (04 §13.1 pins 10.0.26100)
#if defined(_MSC_VER)
#pragma warning(pop)
#endif

#include <algorithm>
#include <cstring> // PropVariantInit is a memset
#include <utility>

#include "engine/os.h"

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

constexpr REFERENCE_TIME kProcessBuffer = 1'000'000; // 100 ms (S1's process-loopback buffer, 05 §8.2)
constexpr uint32_t kRate = 48000;
constexpr int64_t kNsPerSecond = 1'000'000'000;
constexpr uint32_t kConvertFrames = 4800; // PCM16 is converted in pieces of 100 ms

WAVEFORMATEX Format(bool pcm16) noexcept {
  WAVEFORMATEX f{};
  f.wFormatTag = pcm16 ? WAVE_FORMAT_PCM : WAVE_FORMAT_IEEE_FLOAT;
  f.nChannels = 2;
  f.nSamplesPerSec = kRate;
  f.wBitsPerSample = static_cast<WORD>(pcm16 ? 16 : 32);
  f.nBlockAlign = static_cast<WORD>(f.nChannels * f.wBitsPerSample / 8);
  f.nAvgBytesPerSec = kRate * f.nBlockAlign;
  return f;
}

int64_t FramesNs(uint64_t frames) noexcept { return static_cast<int64_t>(frames * kNsPerSecond / kRate); }

// An initialized, evented process-loopback client (05 §8.2, §8.3).
class WasapiClient final : public CaptureClient {
public:
  WasapiClient(win::ComPtr<IAudioClient> client, win::ComPtr<IAudioCaptureClient> capture, win::Handle event,
               bool pcm16)
      : client_(std::move(client)), capture_(std::move(capture)), event_(std::move(event)), pcm16_(pcm16),
        scratch_(pcm16 ? new float[2 * kConvertFrames] : nullptr) {}

  ~WasapiClient() override {
    if (started_ && client_) {
      client_->Stop();
    }
  }

  void *event() const noexcept override { return event_.get(); }

  int32_t Start() noexcept override {
    const HRESULT hr = client_->Start();
    started_ = SUCCEEDED(hr);
    return static_cast<int32_t>(hr);
  }

  int32_t Drain(audiomix::CaptureRing &ring) noexcept override {
    for (int guard = 0; guard < 64; ++guard) { // a packet is 10 ms: 64 is far more than 100 ms of backlog
      UINT32 next = 0;
      HRESULT hr = capture_->GetNextPacketSize(&next);
      if (FAILED(hr)) {
        return static_cast<int32_t>(hr);
      }
      if (next == 0) {
        return S_OK;
      }
      BYTE *data = nullptr;
      UINT32 frames = 0;
      DWORD flags = 0;
      UINT64 qpc100 = 0;
      hr = capture_->GetBuffer(&data, &frames, &flags, nullptr, &qpc100);
      if (hr == AUDCLNT_S_BUFFER_EMPTY) {
        return S_OK;
      }
      if (FAILED(hr)) {
        return static_cast<int32_t>(hr);
      }
      const int64_t now = engine::os::QpcNs();
      // The QPC time the audio engine mixed the packet's first frame (100 ns units); without one, the read time minus
      // the packet's length (05 §8.3).
      int64_t qpc = static_cast<int64_t>(qpc100) * 100;
      if (qpc100 == 0 || (flags & AUDCLNT_BUFFERFLAGS_TIMESTAMP_ERROR) != 0) {
        qpc = now - FramesNs(frames);
      }
      if ((flags & AUDCLNT_BUFFERFLAGS_DATA_DISCONTINUITY) != 0) {
        ++discontinuities_;
      }
      if (frames > 0) {
        if ((flags & AUDCLNT_BUFFERFLAGS_SILENT) != 0 || data == nullptr) {
          ring.Write(nullptr, frames, qpc, now);
        } else if (!pcm16_) {
          ring.Write(reinterpret_cast<const float *>(data), frames, qpc, now);
        } else {
          const auto *in = reinterpret_cast<const int16_t *>(data);
          for (UINT32 done = 0; done < frames;) {
            const UINT32 n = std::min(frames - done, kConvertFrames);
            for (UINT32 i = 0; i < 2 * n; ++i) {
              scratch_[i] = static_cast<float>(in[2 * done + i]) * (1.0f / 32768.0f);
            }
            ring.Write(scratch_.get(), n, qpc + FramesNs(done), now);
            done += n;
          }
        }
      }
      hr = capture_->ReleaseBuffer(frames);
      if (FAILED(hr)) {
        return static_cast<int32_t>(hr);
      }
    }
    return S_OK;
  }

  uint64_t discontinuities() const noexcept override { return discontinuities_; }

private:
  win::ComPtr<IAudioClient> client_;
  win::ComPtr<IAudioCaptureClient> capture_;
  win::Handle event_;
  const bool pcm16_;
  std::unique_ptr<float[]> scratch_;
  bool started_ = false;
  uint64_t discontinuities_ = 0;
};

class WasapiOpener final : public StreamOpener {
public:
  OpenResult Open(const OpenRequest &req, ActivationContext &ctx) override {
    OpenResult r;
    r.id = req.id;
    if (req.kind == audiopolicy::StreamKind::kEndpoint) {
      r.hr = static_cast<int32_t>(E_NOTIMPL); // the endpoint engine is README WA53's
      return r;
    }
    const bool include = req.kind == audiopolicy::StreamKind::kInclude;
    struct Attempt {
      bool pcm16;
      bool convert;
    };
    // S1's order (05 §8.2), each on a freshly activated client: a client whose Initialize failed is not reused.
    constexpr Attempt kAttempts[] = {{false, false}, {true, false}, {false, true}, {true, true}};
    HRESULT last = E_FAIL;
    for (const Attempt &a : kAttempts) {
      if (ctx.aborted()) {
        r.hr = static_cast<int32_t>(E_ABORT);
        r.aborted = true;
        return r;
      }
      std::unique_ptr<LoopbackActivation> act;
      HRESULT hr = LoopbackActivation::Start(req.pid, include, &act);
      if (FAILED(hr)) {
        r.hr = static_cast<int32_t>(hr); // activation itself failed: no format makes it work
        return r;
      }
      switch (ctx.Wait(act->completion())) {
      case WaitResult::kDone:
        break;
      case WaitResult::kTimeout:
        r.hr = static_cast<int32_t>(HRESULT_FROM_WIN32(ERROR_TIMEOUT));
        r.timed_out = true;
        return r;
      case WaitResult::kAborted:
        r.hr = static_cast<int32_t>(E_ABORT);
        r.aborted = true;
        return r;
      }
      win::ComPtr<IAudioClient> client;
      hr = act->Result(client);
      if (FAILED(hr)) {
        r.hr = static_cast<int32_t>(hr);
        return r;
      }
      const WAVEFORMATEX fmt = Format(a.pcm16);
      DWORD flags = AUDCLNT_STREAMFLAGS_LOOPBACK | AUDCLNT_STREAMFLAGS_EVENTCALLBACK;
      if (a.convert) {
        flags |= AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY;
      }
      hr = client->Initialize(AUDCLNT_SHAREMODE_SHARED, flags, kProcessBuffer, 0, &fmt, nullptr);
      if (FAILED(hr)) {
        last = hr;
        ++r.failed_attempts;
        continue;
      }
      win::Handle event(CreateEventW(nullptr, FALSE, FALSE, nullptr));
      if (!event) {
        r.hr = static_cast<int32_t>(HRESULT_FROM_WIN32(GetLastError()));
        return r;
      }
      hr = client->SetEventHandle(event.get());
      win::ComPtr<IAudioCaptureClient> capture;
      if (SUCCEEDED(hr)) {
        hr = client->GetService(__uuidof(IAudioCaptureClient), capture.PutVoid());
      }
      if (FAILED(hr)) {
        r.hr = static_cast<int32_t>(hr);
        return r;
      }
      r.hr = 0;
      r.format = a.pcm16 ? "pcm16" : "float32";
      r.client = std::make_unique<WasapiClient>(std::move(client), std::move(capture), std::move(event), a.pcm16);
      return r;
    }
    r.hr = static_cast<int32_t>(last);
    return r;
  }
};

} // namespace

// The completion handler of one activation: it keeps the result and completes the Completion. Agile (IAgileObject),
// so Windows calls it on any MTA thread without marshaling.
class LoopbackActivation::Handler final : public win::Sink<IActivateAudioInterfaceCompletionHandler> {
public:
  explicit Handler(std::shared_ptr<Completion> done) : done_(std::move(done)) {}

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
    done_->Complete(); // publishes result_ and client_ to the waiter (the Completion's mutex orders them)
    return S_OK;
  }

  HRESULT result() const noexcept { return result_; }
  win::ComPtr<IAudioClient> TakeClient() noexcept { return std::exchange(client_, {}); }

private:
  bool Agile(REFIID riid) const noexcept override { return IsEqualIID(riid, __uuidof(IAgileObject)) != 0; }

  std::shared_ptr<Completion> done_;
  HRESULT result_ = E_PENDING;
  win::ComPtr<IAudioClient> client_;
};

HRESULT LoopbackActivation::Start(uint32_t pid, bool include_tree, std::unique_ptr<LoopbackActivation> *out) {
  out->reset();
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

  std::unique_ptr<LoopbackActivation> a(new LoopbackActivation());
  a->done_ = std::make_shared<Completion>();
  // Windows holds its own reference to the handler while the activation runs.
  a->handler_ = win::ComPtr<IActivateAudioInterfaceCompletionHandler>::Attach(new Handler(a->done_));
  const HRESULT hr =
      activate(VIRTUAL_AUDIO_DEVICE_PROCESS_LOOPBACK, __uuidof(IAudioClient), &pv, a->handler_.Get(), a->op_.Put());
  if (FAILED(hr)) {
    return hr;
  }
  *out = std::move(a);
  return S_OK;
}

LoopbackActivation::~LoopbackActivation() = default;

HRESULT LoopbackActivation::Result(win::ComPtr<IAudioClient> &out) {
  out.Reset();
  if (!done_->done()) {
    return E_PENDING;
  }
  auto *h = static_cast<Handler *>(handler_.Get());
  const HRESULT hr = h->result();
  if (SUCCEEDED(hr)) {
    out = h->TakeClient();
    if (!out) {
      return E_NOINTERFACE;
    }
  }
  return hr;
}

std::shared_ptr<StreamOpener> NewWasapiOpener() { return std::make_shared<WasapiOpener>(); }

void ActivatorThreadHook(void *, bool starting) {
  // Per thread: the apartment's HRESULT and the name token (engine/os.h).
  thread_local HRESULT com = E_FAIL;
  thread_local uintptr_t token = 0;
  if (starting) {
    token = engine::os::EnterThread("isshoni audio activator", engine::os::ThreadClass::kNormal);
    com = CoInitializeEx(nullptr, COINIT_MULTITHREADED);
  } else {
    if (SUCCEEDED(com)) {
      CoUninitialize();
    }
    engine::os::LeaveThread(token);
  }
}

} // namespace im::audio
