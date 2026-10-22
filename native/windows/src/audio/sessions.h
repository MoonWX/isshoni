// Endpoints, audio sessions and their notifications (docs/m2/05-windows-audio.md §5.6, §7.1, §7.2, §7.5; "05"
// below): every active render and capture endpoint with its IAudioSessionManager2, every non-expired session on it
// with its IAudioSessionEvents sink and IAudioMeterInformation, and the IMMNotificationClient for device changes.
//
// Threads: an EndpointSet belongs to the watcher thread (COM MTA), which makes every COM call, registrations and
// unregistrations included, never from inside a callback (05 §7.2). The sinks run on Windows' threads and only bump
// Signals and wake the watcher (05 §7.1: no COM call, no lock held, no waiting).
#pragma once

#include <atomic>
#include <cstddef>
#include <cstdint>
#include <memory>
#include <string>
#include <vector>

#include "audio/win_audio.h"
#include "engine/os.h"
#include "im/audiopolicy/planner.h"

namespace im::audio {

// What the callbacks tell the watcher: counters since the watcher last looked, and the wake-up.
struct Signals {
  std::atomic<uint32_t> sessions{0};        // a session was created, changed state or disconnected
  std::atomic<int64_t> first_session_ns{0}; // engine clock of the first of them (the 20 ms debounce, 05 §7.3)
  std::atomic<uint32_t> devices{0};         // an endpoint was added, removed or changed state
  std::atomic<uint32_t> added{0};
  std::atomic<uint32_t> removed{0};
  std::atomic<uint32_t> default_render{0}; // the default render endpoint (eConsole) changed
  engine::os::Waiter waiter;               // the watcher thread waits on it

  // From the callbacks.
  void OnSession() noexcept;
  void OnDevice(std::atomic<uint32_t> *kind) noexcept;
};

// The device notification client (05 §7.5), registered by the watcher with RegisterEndpointNotificationCallback.
win::ComPtr<IMMNotificationClient> NewDeviceNotifier(std::shared_ptr<Signals> signals);

// One audio session as the watcher keeps it (05 §7.2).
struct TrackedSession {
  std::wstring key; // GetSessionInstanceIdentifier: unique per session instance
  uint32_t pid = 0;
  bool multi_process = false; // GetProcessId answered AUDCLNT_S_NO_SINGLE_PROCESS
  audiopolicy::Flow flow = audiopolicy::Flow::kRender;
  AudioSessionState state = AudioSessionStateInactive;
  win::ComPtr<IAudioSessionControl> control;
  win::ComPtr<IAudioSessionEvents> sink; // registered while non-null
  win::ComPtr<IAudioMeterInformation> meter;
  float peak_db = -120.0f;    // the meter's peak at the last sample (render sessions)
  int64_t last_active_ns = 0; // engine clock of its last activity: active, or the meter above -70 dBFS (05 §12.1)
  bool playing = false;       // activity within the last 2 s
  bool fresh = false;         // seen by the latest enumeration
  bool announced = false;     // its session_created event went out (it needs the app id of a plan)
  std::string app_id;         // from the latest plan ("" while pending)
};

// The report's apps[].playing window (05 §12.1).
inline constexpr int64_t kPlayingWindowNs = 2'000'000'000;
// An activity meter above this counts as playing (05 §12.1).
inline constexpr float kPlayingThresholdDb = -70.0f;

class EndpointSet {
public:
  explicit EndpointSet(std::shared_ptr<Signals> signals);
  ~EndpointSet();
  EndpointSet(const EndpointSet &) = delete;
  EndpointSet &operator=(const EndpointSet &) = delete;

  // Lists the active endpoints of both flows (05 §7.2) and keeps the ones it already has. A new endpoint gets its
  // IAudioSessionManager2 and session enumerator and, when notify, a registered IAudioSessionNotification; a gone one
  // is unregistered and released with its sessions. Returns S_OK or the first failure (AUDCLNT_E_SERVICE_NOT_RUNNING
  // means the audio service went away, 05 §7.5).
  HRESULT Rebuild(IMMDeviceEnumerator *en, bool notify);

  // Re-enumerates every endpoint's sessions: skips the system-sounds session, expired sessions and pid 0 (05 §7.2);
  // a new one gets, when notify, a registered IAudioSessionEvents sink, and a render one its meter; one that is gone
  // is unregistered and moved to gone(). Returns S_OK or the first failure that is not one endpoint's alone.
  HRESULT RefreshSessions(bool notify, int64_t now_ns);

  // The 100 ms sample (05 §12.2): each render session's state and meter peak, its last activity and playing flag.
  // Returns whether any playing flag changed.
  bool Sample(int64_t now_ns);

  // Unregisters and releases everything.
  void Clear();

  uint32_t render_count() const noexcept;
  uint32_t capture_count() const noexcept;
  // Whether every render endpoint has its session notification registered (IM_AUDIO_SESSION_EVENTS, 05 §11.1).
  bool all_render_registered() const noexcept;

  // Every tracked session, in a stable order (by endpoint, then first seen).
  std::vector<TrackedSession *> sessions();
  // The sessions that went away since the last call (their app ids for session_expired events).
  std::vector<std::string> TakeGone();

private:
  struct Endpoint;
  void Release(Endpoint &ep);
  static void ReleaseSession(TrackedSession &s);

  std::shared_ptr<Signals> signals_;
  std::vector<std::unique_ptr<Endpoint>> endpoints_;
  std::vector<std::string> gone_;
};

} // namespace im::audio
