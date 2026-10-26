// The session area's entry points (session.h): the handle table and the limits of 04 §6.4 around session_object.h.
#include "session/session.h"

#include <atomic>
#include <memory>
#include <string_view>
#include <utility>

#include "engine/engine.h"
#include "im/abi/handles.h"
#include "im/abi/status.h"
#include "im/log/ring.h"
#include "session/session_object.h"
#include "session/sources.h"

namespace im::session {
namespace {

using Table = abi::HandleTable<Session, 16>;

Table &Sessions() noexcept {
  static Table *const t = new Table(); // never destroyed, like the engine (src/engine/engine.h)
  return *t;
}

std::atomic<bool> g_probe_stale{false};

// The live session behind s, or IM_ERR_INVALID_ARG naming fn.
im_status Find(const char *fn, im_session s, std::shared_ptr<Session> *out) {
  *out = Sessions().Get(s);
  if (*out) {
    return IM_OK;
  }
  return abi::Failf(IM_ERR_INVALID_ARG, {}, "%s: no session 0x%016llx (stopped, or never started)", fn,
                    static_cast<unsigned long long>(s));
}

} // namespace

im_status Start(const api::StartRequest &req, StartResult &out) {
  const im_start_config &cfg = req.config();
  if (req.audio_only() && cfg.audio.mode == IM_AUDIO_MODE_OFF) {
    return abi::Fail(IM_ERR_INVALID_ARG, {}, "im_start: no source and audio off: the session would capture nothing");
  }
  // The limits first, so a start beyond them opens nothing (the insert below re-checks the count atomically).
  const auto live = Sessions().Snapshot();
  if (live.size() >= kMaxSessions) {
    return abi::Failf(IM_ERR_LIMIT, abi::token::kSessionLimit, "im_start: the engine's %u sessions are in use",
                      static_cast<unsigned>(kMaxSessions));
  }
  if (cfg.audio.mode != IM_AUDIO_MODE_OFF) {
    for (const auto &entry : live) {
      if (entry.second->wants_audio()) {
        return abi::Fail(IM_ERR_LIMIT, abi::token::kSessionLimit,
                         "im_start: another session has audio (one per process: 05's mixer)");
      }
    }
  }

  SourceSpec source;
  uint64_t serial = 0;
  const bool video = !req.audio_only();
  if (video) {
    const bool listed = (cfg.flags & IM_START_ALLOW_LISTED_SOURCE) != 0;
    if (const im_status st = Sources().Resolve(cfg.source_id, listed, &source, &serial); st != IM_OK) {
      return st;
    }
  }

  auto s = std::make_shared<Session>();
  if (const im_status st = s->Start(req, video ? &source : nullptr, engine::CurrentOptions().init_flags); st != IM_OK) {
    return st; // a failed start leaves the pick token valid (04 §7.2)
  }
  const im_session h = Sessions().Insert(s, kMaxSessions);
  if (h == 0) {
    s->Stop();
    return abi::Failf(IM_ERR_LIMIT, abi::token::kSessionLimit, "im_start: the engine's %u sessions are in use",
                      static_cast<unsigned>(kMaxSessions));
  }
  Sources().Consume(serial); // single use: a successful start consumes the pick
  out.handle = h;
  s->FillInfo(out.info);
  return IM_OK;
}

im_status GetInfo(im_session s, im_session_info &info) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_get_session_info", s, &p); st != IM_OK) {
    return st;
  }
  p->FillInfo(info);
  return IM_OK;
}

im_status NextPacket(im_session s, uint32_t timeout_ms, im_packet &out) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_next_packet", s, &p); st != IM_OK) {
    return st;
  }
  return p->Next(timeout_ms, out);
}

im_status RequestKeyframe(im_session s, uint32_t layer) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_request_keyframe", s, &p); st != IM_OK) {
    return st;
  }
  return p->RequestKeyframe(layer);
}

im_status SetBitrate(im_session s, uint32_t layer, uint32_t bps) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_bitrate", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetBitrate(layer, bps);
}

im_status SetLayerActive(im_session s, uint32_t layer, bool active) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_layer_active", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetLayerActive(layer, active);
}

im_status UpdateLayer(im_session s, uint32_t layer, const im_layer_config &cfg) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_update_layer", s, &p); st != IM_OK) {
    return st;
  }
  return p->UpdateLayer(layer, cfg);
}

im_status SetProfile(im_session s, uint32_t profile) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_profile", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetProfile(profile);
}

im_status SetContentHint(im_session s, uint32_t hint) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_content_hint", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetContentHint(hint);
}

im_status SetPaused(im_session s, bool paused) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_paused", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetPaused(paused);
}

im_status SetAudioBitrate(im_session s, uint32_t bps) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_set_audio_bitrate", s, &p); st != IM_OK) {
    return st;
  }
  return p->SetAudioBitrate(bps);
}

im_status GetStats(im_session s, std::string &json) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("im_get_stats", s, &p); st != IM_OK) {
    return st;
  }
  return p->Stats(json);
}

im_status Stop(im_session s) {
  std::shared_ptr<Session> p = Sessions().Remove(s);
  if (!p) {
    return abi::Failf(IM_ERR_INVALID_ARG, {}, "im_stop: no session 0x%016llx (stopped, or never started)",
                      static_cast<unsigned long long>(s));
  }
  p->Stop();
  return IM_OK;
}

void StopAll() noexcept {
  try {
    for (auto &entry : Sessions().RemoveAll()) {
      entry.second->Stop();
    }
  } catch (...) {
    log::Error("session", "stopping the sessions failed (out of memory)");
  }
  Sources().Clear();
}

bool ProbeCacheStale() noexcept { return g_probe_stale.load(std::memory_order_acquire); }
void MarkProbeCacheStale() noexcept { g_probe_stale.store(true, std::memory_order_release); }
void ResetProbeCacheStale() noexcept { g_probe_stale.store(false, std::memory_order_release); }

#if defined(IM_TEST_HOOKS) && IM_TEST_HOOKS
im_status InjectFault(im_session s, uint32_t layer, InjectedFault fault) {
  std::shared_ptr<Session> p;
  if (const im_status st = Find("inject", s, &p); st != IM_OK) {
    return st;
  }
  return p->Inject(layer, fault);
}

uint64_t DeviceOf(im_session s) {
  const std::shared_ptr<Session> p = Sessions().Get(s);
  return p ? p->device() : 0;
}

bool GpuPathOf(im_session s) {
  const std::shared_ptr<Session> p = Sessions().Get(s);
  return p && p->gpu_path();
}
#endif

} // namespace im::session
