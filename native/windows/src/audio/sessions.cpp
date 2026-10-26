#include "audio/sessions.h"

#include <algorithm>
#include <string>
#include <utility>

#include "audio/levels.h"

#if !defined(AUDCLNT_S_NO_SINGLE_PROCESS)
#define AUDCLNT_S_NO_SINGLE_PROCESS AUDCLNT_SUCCESS(0x00d)
#endif

namespace im::audio {
namespace {

// The callbacks: they only count and wake (05 §7.1).

class DeviceNotifier final : public win::Sink<IMMNotificationClient> {
public:
  explicit DeviceNotifier(std::shared_ptr<Signals> s) : s_(std::move(s)) {}

  HRESULT STDMETHODCALLTYPE OnDeviceStateChanged(LPCWSTR, DWORD) override {
    s_->OnDevice(nullptr);
    return S_OK;
  }
  HRESULT STDMETHODCALLTYPE OnDeviceAdded(LPCWSTR) override {
    s_->OnDevice(&s_->added);
    return S_OK;
  }
  HRESULT STDMETHODCALLTYPE OnDeviceRemoved(LPCWSTR) override {
    s_->OnDevice(&s_->removed);
    return S_OK;
  }
  HRESULT STDMETHODCALLTYPE OnDefaultDeviceChanged(EDataFlow flow, ERole role, LPCWSTR) override {
    if (flow == eRender && role == eConsole) {
      s_->OnDevice(&s_->default_render);
    }
    return S_OK;
  }
  HRESULT STDMETHODCALLTYPE OnPropertyValueChanged(LPCWSTR, const PROPERTYKEY) override { return S_OK; }

private:
  std::shared_ptr<Signals> s_;
};

class SessionNotifier final : public win::Sink<IAudioSessionNotification> {
public:
  explicit SessionNotifier(std::shared_ptr<Signals> s) : s_(std::move(s)) {}

  HRESULT STDMETHODCALLTYPE OnSessionCreated(IAudioSessionControl *) override {
    s_->OnSession();
    return S_OK;
  }

private:
  std::shared_ptr<Signals> s_;
};

class SessionEvents final : public win::Sink<IAudioSessionEvents> {
public:
  explicit SessionEvents(std::shared_ptr<Signals> s) : s_(std::move(s)) {}

  HRESULT STDMETHODCALLTYPE OnDisplayNameChanged(LPCWSTR, LPCGUID) override { return S_OK; }
  HRESULT STDMETHODCALLTYPE OnIconPathChanged(LPCWSTR, LPCGUID) override { return S_OK; }
  HRESULT STDMETHODCALLTYPE OnSimpleVolumeChanged(float, BOOL, LPCGUID) override { return S_OK; }
  HRESULT STDMETHODCALLTYPE OnChannelVolumeChanged(DWORD, float[], DWORD, LPCGUID) override { return S_OK; }
  HRESULT STDMETHODCALLTYPE OnGroupingParamChanged(LPCGUID, LPCGUID) override { return S_OK; }
  HRESULT STDMETHODCALLTYPE OnStateChanged(AudioSessionState) override {
    s_->OnSession();
    return S_OK;
  }
  HRESULT STDMETHODCALLTYPE OnSessionDisconnected(AudioSessionDisconnectReason) override {
    s_->OnSession();
    return S_OK;
  }

private:
  std::shared_ptr<Signals> s_;
};

std::wstring TakeCoString(LPWSTR s) {
  std::wstring out = s != nullptr ? std::wstring(s) : std::wstring();
  CoTaskMemFree(s);
  return out;
}

} // namespace

void Signals::OnSession() noexcept {
  sessions.fetch_add(1, std::memory_order_acq_rel);
  int64_t expected = 0;
  first_session_ns.compare_exchange_strong(expected, engine::os::QpcNs(), std::memory_order_acq_rel);
  waiter.Wake();
}

void Signals::OnDevice(std::atomic<uint32_t> *kind) noexcept {
  if (kind != nullptr) {
    kind->fetch_add(1, std::memory_order_acq_rel);
  }
  devices.fetch_add(1, std::memory_order_acq_rel);
  waiter.Wake();
}

win::ComPtr<IMMNotificationClient> NewDeviceNotifier(std::shared_ptr<Signals> signals) {
  return win::ComPtr<IMMNotificationClient>::Attach(new DeviceNotifier(std::move(signals)));
}

struct EndpointSet::Endpoint {
  std::wstring id;
  EDataFlow flow = eRender;
  win::ComPtr<IMMDevice> device;
  win::ComPtr<IAudioSessionManager2> manager;
  win::ComPtr<IAudioSessionNotification> notifier; // registered while non-null
  std::vector<std::unique_ptr<TrackedSession>> sessions;
  bool seen = false;
};

EndpointSet::EndpointSet(std::shared_ptr<Signals> signals) : signals_(std::move(signals)) {}

EndpointSet::~EndpointSet() { Clear(); }

void EndpointSet::ReleaseSession(TrackedSession &s) {
  if (s.sink && s.control) {
    s.control->UnregisterAudioSessionNotification(s.sink.Get());
  }
  s.sink.Reset();
  s.meter.Reset();
  s.control.Reset();
}

void EndpointSet::Release(Endpoint &ep) {
  for (auto &s : ep.sessions) {
    ReleaseSession(*s);
    if (s->announced) {
      gone_.push_back(s->app_id);
    }
  }
  ep.sessions.clear();
  if (ep.notifier && ep.manager) {
    ep.manager->UnregisterSessionNotification(ep.notifier.Get());
  }
  ep.notifier.Reset();
  ep.manager.Reset();
  ep.device.Reset();
}

void EndpointSet::Clear() {
  for (auto &ep : endpoints_) {
    Release(*ep);
  }
  endpoints_.clear();
}

HRESULT EndpointSet::Rebuild(IMMDeviceEnumerator *en, bool notify) {
  if (en == nullptr) {
    Clear();
    return E_POINTER;
  }
  HRESULT first_failure = S_OK;
  for (auto &ep : endpoints_) {
    ep->seen = false;
  }
  for (const EDataFlow flow : {eRender, eCapture}) {
    win::ComPtr<IMMDeviceCollection> coll;
    HRESULT hr = en->EnumAudioEndpoints(flow, DEVICE_STATE_ACTIVE, coll.Put());
    UINT n = 0;
    if (SUCCEEDED(hr)) {
      hr = coll->GetCount(&n);
    }
    if (FAILED(hr)) {
      if (first_failure == S_OK) {
        first_failure = hr;
      }
      // Keep this flow's endpoints until a listing works again: a failed enumeration is no proof they are gone.
      for (auto &ep : endpoints_) {
        if (ep->flow == flow) {
          ep->seen = true;
        }
      }
      continue;
    }
    for (UINT i = 0; i < n; ++i) {
      win::ComPtr<IMMDevice> dev;
      LPWSTR raw = nullptr;
      if (FAILED(coll->Item(i, dev.Put())) || FAILED(dev->GetId(&raw))) {
        continue;
      }
      std::wstring id = TakeCoString(raw);
      const auto known = std::find_if(endpoints_.begin(), endpoints_.end(),
                                      [&](const auto &ep) { return ep->flow == flow && ep->id == id; });
      if (known != endpoints_.end()) {
        if ((*known)->manager && (!notify || (*known)->notifier)) {
          (*known)->seen = true;
          continue;
        }
        // Its session manager or registration failed last time: start it over.
        Release(**known);
        endpoints_.erase(known);
      }
      auto ep = std::make_unique<Endpoint>();
      ep->id = std::move(id);
      ep->flow = flow;
      ep->device = dev;
      ep->seen = true;
      hr = dev->Activate(__uuidof(IAudioSessionManager2), CLSCTX_ALL, nullptr, ep->manager.PutVoid());
      if (SUCCEEDED(hr)) {
        // The session enumerator must exist before RegisterSessionNotification delivers anything (05 §7.2).
        win::ComPtr<IAudioSessionEnumerator> se;
        hr = ep->manager->GetSessionEnumerator(se.Put());
      }
      if (SUCCEEDED(hr) && notify) {
        auto sink = win::ComPtr<IAudioSessionNotification>::Attach(new SessionNotifier(signals_));
        hr = ep->manager->RegisterSessionNotification(sink.Get());
        if (SUCCEEDED(hr)) {
          ep->notifier = sink;
        }
      }
      if (FAILED(hr) && first_failure == S_OK) {
        first_failure = hr;
      }
      endpoints_.push_back(std::move(ep));
    }
  }
  for (auto it = endpoints_.begin(); it != endpoints_.end();) {
    if (!(*it)->seen) {
      Release(**it);
      it = endpoints_.erase(it);
    } else {
      ++it;
    }
  }
  return first_failure;
}

HRESULT EndpointSet::RefreshSessions(bool notify, int64_t now_ns) {
  HRESULT result = S_OK;
  for (auto &ep : endpoints_) {
    if (!ep->manager) {
      continue;
    }
    win::ComPtr<IAudioSessionEnumerator> se;
    HRESULT hr = ep->manager->GetSessionEnumerator(se.Put());
    int count = 0;
    if (SUCCEEDED(hr)) {
      hr = se->GetCount(&count);
    }
    if (FAILED(hr)) {
      if (hr == AUDCLNT_E_SERVICE_NOT_RUNNING) {
        result = hr;
      }
      continue; // keep what this endpoint had: the next refresh tries again
    }
    for (auto &s : ep->sessions) {
      s->fresh = false;
    }
    for (int j = 0; j < count; ++j) {
      win::ComPtr<IAudioSessionControl> ctl;
      if (FAILED(se->GetSession(j, ctl.Put())) || !ctl) {
        continue;
      }
      win::ComPtr<IAudioSessionControl2> ctl2;
      if (FAILED(ctl.As(ctl2)) || ctl2->IsSystemSoundsSession() == S_OK) {
        continue; // Windows' notification sounds
      }
      AudioSessionState state = AudioSessionStateExpired;
      if (FAILED(ctl->GetState(&state)) || state == AudioSessionStateExpired) {
        continue;
      }
      DWORD pid = 0;
      const HRESULT pr = ctl2->GetProcessId(&pid);
      if ((pr != S_OK && pr != AUDCLNT_S_NO_SINGLE_PROCESS) || pid == 0) {
        continue;
      }
      LPWSTR raw = nullptr;
      if (FAILED(ctl2->GetSessionInstanceIdentifier(&raw))) {
        continue;
      }
      const std::wstring key = TakeCoString(raw);
      auto known = std::find_if(ep->sessions.begin(), ep->sessions.end(), [&](const auto &s) { return s->key == key; });
      TrackedSession *s = nullptr;
      if (known != ep->sessions.end()) {
        s = known->get();
      } else {
        auto fresh = std::make_unique<TrackedSession>();
        fresh->key = key;
        fresh->flow = ep->flow == eCapture ? audiopolicy::Flow::kCapture : audiopolicy::Flow::kRender;
        fresh->control = ctl;
        if (notify) {
          auto sink = win::ComPtr<IAudioSessionEvents>::Attach(new SessionEvents(signals_));
          if (SUCCEEDED(ctl->RegisterAudioSessionNotification(sink.Get()))) {
            fresh->sink = sink;
          }
        }
        if (fresh->flow == audiopolicy::Flow::kRender) {
          // The activity meter (as EarTrumpet does); without one, playing falls back to the session state (05 §7.2).
          ctl.As(fresh->meter);
        }
        s = fresh.get();
        ep->sessions.push_back(std::move(fresh));
      }
      s->fresh = true;
      s->pid = static_cast<uint32_t>(pid);
      s->multi_process = pr == AUDCLNT_S_NO_SINGLE_PROCESS;
      s->state = state;
      if (state == AudioSessionStateActive) {
        s->last_active_ns = now_ns;
        s->playing = true;
      }
    }
    for (auto it = ep->sessions.begin(); it != ep->sessions.end();) {
      if (!(*it)->fresh) {
        ReleaseSession(**it);
        if ((*it)->announced) {
          gone_.push_back((*it)->app_id);
        }
        it = ep->sessions.erase(it);
      } else {
        ++it;
      }
    }
  }
  return result;
}

bool EndpointSet::Sample(int64_t now_ns) {
  bool changed = false;
  for (auto &ep : endpoints_) {
    if (ep->flow != eRender) {
      continue;
    }
    for (auto &s : ep->sessions) {
      AudioSessionState state = s->state;
      if (s->control && SUCCEEDED(s->control->GetState(&state)) && state != AudioSessionStateExpired) {
        s->state = state;
      }
      float peak = 0.0f;
      s->peak_db = (s->meter && SUCCEEDED(s->meter->GetPeakValue(&peak))) ? PeakToDb(peak) : kSilenceDb;
      if (s->state == AudioSessionStateActive || s->peak_db > kPlayingThresholdDb) {
        s->last_active_ns = now_ns;
      }
      const bool playing = s->last_active_ns != 0 && now_ns - s->last_active_ns <= kPlayingWindowNs;
      changed = changed || playing != s->playing;
      s->playing = playing;
    }
  }
  return changed;
}

uint32_t EndpointSet::render_count() const noexcept {
  return static_cast<uint32_t>(
      std::count_if(endpoints_.begin(), endpoints_.end(), [](const auto &ep) { return ep->flow == eRender; }));
}

uint32_t EndpointSet::capture_count() const noexcept {
  return static_cast<uint32_t>(
      std::count_if(endpoints_.begin(), endpoints_.end(), [](const auto &ep) { return ep->flow == eCapture; }));
}

bool EndpointSet::all_render_registered() const noexcept {
  bool any = false;
  for (const auto &ep : endpoints_) {
    if (ep->flow == eRender) {
      any = true;
      if (!ep->notifier) {
        return false;
      }
    }
  }
  return any;
}

std::vector<TrackedSession *> EndpointSet::sessions() {
  std::vector<TrackedSession *> out;
  for (auto &ep : endpoints_) {
    for (auto &s : ep->sessions) {
      out.push_back(s.get());
    }
  }
  return out;
}

std::vector<std::string> EndpointSet::TakeGone() { return std::exchange(gone_, {}); }

} // namespace im::audio
